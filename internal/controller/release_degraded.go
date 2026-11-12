package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// degradedCondition reports Argo states that make the release's health
// unknowable: the Application cannot be compared, reports an error condition,
// or its sync operation failed. It is a signal, not blame; rollback is decided
// by the health gate alone.
const degradedCondition = "Degraded"

var releaseDegraded = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "tagupdater_release_degraded", Help: "1 while ArgoCD reports the release's Application with sync Unknown, an error condition or a failed sync operation."}, []string{"namespace", "name"})

// 2026-11-12: argocd/margo sat at sync Unknown with ComparisonError for ~5h
// while RollbackReady said DeploymentHealthy. These states must never read as
// healthy, and must be visible without waiting for the rollback timeout.
type argoCDProblem struct {
	Reason  string
	Message string
}

// errorConditions keeps Application conditions of an *Error type (for example
// ComparisonError, InvalidSpecError, SyncError). Warnings are not failures.
// comparison reports an error that leaves ArgoCD unable to compare the spec.
func errorConditions(app map[string]interface{}) (out []string, comparison bool) {
	raw, _, _ := unstructured.NestedSlice(app, "status", "conditions")
	for _, item := range raw {
		condition, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := condition["type"].(string)
		if !strings.HasSuffix(kind, "Error") {
			continue
		}
		message, _ := condition["message"].(string)
		out = append(out, strings.TrimSpace(kind+": "+message))
		comparison = comparison || kind == "ComparisonError" || kind == "InvalidSpecError"
	}
	return out, comparison
}

func operationFailed(phase string) bool {
	return phase == "Failed" || phase == "Error"
}

// problem returns the first reason this state cannot certify a release.
// operationsSince ignores failed operations that started before the release
// was written: they cannot describe it. A zero time counts every failure.
func (s argoCDState) problem(operationsSince time.Time) *argoCDProblem {
	if s.Sync == "Unknown" {
		message := "ArgoCD sync status is Unknown"
		if len(s.Errors) > 0 {
			message += ": " + strings.Join(s.Errors, "; ")
		}
		return &argoCDProblem{Reason: "ComparisonUnavailable", Message: message}
	}
	if s.ComparisonFailed {
		return &argoCDProblem{Reason: "ComparisonError", Message: strings.Join(s.Errors, "; ")}
	}
	if len(s.Errors) > 0 {
		return &argoCDProblem{Reason: "ApplicationError", Message: strings.Join(s.Errors, "; ")}
	}
	if s.FailedOperation != "" && !s.FailedOperationAt.Before(operationsSince) {
		return &argoCDProblem{Reason: "SyncFailed", Message: s.FailedOperation}
	}
	return nil
}

// setDegraded records the problem in the Degraded condition and gauge and
// reports whether the condition changed.
func setDegraded(tu *v1.TagUpdater, problem *argoCDProblem) bool {
	condition := metav1.Condition{Type: degradedCondition, Status: metav1.ConditionFalse, Reason: "ApplicationsReporting", Message: "ArgoCD compares and syncs the release's Applications without errors"}
	value := 0.0
	if problem != nil {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, problem.Reason, problem.Message
		value = 1
	}
	releaseDegraded.WithLabelValues(tu.Namespace, tu.Name).Set(value)
	return meta.SetStatusCondition(&tu.Status.Conditions, condition)
}

// observeSettledRelease keeps RollbackReady truthful after a watch closes. A
// release ArgoCD can no longer compare or sync is not DeploymentHealthy; the
// claim returns once the Applications are Synced and Healthy again. It never
// blocks writes: a newer release may be the fix.
func (r *TagUpdaterReconciler) observeSettledRelease(ctx context.Context, tu *v1.TagUpdater) error {
	if !gated(tu) || len(healthApps(tu)) == 0 {
		return nil
	}
	state, err := r.releaseHealth(ctx, tu)
	if err != nil {
		log.FromContext(ctx).Info("could not read release Applications", "err", err)
		return nil
	}
	// Only failures since the last write can describe the current release.
	operationsSince := time.Time{}
	if tu.Status.LastUpdated != nil {
		operationsSince = tu.Status.LastUpdated.Time
	}
	problem := state.problem(operationsSince)
	changed := setDegraded(tu, problem)
	ready := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
	switch {
	case problem != nil && ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == "DeploymentHealthy":
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "ReleaseDegraded", Message: fmt.Sprintf("ArgoCD no longer reports tag %s Synced and Healthy: %s", tu.Status.LastTag, problem.Message)})
		changed = true
	case problem == nil && ready != nil && ready.Reason == "ReleaseDegraded" && state.Sync == "Synced" && state.Health == "Healthy" && !state.syncInFlight():
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "DeploymentHealthy", Message: "ArgoCD reports the written release Synced and Healthy"})
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, tu)
}

// comparedPinApplied proves ArgoCD compared the written pins, not just that
// they reached the Application spec. status.sync.comparedTo is the source and
// destination of the last comparison; until it carries a pin, Synced and
// Healthy describe the previous release.
func comparedPinApplied(app map[string]interface{}, field string, expected *string) bool {
	rest, ok := strings.CutPrefix(field, "spec.")
	if !ok {
		return true
	}
	root, _, _ := strings.Cut(rest, ".")
	if root != "source" && root != "sources" && root != "destination" {
		return true
	}
	comparedTo, ok, _ := unstructured.NestedMap(app, "status", "sync", "comparedTo")
	if !ok {
		return false
	}
	current, ok := lookupField(comparedTo, rest)
	return ok && valueMatches(current, expected)
}

func valueMatches(current interface{}, expected *string) bool {
	if expected == nil {
		return current == nil
	}
	return current != nil && fmt.Sprint(current) == *expected
}
