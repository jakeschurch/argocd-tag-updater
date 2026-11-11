package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"strings"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const holdAnnotation = "updater.argocd.io/hold"

var rollbackCount = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tagupdater_rollbacks_total", Help: "Whole-release rollback writes."}, []string{"namespace", "name"})
var suppressedCount = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tagupdater_rollback_suppressed_total", Help: "Rollback suppression transitions."}, []string{"namespace", "name", "reason"})
var ambiguousGate = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "tagupdater_rollback_ambiguous", Help: "Multiple pending changes make rollback blame ambiguous."}, []string{"namespace", "name"})

func appKey(tu *v1.TagUpdater) string {
	if tu.Spec.ArgoCDApp == nil {
		refs := coordinationRefs(tu)
		if len(refs) == 0 {
			return ""
		}
		return refs[0].Namespace + "/" + refs[0].Name
	}
	ns := tu.Spec.ArgoCDApp.Namespace
	if ns == "" {
		ns = "argocd"
	}
	return ns + "/" + tu.Spec.ArgoCDApp.Name
}
func leaseKey(tu *v1.TagUpdater) types.NamespacedName {
	parts := strings.SplitN(appKey(tu), "/", 2)
	return types.NamespacedName{Namespace: parts[0], Name: fmt.Sprintf("tagupdater-app-%x", sha256.Sum256([]byte(appKey(tu))))[:48]}
}
func holder(tu *v1.TagUpdater) string { return tu.Namespace + "/" + tu.Name + "/" + string(tu.UID) }
func (r *TagUpdaterReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// The Lease intentionally never expires: a slow rollout or controller outage
// must not admit a second writer. CAS plus uncached reads fences replicas.
func (r *TagUpdaterReconciler) acquireOneApp(ctx context.Context, tu *v1.TagUpdater) (bool, error) {
	if appKey(tu) == "" {
		return true, nil
	}
	key := leaseKey(tu)
	lease := &coordinationv1.Lease{}
	err := r.reader().Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		identity := holder(tu)
		lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{"updater.argocd.io/app-lease": "true"}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &identity}}
		err = r.Create(ctx, lease)
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return err == nil, err
	}
	if err != nil {
		return false, err
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder(tu) {
		return true, nil
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		// Deletion alone cannot prove whether an unrecorded Git push happened.
		// Keep an orphaned holder fenced for explicit operator recovery.
		return false, nil
	}
	identity := holder(tu)
	lease.Spec.HolderIdentity = &identity
	err = r.Update(ctx, lease)
	if apierrors.IsConflict(err) {
		return false, nil
	}
	return err == nil, err
}
func (r *TagUpdaterReconciler) releaseOneApp(ctx context.Context, tu *v1.TagUpdater) error {
	if appKey(tu) == "" {
		return nil
	}
	lease := &coordinationv1.Lease{}
	if err := r.reader().Get(ctx, leaseKey(tu), lease); err != nil {
		return client.IgnoreNotFound(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder(tu) {
		return nil
	}
	empty := ""
	lease.Spec.HolderIdentity = &empty
	return r.Update(ctx, lease)
}

func (r *TagUpdaterReconciler) appAmbiguous(ctx context.Context, tu *v1.TagUpdater) (bool, error) {
	return r.appAmbiguousForIntent(ctx, tu, tu.Status.WatchingTag != "")
}

func (r *TagUpdaterReconciler) appAmbiguousForIntent(ctx context.Context, tu *v1.TagUpdater, pendingIntent bool) (bool, error) {
	if appKey(tu) == "" {
		return false, nil
	}
	var all v1.TagUpdaterList
	if err := r.reader().List(ctx, &all); err != nil {
		return false, err
	}
	pending := 0
	selfPending := false
	for _, other := range all.Items {
		if appsOverlap(&other, tu) && other.Status.WatchingTag != "" {
			pending++
			if other.Namespace == tu.Namespace && other.Name == tu.Name {
				selfPending = true
			}
		}
	}
	if pendingIntent && !selfPending {
		pending++
	}
	ambiguous := pending > 1
	value := 0.0
	if ambiguous {
		value = 1
	}
	ambiguousGate.WithLabelValues(tu.Namespace, tu.Name).Set(value)
	condition := metav1.Condition{Type: "RollbackAmbiguous", Status: metav1.ConditionFalse, Reason: "SingleInFlight", Message: "at most one release is pending for this app"}
	if ambiguous {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "MultiplePendingChanges"
		condition.Message = "multiple releases are pending for this app; no writes, rollback or skipped tags until an operator resolves ownership"
	}
	if meta.SetStatusCondition(&tu.Status.Conditions, condition) {
		if ambiguous {
			r.suppressed(ctx, tu, "AmbiguousBlame", condition.Message)
		}
		if err := r.Status().Update(ctx, tu); err != nil {
			return false, err
		}
	}
	return ambiguous, nil
}
func (r *TagUpdaterReconciler) suppressed(ctx context.Context, tu *v1.TagUpdater, reason, message string) {
	suppressedCount.WithLabelValues(tu.Namespace, tu.Name, reason).Inc()
	if r.Recorder != nil {
		r.Recorder.Event(tu, corev1.EventTypeWarning, reason, message)
	}
	log.FromContext(ctx).Info("automatic rollback suppressed", "reason", reason, "message", message)
}

func (r *TagUpdaterReconciler) enqueuePeers(ctx context.Context, obj client.Object) []ctrl.Request {
	var key string
	switch item := obj.(type) {
	case *v1.TagUpdater:
		key = appKey(item)
	case *unstructured.Unstructured:
		if item.GetKind() == "Application" {
			key = item.GetNamespace() + "/" + item.GetName()
		}
	}
	var all v1.TagUpdaterList
	if err := r.List(ctx, &all); err != nil {
		log.FromContext(ctx).Error(err, "list app waiters")
		return nil
	}
	var requests []ctrl.Request
	for _, tu := range all.Items {
		match := key != "" && hasApp(&tu, key)
		if item, ok := obj.(*v1.TagUpdater); ok && (appsOverlap(item, &tu) || item.Namespace == tu.Namespace && item.Name == tu.Name) {
			match = true
		}
		if lease, ok := obj.(*coordinationv1.Lease); ok {
			condition := meta.FindStatusCondition(tu.Status.Conditions, "Ready")
			if condition != nil && condition.Reason == "AppQueued" {
				for _, ref := range coordinationRefs(&tu) {
					copy := tu
					copy.Spec.ArgoCDApp = &ref
					if leaseKey(&copy) == client.ObjectKeyFromObject(lease) {
						match = true
					}
				}
			}
		}
		if item, ok := obj.(*unstructured.Unstructured); ok && revisionObserver(&tu) != nil {
			ref := revisionObserver(&tu)
			ns := ref.Namespace
			if ns == "" {
				ns = "argocd"
			}
			if ns+"/"+ref.Name == item.GetNamespace()+"/"+item.GetName() {
				match = true
			}
		}
		if item, ok := obj.(*unstructured.Unstructured); ok && item.GetKind() == "StatefulSet" && tu.Status.WatchingTag != "" {
			match = true
		}
		if match {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&tu)})
		}
	}
	return requests
}

var updaterEvents = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	oldTU, ok := e.ObjectOld.(*v1.TagUpdater)
	if !ok {
		return true
	}
	newTU := e.ObjectNew.(*v1.TagUpdater)
	return oldTU.Generation != newTU.Generation || !oldTU.DeletionTimestamp.Equal(newTU.DeletionTimestamp) || !reflect.DeepEqual(oldTU.Annotations, newTU.Annotations) || oldTU.Status.WatchingTag != newTU.Status.WatchingTag || oldTU.Status.WatchingCommit != newTU.Status.WatchingCommit || oldTU.Status.WatchingRollback != newTU.Status.WatchingRollback
}}

func (r *TagUpdaterReconciler) controllerBuilder(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("tagupdater").
		Watches(&v1.TagUpdater{}, handler.EnqueueRequestsFromMapFunc(r.enqueuePeers), builder.WithPredicates(updaterEvents)).
		WatchesRawSource(r.Gates).
		Watches(&coordinationv1.Lease{}, handler.EnqueueRequestsFromMapFunc(r.enqueuePeers), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).Complete(r)
}

// Every named Application target is part of the release, including multi-file
// units (for example farm workers plus CSI submitters). Lock in sorted order.
func healthApps(tu *v1.TagUpdater) []v1.ArgoCDAppRef {
	refs := map[string]v1.ArgoCDAppRef{}
	add := func(ref v1.ArgoCDAppRef) {
		if ref.Namespace == "" {
			ref.Namespace = "argocd"
		}
		refs[ref.Namespace+"/"+ref.Name] = ref
	}
	if tu.Spec.ArgoCDApp != nil {
		add(*tu.Spec.ArgoCDApp)
	}
	if explicit := activeGate(tu); explicit != nil {
		for _, spec := range explicit.Sources {
			if spec.Preset == "argocd-application" && spec.Name != "" {
				add(v1.ArgoCDAppRef{Name: spec.Name, Namespace: spec.Namespace})
			}
		}
	}
	for _, target := range tu.Spec.Targets {
		if target.APIVersion == "argoproj.io/v1alpha1" && target.Kind == "Application" && target.Name != "" {
			add(v1.ArgoCDAppRef{Name: target.Name, Namespace: target.Namespace})
		}
	}
	for _, field := range tu.Status.RollbackFields {
		if field.APIVersion == "argoproj.io/v1alpha1" && field.Kind == "Application" {
			add(v1.ArgoCDAppRef{Name: field.Name, Namespace: field.Namespace})
		}
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]v1.ArgoCDAppRef, 0, len(keys))
	for _, key := range keys {
		out = append(out, refs[key])
	}
	return out
}
func hasApp(tu *v1.TagUpdater, key string) bool {
	for _, ref := range coordinationRefs(tu) {
		if ref.Namespace+"/"+ref.Name == key {
			return true
		}
	}
	return false
}
func appsOverlap(a, b *v1.TagUpdater) bool {
	for _, ref := range coordinationRefs(a) {
		if hasApp(b, ref.Namespace+"/"+ref.Name) {
			return true
		}
	}
	return false
}
func (r *TagUpdaterReconciler) acquireApp(ctx context.Context, tu *v1.TagUpdater) (bool, error) {
	if !gated(tu) {
		return true, nil
	}
	var acquired []*v1.TagUpdater
	for _, ref := range coordinationRefs(tu) {
		copy := tu.DeepCopy()
		copy.Spec.ArgoCDApp = &ref
		ok, err := r.acquireOneApp(ctx, copy)
		if err != nil || !ok {
			if tu.Status.WatchingTag == "" {
				for _, held := range acquired {
					if releaseErr := r.releaseOneApp(ctx, held); releaseErr != nil {
						return false, releaseErr
					}
				}
			}
			return false, err
		}
		acquired = append(acquired, copy)
	}
	return true, nil
}
func (r *TagUpdaterReconciler) releaseApp(ctx context.Context, tu *v1.TagUpdater) error {
	if !gated(tu) {
		return nil
	}
	for _, ref := range coordinationRefs(tu) {
		copy := tu.DeepCopy()
		copy.Spec.ArgoCDApp = &ref
		if err := r.releaseOneApp(ctx, copy); err != nil {
			return err
		}
	}
	return nil
}
