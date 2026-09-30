// Command fleet-operator reconciles Fleet custom resources against a
// Kubernetes cluster.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/controller"
)

// Version is stamped at build time.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-operator:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
		namespace   string
		showVersion bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe binds to")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"contend for leadership so only one replica reconciles")
	flag.StringVar(&namespace, "namespace", "",
		"restrict reconciliation to one namespace; empty means all of them")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("fleet-operator", Version)
		return nil
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))

	opts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "fleet-operator.zlogic.com",
	}
	if namespace != "" {
		// Scoping the cache rather than filtering in Reconcile: an unscoped
		// watch on every namespace is a full-cluster LIST on startup, and the
		// operator would keep every Fleet resource in the cluster in memory
		// to serve one namespace.
		opts.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{namespace: {}},
		}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	if err := controller.New(mgr); err != nil {
		return fmt.Errorf("register controllers: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("readyz: %w", err)
	}

	logger.Info("starting", "version", Version, "namespace", orAll(namespace))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}

func orAll(ns string) string {
	if ns == "" {
		return "(all)"
	}
	return ns
}
