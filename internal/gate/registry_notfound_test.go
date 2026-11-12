package gate

import (
	"testing"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// A synchronized cache that lacks the object is authoritative absence and must
// read as NotFound, exactly like the direct client, so a deleted Application
// is distinguishable from an unreadable cache.
func TestRegistryObjectAbsentFromSyncedCacheIsNotFound(t *testing.T) {
	obj := workload("Deployment", "", "v2", 2, int64(2))
	r, _, _ := registryHarness(t, obj)
	owner := types.NamespacedName{Namespace: "updaters", Name: "a"}
	if err := r.Subscribe(owner, 1, []v1.GateSpec{{Preset: "deployment", Name: "web", Namespace: "web"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return r.Synced(owner) })
	if _, err := r.Object(obj.GroupVersionKind(), types.NamespacedName{Namespace: "web", Name: "web"}); err != nil {
		t.Fatalf("present object: %v", err)
	}
	_, err := r.Object(obj.GroupVersionKind(), types.NamespacedName{Namespace: "web", Name: "gone"})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("absent object is not NotFound: %v", err)
	}
}
