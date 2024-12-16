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
		obj := r.ShardedReconciler.ChildObject

		err := r.Get(r.ctx, types.NamespacedName{Name: tempName, Namespace: shardedHTTPProxy.GetNamespace()}, obj)
		if err != nil {
			if errors.IsNotFound(err) && conflict != "" {
				// Create a deep copy for the tmp object to modify
				tempShardedHTTPProxy := shardedHTTPProxy.DeepCopy()
				tempShardedHTTPProxy.SetName(tempName)
				tempShardedHTTPProxy.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = r.Conflict
				tempHTTPProxy := r.createHTTPProxy(tempShardedHTTPProxy, tempName, r.Conflict, nil)
				tempHTTPProxy.ObjectMeta.Labels[*r.RootHTTPProxyLabel] = "true"
				httpProxies = append(httpProxies, NewChildObj{
					Shard:     shard.ShardNumber,
					ShardName: ingressClass,
					Obj:       tempHTTPProxy,
				})

				// Handle virtual hosts for the tmp object
				if serverAlias, exists := tempShardedHTTPProxy.Annotations[*r.VirtualHostsHTTPProxyAnnotation]; exists && serverAlias != "" {
					hosts := strings.Split(serverAlias, ",")

					for i, host := range hosts {
						var tls *contourv1.TLS
						if tempShardedHTTPProxy.Spec.Template.Spec.VirtualHost != nil && tempShardedHTTPProxy.Spec.Template.Spec.VirtualHost.TLS != nil {
							tls = tempShardedHTTPProxy.Spec.Template.Spec.VirtualHost.TLS
						}

						httpProxy := r.createHTTPProxy(tempShardedHTTPProxy, fmt.Sprintf("%s-%d", tempName, i), r.Conflict, &contourv1.VirtualHost{
							Fqdn: host,
							TLS:  tls,
						})

						httpProxies = append(httpProxies, NewChildObj{
							Shard:     shard.ShardNumber,
							ShardName: ingressClass,
							Obj:       httpProxy,
						})
					}
				}
				tempShardedHTTPProxy.Spec.Template.Annotations["old-shard"] = r.Conflict
				ingressClass = conflict
			}
		} else {
			if oldClass, ok := r.checkTmpObjAnnotations(obj.GetAnnotations()); ok {
				ingressClass = oldClass
				conflict = oldClass
			}
		}

		mainHTTPProxyName := shardedHTTPProxy.Name
		shardedHTTPProxy.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = ingressClass
		if r.ShardedObject.GetIngressClassName() != shard.ShardName {
			mainHTTPProxyName = fmt.Sprintf("%s-%d", shardedHTTPProxy.Name, shard.ShardNumber)
		}
		shardedHTTPProxy.SetName(mainHTTPProxyName)

		// Create the base HTTPProxy
		baseHTTPProxy := r.createHTTPProxy(shardedHTTPProxy, mainHTTPProxyName, ingressClass, nil)
		baseHTTPProxy.ObjectMeta.Labels[*r.RootHTTPProxyLabel] = "true"
		httpProxies = append(httpProxies, NewChildObj{
			Shard:     shard.ShardNumber,
			ShardName: ingressClass,
			Obj:       baseHTTPProxy,
		})

		// Handle virtual hosts
		if serverAlias, exists := shardedHTTPProxy.Annotations[*r.VirtualHostsHTTPProxyAnnotation]; exists && serverAlias != "" {
			hosts := strings.Split(serverAlias, ",")

			for i, host := range hosts {
				var tls *contourv1.TLS
				if shardedHTTPProxy.Spec.Template.Spec.VirtualHost != nil && shardedHTTPProxy.Spec.Template.Spec.VirtualHost.TLS != nil {
					tls = shardedHTTPProxy.Spec.Template.Spec.VirtualHost.TLS
				}

				httpProxy := r.createHTTPProxy(shardedHTTPProxy, fmt.Sprintf("%s-%d", mainHTTPProxyName, i), ingressClass, &contourv1.VirtualHost{
					Fqdn: host,
					TLS:  tls,
				})

				httpProxies = append(httpProxies, NewChildObj{
					Shard:     shard.ShardNumber,
					ShardName: ingressClass,
					Obj:       httpProxy,
				})
			}
		}
	}
	return httpProxies, nil
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
