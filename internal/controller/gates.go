package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/gate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const gateFinalizer = "updater.argocd.io/gate-subscriptions"

func activeGate(tu *v1.TagUpdater) *v1.Gate {
	if tu.Status.WatchingTag != "" && tu.Status.WatchingGate != nil {
		return tu.Status.WatchingGate
	}
	return tu.Spec.Gate
}
func gated(tu *v1.TagUpdater) bool {
	return activeGate(tu) != nil || (tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Spec.ArgoCDApp != nil)
}

// Without an Application identity, serialize the entire namespace/kind. This
// conservatively covers overlapping selectors and named gates on the same kind.
func coordinationRefs(tu *v1.TagUpdater) []v1.ArgoCDAppRef {
	refs := healthApps(tu)
	if len(refs) != 0 {
		return refs
	}
	if explicit := activeGate(tu); explicit != nil {
		for _, spec := range explicit.Sources {
			kind := spec.APIVersion + "/" + spec.Kind
			if spec.Preset != "" {
				kind = gate.PresetGVK(spec.Preset)
			}
			refs = append(refs, v1.ArgoCDAppRef{Namespace: spec.Namespace, Name: "@" + kind})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Namespace+refs[i].Name < refs[j].Namespace+refs[j].Name })
	return refs
}

func (r *TagUpdaterReconciler) gateSubscriptions(tu *v1.TagUpdater) []v1.GateSpec {
	if !gated(tu) {
		return nil
	}
	var specs []v1.GateSpec
	if explicit := activeGate(tu); explicit != nil {
		specs = append(specs, explicit.Sources...)
	}
	refs := healthApps(tu)
	if ref := revisionObserver(tu); ref != nil {
		refs = append(refs, *ref)
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref.Namespace == "" {
			ref.Namespace = "argocd"
		}
		key := ref.Namespace + "/" + ref.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		specs = append(specs, v1.GateSpec{Preset: "argocd-application", Name: ref.Name, Namespace: ref.Namespace})
		if r.Gates == nil {
			continue
		}
		obj, err := r.Gates.Object(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name})
		if err != nil {
			continue
		}
		resources, _, _ := unstructured.NestedSlice(obj.Object, "status", "resources")
		for _, raw := range resources {
			item, ok := raw.(map[string]interface{})
			if !ok || item["group"] != "apps" || item["kind"] != "StatefulSet" {
				continue
			}
			name, _ := item["name"].(string)
			ns, _ := item["namespace"].(string)
			if name != "" && ns != "" {
				specs = append(specs, v1.GateSpec{Preset: "statefulset", Name: name, Namespace: ns})
			}
		}
	}
	return specs
}

func (r *TagUpdaterReconciler) baselineHealthy(ctx context.Context, tu *v1.TagUpdater) bool {
	for _, ref := range healthApps(tu) {
		state, err := r.argoCDAppState(ctx, &ref)
		if err != nil || state.Health != "Healthy" || state.Sync != "Synced" || state.syncInFlight() {
			return false
		}
	}
	if explicit := activeGate(tu); explicit != nil {
		if r.Gates == nil {
			return false
		}
		_, healthy, _, _ := r.Gates.Evaluate(explicit.Sources, "")
		return healthy
	}
	return tu.Spec.ArgoCDApp != nil
}

// A fresh comparison of an old child spec is still stale for this release.
// Before trusting legacy Application health, prove every changed Application
// scalar is present in the live desired spec (Before during rollback) and, for
// source and destination pins, in the spec ArgoCD last compared.
func (r *TagUpdaterReconciler) applicationPinsApplied(ctx context.Context, tu *v1.TagUpdater) bool {
	for _, field := range tu.Status.RollbackFields {
		if field.APIVersion != "argoproj.io/v1alpha1" || field.Kind != "Application" {
			continue
		}
		ns := field.Namespace
		if ns == "" {
			ns = "argocd"
		}
		obj, err := r.gateObject(ctx, schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}, "applications", types.NamespacedName{Namespace: ns, Name: field.Name})
		if err != nil {
			return false
		}
		expected := field.After
		if tu.Status.WatchingRollback {
			expected = field.Before
		}
		if current, ok := lookupField(obj.Object, field.Field); !ok || !valueMatches(current, expected) || !comparedPinApplied(obj.Object, field.Field, expected) {
			return false
		}
	}
	return true
}

// lookupField walks a dotted path through maps and list indexes. A missing map
// key yields nil; an invalid or out-of-range list index is not walkable.
func lookupField(obj map[string]interface{}, path string) (interface{}, bool) {
	var current interface{} = obj
	for _, part := range strings.Split(path, ".") {
		switch value := current.(type) {
		case map[string]interface{}:
			current = value[part]
		case []interface{}:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(value) {
				return nil, false
			}
			current = value[index]
		default:
			current = nil
		}
	}
	return current, true
}

func (r *TagUpdaterReconciler) gateObject(ctx context.Context, gvk schema.GroupVersionKind, resource string, key types.NamespacedName) (*unstructured.Unstructured, error) {
	if r.Gates != nil {
		return r.Gates.Object(gvk, key)
	}
	if r.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client is not configured")
	}
	// Unit tests and callers without a manager use direct reads. Production
	// always installs the registry and reads synchronized informer snapshots.
	return r.Dynamic.Resource(gvk.GroupVersion().WithResource(resource)).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
}

// Copy spec ownership into status before Git changes; deletion must unsubscribe
// even when no pending health watch remains.
func addGateFinalizer(tu *v1.TagUpdater) bool {
	for _, value := range tu.Finalizers {
		if value == gateFinalizer {
			return false
		}
	}
	tu.Finalizers = append(tu.Finalizers, gateFinalizer)
	return true
}
func removeGateFinalizer(tu *v1.TagUpdater) {
	var kept []string
	for _, value := range tu.Finalizers {
		if value != gateFinalizer {
			kept = append(kept, value)
		}
	}
	tu.Finalizers = kept
}
