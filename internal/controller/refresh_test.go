package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

// patchRecorder is the Application API, recording patches.
type patchRecorder struct {
	dynamic.Interface
	dynamic.NamespaceableResourceInterface
	resource  schema.GroupVersionResource
	namespace string
	name      string
	kind      types.PatchType
	body      string
	err       error
}

func (p *patchRecorder) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	p.resource = resource
	return p
}

func (p *patchRecorder) Namespace(namespace string) dynamic.ResourceInterface {
	p.namespace = namespace
	return p
}

func (p *patchRecorder) Patch(_ context.Context, name string, kind types.PatchType, body []byte, _ metav1.PatchOptions, _ ...string) (*unstructured.Unstructured, error) {
	p.name, p.kind, p.body = name, kind, string(body)
	return nil, p.err
}

func TestWriteBackAsksTheRevisionObserverToRefresh(t *testing.T) {
	workload := &v1alpha1.ArgoCDAppRef{Name: "workload"}
	parent := &v1alpha1.ArgoCDAppRef{Name: "app-of-apps", Namespace: "elsewhere"}

	for _, scenario := range []struct {
		name            string
		app, managing   *v1alpha1.ArgoCDAppRef
		wantName, wantN string
	}{
		{"the workload", workload, nil, "workload", "argocd"},
		{"the app of apps", workload, parent, "app-of-apps", "elsewhere"},
		{"neither", nil, nil, "", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			recorder := &patchRecorder{}
			reconciler := &TagUpdaterReconciler{Dynamic: recorder}
			tu := &v1alpha1.TagUpdater{Spec: v1alpha1.TagUpdaterSpec{ArgoCDApp: scenario.app, ManagingApp: scenario.managing}}

			reconciler.refreshAfterWriteBack(context.Background(), tu)

			if recorder.name != scenario.wantName || recorder.namespace != scenario.wantN {
				t.Fatalf("patched %s/%s", recorder.namespace, recorder.name)
			}

			if scenario.wantName == "" {
				return
			}

			if recorder.resource.Resource != "applications" || recorder.kind != types.MergePatchType ||
				recorder.body != `{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"normal"}}}` {
				t.Fatalf("patch %s %s %s", recorder.resource, recorder.kind, recorder.body)
			}
		})
	}
}

func TestAFailedRefreshIsNotAnError(t *testing.T) {
	recorder := &patchRecorder{err: errors.New("forbidden")}
	reconciler := &TagUpdaterReconciler{Dynamic: recorder}

	// There is nothing to return it through: this only has to not panic or block.
	reconciler.refreshAfterWriteBack(context.Background(), &v1alpha1.TagUpdater{
		Spec: v1alpha1.TagUpdaterSpec{ArgoCDApp: &v1alpha1.ArgoCDAppRef{Name: "workload"}},
	})

	if recorder.name != "workload" {
		t.Fatalf("patched %q", recorder.name)
	}
}

func TestNoDynamicClientNoRefresh(t *testing.T) {
	reconciler := &TagUpdaterReconciler{}
	reconciler.refreshAfterWriteBack(context.Background(), &v1alpha1.TagUpdater{
		Spec: v1alpha1.TagUpdaterSpec{ArgoCDApp: &v1alpha1.ArgoCDAppRef{Name: "workload"}},
	})
}
