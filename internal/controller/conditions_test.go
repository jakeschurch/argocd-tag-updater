package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

// A reconcile that aborts before the write-back leg must not leave the previous
// pass's WriteBackReady verdict standing: that is how a CR ends up reporting a
// credential failure that was fixed months earlier.
func TestWriteBackNotAttemptedClearsStaleFailure(t *testing.T) {
	tu := &v1alpha1.TagUpdater{}
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{
		Type:    "WriteBackReady",
		Status:  metav1.ConditionFalse,
		Reason:  "WriteBackFailed",
		Message: "the key you are authenticating with has been marked as read only",
	})

	setWriteBackNotAttempted(tu, "no tag matched pattern")

	got := meta.FindStatusCondition(tu.Status.Conditions, "WriteBackReady")
	if got == nil {
		t.Fatal("WriteBackReady condition missing")
	}
	if got.Status != metav1.ConditionUnknown {
		t.Errorf("status = %v, want %v", got.Status, metav1.ConditionUnknown)
	}
	if got.Reason != "NotAttempted" {
		t.Errorf("reason = %q, want %q", got.Reason, "NotAttempted")
	}
}
