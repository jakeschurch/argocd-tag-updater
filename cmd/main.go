package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/controller"
)

func main() {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
	)
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Address to bind metrics endpoint")
	flag.StringVar(&probeAddr, "probe-addr", ":8081", "Address for healthz/readyz probes")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election")
	flag.Parse()

	ctrl.SetLogger(zap.New())

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fail("add scheme: %v", err)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		fail("get rest config: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "argocd-tag-updater.updater.argocd.io",
	})
	if err != nil {
		fail("new manager: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fail("dynamic client: %v", err)
	}

	if err := (&controller.TagUpdaterReconciler{
		Client:  mgr.GetClient(),
		Dynamic: dynClient,
	}).SetupWithManager(mgr); err != nil {
		fail("setup controller: %v", err)
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail("start manager: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "argocd-tag-updater: "+format+"\n", args...)
	os.Exit(1)
}
