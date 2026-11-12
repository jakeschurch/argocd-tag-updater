package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// maxConditionMessage bounds condition messages built from Argo text. The CRD
// rejects messages over 32768 bytes, and one oversized message would fail
// every status write; helm or kustomize render errors from several
// Applications easily exceed that.
const maxConditionMessage = 1024

func boundMessage(message string) string {
	const suffix = " ... (truncated)"
	if len(message) <= maxConditionMessage {
		return message
	}
	cut := maxConditionMessage - len(suffix)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + suffix
}

// setDegraded records the problem in the Degraded condition and gauge and
// reports whether the condition changed.
func setDegraded(tu *v1.TagUpdater, problem *argoCDProblem) bool {
	condition := metav1.Condition{Type: degradedCondition, Status: metav1.ConditionFalse, Reason: "ApplicationsReporting", Message: "ArgoCD compares and syncs the release's Applications without errors"}
	value := 0.0
	if problem != nil {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, problem.Reason, boundMessage(problem.Message)
		value = 1
	}
	releaseDegraded.WithLabelValues(tu.Namespace, tu.Name).Set(value)
	return meta.SetStatusCondition(&tu.Status.Conditions, condition)
}

// dropDegraded removes the signal from a TagUpdater that no longer names an
// Application to read, so a stale Degraded=True and gauge cannot linger.
func dropDegraded(tu *v1.TagUpdater) bool {
	releaseDegraded.DeleteLabelValues(tu.Namespace, tu.Name)
	return meta.RemoveStatusCondition(&tu.Status.Conditions, degradedCondition)
}

// errApplicationMissing marks a read where the Application itself is absent,
// as opposed to a missing resource it reports (for example a StatefulSet).
var errApplicationMissing = errors.New("application is missing")

// releaseProblem reads the release's Applications and returns the first reason
// they cannot certify it. A deleted Application is a problem (it cannot vouch
// for anything) and is returned with its read error. Other read errors return
// no problem: a transient API failure must not flap the signal.
func (r *TagUpdaterReconciler) releaseProblem(ctx context.Context, tu *v1.TagUpdater, operationsSince time.Time) (argoCDState, *argoCDProblem, error) {
	state, err := r.releaseHealth(ctx, tu)
	if err != nil {
		if errors.Is(err, errApplicationMissing) {
			return state, &argoCDProblem{Reason: "ApplicationMissing", Message: err.Error()}, err
		}
		return state, nil, err
	}
	return state, state.problem(operationsSince), nil
}

// recordProblem updates Degraded unless an unreadable Application left the
// release's state unknown.
func recordProblem(tu *v1.TagUpdater, problem *argoCDProblem, err error) bool {
	if err != nil && problem == nil {
		return false
	}
	return setDegraded(tu, problem)
}

// observeSettledRelease keeps RollbackReady truthful after a watch closes. A
// release ArgoCD can no longer compare or sync is not DeploymentHealthy; the
// claim returns once the Applications are Synced and Healthy again. It never
// blocks writes: a newer release may be the fix.
func (r *TagUpdaterReconciler) observeSettledRelease(ctx context.Context, tu *v1.TagUpdater) error {
	if !gated(tu) || len(healthApps(tu)) == 0 {
		if dropDegraded(tu) {
			return r.Status().Update(ctx, tu)
		}
		return nil
	}
	// Only failures since the last write can describe the current release.
	operationsSince := time.Time{}
	if tu.Status.LastUpdated != nil {
		operationsSince = tu.Status.LastUpdated.Time
	}
	state, problem, err := r.releaseProblem(ctx, tu, operationsSince)
	if err != nil {
		log.FromContext(ctx).Info("could not read release Applications", "err", err)
		if problem == nil {
			return nil
		}
	}
	changed := setDegraded(tu, problem)
	ready := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady")
	switch {
	case problem != nil && ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == "DeploymentHealthy":
		// The Argo detail lives in Degraded; this message stays stable.
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "ReleaseDegraded", Message: boundMessage(fmt.Sprintf("ArgoCD no longer reports tag %s Synced and Healthy (%s); see the Degraded condition", tu.Status.LastTag, problem.Reason))})
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
