package controller

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/jakeschurch/argocd-tag-updater/internal/patcher"
)

func TestIsPermanentPatchError(t *testing.T) {
	gk := schema.GroupKind{Group: "argoproj.io", Kind: "Application"}
	invalid := apierrors.NewInvalid(gk, "foundry", field.ErrorList{
		field.Invalid(field.NewPath("spec"), nil, "bad"),
	})

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"PathError direct", &patcher.PathError{}, true},
		{"PathError wrapped", fmt.Errorf("set field: %w", &patcher.PathError{}), true},
		{"api Invalid wrapped", fmt.Errorf("patch Application/foundry: %w", invalid), true},
		{"api BadRequest", apierrors.NewBadRequest("nope"), true},
		{"api Conflict transient", apierrors.NewConflict(schema.GroupResource{}, "x", errors.New("c")), false},
		{"network transient", errors.New("dial tcp: no route to host"), false},
	}
	for _, c := range cases {
		if got := isPermanentPatchError(c.err); got != c.want {
			t.Errorf("%s: isPermanentPatchError=%v want %v", c.name, got, c.want)
		}
	}
}

// A mixed batch (one permanent + one transient) must NOT be treated as
// all-permanent — the reconciler keeps the error backoff so the transient
// target still retries promptly.
func TestMixedErrorsNotAllPermanent(t *testing.T) {
	errsList := []error{
		&patcher.PathError{},
		apierrors.NewConflict(schema.GroupResource{}, "x", errors.New("c")),
	}
	all := true
	for _, e := range errsList {
		if !isPermanentPatchError(e) {
			all = false
		}
	}
	if all {
		t.Error("mixed batch classified as all-permanent")
	}
}
