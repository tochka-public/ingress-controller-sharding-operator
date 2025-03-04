package controller

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash"
	"github.com/go-logr/logr"
	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	"github.com/prometheus/client_golang/prometheus"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.tochka.com/sharded-ingress-controller/internal/k8s"
	"k8s.tochka.com/sharded-ingress-controller/internal/metrics"
)

//+kubebuilder:rbac:groups=networking.k8s.tochka.com,resources=shardedingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.tochka.com,resources=shardedingresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.tochka.com,resources=shardedingresses/finalizers,verbs=update
//+kubebuilder:rbac:groups=networking.k8s.tochka.com,resources=shardedhttpproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.tochka.com,resources=shardedhttpproxies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.tochka.com,resources=shardedhttpproxies/finalizers,verbs=update
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=projectcontour.io,resources=httpproxies,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.0/pkg/reconcile

type ShardedReconciler struct {
	client.Client
	Scheme                                   *runtime.Scheme
	MaxShards                                map[string]int
	TerminationPeriod                        *time.Duration
	ShardUpdateCooldown                      *time.Duration
	AllShardsBaseHosts                       *[]string
	DomainSubstring                          *string
	MutatingWebhookAnnotation                *string
	UnregisterAnnotation                     *string
	AdditionalServiceDiscoveryClassLabel     *string
	RootHTTPProxyLabel                       *string
	VirtualHostsHTTPProxyAnnotation          *string
	AdditionalServiceDiscoveryTagsAnnotation *string
	AppNameLabel                             *string
	AllShardsPlacementAnnotation             *string
	FinalizerKey                             *string
	FinalizerTerminationPeriod               *time.Duration
	WaitingList                              map[string]bool
	ReadyList                                map[string]bool
	ManagedList                              map[string]bool
	ErrorList                                map[string]bool
	NextApplyTime                            map[string]*applyPlan
	ShardedCache                             *sync.Map
	ChildCache                               *sync.Map
	Initialized                              bool
	req                                      *ctrl.Request
	ctx                                      context.Context
	ShardedObject                            ShardedObject
	ChildObject                              client.Object
	Shards                                   []Shards
	Conflict                                 string
	SoftResharding                           bool
	objKey                                   string
	ctrlName                                 string
	Regular                                  bool
	UseAllShards                             bool
}

type ShardedObject interface {
	client.Object
	GetCreatedObjects() *map[string][]map[string]string
	SetCreatedObjects(map[string][]map[string]string)
	GetObject() client.Object
	GetIngressClassName() string
	GetKind() string
	GetChildKind() string
}

type ChildObject interface {
	client.Object
	contourv1.HTTPProxy
	networkingv1.Ingress
}

type NewChildObj struct {
	Shard     int
	ShardName string
	Obj       client.Object
	OldShard  string
}

type Shards struct {
	ShardNumber int
	ShardName   string
}

type applyPlan struct {
	lastCreating               time.Time
	lastDeleting               time.Time
	currentDeletingWindowStart time.Time
	nextDeletingWindowStart    time.Time
}

const (
	ExponentialBackoffBaseDelay = 5 * time.Millisecond
	ExponentialBackoffMaxDelay  = 1000 * time.Second

	AutoDeleteAfterAnnotation = "auto-delete-after"
)

func (r *ShardedReconciler) createObject(newObject client.Object, shardName string) (ctrl.Result, error) {
	logger := log.FromContext(r.ctx)
	kind := k8s.KindOf(newObject)

	err := r.Client.Create(r.ctx, newObject)
	if err != nil {
		logger.Error(err, "unable to create", "objectKind", kind, "objectName", newObject.GetName())
		r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.WaitingList, r.ReadyList}, []map[string]bool{r.ErrorList})
		return ctrl.Result{}, err
	}

	logger.Info("successfully created", "objectKind", kind, "objectName", newObject.GetName())
	r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.ErrorList}, []map[string]bool{r.ReadyList})
	createdObjects := r.ShardedObject.GetCreatedObjects()
	err = r.addToStatus(kind, newObject.GetName(), shardName, createdObjects)
	if err != nil {
		return ctrl.Result{}, err
	}
	delete(r.WaitingList, r.objKey)
	metrics.ProcessingCounter.WithLabelValues(r.ctrlName, shardName).Inc()
	return ctrl.Result{}, nil
}

func (r *ShardedReconciler) getObjectChildren() (unstructured.UnstructuredList, error) {
	objKind := r.GetChildKind()
	childObjs := unstructured.UnstructuredList{}

	switch objKind {
	case "Ingress":
		childObjs.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "networking.k8s.io",
			Version: "v1",
			Kind:    "IngressList",
		})
	case "HTTPProxy":
		childObjs.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "projectcontour.io",
			Version: "v1",
			Kind:    "HTTPProxyList",
		})
	default:
		return unstructured.UnstructuredList{}, fmt.Errorf("unsupported object kind: %s", objKind)
	}

	if err := r.List(r.ctx, &childObjs, client.InNamespace(r.req.Namespace)); err != nil {
		return unstructured.UnstructuredList{}, err
	}

	res := unstructured.UnstructuredList{}
	for _, childObj := range childObjs.Items {
		for _, owner := range childObj.GetOwnerReferences() {
			if owner.Name == r.ShardedObject.GetName() && owner.Kind == r.ShardedObject.GetKind() {
				res.Items = append(res.Items, childObj)
			}
		}
	}

	return res, nil
}

func (r *ShardedReconciler) deleteUnlistedObjects(currentList map[string][]map[string]string) (ctrl.Result, error) {
	logger := log.FromContext(r.ctx)
	shdObj := r.ShardedObject
	createdObjects := shdObj.GetCreatedObjects()
	childObjs := unstructured.UnstructuredList{}

	if r.GetChildKind() == "" {
		return ctrl.Result{}, nil
	}

	childObjs, err := r.getObjectChildren()
	if err != nil {
		logger.Error(err, "unable to list child objects")
		return ctrl.Result{}, err
	}

	for _, obj := range childObjs.Items {
		keep := false
		var shardName string
		for _, shard := range r.Shards {
			if findInStatus(shard.ShardName, obj.GetKind(), obj.GetName(), &currentList) {
				keep = true
				shardName = shard.ShardName
				break
			}
		}

		if !keep || (strings.HasPrefix(obj.GetName(), shdObj.GetName()) && strings.HasSuffix(obj.GetName(), "tmp")) {
			for shard, status := range *createdObjects {
				for _, objStatus := range status {
					if objStatus["name"] == obj.GetName() {
						shardName = shard
					}
				}
			}
			shouldDelete, err := r.handleDeletionTiming(&obj, shardName)
			if err != nil {
				logger.Error(err, "error handling deletion timing", "objectKind", obj.GetKind(), "objectName", obj.GetName())
				return ctrl.Result{}, err
			}
			if shouldDelete {
				if err := r.Client.Delete(r.ctx, &obj); err != nil {
					logger.Error(err, "unable to delete", "objectKind", obj.GetKind(), "objectName", obj.GetName())
					return ctrl.Result{}, err
				}
				logger.Info("successfully deleted from cluster", "objectKind", obj.GetKind(), "objectName", obj.GetName())
				metrics.ProcessingCounter.WithLabelValues(r.ctrlName, shardName).Inc()
				return ctrl.Result{}, nil
			} else {
				err := r.addToStatus(obj.GetKind(), obj.GetName(), shardName, createdObjects)
				if err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: *r.TerminationPeriod}, nil
			}
		} else {
			err := r.addToStatus(obj.GetKind(), obj.GetName(), shardName, createdObjects)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	for shard, objStatusSlice := range *createdObjects {
		// Check if the object exists in the currentList
		for _, objStatus := range objStatusSlice {
			found := findInStatus(shard, objStatus["kind"], objStatus["name"], &currentList)
			if !found {
				obj := &unstructured.Unstructured{}
				obj.SetKind(objStatus["kind"])
				obj.SetAPIVersion(shdObj.GetObject().GetObjectKind().GroupVersionKind().Version)
				obj.SetNamespace(shdObj.GetNamespace())
				obj.SetName(objStatus["name"])
				if err := r.Client.Get(r.ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, obj); err != nil {
					// If it does not exist, delete the object from the status
					err := r.delFromCreatedObjects(objStatus["name"])
					if err != nil {
						logger.Error(err, "unable to update status", "objectKind", k8s.KindOf(shdObj), "objectName", obj.GetName())
						return ctrl.Result{}, err
					}
				}
				r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.WaitingList, r.ErrorList}, []map[string]bool{r.ReadyList})
			}
		}
	}

	r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.WaitingList, r.ErrorList}, []map[string]bool{r.ReadyList})
	return ctrl.Result{}, nil
}

func (r *ShardedReconciler) updateStatus() error {
	err := r.Status().Update(r.ctx, r.ShardedObject)
	if err != nil {
		return err
	}
	return nil
}

func (r *ShardedReconciler) updateStatusWithRetry(updateFunc func() error) error {
	maxRetries := 5
	var err error
	for i := 0; i < maxRetries; i++ {
		err = updateFunc()
		if err != nil {
			if errors.IsConflict(err) {
				// The object has been modified by someone else, fetch the latest version and try again
				err = r.Get(r.ctx, types.NamespacedName{Name: r.ShardedObject.GetName(), Namespace: r.ShardedObject.GetNamespace()}, r.ShardedObject)
				if err != nil {
					return err
				}
			} else {
				return err
			}
		} else {
			break
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func (r *ShardedReconciler) updateObject(orig, new client.Object, shardName string) (ctrl.Result, error) {
	logger := log.FromContext(r.ctx)

	equal, err := IsObjectEqual(orig.DeepCopyObject().(client.Object), new.DeepCopyObject().(client.Object), r.DomainSubstring, r.MutatingWebhookAnnotation)
	if err != nil {
		logger.Error(err, "unable to compare", "objectKind", k8s.KindOf(new), "objectName", new.GetName())
		return ctrl.Result{}, err
	}

	if !equal {

		newObj, err := restoreOriginal(orig, new)
		if err != nil {
			logger.Error(err, "unable to update Spec", "objectKind", k8s.KindOf(new), "objectName", new.GetName())
			r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.WaitingList, r.ReadyList}, []map[string]bool{r.ErrorList})
			return ctrl.Result{}, err
		}

		err = r.Update(r.ctx, newObj)
		if err != nil {
			logger.Error(err, "unable to update", "objectKind", k8s.KindOf(new), "objectName", new.GetName())
			r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.WaitingList, r.ReadyList}, []map[string]bool{r.ErrorList})
			return ctrl.Result{}, err
		}
		logger.Info("successfully updated", "objectKind", k8s.KindOf(new), "objectName", new.GetName())
		r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.ErrorList}, []map[string]bool{r.ReadyList})
		createdObjects := r.ShardedObject.GetCreatedObjects()
		err = r.addToStatus(k8s.KindOf(new), new.GetName(), shardName, createdObjects)
		if err != nil {
			return ctrl.Result{}, err
		}
		delete(r.WaitingList, r.objKey)
		metrics.ProcessingCounter.WithLabelValues(r.ctrlName, shardName).Inc()

	}
	return ctrl.Result{}, nil
}

func restoreOriginal(orig, new client.Object) (client.Object, error) {
	switch oldObj := orig.(type) {
	case *networkingv1.Ingress:
		if newObj, ok := new.(*networkingv1.Ingress); ok {
			newObj = updateIngressObj(oldObj, newObj)
			return newObj, nil
		}
	case *contourv1.HTTPProxy:
		if newObj, ok := new.(*contourv1.HTTPProxy); ok {
			newObj = updateHTTPProxyObj(oldObj, newObj)
			return newObj, nil
		}
	}
	return nil, fmt.Errorf("unsupported object type: %s", k8s.KindOf(orig))
}

func IsObjectEqual(old, new client.Object, domainSubstring, mutatingWebhookAnnotation *string) (bool, error) {
	switch old := old.(type) {
	// Slow path: compare the content of the objects.
	case *contourv1.HTTPProxy:
		return apiequality.Semantic.DeepEqual(old.Annotations, new.(*contourv1.HTTPProxy).Annotations) &&
			apiequality.Semantic.DeepEqual(old.Spec, new.(*contourv1.HTTPProxy).Spec) &&
			apiequality.Semantic.DeepEqual(old.Labels, new.(*contourv1.HTTPProxy).Labels) &&
			apiequality.Semantic.DeepEqual(old.OwnerReferences, new.(*contourv1.HTTPProxy).OwnerReferences), nil

	case *networkingv1.Ingress:
		mutateHostsValue, exists := new.(*networkingv1.Ingress).Annotations[*mutatingWebhookAnnotation]
		if exists && mutateHostsValue != "" && mutateHostsValue != "false" {
			oldAnnotations := old.Annotations
			newAnnotations := new.(*networkingv1.Ingress).Annotations

			newServerAlias := newAnnotations["nginx.ingress.kubernetes.io/server-alias"]
			oldServerAlias := oldAnnotations["nginx.ingress.kubernetes.io/server-alias"]
			if newServerAlias == "" && oldServerAlias != "" {
				newAnnotations["nginx.ingress.kubernetes.io/server-alias"] = oldAnnotations["nginx.ingress.kubernetes.io/server-alias"]
			} else if newServerAlias != "" && oldServerAlias != "" {
				allExist := true
				for _, alias := range strings.Split(newServerAlias, ",") {
					if !strings.Contains(oldServerAlias, alias) {
						allExist = false
						break
					}
				}
				if allExist {
					newAnnotations["nginx.ingress.kubernetes.io/server-alias"] = oldServerAlias
				}
			} else if newServerAlias == "" && oldServerAlias == "" {
				delete(newAnnotations, "nginx.ingress.kubernetes.io/server-alias")
				delete(oldAnnotations, "nginx.ingress.kubernetes.io/server-alias")
			}

			old.Annotations = oldAnnotations
			new.(*networkingv1.Ingress).Annotations = newAnnotations

			newTLS := new.(*networkingv1.Ingress).Spec.TLS
			oldTLS := old.Spec.TLS
			allTLSExist := true

			// Check if all hosts in newTLS exist in oldTLS
			for _, newTLSHost := range newTLS {
				for _, host := range newTLSHost.Hosts {
					hostExists := false
					for _, oldTLSHost := range oldTLS {
						if contains(oldTLSHost.Hosts, host) {
							hostExists = true
							break
						}
					}
					if !hostExists {
						allTLSExist = false
						break
					}
				}
				if !allTLSExist {
					break
				}
			}

			// Check if any host in oldTLS does not exist in newTLS and does not contain "mainDomainSubstring", because that mean that additionalHosts removed
			for _, oldTLSHost := range oldTLS {
				for _, host := range oldTLSHost.Hosts {
					if !containsAllHosts(newTLS, host) && !strings.Contains(host, *domainSubstring) {
						allTLSExist = false
						break
					}
				}
				if !allTLSExist {
					break
				}
			}

			// No need update TLS block if all conditions are met
			if allTLSExist {
				new.(*networkingv1.Ingress).Spec.TLS = old.Spec.TLS
			}
		}

		return apiequality.Semantic.DeepEqual(old.Annotations, new.(*networkingv1.Ingress).Annotations) &&
			apiequality.Semantic.DeepEqual(old.Spec, new.(*networkingv1.Ingress).Spec) &&
			apiequality.Semantic.DeepEqual(old.Labels, new.(*networkingv1.Ingress).Labels) &&
			apiequality.Semantic.DeepEqual(old.OwnerReferences, new.(*networkingv1.Ingress).OwnerReferences), nil
	}

	// we don't know how to compare the object type.
	// This should never happen and indicates that new type was added to the code but is missing in the switch above.
	return false, fmt.Errorf("do not know how to compare %T and %T", old, new)
}

func getShardInfo(name, className string, shardSettings map[string]int, useAllShards bool) ([]Shards, bool, error) {
	maxShards, ok := shardSettings[className]
	if !ok {
		return []Shards{{ShardNumber: 0, ShardName: className}}, true, fmt.Errorf("shard type %s not found in config", className)
	}
	if maxShards == 0 {
		return []Shards{{ShardNumber: 0, ShardName: className}}, true, nil
	}

	var shards []Shards

	if useAllShards {
		for i := 0; i < maxShards; i++ {
			shardName := fmt.Sprintf("%s-%d", className, i)
			shards = append(shards, Shards{ShardNumber: i, ShardName: shardName})
		}
		return shards, false, nil
	}

	hash := xxhash.Sum64String(name)
	shardNumber := int(hash % uint64(maxShards))
	shardName := fmt.Sprintf("%s-%d", className, shardNumber)
	return []Shards{{ShardNumber: shardNumber, ShardName: shardName}}, false, nil
}

func findInStatus(shard, kind, name string, createdObjects *map[string][]map[string]string) bool {
	if objList, ok := (*createdObjects)[shard]; ok {
		for _, obj := range objList {
			if obj["kind"] == kind && obj["name"] == name {
				return true
			}
		}
	}
	return false
}

func (r *ShardedReconciler) GetCreatedObjects() *map[string][]map[string]string {
	return r.ShardedObject.GetCreatedObjects()
}

func (r *ShardedReconciler) SetCreatedObjects(s map[string][]map[string]string) {
	r.ShardedObject.SetCreatedObjects(s)
}

func (r *ShardedReconciler) addToCreatedObjects(shard, kind, name string) error {
	return r.updateStatusWithRetry(func() error {
		createdObjects := *r.GetCreatedObjects()
		if findInStatus(shard, kind, name, &createdObjects) {
			return errors.NewAlreadyExists(schema.GroupResource{}, shard)
		}
		if createdObjects == nil {
			createdObjects = make(map[string][]map[string]string)
		}
		createdObjects[shard] = append(createdObjects[shard], map[string]string{"kind": kind, "name": name})

		// Clean up empty entries
		for key, value := range createdObjects {
			if len(value) == 0 {
				delete(createdObjects, key)
			}
		}

		// Sort the slice based on the "name" field
		sort.Slice(createdObjects[shard], func(i, j int) bool {
			return createdObjects[shard][i]["name"] < createdObjects[shard][j]["name"]
		})

		r.SetCreatedObjects(createdObjects)
		return r.updateStatus()
	})
}

func (r *ShardedReconciler) delFromCreatedObjects(obj string) error {
	return r.updateStatusWithRetry(func() error {
		status := r.GetCreatedObjects()
		removeObjFromStatus(status, obj)
		r.SetCreatedObjects(*status)
		return r.updateStatus()
	})
}

func removeObjFromStatus(status *map[string][]map[string]string, obj string) {
	for key, valSlice := range *status {
		for i, valMap := range valSlice {
			if valMap["name"] == obj {
				(*status)[key] = append(valSlice[:i], valSlice[i+1:]...)
				break
			}
		}
		if len((*status)[key]) == 0 {
			delete(*status, key)
		}
	}
}

func (r *ShardedReconciler) GetIngressClassName() string {
	return r.ShardedObject.GetIngressClassName()
}

func (r *ShardedReconciler) GetShardInfo() ([]Shards, bool, error) {
	return getShardInfo(r.ShardedObject.GetNamespace(), r.ShardedObject.GetIngressClassName(), r.MaxShards, r.UseAllShards)
}

func (r *ShardedReconciler) GetChildKind() string {
	return r.ChildObject.GetObjectKind().GroupVersionKind().Kind
}

func (r *ShardedReconciler) GetKind() string {
	return r.ShardedObject.GetObjectKind().GroupVersionKind().Kind
}

func (r *ShardedReconciler) applyObjectsToCluster(objList []NewChildObj) (ctrl.Result, error) {
	logger := log.FromContext(r.ctx)
	statusList := make(map[string][]map[string]string)

	if !r.keyManaged(r.objKey) {
		r.addKey(r.objKey, r.ManagedList)
	}

	if ingressClass, exists := r.ShardedCache.Load(r.objKey); exists {
		if ingressClassStr, ok := ingressClass.(string); ok {
			if ingressClassStr != r.ShardedObject.GetIngressClassName() {
				metrics.ShardedIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": ingressClassStr}).Dec()
				r.ShardedCache.Delete(r.objKey)
				metrics.ShardedIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": r.ShardedObject.GetIngressClassName()}).Inc()
				r.ShardedCache.Store(r.objKey, r.ShardedObject.GetIngressClassName())
			}
		}
	} else {
		metrics.ShardedIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": r.ShardedObject.GetIngressClassName()}).Inc()
		r.ShardedCache.Store(r.objKey, r.ShardedObject.GetIngressClassName())
	}

	for _, current := range objList {
		found := r.ChildObject
		if err := ctrl.SetControllerReference(r.ShardedObject, current.Obj, r.Scheme); err != nil {
			logger.Error(err, "unable to set controller reference", "objectKind", current, "objectName", current.Obj.GetName())
		}
		err := r.Get(r.ctx, types.NamespacedName{Name: current.Obj.GetName(), Namespace: current.Obj.GetNamespace()}, found)
		if err != nil {
			if errors.IsNotFound(err) {
				result, err := r.createObject(current.Obj, current.ShardName)
				if err != nil {
					logger.Error(err, "unable to create", "objectKind", k8s.KindOf(found), "objectName", current.Obj.GetName())
				}
				return result, nil
			} else {
				logger.Error(err, "unable to get", "objectKind", k8s.KindOf(found), "objectName", current.Obj.GetName())
			}
		} else {
			_, err := r.updateObject(found, current.Obj, current.ShardName)
			if err != nil {
				logger.Error(err, "unable to update", "objectKind", k8s.KindOf(found), "objectName", current.Obj.GetName())
			}
			// No update needed, skip
		}
		if current.OldShard != "" {
			if r.Regular {
				current.Obj.SetName(r.ShardedObject.GetName())
			} else {
				current.Obj.SetName(fmt.Sprintf("%s-%d", r.ShardedObject.GetName(), current.Shard))
			}
			statusList[current.ShardName] = append(statusList[current.ShardName], map[string]string{"kind": k8s.KindOf(found), "name": current.Obj.GetName()})
			current.Obj.SetName(fmt.Sprintf("%s-%d-%s", r.ShardedObject.GetName(), current.Shard, "tmp"))
			statusList[current.ShardName] = append(statusList[current.ShardName], map[string]string{"kind": k8s.KindOf(found), "name": current.Obj.GetName()})
		} else {
			statusList[current.ShardName] = append(statusList[current.ShardName], map[string]string{"kind": k8s.KindOf(found), "name": current.Obj.GetName()})
		}
	}

	result, err := r.deleteUnlistedObjects(statusList)
	if err != nil {
		logger.Error(err, "unable to delete unlisted objects")
	}

	return result, nil
}

func (r *ShardedReconciler) CheckClusterShards() error {
	logger := log.Log.WithName(r.ctrlName)
	shardCounts := make(map[string]int)

	ingressClassList := &networkingv1.IngressClassList{}
	if err := r.Client.List(r.ctx, ingressClassList); err != nil {
		return err
	}

	shardSuffixRegex := regexp.MustCompile(`-(\d+)$`)

	for _, ingressClass := range ingressClassList.Items {
		if shardSuffixRegex.MatchString(ingressClass.Name) {
			baseName := shardSuffixRegex.ReplaceAllString(ingressClass.Name, "")
			shardCounts[baseName]++
		}
		r.initApplyPlan(ingressClass.Name)
	}

	for className, configShard := range r.MaxShards {
		if count, exists := shardCounts[className]; exists {
			if count < configShard {
				logger.Info("Reducing shard count to match Cluster value", "IngressClass", className, "ConfiguredShards", configShard, "CurrentShards", count)
				r.MaxShards[className] = count
			}
			for i := 0; i < configShard; i++ {
				shardName := fmt.Sprintf("%s-%d", className, i)
				r.initApplyPlan(shardName)
			}
		} else {
			logger.Info("ClassName from r.MaxShards not found in Cluster, setting to 0", "IngressClass", className)
			r.MaxShards[className] = 0
			r.initApplyPlan(className)
		}
	}
	return nil
}

func (r *ShardedReconciler) initApplyPlan(shardName string) {
	ap, exists := r.NextApplyTime[shardName]
	if !exists {
		ap = &applyPlan{}
		r.NextApplyTime[shardName] = ap
	}
	ap.SetLastCreating(time.Now())
}

func (r *ShardedReconciler) CheckReshardingConflict(newShard string, objName string) string {
	for oldShard, objs := range *r.ShardedObject.GetCreatedObjects() {
		for _, obj := range objs {
			if obj["name"] == objName && oldShard == newShard {
				return ""
			}
		}
	}
	for oldShard, objs := range *r.ShardedObject.GetCreatedObjects() {
		for _, obj := range objs {
			if obj["name"] == objName && oldShard != newShard {
				return oldShard
			}
		}
	}
	return ""
}

func parseDeleteAfterAnnotation(obj *unstructured.Unstructured) (deleteAfter time.Time, exists bool, err error) {
	if obj.GetAnnotations() == nil {
		return time.Time{}, false, nil
	}

	deleteAfterStr, exists := obj.GetAnnotations()[AutoDeleteAfterAnnotation]
	if !exists {
		return time.Time{}, false, nil
	}
	deleteAfter, err = time.Parse(time.RFC3339, deleteAfterStr)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("unable to parse auto-delete-after annotation as RFC3339: %w", err)
	}
	return deleteAfter, true, nil
}

func (r *ShardedReconciler) updateDeleteAfterAnnotation(obj *unstructured.Unstructured, period time.Duration) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[AutoDeleteAfterAnnotation] = time.Now().Add(period).UTC().Format(time.RFC3339)
	obj.SetAnnotations(annotations)
}

func (r *ShardedReconciler) markObjectForDeletion(obj *unstructured.Unstructured) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[*r.UnregisterAnnotation] = "true"
	obj.SetAnnotations(annotations)
}

func (r *ShardedReconciler) isObjectMarkedForDeletion(obj *unstructured.Unstructured) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	return annotations[*r.UnregisterAnnotation] == "true"
}

func (r *ShardedReconciler) handleDeletionTiming(obj *unstructured.Unstructured, shardName string) (shouldDelete bool, err error) {
	logger := log.FromContext(r.ctx)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	deleteAfterTime, deleteAfterExists, err := parseDeleteAfterAnnotation(obj)
	if err != nil {
		logger.Error(err, "unable to parse auto-delete-after annotation", "objectKind", obj.GetKind(), "objectName", obj.GetName())
		return false, err
	}
	delTime := *r.TerminationPeriod * 2
	isTempObject := (strings.HasPrefix(obj.GetName(), r.ShardedObject.GetName()) && strings.HasSuffix(obj.GetName(), "tmp"))
	if isTempObject {
		delTime = *r.TerminationPeriod * 3
	}
	markedForDeletion := r.isObjectMarkedForDeletion(obj)

	if deleteAfterExists {
		if time.Now().After(deleteAfterTime) && markedForDeletion {
			// Time to delete
			return true, nil
		}

		timeBeforeUnregister := deleteAfterTime.Add(-*r.TerminationPeriod)
		if time.Now().After(timeBeforeUnregister) {

			if !markedForDeletion {
				r.markObjectForDeletion(obj)
				r.updateDeleteAfterAnnotation(obj, delTime)

				if err := r.Update(r.ctx, obj); err != nil {
					logger.Error(err, "unable to update object with marked-for-deletion annotation", "objectKind", obj.GetKind(), "objectName", obj.GetName())
					return false, err
				}
				logger.Info("marked-for-deletion annotation set", "objectKind", obj.GetKind(), "objectName", obj.GetName())
				metrics.ProcessingCounter.WithLabelValues(r.ctrlName, shardName).Inc()
			}
		}

		return false, nil
	}

	r.updateDeleteAfterAnnotation(obj, delTime)
	if err := r.Update(r.ctx, obj); err != nil {
		logger.Error(err, "unable to update auto-delete-after annotation", "objectKind", obj.GetKind(), "objectName", obj.GetName())
		return false, err
	}
	logger.Info("auto-delete-after annotation set", "objectKind", obj.GetKind(), "objectName", obj.GetName(), "auto-delete-after", obj.GetAnnotations()[AutoDeleteAfterAnnotation])
	metrics.ProcessingCounter.WithLabelValues(r.ctrlName, shardName).Inc()
	r.moveKeyBetweenMultipleLists(r.objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
	return false, nil
}

func (r *ShardedReconciler) addToStatus(kind string, name string, shardName string, createdObjects *map[string][]map[string]string) error {
	if shardName == "" || kind == "" || name == "" {
		return nil
	}
	inStatus := findInStatus(shardName, kind, name, createdObjects)
	if !inStatus {
		err := r.addToCreatedObjects(shardName, kind, name)
		if err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			} else {
				return err
			}
		}
	}
	if ingressClass, exists := r.ChildCache.Load(r.objKey); exists {
		if ingressClassStr, ok := ingressClass.(string); ok {
			if ingressClassStr != shardName {
				metrics.ChildIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": ingressClassStr}).Dec()
				r.ChildCache.Delete(r.objKey)
				metrics.ChildIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": shardName}).Inc()
				r.ChildCache.Store(r.objKey, shardName)
			}
		}
	} else {
		metrics.ChildIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": shardName}).Inc()
		r.ChildCache.Store(r.objKey, shardName)
	}
	return nil
}

func (r *ShardedReconciler) checkTmpObjAnnotations(annotations map[string]string) (string, bool) {
	if deleteAfterStr, exists := annotations["auto-delete-after"]; exists {
		deleteAfterTime, err := time.Parse(time.RFC3339, deleteAfterStr)
		if err != nil {
			return "", false
		}
		timeBeforeChange := deleteAfterTime.Add(-*r.TerminationPeriod * 2)
		if !time.Now().After(timeBeforeChange) {
			if oldShard, exists := annotations["old-shard"]; exists {
				return oldShard, true
			}
		}
	} else if oldShard, exists := annotations["old-shard"]; exists {
		return oldShard, true
	}
	return "", false
}

func (r *ShardedReconciler) moveKeyBetweenMultipleLists(key string, srcs []map[string]bool, dests []map[string]bool) {
	for _, src := range srcs {
		delete(src, key)
	}
	for _, dest := range dests {
		dest[key] = true
	}
	r.updateMetrics()
}

func (r *ShardedReconciler) finalCleanKey(key string) {
	if _, exists := r.ErrorList[key]; exists {
		parts := strings.Split(key, "/")
		if len(parts) == 2 {
			ns := parts[0]
			name := parts[1]
			metrics.ErrorListGauge.Delete(prometheus.Labels{
				"controller": r.ctrlName,
				"name":       name,
				"namespace":  ns,
			})
		}
	}
	delete(r.WaitingList, key)
	delete(r.ReadyList, key)
	delete(r.ManagedList, key)
	delete(r.ErrorList, key)
}

func (r *ShardedReconciler) keyManaged(key string) bool {
	return r.ManagedList[key]
}

func (r *ShardedReconciler) keyWaited(key string) bool {
	return r.WaitingList[key]
}

func (r *ShardedReconciler) addKey(key string, dest map[string]bool) {
	dest[key] = true
}

func (r *ShardedReconciler) updateMetrics() {
	metrics.WaitingListGauge.WithLabelValues(r.ctrlName).Set(float64(len(r.WaitingList)))
	metrics.ReadyListGauge.WithLabelValues(r.ctrlName).Set(float64(len(r.ReadyList)))
	metrics.ManagedListGauge.WithLabelValues(r.ctrlName).Set(float64(len(r.ManagedList)))
	for key := range r.ErrorList {
		parts := strings.Split(key, "/")
		if len(parts) == 2 {
			ns := parts[0]
			name := parts[1]
			metrics.ErrorListGauge.WithLabelValues(r.ctrlName, ns, name).Set(float64(1))
		}
	}
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func containsAllHosts(tlsList []networkingv1.IngressTLS, host string) bool {
	for _, tls := range tlsList {
		if contains(tls.Hosts, host) {
			return true
		}
	}
	return false
}

func (r *ShardedReconciler) initializeCache() {
	if r.WaitingList == nil {
		r.WaitingList = make(map[string]bool)
	}
	if r.ReadyList == nil {
		r.ReadyList = make(map[string]bool)
	}
	if r.ManagedList == nil {
		r.ManagedList = make(map[string]bool)
	}
	if r.ErrorList == nil {
		r.ErrorList = make(map[string]bool)
	}
	if r.ShardedCache == nil {
		r.ShardedCache = &sync.Map{}
	}
	if r.ChildCache == nil {
		r.ChildCache = &sync.Map{}
	}
	if r.NextApplyTime == nil {
		r.NextApplyTime = make(map[string]*applyPlan)
	}
}

func (r *ShardedReconciler) handleNotFound(objKey string, logger logr.Logger) {
	logger.Info("Resource not found. Ignoring since object must be deleted", "objectKey", objKey)
	r.finalCleanKey(objKey)
	r.updateCacheMetrics(objKey)
	r.updateMetrics()
}

func (r *ShardedReconciler) handleFinalizer(finalizerKey string) (ctrl.Result, error) {
	logger := log.FromContext(r.ctx)
	childrenList, err := r.getObjectChildren()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot get children list: %w", err)
	}

	log.FromContext(r.ctx).Info("[finalizer] mark all object children gracefully")

	// step 1: get object children
	// step 2: mark all children for deletion, set unregister mark instantly
	// step 3: check which children should be deleted now, delete them
	// step 4: if no children left waiting for deletion - remove finalizer, otherwise requeue after finalizerTerminationPeriod
	waitingForDeletion := 0
	for _, child := range childrenList.Items {
		var shardName string
		for shard, status := range *r.ShardedObject.GetCreatedObjects() {
			for _, objStatus := range status {
				if objStatus["name"] == child.GetName() {
					shardName = shard
				}
			}
		}

		annotations := child.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}

		deleteAfter, deleteAfterExists, err := parseDeleteAfterAnnotation(&child)
		if err != nil {
			logger.Error(err, "[finalizer] unable to parse auto-delete-after annotation", "objectKind", child.GetKind(), "objectName", child.GetName())
			return ctrl.Result{}, fmt.Errorf("[finalizer] cannot parse delete after annotation: %w", err)
		}

		if !deleteAfterExists || !r.isObjectMarkedForDeletion(&child) {
			r.markObjectForDeletion(&child)
			r.updateDeleteAfterAnnotation(&child, *r.FinalizerTerminationPeriod)

			if err := r.Update(r.ctx, &child); err != nil {
				logger.Error(err, "[finalizer] unable to set auto-delete-after and unregister annotation on child", "objectKind", child.GetKind(), "objectName", child.GetName())
				return ctrl.Result{}, fmt.Errorf("[finalizer] unable to set auto-delete-after and unregister annotation on child: %w", err)
			}
			waitingForDeletion++
			continue
		}

		if time.Now().After(deleteAfter) {
			if err := r.Delete(r.ctx, &child); err != nil {
				logger.Error(err, "[finalizer] unable to delete child", "objectKind", child.GetKind(), "objectName", child.GetName())
				return ctrl.Result{}, err
			}
			logger.Info("[finalizer] successfully deleted child from cluster", "objectKind", child.GetKind(), "objectName", child.GetName())
			metrics.ProcessingCounter.WithLabelValues(r.ctrlName, shardName).Inc()
		} else {
			waitingForDeletion++
		}
	}

	if waitingForDeletion == 0 {
		controllerutil.RemoveFinalizer(r.ShardedObject, finalizerKey)
		err := r.Update(r.ctx, r.ShardedObject)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot remove finalizer from object: %w", err)
		}
		log.FromContext(r.ctx).Info("removed finalizer from object")
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: *r.FinalizerTerminationPeriod}, nil
}

func (r *ShardedReconciler) updateCacheMetrics(objKey string) {
	if ingressClass, exists := r.ShardedCache.Load(objKey); exists {
		metrics.DeletingCounter.WithLabelValues("shardedingress").Inc()
		metrics.ShardedIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": ingressClass.(string)}).Dec()
		r.ShardedCache.Delete(objKey)
	}
	if ingressClass, exists := r.ChildCache.Load(objKey); exists {
		metrics.ChildIngressClassObjectCount.With(prometheus.Labels{"controller": r.ctrlName, "ingress_class": ingressClass.(string)}).Dec()
		r.ChildCache.Delete(objKey)
	}
}

func (r *ShardedReconciler) applyRateLimit(objKey string, logger logr.Logger) (ctrl.Result, error) {
	lastUpdatingTime := time.Now()
	lastDeletingTime := time.Now()
	var diffShard, statusShards, applyShards []string
	var currentShard string
	var creating, deleting bool

	createdObjects := r.ShardedObject.GetCreatedObjects()
	if len(*createdObjects) > 0 {
		for shard, v := range *createdObjects {
			if len(v) > 0 {
				statusShards = append(statusShards, shard)
			}
		}
		for _, shard := range r.Shards {
			applyShards = append(applyShards, shard.ShardName)
		}
		diffShard = difference(statusShards, applyShards)
		if len(diffShard) == 0 && len(applyShards) == 1 && len(statusShards) == 0 ||
			(len(statusShards) == 1 && len(applyShards) == 1 && applyShards[0] != statusShards[0]) ||
			reflect.DeepEqual(diffShard, statusShards) {
			currentShard = applyShards[0]
			creating = true
		} else if len(diffShard) >= 1 && len(statusShards) > 1 && r.keyManaged(objKey) {
			currentShard = diffShard[0]
			deleting = true
		} else if !r.keyManaged(objKey) && len(diffShard) != 0 {
			currentShard = diffShard[0]
			creating = true
		} else if len(diffShard) == 0 {
			r.moveKeyBetweenMultipleLists(objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
			return ctrl.Result{Requeue: true}, nil
		} else {
			logger.Info("Rescheduling", "use-all-class-shards object", objKey)
			r.moveKeyBetweenMultipleLists(objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		r.moveKeyBetweenMultipleLists(objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
	}

	if creating {
		ap := r.NextApplyTime[currentShard]
		// check for future updates
		if lastUpdatingTime.Add(-*r.ShardUpdateCooldown).Before(r.NextApplyTime[currentShard].lastCreating) {
			// creating later
			lastUpdatingTime = r.NextApplyTime[currentShard].lastCreating.Add(*r.ShardUpdateCooldown)
		} else {
			// last creating time is in the past
			// creating now
			lastUpdatingTime = time.Now().Add(1 * time.Second)
		}
		ap.SetLastCreating(lastUpdatingTime)
		ap.SetLastDeleting(lastUpdatingTime)
		ap.SetCurrentDeletingWindowStart(lastUpdatingTime.Add(*r.TerminationPeriod))
		ap.SetNextDeletingWindowStart(lastUpdatingTime.Add(*r.TerminationPeriod * 3))
		r.moveKeyBetweenMultipleLists(objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
		delay := time.Until(lastUpdatingTime)
		return ctrl.Result{RequeueAfter: delay}, nil
	}

	if deleting {
		ap := r.NextApplyTime[currentShard]
		// check for first deletion on shard
		if lastDeletingTime.Add(-*r.ShardUpdateCooldown).Before(r.NextApplyTime[currentShard].lastCreating) && lastDeletingTime.After(r.NextApplyTime[currentShard].nextDeletingWindowStart) {
			lastDeletingTime = r.NextApplyTime[currentShard].lastCreating
			ap.SetLastCreating(lastDeletingTime.Add(*r.ShardUpdateCooldown))
			ap.SetLastDeleting(lastDeletingTime.Add(*r.ShardUpdateCooldown))
			ap.SetCurrentDeletingWindowStart(lastDeletingTime.Add(*r.TerminationPeriod))
			ap.SetNextDeletingWindowStart(lastDeletingTime.Add(*r.TerminationPeriod * 3))
			logger.Info("Print timings", "object", objKey, "last-creating", time.Until(r.NextApplyTime[currentShard].lastCreating), "last-deleting", time.Until(r.NextApplyTime[currentShard].lastDeleting), "current-deleting", time.Until(r.NextApplyTime[currentShard].currentDeletingWindowStart), "next-deleting", time.Until(r.NextApplyTime[currentShard].nextDeletingWindowStart))
		}

		if lastDeletingTime.Add(*r.TerminationPeriod * 2).Before(r.NextApplyTime[currentShard].nextDeletingWindowStart) {
			lastDeletingTime = r.NextApplyTime[currentShard].lastDeleting.Add(*r.ShardUpdateCooldown)
		} else if lastDeletingTime.Add(*r.TerminationPeriod).Before(r.NextApplyTime[currentShard].nextDeletingWindowStart) {
			lastDeletingTime = r.NextApplyTime[currentShard].nextDeletingWindowStart.Add(*r.ShardUpdateCooldown)
			ap.SetCurrentDeletingWindowStart(lastDeletingTime.Add(*r.TerminationPeriod))
			ap.SetNextDeletingWindowStart(lastDeletingTime.Add(*r.TerminationPeriod * 3))
			logger.Info("New termination window created and deleting later in new termination window", "object", objKey, "delay", time.Until(lastDeletingTime))
		} else {
			lastDeletingTime = time.Now().Add(1 * time.Second)
			logger.Info("Deleting now", "object", objKey, "delay", time.Until(lastDeletingTime))
		}
		ap.SetLastDeleting(lastDeletingTime)
		ap.SetLastCreating(lastDeletingTime)
		r.moveKeyBetweenMultipleLists(objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
		delay := time.Until(lastDeletingTime)
		return ctrl.Result{RequeueAfter: delay}, nil
	}

	r.moveKeyBetweenMultipleLists(objKey, []map[string]bool{r.ReadyList, r.ErrorList}, []map[string]bool{r.WaitingList})
	return ctrl.Result{Requeue: true}, nil
}

func (r *ShardedReconciler) setShardInfo(logger logr.Logger) error {
	var err error
	r.Shards, r.Regular, err = r.GetShardInfo()
	if err != nil {
		logger.Error(err, "Unable to use shard")
		return err
	}
	return nil
}

func difference(a, b []string) []string {
	mb := make(map[string]bool)
	for _, x := range b {
		mb[x] = true
	}
	var diff []string
	for _, x := range a {
		if !mb[x] {
			diff = append(diff, x)
		}
	}
	return diff
}
func (ap *applyPlan) SetLastCreating(t time.Time) {
	ap.lastCreating = t
}

func (ap *applyPlan) SetLastDeleting(t time.Time) {
	ap.lastDeleting = t
}

func (ap *applyPlan) SetCurrentDeletingWindowStart(t time.Time) {
	ap.currentDeletingWindowStart = t
}

func (ap *applyPlan) SetNextDeletingWindowStart(t time.Time) {
	ap.nextDeletingWindowStart = t
}
