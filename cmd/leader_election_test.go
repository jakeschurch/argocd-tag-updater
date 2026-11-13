package main

import (
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

func TestLeaderElectionOptions(t *testing.T) {
	options := leaderElectionOptions(ctrl.Options{})
	for _, timing := range []struct {
		name string
		got  *time.Duration
		want time.Duration
	}{
		{"LeaseDuration", options.LeaseDuration, 60 * time.Second},
		{"RenewDeadline", options.RenewDeadline, 40 * time.Second},
		{"RetryPeriod", options.RetryPeriod, 10 * time.Second},
	} {
		if timing.got == nil {
			t.Fatalf("%s is unset; want %s", timing.name, timing.want)
		}
		if *timing.got != timing.want {
			t.Errorf("%s = %s; want %s", timing.name, *timing.got, timing.want)
		}
	}
	if *options.LeaseDuration <= *options.RenewDeadline {
		t.Error("LeaseDuration must exceed RenewDeadline")
	}
	if *options.RenewDeadline <= *options.RetryPeriod*6/5 {
		t.Error("RenewDeadline must exceed 1.2 * RetryPeriod")
	}
}
