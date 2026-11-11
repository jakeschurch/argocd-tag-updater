package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

type fakeProbeRegistrar struct {
	health   map[string]healthz.Checker
	ready    map[string]healthz.Checker
	failName string
	err      error
}

func (f *fakeProbeRegistrar) AddHealthzCheck(name string, check healthz.Checker) error {
	if name == f.failName {
		return f.err
	}
	if f.health == nil {
		f.health = map[string]healthz.Checker{}
	}
	f.health[name] = check
	return nil
}

func (f *fakeProbeRegistrar) AddReadyzCheck(name string, check healthz.Checker) error {
	if name == f.failName {
		return f.err
	}
	if f.ready == nil {
		f.ready = map[string]healthz.Checker{}
	}
	f.ready[name] = check
	return nil
}

type fakeCacheSyncer func(context.Context) bool

func (f fakeCacheSyncer) WaitForCacheSync(ctx context.Context) bool { return f(ctx) }

func TestRegisterProbesKeepsWatchdogsOnLiveness(t *testing.T) {
	resolutionFailure := errors.New("git resolution stale")
	progressFailure := errors.New("reconcile loop frozen")
	registrar := &fakeProbeRegistrar{}
	cache := fakeCacheSyncer(func(context.Context) bool { return true })
	err := registerProbes(context.Background(), registrar, cache,
		func(*http.Request) error { return resolutionFailure },
		func(*http.Request) error { return progressFailure })
	if err != nil {
		t.Fatal(err)
	}
	if len(registrar.health) != 3 || len(registrar.ready) != 2 {
		t.Fatalf("probe mapping: health=%v ready=%v", registrar.health, registrar.ready)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for name, want := range map[string]error{
		"healthz": nil, "tag-resolution": resolutionFailure, "reconcile-progress": progressFailure,
	} {
		check, ok := registrar.health[name]
		if !ok {
			t.Fatalf("missing healthz check %q", name)
		}
		if err := check(request); !errors.Is(err, want) {
			t.Errorf("healthz %s = %v; want %v", name, err, want)
		}
	}
	capability, ok := registrar.ready["gated-promotion-v2"]
	if !ok {
		t.Fatal("missing gated-promotion-v2 check")
	}
	if err := capability(request); err != nil {
		t.Fatalf("gated-promotion-v2 check: %v", err)
	}
	check, ok := registrar.ready["readyz"]
	if !ok {
		t.Fatal("missing readyz check")
	}
	if err := check(request); err != nil {
		t.Fatalf("watchdog failures must not affect cache-synced readiness: %v", err)
	}
}

func TestRegisteredReadiness(t *testing.T) {
	for _, name := range []string{"synced", "unsynced", "shutdown", "shutdown-during-sync", "request-canceled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			called := false
			cache := fakeCacheSyncer(func(probeCtx context.Context) bool {
				called = true
				switch name {
				case "unsynced", "request-canceled":
					<-probeCtx.Done()
					return false
				case "shutdown-during-sync":
					cancel()
				}
				return true
			})
			if name == "shutdown" {
				cancel()
			}
			if name == "request-canceled" {
				requestCtx, cancelRequest := context.WithCancel(request.Context())
				cancelRequest()
				request = request.WithContext(requestCtx)
			}
			registrar := &fakeProbeRegistrar{}
			if err := registerProbes(ctx, registrar, cache, healthz.Ping, healthz.Ping); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			err := registrar.ready["readyz"](request)
			if name == "synced" && err != nil {
				t.Fatalf("synced cache: %v", err)
			}
			if name != "synced" && err == nil {
				t.Fatal("readiness unexpectedly passed")
			}
			if name == "shutdown" && called {
				t.Fatal("shutdown must fail without waiting on cache")
			}
			if elapsed := time.Since(start); elapsed >= time.Second {
				t.Fatalf("readiness exceeded kubelet timeout: %s", elapsed)
			}
		})
	}
}

func TestRegisterProbesReportsRegistrationErrors(t *testing.T) {
	want := errors.New("registration failed")
	for _, name := range []string{"healthz", "tag-resolution", "reconcile-progress", "gated-promotion-v2", "readyz"} {
		t.Run(name, func(t *testing.T) {
			registrar := &fakeProbeRegistrar{failName: name, err: want}
			cache := fakeCacheSyncer(func(context.Context) bool { return true })
			if err := registerProbes(context.Background(), registrar, cache, healthz.Ping, healthz.Ping); !errors.Is(err, want) {
				t.Fatalf("registerProbes = %v; want %v", err, want)
			}
		})
	}
}
