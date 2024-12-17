package main

import (
	"flag"
	"os"
	"sync"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	networkingv1 "k8s.io/api/networking/v1"
	controllerv1 "k8s.tochka.com/sharded-ingress-controller/api/v1"
	"k8s.tochka.com/sharded-ingress-controller/internal/config"
	"k8s.tochka.com/sharded-ingress-controller/internal/controller"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(controllerv1.AddToScheme(scheme))

	utilruntime.Must(contourv1.AddToScheme(scheme))

	utilruntime.Must(networkingv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var runShardedIngress bool
	var runShardedHTTPProxy bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&runShardedIngress, "sharded-ingress", false, "Run ShardedIngress Reconciler")
	flag.BoolVar(&runShardedHTTPProxy, "sharded-httpproxy", false, "Run ShardedHTTPProxy Reconciler")
	configPathFlag := flag.String("config", "config.yml", "path to config file")

	opts := zap.Options{
		Development:     true,
		StacktraceLevel: zapcore.FatalLevel,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	conf, err := config.LoadConfig(*configPathFlag, runShardedIngress, runShardedHTTPProxy)
	if err != nil {
		panic(err)
	}

	if !runShardedIngress && !runShardedHTTPProxy {
		setupLog.Info("invalid configuration", "at least one controller must be specified: use --sharded-ingress or --sharded-httpproxy or both")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ingress-sharding-controller.k8s.tochka.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if runShardedIngress {
		shardedIngressReconciler := &controller.ShardedIngressReconciler{
			ShardedReconciler: controller.ShardedReconciler{
				Client:                                   mgr.GetClient(),
				Scheme:                                   mgr.GetScheme(),
				MaxShards:                                conf.ShardedIngress.Shards,
				TerminationPeriod:                        &conf.RateLimit.UpdateCooldown.Object,
				ShardUpdateCooldown:                      &conf.RateLimit.UpdateCooldown.Shard,
				DomainSubstring:                          &conf.General.DomainSubstring,
				MutatingWebhookAnnotation:                &conf.General.Annotations.MutatingWebhook,
				UnregisterAnnotation:                     &conf.AdditionalServiceDiscovery.Annotations.Unregistering,
				AdditionalServiceDiscoveryClassLabel:     &conf.AdditionalServiceDiscovery.Labels.Class,
				AdditionalServiceDiscoveryTagsAnnotation: &conf.AdditionalServiceDiscovery.Annotations.Tags,
				AppNameLabel:                             &conf.AdditionalServiceDiscovery.Labels.AppName,
				AllShardsPlacementAnnotation:             &conf.AllShardsPlacement.Annotations.Enabled,
				AllShardsBaseHosts:                       &conf.AllShardsPlacement.ShardBaseDomains,
				WaitingList:                              make(map[string]bool),
				ReadyList:                                make(map[string]bool),
				ManagedList:                              make(map[string]bool),
				ErrorList:                                make(map[string]bool),
				ShardedCache:                             &sync.Map{},
				ChildCache:                               &sync.Map{},
			},
			ShardedIngress: &controllerv1.ShardedIngress{},
			ChildObject:    networkingv1.Ingress{},
		}

		if err := shardedIngressReconciler.SetupWithManager(mgr, 1, conf.RateLimit.ApiRateLimit, conf.RateLimit.ApiBurstLimit); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ShardedIngress")
			os.Exit(1)
		}
	}

	if runShardedHTTPProxy {
		shardedHTTPProxyReconciler := &controller.ShardedHTTPProxyReconciler{
			ShardedReconciler: controller.ShardedReconciler{
				Client:                                   mgr.GetClient(),
				Scheme:                                   mgr.GetScheme(),
				MaxShards:                                conf.ShardedIngress.Shards,
				TerminationPeriod:                        &conf.RateLimit.UpdateCooldown.Object,
				ShardUpdateCooldown:                      &conf.RateLimit.UpdateCooldown.Shard,
				DomainSubstring:                          &conf.General.DomainSubstring,
				MutatingWebhookAnnotation:                &conf.General.Annotations.MutatingWebhook,
				UnregisterAnnotation:                     &conf.AdditionalServiceDiscovery.Annotations.Unregistering,
				AdditionalServiceDiscoveryClassLabel:     &conf.AdditionalServiceDiscovery.Labels.Class,
				RootHTTPProxyLabel:                       &conf.ShardedHTTPProxy.Labels.RootHTTPProxy,
				VirtualHostsHTTPProxyAnnotation:          &conf.ShardedHTTPProxy.Annotations.VirtualHosts,
				AdditionalServiceDiscoveryTagsAnnotation: &conf.AdditionalServiceDiscovery.Annotations.Tags,
				AppNameLabel:                             &conf.AdditionalServiceDiscovery.Labels.AppName,
				AllShardsPlacementAnnotation:             &conf.AllShardsPlacement.Annotations.Enabled,
				AllShardsBaseHosts:                       &conf.AllShardsPlacement.ShardBaseDomains,
				WaitingList:                              make(map[string]bool),
				ReadyList:                                make(map[string]bool),
				ManagedList:                              make(map[string]bool),
				ErrorList:                                make(map[string]bool),
				ShardedCache:                             &sync.Map{},
				ChildCache:                               &sync.Map{},
			},
			ShardedHTTPProxy: &controllerv1.ShardedHTTPProxy{},
			ChildObject:      contourv1.HTTPProxy{},
		}

		if err := shardedHTTPProxyReconciler.SetupWithManager(mgr, 1, conf.RateLimit.ApiRateLimit, conf.RateLimit.ApiBurstLimit); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ShardedHTTPProxy")
			os.Exit(1)
		}
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
