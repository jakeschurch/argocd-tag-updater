package controller

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/matcher"
	"github.com/jakeschurch/argocd-tag-updater/internal/patcher"
	"github.com/jakeschurch/argocd-tag-updater/internal/source"
)

const defaultInterval = 2 * time.Minute

type TagUpdaterReconciler struct {
	client.Client
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
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

		patched, err := p.ApplyAll(ctx, patcher.Target{
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
		log.Info("patched", "kind", target.Kind, "names", patched, "tag", latest.Tag)
	}

	if len(patchErrors) > 0 {
		return ctrl.Result{RequeueAfter: interval}, r.setFailed(ctx, &tu, fmt.Errorf("%s", strings.Join(patchErrors, "; ")))
	}

	if tu.Spec.ManagingApp != nil {
		if err := r.ensureRespectIgnoreDifferences(ctx, tu.Spec.ManagingApp); err != nil {
			log.Error(err, "failed to ensure RespectIgnoreDifferences on managing app")
		}
	}

	if latest.Tag != tu.Status.LastTag {
		if tu.Spec.ArgoCDApp != nil {
			if err := r.triggerArgoCDSync(ctx, tu.Spec.ArgoCDApp); err != nil {
				log.Error(err, "failed to trigger ArgoCD sync")
			}
		}

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
	url := fmt.Sprintf("http://argocd-server.%s.svc.cluster.local/api/v1/applications/%s/sync", ns, ref.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("argocd sync returned %d", resp.StatusCode)
	}
	return nil
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

func sourceFor(spec v1alpha1.SourceSpec) (source.Source, error) {
	switch spec.Type {
	case v1alpha1.SourceTypeGit:
		return &source.Git{Repo: spec.Repo, SSHKeyFile: os.Getenv("GIT_SSH_KEY_FILE")}, nil
	case v1alpha1.SourceTypeOCI:
		return &source.OCI{Repo: spec.Repo}, nil
	default:
		return nil, fmt.Errorf("unknown source type %q", spec.Type)
	}
}

func (r *TagUpdaterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.TagUpdater{}).
		Complete(r)
}
