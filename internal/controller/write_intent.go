package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/writeback"
)

func specHash(tu *v1.TagUpdater) string {
	data, _ := json.Marshal(tu.Spec)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func prepareWrite(tu *v1.TagUpdater) {
	// 2026-11-11: retries replace the prepared intent, never its checkpoint.
	if tu.Status.PendingWriteStatus == "" {
		previous, _ := json.Marshal(tu.Status)
		tu.Status.PendingWriteStatus = string(previous)
	}
}

// settleWrite handles both a rejected push and a lost successful push response.
// Read failures retain the checkpoint for retry; absence proven by reading the
// remote branch restores the exact pre-arm status, including a pending rollback.
func (r *TagUpdaterReconciler) settleWrite(ctx context.Context, tu *v1.TagUpdater, writer writeback.Writer, pushErr error) error {
	if tu.Status.PendingWriteStatus == "" {
		return pushErr
	}
	reachable, err := writer.ContainsCommit(ctx, tu.Status.WatchingCommit)
	if err != nil {
		return fmt.Errorf("confirm prepared write (push: %v): %w", pushErr, err)
	}
	if reachable {
		tu.Status.PendingWriteStatus = ""
		return r.Status().Update(ctx, tu)
	}
	var previous v1.TagUpdaterStatus
	if err := json.Unmarshal([]byte(tu.Status.PendingWriteStatus), &previous); err != nil {
		return fmt.Errorf("decode write checkpoint: %w", err)
	}
	tu.Status = previous
	if err := r.Status().Update(ctx, tu); err != nil {
		return err
	}
	// 2026-11-11: a rejected rollback restores an active forward watch.
	// Keep its ownership until that release is resolved or explicitly drained.
	if tu.Status.WatchingTag == "" {
		if err := r.releaseApp(ctx, tu); err != nil {
			return err
		}
	}
	if pushErr != nil {
		return pushErr
	}
	return fmt.Errorf("prepared commit is absent from the remote branch; restored pre-push status")
}
