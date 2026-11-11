package controller

import (
	"context"
	"fmt"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/writeback"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (r *TagUpdaterReconciler) restoreRelease(ctx context.Context, tu *v1.TagUpdater) error {
	ambiguous, err := r.appAmbiguous(ctx, tu)
	if err != nil {
		return err
	}
	if ambiguous {
		return nil
	}
	acquired, err := r.acquireApp(ctx, tu)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("another updater owns this app lease; refusing rollback")
	}
	credentials, err := r.resolveWriteBackCredentials(ctx, tu)
	if err != nil {
		return err
	}
	spec := tu.Spec.WriteBack
	spec.Path = ""
	targets := make([]v1.TargetSpec, 0, len(tu.Status.RollbackFields))
	for _, field := range tu.Status.RollbackFields {
		targets = append(targets, v1.TargetSpec{Path: field.Path, APIVersion: field.APIVersion, Kind: field.Kind, Name: field.Name, Namespace: field.Namespace})
	}
	badTag := tu.Status.WatchingTag
	writer := writeback.Writer{Spec: spec, Credentials: credentials, Targets: targets, Restore: tu.Status.RollbackFields, Data: map[string]string{"tag": tu.Status.PreviousTag}}
	writer.BeforePush = func(result writeback.Result) error {
		prepareWrite(tu)
		now := metav1.NewTime(r.clock())
		tu.Status.LastTag = tu.Status.PreviousTag
		tu.Status.LastImageDigest, tu.Status.LastSignedDigest = tu.Status.PreviousImageDigest, tu.Status.PreviousSignedDigest
		tu.Status.LastWriteCommit, tu.Status.LastUpdated = result.CommitSHA, &now
		tu.Status.WatchingCommit, tu.Status.WatchingArmedAt, tu.Status.WatchingSince = result.CommitSHA, &now, nil
		tu.Status.WatchingRollback = true
		tu.Status.WatchingDeployed = false
		// Blame is unambiguous and the rollback intent is durable before push.
		tu.Status.SkippedTags = appendUnique(tu.Status.SkippedTags, badTag)
		return r.Status().Update(ctx, tu)
	}
	result, err := writer.Apply(ctx)
	if tu.Status.PendingWriteStatus != "" {
		err = r.settleWrite(ctx, tu, writer, err)
		if err == nil && result.CommitSHA == "" {
			result.CommitSHA = tu.Status.WatchingCommit
		}
	}
	if err != nil {
		return err
	}
	message := fmt.Sprintf("release %s failed; restored every changed field from the Healthy baseline in commit %s", badTag, result.CommitSHA)
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RolledBack", Message: message})
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "RollbackWritten", Message: message})
	rollbackCount.WithLabelValues(tu.Namespace, tu.Name).Inc()
	if r.Recorder != nil {
		r.Recorder.Event(tu, corev1.EventTypeWarning, "RolledBack", message)
	}
	log.FromContext(ctx).Info("rolled back release", "tag", badTag, "commit", result.CommitSHA)
	r.refreshAfterWriteBack(ctx, tu)
	return r.Status().Update(ctx, tu)
}
