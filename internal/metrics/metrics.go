package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	WaitingListGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shardedcontroller_resharding_list_size",
			Help: "Number of objects that are waiting for resharding and are scheduled for updates in the cluster.",
		},
		[]string{"controller"},
	)

	ReadyListGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shardedcontroller_ready_list_size",
			Help: "Number of objects that have been successfully checked and require no further actions.",
		},
		[]string{"controller"},
	)

	ManagedListGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shardedcontroller_managed_list_size",
			Help: "Number of objects in the cluster that the controller is managing and monitoring for changes.",
		},
		[]string{"controller"},
	)

	ErrorListGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shardedcontroller_error_list",
			Help: "Number of objects that have errored and cannot be reconciled in the cluster.",
		},
		[]string{"controller", "namespace", "name"},
	)

	ProcessingCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shardedcontroller_processing_total",
			Help: "Total number of times the controller has applied changes to any objects in the cluster.",
		},
		[]string{"controller", "ingress_class"},
	)

	DeletingCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shardedcontroller_deletions_total",
			Help: "Total number of times the controller has lost control over objects because they stopped existing (deleted).",
		},
		[]string{"controller"},
	)

	ShardedIngressClassObjectCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shardedcontroller_shardedclass_objects_count",
			Help: "Number of sharded objects owned by the controller, grouped by ingress class",
		},
		[]string{"controller", "ingress_class"},
	)

	ChildIngressClassObjectCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shardedcontroller_ingressclass_objects_count",
			Help: "Number of child objects owned by the controller, grouped by ingress class",
		},
		[]string{"controller", "ingress_class"},
	)
)

func init() {
	metrics.Registry.MustRegister(WaitingListGauge, ReadyListGauge, ManagedListGauge, ErrorListGauge, ProcessingCounter, DeletingCounter, ShardedIngressClassObjectCount, ChildIngressClassObjectCount)
}
