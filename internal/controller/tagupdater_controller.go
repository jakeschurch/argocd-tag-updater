package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	if latest.Tag == tu.Status.LastTag {
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	// Add the full tag itself as a capture so templates can reference {{ .tag }}
	data := latest.Captures
	data["tag"] = latest.Tag

	p := patcher.Patcher{Client: r.Dynamic}
	applied, err := p.Apply(ctx, patcher.Target{
		APIVersion: tu.Spec.Target.APIVersion,
		Kind:       tu.Spec.Target.Kind,
		Name:       tu.Spec.Target.Name,
		Namespace:  tu.Spec.Target.Namespace,
		Field:      tu.Spec.Target.Field,
	}, tu.Spec.Template, data)
	if err != nil {
		return ctrl.Result{RequeueAfter: interval}, r.setFailed(ctx, &tu, err)
	}

	log.Info("patched target", "tag", latest.Tag, "field", tu.Spec.Target.Field, "value", applied)

	if tu.Spec.ArgoCDApp != nil {
		if err := r.triggerArgoCDSync(ctx, tu.Spec.ArgoCDApp); err != nil {
			log.Error(err, "failed to trigger ArgoCD sync")
		}
	}

	now := metav1.Now()
	tu.Status.LastTag = latest.Tag
	tu.Status.LastApplied = applied
	tu.Status.LastUpdated = &now
	tu.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Updated",
		Message:            fmt.Sprintf("patched %s to %s", tu.Spec.Target.Field, latest.Tag),
		LastTransitionTime: now,
	}}
	if err := r.Status().Update(ctx, &tu); err != nil {
		return ctrl.Result{}, err
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

func sourceFor(spec v1alpha1.SourceSpec) (source.Source, error) {
	switch spec.Type {
	case v1alpha1.SourceTypeGit:
		return &source.Git{Repo: spec.Repo}, nil
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
