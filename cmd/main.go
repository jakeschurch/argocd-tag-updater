package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fail("add clientgo scheme: %v", err)
	}
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

	reconciler := &controller.TagUpdaterReconciler{
		Client:  mgr.GetClient(),
		Dynamic: dynClient,
		Mapper:  mgr.GetRESTMapper(),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		fail("setup controller: %v", err)
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	// Liveness also fails when git tag->rev resolution has been broken past its
	// staleness window, so a sustained resolver outage restarts the pod and
	// migrates leadership instead of silently pinning targets at a stale tag.
	_ = mgr.AddHealthzCheck("tag-resolution", reconciler.RevResolutionHealthz())
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail("start manager: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "argocd-tag-updater: "+format+"\n", args...)
	os.Exit(1)
}
