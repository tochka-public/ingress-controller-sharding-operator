package controller

import (
	"context"
	"fmt"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	controllerv1 "k8s.tochka.com/sharded-ingress-controller/api/v1"
)

// ShardedIngressReconciler reconciles a ShardedIngress object
type ShardedIngressReconciler struct {
	ShardedReconciler
	*controllerv1.ShardedIngress
	ChildObject networkingv1.Ingress
}

func (r *ShardedIngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		ShardedObject:                            r.ShardedIngress,
		ChildObject:                              &r.ChildObject,
		objKey:                                   req.NamespacedName.String(),
		ctrlName:                                 "shardedingress",
	}

	if !r.Initialized {
		r.initializeCache()
		if err := r.CheckClusterShards(); err != nil {
			return ctrl.Result{}, err
		}
		r.Initialized = true
	}

	// Fetch the ShardedIngress instance
	err := r.Get(ctx, req.NamespacedName, r.ShardedIngress)
	if err != nil {
		if errors.IsNotFound(err) {
			r.handleNotFound(r.objKey, logger)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch ShardedIngress")
		return ctrl.Result{}, err
	}

	if val := r.ShardedIngress.Annotations[*r.AllShardsPlacementAnnotation]; val == "true" {
		r.UseAllShards = true
	}

	if err := r.setShardInfo(logger); err != nil {
		return ctrl.Result{}, nil
	}

	if !r.keyWaited(r.objKey) {
		return r.applyRateLimit(r.objKey, logger)
	}

	// Convert the ShardedIngress to multiple Ingress objects
	ingresses, err := r.NewIngressFromShardedIngress()
	if err != nil {
		logger.Error(err, "children object can't be generated")
		return ctrl.Result{}, nil
	}

	r.updateMetrics()
	return r.applyObjectsToCluster(ingresses)
}

func (r *ShardedIngressReconciler) NewIngressFromShardedIngress() ([]NewChildObj, error) {
	var ingresses []NewChildObj

	for _, shard := range r.Shards {
		shardedIngress := r.ShardedObject.(*controllerv1.ShardedIngress).DeepCopy()
		if shardedIngress.Spec.Template.Labels == nil {
			shardedIngress.Spec.Template.Labels = make(map[string]string)
		}
		if shardedIngress.Spec.Template.Annotations == nil {
			shardedIngress.Spec.Template.Annotations = make(map[string]string)
		}

		conflict := r.CheckReshardingConflict(shard.ShardName, fmt.Sprintf("%s-%d", shardedIngress.Name, shard.ShardNumber))
		ingressClass := shard.ShardName
		tempName := fmt.Sprintf("%s-%d-%s", shardedIngress.Name, shard.ShardNumber, "tmp")
		obj := r.ShardedReconciler.ChildObject

		err := r.Get(r.ctx, types.NamespacedName{Name: tempName, Namespace: shardedIngress.GetNamespace()}, obj)
		if err != nil {
			if errors.IsNotFound(err) && conflict != "" {
				// Create a deep copy for the tmp object to modify
				tempShardedIngress := shardedIngress.DeepCopy()
				tempShardedIngress.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = conflict
				tempIngress := r.createIngress(tempShardedIngress, tempName, conflict)
				ingresses = append(ingresses, NewChildObj{
					Shard:     shard.ShardNumber,
					ShardName: shard.ShardName,
					Obj:       tempIngress,
					OldShard:  "",
				})
				tempShardedIngress.Spec.Template.Annotations["old-shard"] = conflict
				ingressClass = conflict
			}
		} else {
			if oldClass, ok := r.checkTmpObjAnnotations(obj.GetAnnotations()); ok {
				ingressClass = oldClass
				conflict = oldClass
			}
		}

		mainIngressName := shardedIngress.Name
		shardedIngress.Spec.Template.Labels[*r.AdditionalServiceDiscoveryClassLabel] = ingressClass

		if r.UseAllShards && shardedIngress.Spec.Template.Labels[*r.AppNameLabel] != "" {
			app := shardedIngress.Spec.Template.Labels[*r.AppNameLabel]
			existingTags := shardedIngress.Spec.Template.Annotations[*r.AdditionalServiceDiscoveryTagsAnnotation]
			if existingTags != "" {
				shardedIngress.Spec.Template.Annotations[*r.AdditionalServiceDiscoveryTagsAnnotation] = existingTags + "," + ingressClass
			} else {
				shardedIngress.Spec.Template.Annotations[*r.AdditionalServiceDiscoveryTagsAnnotation] = ingressClass
			}

			if len(shardedIngress.Spec.Template.Spec.Rules) > 0 {
				firstRule := shardedIngress.Spec.Template.Spec.Rules[0]
				for _, host := range *r.AllShardsBaseHosts {
					newRule := firstRule.DeepCopy()
					newRule.Host = fmt.Sprintf("%s.%s-%s.%s", ingressClass, shardedIngress.Namespace, app, host)
					shardedIngress.Spec.Template.Spec.Rules = append(shardedIngress.Spec.Template.Spec.Rules, *newRule)
				}
			}
		}

		if r.ShardedObject.GetIngressClassName() != shard.ShardName {
			mainIngressName = fmt.Sprintf("%s-%d", shardedIngress.Name, shard.ShardNumber)
		}

		mainIngress := r.createIngress(shardedIngress, mainIngressName, ingressClass)
		ingresses = append(ingresses, NewChildObj{
			Shard:     shard.ShardNumber,
			ShardName: shard.ShardName,
			Obj:       mainIngress,
			OldShard:  conflict,
		})
	}
	return ingresses, nil
}

func (r *ShardedIngressReconciler) createIngress(shardedIngress *controllerv1.ShardedIngress, name, ingressClass string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   shardedIngress.Namespace,
			Annotations: shardedIngress.Spec.Template.Annotations,
			Labels:      shardedIngress.Spec.Template.Labels,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			DefaultBackend:   shardedIngress.Spec.Template.Spec.DefaultBackend,
			TLS:              shardedIngress.Spec.Template.Spec.TLS,
			Rules:            shardedIngress.Spec.Template.Spec.Rules,
		},
	}
}

func updateIngressObj(old, new *networkingv1.Ingress) *networkingv1.Ingress {
	old.Spec = new.Spec
	old.Annotations = new.Annotations
	old.Labels = new.Labels
	old.OwnerReferences = new.OwnerReferences

	return old
}

func (r *ShardedIngressReconciler) SetupWithManager(mgr ctrl.Manager, parallel int, qps int, burst int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controllerv1.ShardedIngress{}).Owns(&networkingv1.Ingress{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: parallel,
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](ExponentialBackoffBaseDelay, ExponentialBackoffMaxDelay),
				&workqueue.TypedBucketRateLimiter[ctrl.Request]{Limiter: rate.NewLimiter(rate.Limit(qps), burst)},
			)}).
		Complete(r)
}

func (r *ShardedIngressReconciler) GetCreatedObjects() *map[string][]map[string]string {
	return &r.Status.CreatedObjects
}

func (r *ShardedIngressReconciler) SetCreatedObjects(s map[string][]map[string]string) {
	r.Status.CreatedObjects = s
}
