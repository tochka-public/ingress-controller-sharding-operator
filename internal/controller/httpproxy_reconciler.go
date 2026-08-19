package controller

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
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
		return ctrl.Result{}, err
	}

	r.updateMetrics()
	return r.applyObjectsToCluster(httpProxies)
}

func (r *ShardedHTTPProxyReconciler) NewHTTPProxiesFromShardedHTTPProxy() ([]NewChildObj, error) {
	var httpProxies []NewChildObj

	for _, shard := range r.Shards {
		shardedHTTPProxy := r.ShardedObject.(*controllerv1.ShardedHTTPProxy).DeepCopy()
		if shardedHTTPProxy.Spec.Template.Labels == nil {
			shardedHTTPProxy.Spec.Template.Labels = make(map[string]string)
		}
		if shardedHTTPProxy.Spec.Template.Annotations == nil {
			shardedHTTPProxy.Spec.Template.Annotations = make(map[string]string)
		}

		conflict := r.CheckReshardingConflict(shard.ShardName, fmt.Sprintf("%s-%d", shardedHTTPProxy.Name, shard.ShardNumber))
		ingressClass := shard.ShardName
		tempName := fmt.Sprintf("%s-%d-%s", shardedHTTPProxy.Name, shard.ShardNumber, "tmp")

		// oldShard is non-empty while the migration window is open: the tmp
		// tree holds the old shard so service discovery keeps serving it while
		// the main children move to the new class.
		var oldShard string
		tmpRoot := &contourv1.HTTPProxy{}
		err := r.Get(r.ctx, types.NamespacedName{Name: tempName, Namespace: shardedHTTPProxy.GetNamespace()}, tmpRoot)
		switch {
		case err == nil:
			if oldClass, ok := r.checkTmpObjAnnotations(tmpRoot.GetAnnotations()); ok {
				oldShard = oldClass
			}
		case errors.IsNotFound(err):
			oldShard = conflict
		default:
			// Deciding without knowing whether the tmp object exists would
			// flip the main children to the new class ahead of time.
			return nil, fmt.Errorf("unable to get tmp object %s: %w", tempName, err)
		}

		if oldShard != "" {
			ingressClass = oldShard
			tmpTree, err := r.newTmpTree(shardedHTTPProxy, tempName, oldShard, shard)
			if err != nil {
				return nil, err
			}
			httpProxies = append(httpProxies, tmpTree...)
		}

		mainHTTPProxyName := shardedHTTPProxy.Name
		shardedHTTPProxy.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = ingressClass
		if r.ShardedObject.GetIngressClassName() != shard.ShardName {
			mainHTTPProxyName = fmt.Sprintf("%s-%d", shardedHTTPProxy.Name, shard.ShardNumber)
		}
		shardedHTTPProxy.SetName(mainHTTPProxyName)

		// Create the base HTTPProxy.
		// ShardName is always the new shard, even while the object spec still
		// carries the old ingress class during migration: applyObjectsToCluster
		// registers children in the status list under ShardName, and
		// deleteUnlistedObjects only treats objects listed under the current
		// shards as alive. Registering the main children under the old shard
		// makes deleteUnlistedObjects schedule the live objects for deletion,
		// which then loops setting/wiping auto-delete-after forever.
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

			for i, host := range hosts {
				virtualHost := newVirtualHostFromTemplate(shardedHTTPProxy.Spec.Template.Spec.VirtualHost, host)

				httpProxy := r.createHTTPProxy(shardedHTTPProxy, fmt.Sprintf("%s-%d", mainHTTPProxyName, i), ingressClass, virtualHost)

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

// newTmpTree returns the tmp copies of the child tree (root proxy plus one
// proxy per virtual host) that are still missing from the cluster. The whole
// tree stays on the old shard for the duration of the migration window.
//
// Objects that already exist are deliberately left out: applyObjectsToCluster
// would otherwise reconcile them back to the generated spec and wipe the
// auto-delete-after annotation that drives their unregister/delete timeline.
func (r *ShardedHTTPProxyReconciler) newTmpTree(shardedHTTPProxy *controllerv1.ShardedHTTPProxy, tempName, oldShard string, shard Shards) ([]NewChildObj, error) {
	var objs []NewChildObj

	tmpSharded := shardedHTTPProxy.DeepCopy()
	tmpSharded.SetName(tempName)
	tmpSharded.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = oldShard
	tmpSharded.Spec.Template.Annotations["old-shard"] = oldShard

	exists, err := r.childExists(tempName, tmpSharded.GetNamespace())
	if err != nil {
		return nil, err
	}
	if !exists {
		tmpRoot := r.createHTTPProxy(tmpSharded, tempName, oldShard, nil)
		tmpRoot.ObjectMeta.Labels[*r.RootHTTPProxyLabel] = "true"
		objs = append(objs, NewChildObj{
			Shard:     shard.ShardNumber,
			ShardName: shard.ShardName,
			Obj:       tmpRoot,
		})
	}

	serverAlias, hasAlias := tmpSharded.Annotations[*r.VirtualHostsHTTPProxyAnnotation]
	if !hasAlias || serverAlias == "" {
		return objs, nil
	}

	for i, host := range strings.Split(serverAlias, ",") {
		hostName := fmt.Sprintf("%s-%d", tempName, i)
		exists, err := r.childExists(hostName, tmpSharded.GetNamespace())
		if err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		virtualHost := newVirtualHostFromTemplate(tmpSharded.Spec.Template.Spec.VirtualHost, host)
		objs = append(objs, NewChildObj{
			Shard:     shard.ShardNumber,
			ShardName: shard.ShardName,
			Obj:       r.createHTTPProxy(tmpSharded, hostName, oldShard, virtualHost),
		})
	}

	return objs, nil
}

func (r *ShardedHTTPProxyReconciler) childExists(name, namespace string) (bool, error) {
	err := r.Get(r.ctx, types.NamespacedName{Name: name, Namespace: namespace}, &contourv1.HTTPProxy{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("unable to get %s: %w", name, err)
	}
	return true, nil
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
			Name:      name,
			Namespace: shardedHTTPProxy.Namespace,
			// Both maps are copied: children built from the same template
			// otherwise alias one map, so annotating a single child (for
			// instance auto-delete-after on a tmp object) silently annotates
			// all of its siblings.
			Annotations: copyStringMap(shardedHTTPProxy.Spec.Template.Annotations),
			Labels:      copyStringMap(shardedHTTPProxy.Spec.Template.Labels),
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

func copyStringMap(source map[string]string) map[string]string {
	res := make(map[string]string, len(source))
	for k, v := range source {
		res[k] = v
	}
	return res
}
