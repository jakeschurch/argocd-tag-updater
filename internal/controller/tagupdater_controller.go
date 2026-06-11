package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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

	src, err := sourceFor(tu.Spec.Source)
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

	latest, ok := m.Latest(tags)
	if !ok {
		log.Info("no tags matched pattern", "pattern", tu.Spec.Source.TagPattern)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	data := latest.Captures
	data["tag"] = latest.Tag
	for k, v := range parseRepo(tu.Spec.Source.Repo) {
		data[k] = v
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

	// Only nudge ArgoCD when a target actually changed: the sync trigger
	// writes .operation on the Application, and doing it every reconcile
	// (~15s) drove metadata.generation into the 100k+ range while masking
	// real churn (foundrybox-cmk).
	if tu.Spec.ArgoCDApp != nil && anyChanged {
		if err := r.triggerArgoCDSync(ctx, tu.Spec.ArgoCDApp); err != nil {
			log.Error(err, "failed to trigger ArgoCD sync")
		}
	}

	if latest.Tag != tu.Status.LastTag {
		now := metav1.Now()
		tu.Status.LastTag = latest.Tag
		tu.Status.LastUpdated = &now
		tu.Status.Conditions = []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Updated",
			Message:            fmt.Sprintf("patched %d target(s) to tag %s", len(tu.Spec.Targets), latest.Tag),
			LastTransitionTime: now,
		}}
		if err := r.Status().Update(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.ensureWatches(ctx, &tu)

	return ctrl.Result{RequeueAfter: interval}, nil
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

func sourceFor(spec v1alpha1.SourceSpec) (intsource.Source, error) {
	switch spec.Type {
	case v1alpha1.SourceTypeGit:
		return &intsource.Git{
			Repo:       spec.Repo,
			SSHKeyFile: os.Getenv("GIT_SSH_KEY_FILE"),
			Token:      os.Getenv("GIT_TOKEN"),
		}, nil
	case v1alpha1.SourceTypeOCI:
		return &intsource.OCI{Repo: spec.Repo}, nil
	default:
		return nil, fmt.Errorf("unknown source type %q", spec.Type)
	}
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
