package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func TestArgoCDStateObservesWrittenCommit(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	pushed := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	if !(argoCDState{Revision: commit}).observes(commit, &pushed) {
		t.Fatal("single-source revision did not match pushed commit")
	}
	if !(argoCDState{Revisions: []string{"other", commit}}).observes(commit, &pushed) {
		t.Fatal("multi-source revisions did not match pushed commit")
	}
	if (argoCDState{Revision: "other"}).observes(commit, &pushed) {
		t.Fatal("different revision matched pushed commit")
	}
	if (argoCDState{Revision: ""}).observes("", &pushed) {
		t.Fatal("empty commit must never count as observed")
	}
}

// status.sync.revision only names the newest revision ArgoCD has looked at, so
// by the time a rollout finishes the live head has usually moved on. A commit
// must count as observed once ArgoCD retains it anywhere — this is the case
// that reported CommitNeverObserved for commits sitting in the app's history.
func TestArgoCDStateObservesSupersededCommitFromHistory(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	pushed := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	state := argoCDState{
		Revision: "newerhead",
		Deployments: []argoCDDeployment{
			{Revisions: []string{"older"}, StartedAt: pushed.Add(-time.Minute)},
			{Revisions: []string{commit}, StartedAt: pushed.Add(30 * time.Second)},
		},
	}
	if !state.observes(commit, &pushed) {
		t.Fatal("commit retained in ArgoCD's deployment history was not observed")
	}
}

// A commit superseded before ArgoCD next resolves the branch is coalesced into
// a later rollout and never named anywhere in the Application's status, yet it
// is live. Any rollout that began after the push carries it, because the branch
// only ever fast-forwards.
func TestArgoCDStateObservesCommitCarriedByLaterDeployment(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	pushed := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	carried := argoCDState{
		Revision:    "descendant",
		Deployments: []argoCDDeployment{{Revisions: []string{"descendant"}, StartedAt: pushed.Add(time.Minute)}},
	}
	if !carried.observes(commit, &pushed) {
		t.Fatal("deployment that began after the push must carry the commit")
	}

	// A rollout already under way when the commit landed proves nothing, and
	// neither does one whose start ArgoCD never recorded.
	stale := argoCDState{
		Revision: "ancestor",
		Deployments: []argoCDDeployment{
			{Revisions: []string{"ancestor"}, StartedAt: pushed.Add(-time.Minute)},
			{Revisions: []string{"untimed"}},
		},
	}
	if stale.observes(commit, &pushed) {
		t.Fatal("rollout that began before the push must not count as observing it")
	}
}

// A watch armed by an older build carries no push time. Without one there is
// nothing to order deployments against, so only exact matches may count —
// guessing here would open the health gate on an unrelated rollout.
func TestArgoCDStateWithoutPushTimeRequiresExactMatch(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	state := argoCDState{
		Revision:    "descendant",
		Deployments: []argoCDDeployment{{Revisions: []string{"descendant"}, StartedAt: time.Now()}},
	}
	if state.observes(commit, nil) {
		t.Fatal("missing push time must fall back to exact matching")
	}
	state.Deployments = append(state.Deployments, argoCDDeployment{Revisions: []string{commit}})
	if !state.observes(commit, nil) {
		t.Fatal("exact match must still hold without a push time")
	}
}

func TestDeploymentsFromArgoCDStatus(t *testing.T) {
	app := map[string]interface{}{
		"status": map[string]interface{}{
			"history": []interface{}{
				map[string]interface{}{
					"revision":        "first",
					"deployStartedAt": "2026-09-20T16:11:31Z",
					"deployedAt":      "2026-09-20T16:12:37Z",
				},
				// deployStartedAt is absent on older ArgoCD releases; the
				// completion time is the only ordering left.
				map[string]interface{}{"revision": "second", "deployedAt": "2026-09-20T16:15:41Z"},
				map[string]interface{}{"revision": ""},
				// A multi-source rollout names one revision per source and
				// leaves the singular field empty.
				map[string]interface{}{"revisions": []interface{}{"multi-a", "multi-b"}, "deployedAt": "2026-09-20T16:17:00Z"},
			},
			"operationState": map[string]interface{}{
				"phase":      "Succeeded",
				"startedAt":  "2026-09-20T16:19:11Z",
				"syncResult": map[string]interface{}{"revision": "third"},
			},
		},
	}

	got := deploymentsFrom(app)
	if len(got) != 4 {
		t.Fatalf("deployments = %#v, want first, second, the multi-source rollout and the succeeded operation", got)
	}
	if got[0].Revisions[0] != "first" || !got[0].StartedAt.Equal(time.Date(2026, 9, 20, 16, 11, 31, 0, time.UTC)) {
		t.Errorf("history start = %#v, want deployStartedAt", got[0])
	}
	if got[1].Revisions[0] != "second" || !got[1].StartedAt.Equal(time.Date(2026, 9, 20, 16, 15, 41, 0, time.UTC)) {
		t.Errorf("history fallback = %#v, want deployedAt", got[1])
	}
	if len(got[2].Revisions) != 2 || got[2].Revisions[0] != "multi-a" || got[2].Revisions[1] != "multi-b" {
		t.Errorf("multi-source rollout = %#v, want both source revisions", got[2])
	}
	if got[3].Revisions[0] != "third" || !got[3].StartedAt.Equal(time.Date(2026, 9, 20, 16, 19, 11, 0, time.UTC)) {
		t.Errorf("in-flight operation = %#v, want the succeeded sync result", got[3])
	}

	// A sync still running has deployed nothing, so it must not be offered as
	// evidence that anything reached the cluster.
	running := map[string]interface{}{"status": map[string]interface{}{
		"operationState": map[string]interface{}{
			"phase":      "Running",
			"startedAt":  "2026-09-20T16:19:11Z",
			"syncResult": map[string]interface{}{"revision": "third"},
		},
	}}
	if got := deploymentsFrom(running); len(got) != 0 {
		t.Errorf("running operation contributed %#v", got)
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
