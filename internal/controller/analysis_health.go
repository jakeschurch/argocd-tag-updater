package controller

import (
	"context"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// When analysis infrastructure errors explain aggregate Degraded, require an
// independently degraded resource before blaming another part of the unit.
func (r *TagUpdaterReconciler) failureOutsideAnalysis(ctx context.Context, tu *v1.TagUpdater) bool {
	for _, ref := range healthApps(tu) {
		app, err := r.gateObject(ctx, schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}, "applications", types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name})
		if err != nil {
			continue
		}
		resources, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
		for _, raw := range resources {
			item, ok := raw.(map[string]interface{})
			if !ok || (item["group"] == "argoproj.io" && (item["kind"] == "Rollout" || item["kind"] == "AnalysisRun")) {
				continue
			}
			status, _, _ := unstructured.NestedString(item, "health", "status")
			if status == "Degraded" {
				return true
			}
		}
	}
	return false
}
