package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

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
