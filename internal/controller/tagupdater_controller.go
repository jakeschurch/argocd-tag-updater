package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/log"
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
}

func (r *TagUpdaterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var tu v1alpha1.TagUpdater
	if err := r.Get(ctx, req.NamespacedName, &tu); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := defaultInterval
	if tu.Spec.Interval.Duration > 0 {
		interval = tu.Spec.Interval.Duration
	}

	// If rollback is enabled and we're mid-watch, check ArgoCD health before polling tags.
	if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Status.WatchingTag != "" {
		requeue, err := r.checkHealthAndMaybeRollback(ctx, &tu, interval)
		if err != nil {
			return ctrl.Result{RequeueAfter: interval}, err
		}
		if requeue > 0 {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		// health confirmed OK — fall through to normal tag poll
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
		tu.Status.Conditions = []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Updated",
			Message:            fmt.Sprintf("patched %d target(s) to tag %s", len(tu.Spec.Targets), latest.Tag),
			LastTransitionTime: now,
		}}

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
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	r.ensureWatches(ctx, &tu)

	return ctrl.Result{RequeueAfter: interval}, nil
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
	tu.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "RolledBack",
		Message:            fmt.Sprintf("tag %s failed, reverted to %s", badTag, prevTag),
		LastTransitionTime: now,
	}}
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
	now := metav1.Now()
	tu.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Error",
		Message:            cause.Error(),
		LastTransitionTime: now,
	}}
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
