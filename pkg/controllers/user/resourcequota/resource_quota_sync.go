package resourcequota

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	namespaceutil "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/types/apis/core/v1"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/cache"
	clientcache "k8s.io/client-go/tools/cache"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/quota"
)

const (
	projectIDAnnotation             = "field.cattle.io/projectId"
	resourceQuotaLabel              = "resourcequota.management.cattle.io/default-resource-quota"
	resourceQuotaInitCondition      = "ResourceQuotaInit"
	resourceQuotaAnnotation         = "field.cattle.io/resourceQuota"
	resourceQuotaValidatedCondition = "ResourceQuotaValidated"
)

var (
	projectLockCache = cache.NewLRUExpireCache(1000)
)

/*
SyncController takes care of creating Kubernetes resource quota based on the resource limits
defined in namespace.resourceQuota
*/
type SyncController struct {
	ProjectLister       v3.ProjectLister
	Namespaces          v1.NamespaceInterface
	NamespaceLister     v1.NamespaceLister
	ResourceQuotas      v1.ResourceQuotaInterface
	ResourceQuotaLister v1.ResourceQuotaLister
	NsIndexer           clientcache.Indexer
}

func (c *SyncController) syncResourceQuota(key string, ns *corev1.Namespace) error {
	if ns == nil || ns.DeletionTimestamp != nil {
		return nil
	}

	_, err := c.CreateResourceQuota(ns)
	return err
}

func (c *SyncController) CreateResourceQuota(ns *corev1.Namespace) (*corev1.Namespace, error) {
	existing, err := c.getExistingResourceQuota(ns)
	if err != nil {
		return ns, err
	}

	projectLimit, _, err := getProjectResourceQuotaLimit(ns, c.ProjectLister)
	if err != nil {
		return ns, err
	}

	quotaSpec, err := c.getNamespaceResourceQuota(ns, projectLimit != nil)
	if err != nil {
		return ns, err
	}

	operation := "none"
	if existing == nil {
		if quotaSpec != nil {
			operation = "create"
		}
	} else {
		if quotaSpec == nil {
			operation = "delete"
		} else if !reflect.DeepEqual(existing.Spec.Hard, quotaSpec.Hard) {
			operation = "update"
		}
	}

	switch operation {
	case "create":
		isFit, err := c.validateNamespaceQuota(ns)
		if err != nil || !isFit {
			return ns, err
		}
		err = c.createDefaultResourceQuota(ns, quotaSpec)
	case "update":
		isFit, err := c.validateNamespaceQuota(ns)
		if err != nil || !isFit {
			return ns, err
		}
		err = c.updateResourceQuota(existing, quotaSpec)
	case "delete":
		err = c.deleteResourceQuota(existing)
	}
	if err == nil {
		set, err := namespaceutil.IsNamespaceConditionSet(ns, resourceQuotaInitCondition, true)
		if err != nil || set {
			return ns, err
		}
		toUpdate, err := c.Namespaces.Get(ns.Name, metav1.GetOptions{})
		if err != nil {
			return toUpdate, err
		}
		namespaceutil.SetNamespaceCondition(toUpdate, time.Second*1, resourceQuotaInitCondition, true, "")
		return c.Namespaces.Update(toUpdate)
	}

	return ns, err
}

func (c *SyncController) updateResourceQuota(quota *corev1.ResourceQuota, spec *corev1.ResourceQuotaSpec) error {
	toUpdate := quota.DeepCopy()
	toUpdate.Spec = *spec
	logrus.Infof("Updating default resource quota for namespace %v", toUpdate.Namespace)
	_, err := c.ResourceQuotas.Update(toUpdate)
	return err
}

func (c *SyncController) deleteResourceQuota(quota *corev1.ResourceQuota) error {
	logrus.Infof("Deleting default resource quota for namespace %v", quota.Namespace)
	return c.ResourceQuotas.DeleteNamespaced(quota.Namespace, quota.Name, &metav1.DeleteOptions{})
}

func (c *SyncController) getExistingResourceQuota(ns *corev1.Namespace) (*corev1.ResourceQuota, error) {
	set := labels.Set(map[string]string{resourceQuotaLabel: "true"})
	quota, err := c.ResourceQuotaLister.List(ns.Name, set.AsSelector())
	if err != nil {
		return nil, err
	}
	if len(quota) == 0 {
		return nil, nil
	}
	return quota[0], nil
}

func (c *SyncController) getNamespaceResourceQuota(ns *corev1.Namespace, setDefault bool) (*corev1.ResourceQuotaSpec, error) {
	validated := true
	set := getNamespaceResourceQuota(ns) != ""
	var err error
	if set {
		validated, err = isQuotaValidated(ns)
		if err != nil {
			return nil, err
		}
	}

	var limit *v3.ResourceQuotaLimit
	if validated {
		limit, err = getNamespaceLimit(ns)
		if err != nil {
			return nil, err
		}
	} else {
		if setDefault {
			limit = defaultResourceLimit
		} else {
			return nil, nil
		}
	}

	return convertResourceLimitResourceQuotaSpec(limit)
}

var defaultResourceLimit = &v3.ResourceQuotaLimit{
	Pods:                   "0",
	Services:               "0",
	ReplicationControllers: "0",
	Secrets:                "0",
	ConfigMaps:             "0",
	PersistentVolumeClaims: "0",
	ServicesNodePorts:      "0",
	ServicesLoadBalancers:  "0",
	RequestsCPU:            "0",
	RequestsMemory:         "0",
	RequestsStorage:        "0",
	LimitsCPU:              "0",
	LimitsMemory:           "0",
}

func getProjectID(ns *corev1.Namespace) string {
	if ns.Annotations != nil {
		return ns.Annotations[projectIDAnnotation]
	}
	return ""
}

func getProjectNamespaceName(projectID string) (string, string) {
	if projectID == "" {
		return "", ""
	}
	parts := strings.Split(projectID, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

func (c *SyncController) createDefaultResourceQuota(ns *corev1.Namespace, spec *corev1.ResourceQuotaSpec) error {
	resourceQuota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "default-",
			Namespace:    ns.Name,
			Labels:       map[string]string{resourceQuotaLabel: "true"},
		},
		Spec: *spec,
	}
	logrus.Infof("Creating default resource quota for namespace %v", ns.Name)
	_, err := c.ResourceQuotas.Create(resourceQuota)
	return err
}

func (c *SyncController) validateNamespaceQuota(ns *corev1.Namespace) (bool, error) {
	if ns == nil || ns.DeletionTimestamp != nil {
		return true, nil
	}

	// get project limit
	projectLimit, projectID, err := getProjectResourceQuotaLimit(ns, c.ProjectLister)
	if err != nil {
		return false, err
	}

	if projectLimit == nil {
		return true, err
	}

	// set default quota if not set
	quotaToUpdate, err := c.getResourceQuotaToUpdate(ns)
	if err != nil {
		return false, err
	}
	finalNs := *ns
	if quotaToUpdate != "" {
		toUpdate := ns.DeepCopy()
		if toUpdate.Annotations == nil {
			toUpdate.Annotations = map[string]string{}
		}
		toUpdate.Annotations[resourceQuotaAnnotation] = quotaToUpdate
		toUpdate, err = c.Namespaces.Update(toUpdate)
		if err != nil {
			return false, err
		}
		finalNs = *toUpdate
	}

	// validate resource quota
	mu := getProjectLock(projectID)
	mu.Lock()
	defer mu.Unlock()
	isFit, msg, err := c.isQuotaFit(&finalNs, projectID, projectLimit)
	if err != nil {
		return false, err
	}
	return isFit, c.setValidated(ns, isFit, msg)
}

func (c *SyncController) setValidated(ns *corev1.Namespace, value bool, msg string) error {
	set, err := namespaceutil.IsNamespaceConditionSet(ns, resourceQuotaValidatedCondition, value)
	if set || err != nil {
		return err
	}

	toUpdate := ns.DeepCopy()
	err = namespaceutil.SetNamespaceCondition(toUpdate, time.Second*1, resourceQuotaValidatedCondition, value, msg)
	if err != nil {
		return err
	}
	_, err = c.Namespaces.Update(toUpdate)

	return err
}

func getProjectLock(projectID string) *sync.Mutex {
	val, ok := projectLockCache.Get(projectID)
	if !ok {
		projectLockCache.Add(projectID, &sync.Mutex{}, time.Hour)
		val, _ = projectLockCache.Get(projectID)
	}
	mu := val.(*sync.Mutex)
	return mu
}

func (c *SyncController) isQuotaFit(ns *corev1.Namespace, projectID string, projectLimit *v3.ResourceQuotaLimit) (bool, string, error) {
	nssResourceList := api.ResourceList{}
	nsLimit, err := getNamespaceLimit(ns)
	if err != nil {
		return false, "", err
	}
	// add itself on create
	nsResourceList, err := convertLimitToResourceList(nsLimit)
	if err != nil {
		return false, "", err
	}
	nssResourceList = quota.Add(nssResourceList, nsResourceList)

	// get other Namespaces
	namespaces, err := c.NsIndexer.ByIndex(nsByProjectIndex, projectID)
	if err != nil {
		return false, "", err
	}

	for _, n := range namespaces {
		// only consider namespaces who's resource quota is set
		set, err := namespaceutil.IsNamespaceConditionSet(ns, resourceQuotaValidatedCondition, true)
		if err != nil {
			return false, "", err
		}
		if !set {
			continue
		}
		other := n.(*corev1.Namespace)
		if other.Name == ns.Name {
			continue
		}
		nsLimit, err := getNamespaceLimit(other)
		if err != nil {
			return false, "", err
		}
		nsResourceList, err := convertLimitToResourceList(nsLimit)
		if err != nil {
			return false, "", err
		}
		nssResourceList = quota.Add(nssResourceList, nsResourceList)
	}

	projectResourceList, err := convertLimitToResourceList(projectLimit)
	if err != nil {
		return false, "", err
	}

	allowed, exceeded := quota.LessThanOrEqual(nssResourceList, projectResourceList)
	if allowed {
		return true, "", nil
	}
	failedHard := quota.Mask(nssResourceList, exceeded)
	return false, fmt.Sprintf("Resource quota [%v] exceeds project limit ", prettyPrint(failedHard)), nil
}

func prettyPrint(item api.ResourceList) string {
	parts := []string{}
	keys := []string{}
	for key := range item {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := item[api.ResourceName(key)]
		constraint := key + "=" + value.String()
		parts = append(parts, constraint)
	}
	return strings.Join(parts, ",")
}

func (c *SyncController) getResourceQuotaToUpdate(ns *corev1.Namespace) (string, error) {
	if getNamespaceResourceQuota(ns) != "" {
		return "", nil
	}

	quota, err := getProjectNamespaceDefaultQuota(ns, c.ProjectLister)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(quota)
	if err != nil {
		return "", err
	}
	return string(b), nil

}
