package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/matcher"
	"github.com/jakeschurch/argocd-tag-updater/internal/patcher"
	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
)

const defaultRollbackTimeout = 10 * time.Minute

const defaultInterval = 2 * time.Minute

type TagUpdaterReconciler struct {
	client.Client
	Dynamic     dynamic.Interface
	Mapper      meta.RESTMapper
	Cache       cache.Cache
	ctrl        controller.Controller
	watchedGVKs sync.Map // map[schema.GroupVersionKind]struct{}

	// revAttempted flips true the first time a git source is asked to resolve a
	// tag to its immutable commit sha; lastRevSuccess records the unix-nano of
	// the most recent success. Together they drive RevResolutionHealthz — while
	// no git source has ever been reconciled the check is inert, so a nix-only
	// deployment never trips it.
	revAttempted   atomic.Bool
	lastRevSuccess atomic.Int64

	// progress tracks per-updater reconcile attempts/successes (the per-updater
	// analogue of revAttempted/lastRevSuccess) and drives the
	// tagupdater_reconcile_stale metric, the Stalled condition, and
	// ReconcileProgressHealthz.
	progress progressTracker

	// StaleMultiplier and StaleFloor configure the reconcile-progress staleness
	// window: an updater is stale when no reconcile has succeeded within
	// max(StaleMultiplier*interval, StaleFloor). Zero values use the defaults
	// (10x interval, 15m).
	StaleMultiplier int
	StaleFloor      time.Duration
}

// revStaleAfter is how long git tag->rev resolution may keep failing before
// RevResolutionHealthz reports unhealthy. Longer than a couple reconcile
// intervals so a transient ls-remote blip does not restart the pod, short
// enough that a sustained resolver outage (the failure mode that silently
// froze all deploys) migrates leadership off the broken replica.
const revStaleAfter = 5 * time.Minute

func (r *TagUpdaterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var tu v1alpha1.TagUpdater
	if err := r.Get(ctx, req.NamespacedName, &tu); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted updaters must stop counting toward staleness metrics and
			// the aggregate healthz.
			r.progress.forget(req.NamespacedName.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := defaultInterval
	if tu.Spec.Interval.Duration > 0 {
		interval = tu.Spec.Interval.Duration
	}

	progressKey := req.NamespacedName.String()
	r.progress.attempt(progressKey, interval, time.Now())

	// If rollback is enabled and we're mid-watch, check ArgoCD health before polling tags.
	if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Status.WatchingTag != "" {
		requeue, err := r.checkHealthAndMaybeRollback(ctx, &tu, interval)
		if err != nil {
			return ctrl.Result{RequeueAfter: interval}, err
		}
		if requeue > 0 {
			// A completed health poll is a successful reconcile — the pipeline
			// is alive even though no new tag work happened.
			r.markReconcileSucceeded(ctx, &tu, progressKey)
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		// health confirmed OK — fall through to normal tag poll
	}

	// Best-effort guard: an error condition on the target Application
	// (ComparisonError after a CRD/manifest skew, InvalidSpecError, ...) means
	// ArgoCD is not converging even though patching succeeds — deploys freeze
	// silently. Surface it loudly, but never fail the reconcile on it.
	if tu.Spec.ArgoCDApp != nil {
		r.checkTargetAppConditions(ctx, tu.Spec.ArgoCDApp)
	}

	var ociBasicAuth string
	if tu.Spec.Source.Type == v1alpha1.SourceTypeOCI && tu.Spec.Source.ImagePullSecretRef != nil {
		auth, aerr := r.resolveDockerAuth(ctx, tu.Namespace, tu.Spec.Source.ImagePullSecretRef.Name, tu.Spec.Source.Repo)
		if aerr != nil {
			log.Info("could not resolve imagePullSecret for OCI source; proceeding unauthenticated", "err", aerr)
		} else {
			ociBasicAuth = auth
		}
	}

	src, err := sourceFor(tu.Spec.Source, ociBasicAuth)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}

	tags, err := src.Tags(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: interval}, r.setFailed(ctx, &tu, err)
	}

	m, err := matcher.New(tu.Spec.Source.TagPattern)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}

	// Filter skipped tags so they are never re-applied.
	filteredTags := filterSkipped(tags, tu.Status.SkippedTags)

	latest, ok := m.Latest(filteredTags)
	if !ok {
		log.Info("no tags matched pattern", "pattern", tu.Spec.Source.TagPattern)
		// The pipeline completed — there was simply nothing to apply. Counts as
		// a successful reconcile for staleness purposes.
		r.markReconcileSucceeded(ctx, &tu, progressKey)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	data := latest.Captures
	data["tag"] = latest.Tag
	for k, v := range parseRepo(tu.Spec.Source.Repo) {
		data[k] = v
	}

	// If the source can resolve extra per-release fields (the nix source
	// surfacing the content-addressed store_path a tag can't encode), merge
	// them so a target can template `{{ .store_path }}`. Best-effort: a
	// resolver fault degrades to tag-only templating, never fails the update.
	if resolver, ok := src.(intsource.TagResolver); ok {
		if extra, rerr := resolver.Resolve(ctx); rerr != nil {
			log.Info("tag resolver failed; templating with tag captures only", "err", rerr)
		} else {
			for k, v := range extra {
				data[k] = v
			}
		}
	}

	// If the source can map a tag to its immutable commit sha (the git source),
	// expose the matched tag's sha as `{{ .rev }}` so a target can pin an
	// immutable flake ref `github:owner/repo/{{ .rev }}#attr`. Fail-closed: an
	// unresolvable rev aborts the update rather than patching an impure ref.
	if err := r.addRev(ctx, src, latest.Tag, data); err != nil {
		return ctrl.Result{RequeueAfter: interval}, r.setFailed(ctx, &tu, err)
	}

	p := patcher.Patcher{Client: r.Dynamic, Mapper: r.Mapper}

	var patchErrors []string
	anyChanged := false
	for _, target := range tu.Spec.Targets {
		selector := ""
		if target.Selector != nil {
			sel, err := metav1.LabelSelectorAsSelector(target.Selector)
			if err != nil {
				patchErrors = append(patchErrors, fmt.Sprintf("%s/%s selector: %v", target.Kind, target.Name, err))
				continue
			}
			selector = sel.String()
		}

		patches := make([]patcher.Patch, len(target.Patches))
		for i, patch := range target.Patches {
			patches[i] = patcher.Patch{Field: patch.Field, Template: patch.Template}
		}

		_, changed, err := p.ApplyAll(ctx, patcher.Target{
			APIVersion: target.APIVersion,
			Kind:       target.Kind,
			Name:       target.Name,
			Namespace:  target.Namespace,
			Selector:   selector,
		}, patches, data)
		if err != nil {
			patchErrors = append(patchErrors, err.Error())
			continue
		}
		if len(changed) > 0 {
			log.Info("patched", "kind", target.Kind, "names", changed, "tag", latest.Tag)
			anyChanged = true
		}
	}

	if len(patchErrors) > 0 {
		return ctrl.Result{RequeueAfter: interval}, r.setFailed(ctx, &tu, fmt.Errorf("%s", strings.Join(patchErrors, "; ")))
	}

	if tu.Spec.ManagingApp != nil {
		if err := r.ensureRespectIgnoreDifferences(ctx, tu.Spec.ManagingApp); err != nil {
			log.Error(err, "failed to ensure RespectIgnoreDifferences on managing app")
		}
	}

	// Only nudge ArgoCD when a target actually changed.
	if tu.Spec.ArgoCDApp != nil && anyChanged {
		if err := r.triggerArgoCDSync(ctx, tu.Spec.ArgoCDApp); err != nil {
			log.Error(err, "failed to trigger ArgoCD sync")
		}
	}

	if latest.Tag != tu.Status.LastTag {
		now := metav1.Now()
		// Preserve the previous tag before overwriting so rollback can revert to it.
		prevTag := tu.Status.LastTag
		tu.Status.PreviousTag = prevTag
		tu.Status.LastTag = latest.Tag
		tu.Status.LastUpdated = &now
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "Updated",
			Message: fmt.Sprintf("patched %d target(s) to tag %s", len(tu.Spec.Targets), latest.Tag),
		})

		// Start health watch if rollback is enabled and we have an ArgoCD app to watch.
		if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Spec.ArgoCDApp != nil {
			tu.Status.WatchingTag = latest.Tag
			tu.Status.WatchingSince = &now
		}

		if err := r.Status().Update(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}

		// Requeue quickly to start health polling.
		if tu.Status.WatchingTag != "" {
			r.ensureWatches(ctx, &tu)
			r.markReconcileSucceeded(ctx, &tu, progressKey)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	r.ensureWatches(ctx, &tu)
	r.markReconcileSucceeded(ctx, &tu, progressKey)

	return ctrl.Result{RequeueAfter: interval}, nil
}

// markReconcileSucceeded records that the resolve+patch pipeline for tu
// completed (whether or not a new tag existed) and clears the Stalled
// condition, status-updating only when the condition actually flips.
func (r *TagUpdaterReconciler) markReconcileSucceeded(ctx context.Context, tu *v1alpha1.TagUpdater, key string) {
	r.progress.success(key, time.Now())
	if meta.SetStatusCondition(&tu.Status.Conditions, r.stalledCondition(key)) {
		if err := r.Status().Update(ctx, tu); err != nil {
			log.FromContext(ctx).Error(err, "failed to update Stalled condition")
		}
	}
}

// stalledCondition renders the Stalled condition for key from the progress
// tracker. Stalled=True means reconciles have not succeeded within the
// staleness window — the per-updater "deploys are silently frozen" signal.
func (r *TagUpdaterReconciler) stalledCondition(key string) metav1.Condition {
	if r.progress.isStale(key, time.Now()) {
		return metav1.Condition{
			Type:    "Stalled",
			Status:  metav1.ConditionTrue,
			Reason:  "ReconcileStale",
			Message: "no successful reconcile within the staleness window; deploys for this updater may be frozen",
		}
	}
	return metav1.Condition{
		Type:    "Stalled",
		Status:  metav1.ConditionFalse,
		Reason:  "Progressing",
		Message: "reconciles are completing within the staleness window",
	}
}

// checkTargetAppConditions best-effort inspects the target ArgoCD Application
// for error conditions (ComparisonError, InvalidSpecError, ...). ArgoCD parks
// an App in ComparisonError on CRD/manifest schema skew and simply stops
// converging — patches still apply but nothing deploys. Emits an error-level
// log and increments tagupdater_target_app_error; never fails the reconcile.
func (r *TagUpdaterReconciler) checkTargetAppConditions(ctx context.Context, ref *v1alpha1.ArgoCDAppRef) {
	log := log.FromContext(ctx)
	ns := ref.Namespace
	if ns == "" {
		ns = "argocd"
	}
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	obj, err := r.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		if !strings.HasSuffix(conditionType, "Error") {
			continue
		}
		message, _ := condition["message"].(string)
		log.Error(fmt.Errorf("%s: %s", conditionType, message),
			"target ArgoCD Application has an error condition; syncs may be silently frozen",
			"app", ns+"/"+ref.Name, "conditionType", conditionType)
		targetAppErrorTotal.WithLabelValues(ref.Name, conditionType).Inc()
	}
}

// checkHealthAndMaybeRollback checks the ArgoCD app health while WatchingTag is set.
// Returns (requeue duration, error): requeue>0 means come back later; 0 means health watch complete.
func (r *TagUpdaterReconciler) checkHealthAndMaybeRollback(ctx context.Context, tu *v1alpha1.TagUpdater, interval time.Duration) (time.Duration, error) {
	log := log.FromContext(ctx)

	timeout := defaultRollbackTimeout
	if tu.Spec.Rollback.Timeout.Duration > 0 {
		timeout = tu.Spec.Rollback.Timeout.Duration
	}

	watching := tu.Status.WatchingTag
	since := tu.Status.WatchingSince

	elapsed := time.Duration(0)
	if since != nil {
		elapsed = time.Since(since.Time)
	}

	health, degradeReason, err := r.argoCDAppHealth(ctx, tu.Spec.ArgoCDApp)
	if err != nil {
		log.Error(err, "failed to read ArgoCD app health during rollback watch", "watchingTag", watching)
		if elapsed > timeout {
			log.Info("rollback timeout: health unreadable, reverting", "watchingTag", watching, "elapsed", elapsed)
			return 0, r.doRollback(ctx, tu, watching)
		}
		return 15 * time.Second, nil
	}

	switch health {
	case "Healthy":
		log.Info("deployment healthy, rollback watch complete", "tag", watching)
		tu.Status.WatchingTag = ""
		tu.Status.WatchingSince = nil
		// Clear skipped tags — a successful new deployment means previous skips are stale.
		tu.Status.SkippedTags = nil
		if err := r.Status().Update(ctx, tu); err != nil {
			return 0, err
		}
		return 0, nil

	case "Degraded":
		log.Info("deployment degraded, rolling back", "watchingTag", watching, "reason", degradeReason)
		return 0, r.doRollback(ctx, tu, watching)

	default:
		if elapsed > timeout {
			log.Info("rollback timeout waiting for healthy, reverting", "watchingTag", watching, "elapsed", elapsed, "health", health)
			return 0, r.doRollback(ctx, tu, watching)
		}
		log.Info("waiting for deployment health", "watchingTag", watching, "health", health, "elapsed", elapsed)
		return 15 * time.Second, nil
	}
}

// doRollback reverts all targets to previousTag and records badTag in skippedTags.
func (r *TagUpdaterReconciler) doRollback(ctx context.Context, tu *v1alpha1.TagUpdater, badTag string) error {
	log := log.FromContext(ctx)

	prevTag := tu.Status.PreviousTag
	if prevTag == "" || prevTag == badTag {
		log.Info("no previous tag to roll back to, skipping rollback", "badTag", badTag, "previousTag", prevTag)
		tu.Status.WatchingTag = ""
		tu.Status.WatchingSince = nil
		tu.Status.SkippedTags = appendUnique(tu.Status.SkippedTags, badTag)
		return r.Status().Update(ctx, tu)
	}

	log.Info("rolling back to previous tag", "badTag", badTag, "previousTag", prevTag)

	m, err := matcher.New(tu.Spec.Source.TagPattern)
	if err != nil {
		return err
	}

	match, ok := m.Latest([]string{prevTag})
	if !ok {
		log.Info("previous tag does not match pattern, cannot roll back", "previousTag", prevTag)
		tu.Status.WatchingTag = ""
		tu.Status.WatchingSince = nil
		tu.Status.SkippedTags = appendUnique(tu.Status.SkippedTags, badTag)
		return r.Status().Update(ctx, tu)
	}

	data := match.Captures
	data["tag"] = match.Tag
	for k, v := range parseRepo(tu.Spec.Source.Repo) {
		data[k] = v
	}

	// Re-resolve the previous tag's immutable sha so a `{{ .rev }}` template
	// reverts to a valid pinned ref instead of an empty one. Fail-closed: if the
	// rev cannot be resolved, abort the rollback rather than patch an impure ref.
	if src, serr := sourceFor(tu.Spec.Source, ""); serr == nil {
		if err := r.addRev(ctx, src, prevTag, data); err != nil {
			log.Error(err, "cannot resolve previous tag to sha; skipping rollback patch", "previousTag", prevTag)
			return err
		}
	}

	p := patcher.Patcher{Client: r.Dynamic, Mapper: r.Mapper}
	for _, target := range tu.Spec.Targets {
		patches := make([]patcher.Patch, len(target.Patches))
		for i, patch := range target.Patches {
			patches[i] = patcher.Patch{Field: patch.Field, Template: patch.Template}
		}
		_, changed, err := p.ApplyAll(ctx, patcher.Target{
			APIVersion: target.APIVersion,
			Kind:       target.Kind,
			Name:       target.Name,
			Namespace:  target.Namespace,
		}, patches, data)
		if err != nil {
			log.Error(err, "rollback patch failed", "kind", target.Kind)
			continue
		}
		if len(changed) > 0 {
			log.Info("rolled back", "kind", target.Kind, "names", changed, "tag", prevTag)
		}
	}

	if tu.Spec.ArgoCDApp != nil {
		if err := r.triggerArgoCDSync(ctx, tu.Spec.ArgoCDApp); err != nil {
			log.Error(err, "failed to trigger ArgoCD sync after rollback")
		}
	}

	now := metav1.Now()
	tu.Status.LastTag = prevTag
	tu.Status.LastUpdated = &now
	tu.Status.WatchingTag = ""
	tu.Status.WatchingSince = nil
	tu.Status.SkippedTags = appendUnique(tu.Status.SkippedTags, badTag)
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  "RolledBack",
		Message: fmt.Sprintf("tag %s failed, reverted to %s", badTag, prevTag),
	})
	return r.Status().Update(ctx, tu)
}

// argoCDAppHealth returns the ArgoCD Application health.status and a description
// of any degraded reason (for logging). Returns ("", "", err) on read failure.
func (r *TagUpdaterReconciler) argoCDAppHealth(ctx context.Context, ref *v1alpha1.ArgoCDAppRef) (string, string, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = "argocd"
	}
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	obj, err := r.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("get application %s/%s: %w", ns, ref.Name, err)
	}

	health, _, _ := unstructured.NestedString(obj.Object, "status", "health", "status")
	opMsg, _, _ := unstructured.NestedString(obj.Object, "status", "operationState", "message")

	// Treat ProgressDeadlineExceeded in the operation message as Degraded.
	if health != "Degraded" && strings.Contains(opMsg, "ProgressDeadlineExceeded") {
		health = "Degraded"
	}

	return health, opMsg, nil
}

// addRev enriches the template data with `rev` — the immutable full commit sha
// of tag — when the source implements TagRevResolver (the git source). It is
// fail-closed: for a git source, a resolver fault or an absent sha returns an
// error so the caller aborts the update rather than rendering an impure
// tag-only (or empty `?rev=`) flakeRef. A source that cannot resolve revs at
// all (the nix source) is not a TagRevResolver and returns nil unchanged.
func (r *TagUpdaterReconciler) addRev(ctx context.Context, src intsource.Source, tag string, data map[string]string) error {
	resolver, ok := src.(intsource.TagRevResolver)
	if !ok {
		return nil
	}
	r.revAttempted.Store(true)
	revs, err := resolver.TagRevs(ctx)
	if err != nil {
		return fmt.Errorf("resolve tag %q to commit sha: %w", tag, err)
	}
	sha := revs[tag]
	if sha == "" {
		return fmt.Errorf("tag %q resolved to no commit sha; refusing impure tag-only flakeRef", tag)
	}
	data["rev"] = sha
	r.lastRevSuccess.Store(time.Now().UnixNano())
	return nil
}

// RevResolutionHealthz reports unhealthy once a git source has been reconciled
// but tag->rev resolution has not succeeded within revStaleAfter. Wired as a
// liveness check so a sustained resolver outage — the class of failure that
// silently pinned every target at a stale tag — restarts the pod and, under
// leader election, hands off to a replica that can reach the git remote. Inert
// until the first git-source reconcile so nix-only deployments never trip it.
func (r *TagUpdaterReconciler) RevResolutionHealthz() healthz.Checker {
	return func(*http.Request) error {
		if !r.revAttempted.Load() {
			return nil
		}
		last := r.lastRevSuccess.Load()
		if last == 0 {
			return fmt.Errorf("git tag->rev resolution has never succeeded")
		}
		if age := time.Since(time.Unix(0, last)); age > revStaleAfter {
			return fmt.Errorf("git tag->rev resolution stale: last success %s ago (>%s)", age.Round(time.Second), revStaleAfter)
		}
		return nil
	}
}

// filterSkipped removes any tags that appear in the skipped list.
func filterSkipped(tags []string, skipped []string) []string {
	if len(skipped) == 0 {
		return tags
	}
	skip := make(map[string]struct{}, len(skipped))
	for _, s := range skipped {
		skip[s] = struct{}{}
	}
	out := tags[:0:0]
	for _, t := range tags {
		if _, bad := skip[t]; !bad {
			out = append(out, t)
		}
	}
	return out
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func (r *TagUpdaterReconciler) setFailed(ctx context.Context, tu *v1alpha1.TagUpdater, cause error) error {
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  "Error",
		Message: cause.Error(),
	})
	// Also surface reconcile-progress staleness on the CR itself: sustained
	// failures flip Stalled=True so `kubectl get tagupdater -o yaml` shows the
	// silent-freeze state, not just the latest error.
	meta.SetStatusCondition(&tu.Status.Conditions, r.stalledCondition(client.ObjectKeyFromObject(tu).String()))
	_ = r.Status().Update(ctx, tu)
	return cause
}

func (r *TagUpdaterReconciler) ensureRespectIgnoreDifferences(ctx context.Context, ref *v1alpha1.ArgoCDAppRef) error {
	ns := ref.Namespace
	if ns == "" {
		ns = "argocd"
	}
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	obj, err := r.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get application %s/%s: %w", ns, ref.Name, err)
	}
	opts, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "syncPolicy", "syncOptions")
	const opt = "RespectIgnoreDifferences=true"
	for _, o := range opts {
		if o == opt {
			return nil
		}
	}
	modified := obj.DeepCopy()
	opts = append(opts, opt)
	if err := unstructured.SetNestedStringSlice(modified.Object, opts, "spec", "syncPolicy", "syncOptions"); err != nil {
		return fmt.Errorf("set syncOptions: %w", err)
	}
	_, err = r.Dynamic.Resource(gvr).Namespace(ns).Update(ctx, modified, metav1.UpdateOptions{})
	return err
}

func (r *TagUpdaterReconciler) triggerArgoCDSync(ctx context.Context, ref *v1alpha1.ArgoCDAppRef) error {
	ns := ref.Namespace
	if ns == "" {
		ns = "argocd"
	}
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	obj, err := r.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get application %s/%s: %w", ns, ref.Name, err)
	}
	patch := obj.DeepCopy()
	if err := unstructured.SetNestedMap(patch.Object, map[string]any{
		"initiatedBy": map[string]any{"username": "argocd-tag-updater"},
		"sync":        map[string]any{},
	}, "operation"); err != nil {
		return fmt.Errorf("set operation: %w", err)
	}
	_, err = r.Dynamic.Resource(gvr).Namespace(ns).Update(ctx, patch, metav1.UpdateOptions{})
	return err
}

func parseRepo(raw string) map[string]string {
	out := map[string]string{"repoURL": raw}
	raw = strings.TrimSuffix(raw, ".git")

	if strings.HasPrefix(raw, "git@") {
		raw = strings.TrimPrefix(raw, "git@")
		host, path, ok := strings.Cut(raw, ":")
		if ok {
			out["host"] = host
			setOwnerRepo(out, path)
		}
		return out
	}

	if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		out["host"] = raw[:i] + ".com"
		setOwnerRepo(out, raw[i+1:])
		return out
	}

	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		raw = strings.SplitN(raw, "://", 2)[1]
		slash := strings.Index(raw, "/")
		if slash > 0 {
			out["host"] = raw[:slash]
			setOwnerRepo(out, raw[slash+1:])
		}
	}
	return out
}

func setOwnerRepo(out map[string]string, path string) {
	owner, repo, ok := strings.Cut(path, "/")
	if ok {
		out["owner"] = owner
		out["repo"] = repo
	}
}

func sourceFor(spec v1alpha1.SourceSpec, ociBasicAuth string) (intsource.Source, error) {
	switch spec.Type {
	case v1alpha1.SourceTypeGit:
		return &intsource.Git{
			Repo:       spec.Repo,
			SSHKeyFile: os.Getenv("GIT_SSH_KEY_FILE"),
			Token:      os.Getenv("GIT_TOKEN"),
		}, nil
	case v1alpha1.SourceTypeOCI:
		return &intsource.OCI{Repo: spec.Repo, BasicAuth: ociBasicAuth}, nil
	case v1alpha1.SourceTypeNix:
		return &intsource.Nix{
			Repo:  spec.Repo,
			Token: os.Getenv("NIX_CACHE_TOKEN"),
		}, nil
	default:
		return nil, fmt.Errorf("unknown source type %q", spec.Type)
	}
}

// resolveDockerAuth reads a kubernetes.io/dockerconfigjson Secret and returns
// the base64-encoded "user:pass" auth string for the registry host derived from
// repo. Returns an error if the secret is missing or contains no matching entry.
func (r *TagUpdaterReconciler) resolveDockerAuth(ctx context.Context, ns, secretName, repo string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", ns, secretName, err)
	}

	raw, ok := secret.Data[".dockerconfigjson"]
	if !ok {
		return "", fmt.Errorf("secret %s/%s missing .dockerconfigjson key", ns, secretName)
	}

	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse dockerconfigjson in %s/%s: %w", ns, secretName, err)
	}

	// Strip scheme prefix and extract the registry host from repo.
	ref := strings.TrimPrefix(strings.TrimPrefix(repo, "https://"), "http://")
	host, _, _ := strings.Cut(ref, "/")

	for registryHost, entry := range cfg.Auths {
		if registryHost != host {
			continue
		}
		if entry.Auth != "" {
			return entry.Auth, nil
		}
		if entry.Username != "" && entry.Password != "" {
			return base64.StdEncoding.EncodeToString([]byte(entry.Username + ":" + entry.Password)), nil
		}
	}
	return "", fmt.Errorf("no auth entry for host %q in secret %s/%s", host, ns, secretName)
}

func (r *TagUpdaterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Cache = mgr.GetCache()
	r.progress.multiplier = r.StaleMultiplier
	r.progress.floor = r.StaleFloor
	metrics.Registry.MustRegister(progressCollector{tracker: &r.progress})
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.TagUpdater{}).
		Build(r)
	if err != nil {
		return err
	}
	r.ctrl = c
	return nil
}

func parseGVK(apiVersion, kind string) schema.GroupVersionKind {
	gv, _ := schema.ParseGroupVersion(apiVersion)
	return gv.WithKind(kind)
}

func (r *TagUpdaterReconciler) ensureWatches(ctx context.Context, tu *v1alpha1.TagUpdater) {
	log := log.FromContext(ctx)
	for _, target := range tu.Spec.Targets {
		gvk := parseGVK(target.APIVersion, target.Kind)
		if _, loaded := r.watchedGVKs.LoadOrStore(gvk, struct{}{}); loaded {
			continue
		}
		log.Info("adding watch", "gvk", gvk)
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := r.ctrl.Watch(source.Kind(r.Cache, obj,
			handler.TypedEnqueueRequestsFromMapFunc(r.mapTargetToTagUpdater),
		)); err != nil {
			log.Error(err, "failed to add watch", "gvk", gvk)
			// remove from map so next reconcile retries
			r.watchedGVKs.Delete(gvk)
		}
	}
}

func (r *TagUpdaterReconciler) mapTargetToTagUpdater(ctx context.Context, obj *unstructured.Unstructured) []reconcile.Request {
	var tuList v1alpha1.TagUpdaterList
	if err := r.List(ctx, &tuList); err != nil {
		return nil
	}

	objGVK := obj.GroupVersionKind()
	seen := map[types.NamespacedName]struct{}{}
	var requests []reconcile.Request

	for _, tu := range tuList.Items {
		for _, target := range tu.Spec.Targets {
			gvk := parseGVK(target.APIVersion, target.Kind)
			if gvk != objGVK {
				continue
			}

			matched := false
			if target.Name != "" {
				ns := target.Namespace
				if ns == "" {
					ns = obj.GetNamespace()
				}
				matched = target.Name == obj.GetName() && ns == obj.GetNamespace()
			} else if target.Selector != nil {
				sel, err := metav1.LabelSelectorAsSelector(target.Selector)
				if err != nil {
					continue
				}
				matched = sel.Matches(labels.Set(obj.GetLabels()))
			} else {
				// no name or selector — match all objects of this GVK
				matched = true
			}

			if matched {
				key := types.NamespacedName{Namespace: tu.Namespace, Name: tu.Name}
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					requests = append(requests, reconcile.Request{NamespacedName: key})
				}
				break
			}
		}
	}
	return requests
}
