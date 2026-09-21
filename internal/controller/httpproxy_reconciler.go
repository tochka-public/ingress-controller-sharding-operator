package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	controllerv1 "k8s.tochka.com/sharded-ingress-controller/api/v1"
)

type ShardedHTTPProxyReconciler struct {
	ShardedReconciler
	*controllerv1.ShardedHTTPProxy
	ChildObject contourv1.HTTPProxy
}

func (r *ShardedHTTPProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	r.ShardedReconciler = ShardedReconciler{
		Client:                                   r.Client,
		Scheme:                                   r.Scheme,
		MaxShards:                                r.MaxShards,
		TerminationPeriod:                        r.TerminationPeriod,
		ShardUpdateCooldown:                      r.ShardUpdateCooldown,
		AllShardsBaseHosts:                       r.AllShardsBaseHosts,
		DomainSubstring:                          r.DomainSubstring,
		MutatingWebhookAnnotation:                r.MutatingWebhookAnnotation,
		UnregisterAnnotation:                     r.UnregisterAnnotation,
		AdditionalServiceDiscoveryClassLabel:     r.AdditionalServiceDiscoveryClassLabel,
		RootHTTPProxyLabel:                       r.RootHTTPProxyLabel,
		VirtualHostsHTTPProxyAnnotation:          r.VirtualHostsHTTPProxyAnnotation,
		AdditionalServiceDiscoveryTagsAnnotation: r.AdditionalServiceDiscoveryTagsAnnotation,
		AppNameLabel:                             r.AppNameLabel,
		AllShardsPlacementAnnotation:             r.AllShardsPlacementAnnotation,
		FinalizerKey:                             r.FinalizerKey,
		FinalizerTerminationPeriod:               r.FinalizerTerminationPeriod,
		FinalizerDeletionTerminationPeriod:       r.FinalizerDeletionTerminationPeriod,
		WaitingList:                              r.WaitingList,
		ReadyList:                                r.ReadyList,
		ManagedList:                              r.ManagedList,
		ErrorList:                                r.ErrorList,
		NextApplyTime:                            r.NextApplyTime,
		ShardedCache:                             r.ShardedCache,
		ChildCache:                               r.ChildCache,
		Initialized:                              r.Initialized,
		req:                                      &req,
		ctx:                                      ctx,
		ShardedObject:                            r.ShardedHTTPProxy,
		ChildObject:                              &r.ChildObject,
		objKey:                                   req.NamespacedName.String(),
		ctrlName:                                 "shardedhttpproxy",
	}

	if !r.Initialized {
		r.initializeCache()
		if err := r.CheckClusterShards(); err != nil {
			return ctrl.Result{}, err
		}
		r.Initialized = true
	}

	// Fetch the ShardedHTTPProxy instance
	err := r.Get(ctx, req.NamespacedName, r.ShardedHTTPProxy)
	if err != nil {
		if errors.IsNotFound(err) {
			r.handleNotFound(r.objKey, logger)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch ShardedHTTPProxy")
		return ctrl.Result{}, err
	}

	// If object doesn't have finalizer — set finalizer
	if r.ShardedHTTPProxy.GetObjectMeta().GetDeletionTimestamp().IsZero() && !controllerutil.ContainsFinalizer(r.ShardedHTTPProxy, *r.FinalizerKey) {
		controllerutil.AddFinalizer(r.ShardedHTTPProxy, *r.FinalizerKey)
		if err := r.Update(ctx, r.ShardedHTTPProxy); err != nil {
			logger.Error(err, "unable to set controller finalizer on ShardedHTTPProxy")
			return ctrl.Result{}, fmt.Errorf("cannot set controller finalizer: %w", err)
		}
	}

	if !r.ShardedHTTPProxy.GetObjectMeta().GetDeletionTimestamp().IsZero() {
		return r.handleFinalizer(*r.FinalizerKey)
	}

	if val, ok := r.ShardedHTTPProxy.Annotations[*r.AllShardsPlacementAnnotation]; ok && val == "true" {
		r.UseAllShards = true
	}

	if err := r.setShardInfo(logger); err != nil {
		return ctrl.Result{}, nil
	}

	if !r.keyWaited(r.objKey) {
		return r.applyRateLimit(r.objKey, logger)
	}

	// Convert the ShardedHTTPProxy to multiple HTTPProxy objects
	httpProxies, err := r.NewHTTPProxiesFromShardedHTTPProxy()
	if err != nil {
		logger.Error(err, "children object can't be generated")
		return ctrl.Result{}, nil
	}

	r.updateMetrics()
	return r.applyObjectsToCluster(httpProxies)
}

// splitIndexedName splits "app-0-12" into ("app-0", 12). Only the canonical
// decimal form the controller itself generates is accepted, so "app-0-tmp" and
// "app-007" are ignored.
func splitIndexedName(name string) (prefix string, idx int, ok bool) {
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return "", 0, false
	}
	suffix := name[i+1:]
	idx, err := strconv.Atoi(suffix)
	if err != nil || idx < 0 || strconv.Itoa(idx) != suffix {
		return "", 0, false
	}
	return name[:i], idx, true
}

// vhostIndexAllocator hands out the numeric suffix of the per-virtual-host
// children "<prefix>-<n>".
//
// The suffix used to be the position of the host in the virtual-hosts
// annotation, so dropping a host from the middle of the list shifted every
// later host one index down: the controller rewrote the fqdn of every
// following child and left the last one — still carrying the fqdn the child
// before it had just been given — for the auto-delete-after grace period.
// Until it was collected Contour saw two HTTPProxies with the same fqdn on the
// same ingress class, rejected both with DuplicateVhost, and the host was down.
//
// Keeping a host pinned to the index it already has means removing or adding
// one host touches exactly the one child that serves it.
type vhostIndexAllocator struct {
	used   map[int]struct{}
	byFqdn map[string]int
	next   int
}

func newVhostIndexAllocator() *vhostIndexAllocator {
	return &vhostIndexAllocator{used: map[int]struct{}{}, byFqdn: map[string]int{}}
}

// reserve records that idx is taken. An empty fqdn still holds the index but
// claims no host: main objects left over from another shard layout carry no
// virtualHost, and handing their index to a live host would rewrite an object
// that is on its way out.
func (a *vhostIndexAllocator) reserve(idx int, fqdn string) {
	a.used[idx] = struct{}{}
	if fqdn == "" {
		return
	}
	// Lowest index wins, so a namespace that already carries a duplicate
	// converges on one child instead of flapping between the two.
	if cur, ok := a.byFqdn[fqdn]; !ok || idx < cur {
		a.byFqdn[fqdn] = idx
	}
}

// indexFor returns the index of the child already serving fqdn, or the lowest
// index that is neither in use nor handed out earlier in this pass.
//
// A child that is draining keeps its fqdn registered here on purpose: if the
// host comes back before the child is collected, reusing the index revives that
// object in place instead of creating a second one for an fqdn its dying twin
// still holds.
func (a *vhostIndexAllocator) indexFor(fqdn string) int {
	if idx, ok := a.byFqdn[fqdn]; ok {
		return idx
	}
	for {
		if _, taken := a.used[a.next]; !taken {
			break
		}
		a.next++
	}
	a.reserve(a.next, fqdn)
	return a.next
}

// vhostAllocators buckets the children by name prefix in a single pass, so
// every prefix in play gets its allocator at once. "app-2-tmp-7" buckets under
// "app-2-tmp" and never under "app-2", and the tmp root "app-2-tmp" has a
// non-numeric suffix and is skipped entirely.
func vhostAllocators(children []contourv1.HTTPProxy) map[string]*vhostIndexAllocator {
	allocators := map[string]*vhostIndexAllocator{}
	for i := range children {
		prefix, idx, ok := splitIndexedName(children[i].GetName())
		if !ok {
			continue
		}
		var fqdn string
		if vh := children[i].Spec.VirtualHost; vh != nil {
			fqdn = vh.Fqdn
		}
		allocatorFor(allocators, prefix).reserve(idx, fqdn)
	}
	return allocators
}

func allocatorFor(allocators map[string]*vhostIndexAllocator, prefix string) *vhostIndexAllocator {
	if a, ok := allocators[prefix]; ok {
		return a
	}
	a := newVhostIndexAllocator()
	allocators[prefix] = a
	return a
}

// listOwnedHTTPProxies returns the children of the sharded object from the
// typed informer cache. getObjectChildren is deliberately not reused here: it
// lists unstructured, and controller-runtime bypasses the cache for
// unstructured reads, so calling it would add a second live LIST of every
// HTTPProxy in the namespace to every reconcile — at exactly the scale where
// this bug hurts. The owner filter mirrors getObjectChildren so that the
// allocator and the deletion pass agree on the child set.
func (r *ShardedHTTPProxyReconciler) listOwnedHTTPProxies() ([]contourv1.HTTPProxy, error) {
	var list contourv1.HTTPProxyList
	if err := r.List(r.ctx, &list, client.InNamespace(r.req.Namespace)); err != nil {
		return nil, err
	}
	owned := make([]contourv1.HTTPProxy, 0, len(list.Items))
	for _, item := range list.Items {
		for _, owner := range item.GetOwnerReferences() {
			if owner.Name == r.ShardedObject.GetName() && owner.Kind == r.ShardedObject.GetKind() {
				owned = append(owned, item)
				break
			}
		}
	}
	return owned, nil
}

func (r *ShardedHTTPProxyReconciler) NewHTTPProxiesFromShardedHTTPProxy() ([]NewChildObj, error) {
	var httpProxies []NewChildObj

	children, err := r.listOwnedHTTPProxies()
	if err != nil {
		return nil, fmt.Errorf("cannot list child HTTPProxies: %w", err)
	}
	allocators := vhostAllocators(children)

	for _, shard := range r.Shards {
		shardedHTTPProxy := r.ShardedObject.(*controllerv1.ShardedHTTPProxy).DeepCopy()
		if shardedHTTPProxy.Spec.Template.Labels == nil {
			shardedHTTPProxy.Spec.Template.Labels = make(map[string]string)
		}
		if shardedHTTPProxy.Spec.Template.Annotations == nil {
			shardedHTTPProxy.Spec.Template.Annotations = make(map[string]string)
		}

		// mainHTTPProxyName is the name applyObjectsToCluster books in the
		// status, so it is also the name the resharding conflict has to be
		// looked up under. Keying the lookup on "<name>-<shardNumber>"
		// unconditionally asked about the first virtual host child instead of
		// the main object whenever the class is unsharded, where the main
		// object keeps the bare name.
		mainHTTPProxyName := shardedHTTPProxy.Name
		if r.ShardedObject.GetIngressClassName() != shard.ShardName {
			mainHTTPProxyName = fmt.Sprintf("%s-%d", shardedHTTPProxy.Name, shard.ShardNumber)
		}

		conflict := r.CheckReshardingConflict(shard.ShardName, mainHTTPProxyName)
		ingressClass := shard.ShardName
		tempName := fmt.Sprintf("%s-%d-%s", shardedHTTPProxy.Name, shard.ShardNumber, "tmp")
		obj := r.ShardedReconciler.ChildObject

		if err := r.Get(r.ctx, types.NamespacedName{Name: tempName, Namespace: shardedHTTPProxy.GetNamespace()}, obj); err != nil {
			if errors.IsNotFound(err) && conflict != "" {
				// Create a deep copy for the tmp object to modify
				tempShardedHTTPProxy := shardedHTTPProxy.DeepCopy()
				tempShardedHTTPProxy.SetName(tempName)
				tempShardedHTTPProxy.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = conflict
				tempHTTPProxy := r.createHTTPProxy(tempShardedHTTPProxy, tempName, conflict, nil)
				tempHTTPProxy.ObjectMeta.Labels[*r.RootHTTPProxyLabel] = "true"
				httpProxies = append(httpProxies, NewChildObj{
					Shard:     shard.ShardNumber,
					ShardName: shard.ShardName,
					Obj:       tempHTTPProxy,
				})

				// Handle virtual hosts for the tmp object
				if serverAlias, exists := tempShardedHTTPProxy.Annotations[*r.VirtualHostsHTTPProxyAnnotation]; exists && serverAlias != "" {
					hosts := strings.Split(serverAlias, ",")
					alloc := allocatorFor(allocators, tempName)

					for _, host := range hosts {
						virtualHost := newVirtualHostFromTemplate(tempShardedHTTPProxy.Spec.Template.Spec.VirtualHost, host)

						httpProxy := r.createHTTPProxy(tempShardedHTTPProxy, fmt.Sprintf("%s-%d", tempName, alloc.indexFor(host)), conflict, virtualHost)

						httpProxies = append(httpProxies, NewChildObj{
							Shard:     shard.ShardNumber,
							ShardName: shard.ShardName,
							Obj:       httpProxy,
						})
					}
				}
				tempShardedHTTPProxy.Spec.Template.Annotations["old-shard"] = conflict
				ingressClass = conflict
			}
		} else {
			if oldClass, ok := r.checkTmpObjAnnotations(obj.GetAnnotations()); ok {
				ingressClass = oldClass
				conflict = oldClass
			}
		}

		shardedHTTPProxy.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = ingressClass
		shardedHTTPProxy.SetName(mainHTTPProxyName)

		// Create the base HTTPProxy.
		// ShardName is the new shard even while the spec still carries the old
		// ingress class: it is status bookkeeping, and deleteUnlistedObjects
		// only keeps children listed under the current shards. Booking live
		// children under the old shard makes it schedule them for deletion,
		// and the next reconcile wipes the annotation it just set — an endless
		// auto-delete-after churn that never finishes the migration.
		baseHTTPProxy := r.createHTTPProxy(shardedHTTPProxy, mainHTTPProxyName, ingressClass, nil)
		baseHTTPProxy.ObjectMeta.Labels[*r.RootHTTPProxyLabel] = "true"
		httpProxies = append(httpProxies, NewChildObj{
			Shard:     shard.ShardNumber,
			ShardName: shard.ShardName,
			Obj:       baseHTTPProxy,
		})

		// Handle virtual hosts
		if serverAlias, exists := shardedHTTPProxy.Annotations[*r.VirtualHostsHTTPProxyAnnotation]; exists && serverAlias != "" {
			hosts := strings.Split(serverAlias, ",")
			alloc := allocatorFor(allocators, mainHTTPProxyName)

			for _, host := range hosts {
				virtualHost := newVirtualHostFromTemplate(shardedHTTPProxy.Spec.Template.Spec.VirtualHost, host)

				httpProxy := r.createHTTPProxy(shardedHTTPProxy, fmt.Sprintf("%s-%d", mainHTTPProxyName, alloc.indexFor(host)), ingressClass, virtualHost)

				httpProxies = append(httpProxies, NewChildObj{
					Shard:     shard.ShardNumber,
					ShardName: shard.ShardName,
					Obj:       httpProxy,
				})
			}
		}
	}
	return httpProxies, nil
}

// newVirtualHostFromTemplate copies the template's VirtualHost (all fields, current and future)
// and replaces Fqdn with the given host.
func newVirtualHostFromTemplate(template *contourv1.VirtualHost, host string) *contourv1.VirtualHost {
	if template == nil {
		return &contourv1.VirtualHost{Fqdn: host}
	}
	virtualHost := template.DeepCopy()
	virtualHost.Fqdn = host
	return virtualHost
}

func (r *ShardedHTTPProxyReconciler) createHTTPProxy(shardedHTTPProxy *controllerv1.ShardedHTTPProxy, name, ingressClass string, virtualHost *contourv1.VirtualHost) *contourv1.HTTPProxy {
	httpProxy := &contourv1.HTTPProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   shardedHTTPProxy.Namespace,
			Annotations: shardedHTTPProxy.Spec.Template.Annotations,
			Labels:      copyLabels(shardedHTTPProxy.Spec.Template.Labels),
		},
		Spec: contourv1.HTTPProxySpec{
			VirtualHost:      virtualHost,
			Routes:           shardedHTTPProxy.Spec.Template.Spec.Routes,
			TCPProxy:         shardedHTTPProxy.Spec.Template.Spec.TCPProxy,
			IngressClassName: ingressClass,
		},
	}

	if virtualHost != nil {
		httpProxy.Spec.Includes = []contourv1.Include{
			{
				Name:      shardedHTTPProxy.Name,
				Namespace: shardedHTTPProxy.Namespace,
			},
		}
	}

	return httpProxy
}

func updateHTTPProxyObj(old, new *contourv1.HTTPProxy) *contourv1.HTTPProxy {
	old.Spec = new.Spec
	old.Annotations = new.Annotations
	old.Labels = new.Labels
	old.OwnerReferences = new.OwnerReferences

	return old
}

func (r *ShardedHTTPProxyReconciler) SetupWithManager(mgr ctrl.Manager, parallel int, qps int, burst int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controllerv1.ShardedHTTPProxy{}).Owns(&contourv1.HTTPProxy{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: parallel,
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](ExponentialBackoffBaseDelay, ExponentialBackoffMaxDelay),
				&workqueue.TypedBucketRateLimiter[ctrl.Request]{Limiter: rate.NewLimiter(rate.Limit(qps), burst)},
			)}).
		Complete(r)
}

func (r *ShardedHTTPProxyReconciler) GetCreatedObjects() *map[string][]map[string]string {
	return &r.Status.CreatedObjects
}

func (r *ShardedHTTPProxyReconciler) SetCreatedObjects(s map[string][]map[string]string) {
	r.Status.CreatedObjects = s
}

func copyLabels(source map[string]string) map[string]string {
	res := make(map[string]string, len(source))
	for k, v := range source {
		res[k] = v
	}
	return res
}
