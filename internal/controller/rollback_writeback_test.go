package controller

import (
	"testing"
	"time"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func TestArgoCDStateObservesWrittenCommit(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	if !(argoCDState{Revision: commit}).observes(commit) {
		t.Fatal("single-source revision did not match pushed commit")
	}
	if !(argoCDState{Revisions: []string{"other", commit}}).observes(commit) {
		t.Fatal("multi-source revisions did not match pushed commit")
	}
	if (argoCDState{Revision: "other"}).observes(commit) {
		t.Fatal("different revision matched pushed commit")
	}
}

func TestRevisionObserverPrefersManagingApp(t *testing.T) {
	health := &v1alpha1.ArgoCDAppRef{Name: "workload"}
	managing := &v1alpha1.ArgoCDAppRef{Name: "app-of-apps"}
	tu := &v1alpha1.TagUpdater{Spec: v1alpha1.TagUpdaterSpec{ArgoCDApp: health}}
	if got := revisionObserver(tu); got != health {
		t.Fatalf("direct-managed observer = %#v", got)
	}
	tu.Spec.ManagingApp = managing
	if got := revisionObserver(tu); got != managing {
		t.Fatalf("app-of-apps observer = %#v", got)
	}
}

func TestArgoCDStateSyncInFlight(t *testing.T) {
	for _, phase := range []string{"Running", "Terminating"} {
		if !(argoCDState{Operation: phase}).syncInFlight() {
			t.Errorf("phase %s should be in flight", phase)
		}
	}
	for _, phase := range []string{"", "Succeeded", "Failed", "Error"} {
		if (argoCDState{Operation: phase}).syncInFlight() {
			t.Errorf("phase %s should be terminal", phase)
		}
	}
}

func TestRollbackDefaultsAndSkippedTags(t *testing.T) {
	if defaultRollbackTimeout != 20*time.Minute {
		t.Fatalf("default rollback timeout = %s", defaultRollbackTimeout)
	}
	filtered := filterSkipped([]string{"v1", "v2", "v3"}, []string{"v2"})
	if len(filtered) != 2 || filtered[0] != "v1" || filtered[1] != "v3" {
		t.Fatalf("filterSkipped returned %#v", filtered)
	}
	if got := appendUnique([]string{"v2"}, "v2"); len(got) != 1 {
		t.Fatalf("appendUnique duplicated failed tag: %#v", got)
	}
}
