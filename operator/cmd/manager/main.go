// Command fleet-operator reconciles Fleet custom resources against a
// Kubernetes cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
	"github.com/zlogic-labs/fleet/operator/internal/controller"
	"github.com/zlogic-labs/fleet/operator/internal/report"
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
		reportTo    string
		reportEvery time.Duration
		clusterName string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe binds to")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"contend for leadership so only one replica reconciles")
	flag.StringVar(&namespace, "namespace", "",
		"restrict reconciliation to one namespace; empty means all of them")
	flag.StringVar(&reportTo, "report-to", "",
		"control plane base URL to report cluster inventory to, e.g. http://127.0.0.1:8081; empty disables reporting")
	flag.DurationVar(&reportEvery, "report-every", 30*time.Second, "how often to report cluster inventory")
	flag.StringVar(&clusterName, "cluster-name", "",
		"name this operator reports its cluster under; defaults to the node name")
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

	if reportTo != "" {
		name, err := clusterNameFor(mgr, clusterName)
		if err != nil {
			return fmt.Errorf("name this cluster: %w", err)
		}
		reporter := report.New(reportTo, name)
		collector := &report.Collector{Reader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Version: Version}
		// Reporting is a manager Runnable rather than part of a reconcile:
		// an inventory report is a periodic fact about the whole cluster, not
		// a reaction to one object, and putting it in a reconcile would make
		// its frequency depend on how busy the cluster is.
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			return collector.Run(ctx, reporter, reportEvery)
		})); err != nil {
			return fmt.Errorf("register inventory reporter: %w", err)
		}
		logger.Info("will report inventory", "to", reportTo, "cluster", name, "every", reportEvery)
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

// clusterNameFor is the name this operator reports its cluster under.
//
// The node name is the fallback because it is the one identifier a single-node
// install always has, and the console needs something to key a report on. It is
// only ever a fallback: a control plane serving several clusters needs names
// that mean something to their operators, and a hostname does not.
func clusterNameFor(mgr ctrl.Manager, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	var nodes corev1.NodeList
	if err := mgr.GetAPIReader().List(context.Background(), &nodes); err != nil {
		return "", fmt.Errorf("read node name: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes in the cluster and --cluster-name not set")
	}
	return nodes.Items[0].Name, nil
}
