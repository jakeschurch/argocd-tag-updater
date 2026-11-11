package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/controller"
	"github.com/jakeschurch/argocd-tag-updater/internal/gate"
	"github.com/jakeschurch/argocd-tag-updater/internal/writeback"
)

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		leaderElect     bool
		staleMultiplier int
		staleFloor      time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Address to bind metrics endpoint")
	flag.StringVar(&probeAddr, "probe-addr", ":8081", "Address for healthz/readyz probes")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election")
	flag.IntVar(&staleMultiplier, "reconcile-stale-multiplier", 10,
		"A TagUpdater is stale when no reconcile succeeded within max(multiplier*interval, floor)")
	flag.DurationVar(&staleFloor, "reconcile-stale-floor", 15*time.Minute,
		"Minimum reconcile-progress staleness window regardless of interval")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))
	releaseWritebackTempDir, err := writeback.PrepareWritebackTempDir()
	if err != nil {
		fail("prepare write-back temp directory: %v", err)
	}
	defer releaseWritebackTempDir()

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

	watchNamespace := os.Getenv("WATCH_NAMESPACE")
	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "argocd-tag-updater.updater.argocd.io",
	}
	if watchNamespace != "" {
		// Credentials are always namespace-local to a TagUpdater. Restrict the
		// Secret informer accordingly so the controller never needs to list
		// Secrets across the cluster.
		mgrOptions.Cache = cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Namespaces: map[string]cache.Config{watchNamespace: {}}},
		}}
	}
	mgr, err := ctrl.NewManager(cfg, mgrOptions)
	if err != nil {
		fail("new manager: %v", err)
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fail("dynamic client: %v", err)
	}
	allowed := gate.Defaults
	if value, present := os.LookupEnv("GATE_KINDS"); present {
		if err := json.Unmarshal([]byte(value), &allowed); err != nil {
			fail("parse GATE_KINDS: %v", err)
		}
	}
	gateRegistry, err := gate.NewRegistry(dynClient, allowed)
	if err != nil {
		fail("gate registry: %v", err)
	}

	reconciler := &controller.TagUpdaterReconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		Dynamic:         dynClient,
		Gates:           gateRegistry,
		Recorder:        mgr.GetEventRecorderFor("argocd-tag-updater"), //nolint:staticcheck // controller currently uses the typed core/v1 recorder API
		StaleMultiplier: staleMultiplier,
		StaleFloor:      staleFloor,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		fail("setup controller: %v", err)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := registerProbes(ctx, mgr, mgr.GetCache(), reconciler.RevResolutionHealthz(), reconciler.ReconcileProgressHealthz()); err != nil {
		fail("register probes: %v", err)
	}

	if err := mgr.Start(ctx); err != nil {
		fail("start manager: %v", err)
	}
}

// probeRegistrar keeps the endpoint wiring testable without a running manager.
type probeRegistrar interface {
	AddHealthzCheck(string, healthz.Checker) error
	AddReadyzCheck(string, healthz.Checker) error
}

type cacheSyncer interface {
	WaitForCacheSync(context.Context) bool
}

func registerProbes(ctx context.Context, registrar probeRegistrar, cache cacheSyncer, tagResolution, reconcileProgress healthz.Checker) error {
	// 2026-11-11: these in-process watchdogs restart a frozen leader so its
	// standby can take over. Git staleness must not remove readiness and block
	// eviction of the healthy standby through the PodDisruptionBudget.
	for _, check := range []struct {
		name  string
		check healthz.Checker
	}{
		{"healthz", healthz.Ping},
		{"tag-resolution", tagResolution},
		{"reconcile-progress", reconcileProgress},
	} {
		if err := registrar.AddHealthzCheck(check.name, check.check); err != nil {
			return fmt.Errorf("register healthz %s: %w", check.name, err)
		}
	}
	return registrar.AddReadyzCheck("readyz", func(request *http.Request) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Leave time for the error response within the kubelet's 1s timeout.
		probeCtx, cancel := context.WithTimeout(request.Context(), 500*time.Millisecond)
		defer cancel()
		if !cache.WaitForCacheSync(probeCtx) {
			return fmt.Errorf("cache not synchronized")
		}
		return ctx.Err()
	})
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "argocd-tag-updater: "+format+"\n", args...)
	os.Exit(1)
}
