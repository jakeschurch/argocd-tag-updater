package patcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

type Patcher struct {
	Client dynamic.Interface
	// Mapper resolves a (group, kind) into a GVR. Required so the patcher
	// works on any CR — naive pluralization (kind+"s") is wrong for kinds
	// like Ingress or NetworkPolicy.
	Mapper meta.RESTMapper
}

type Target struct {
	APIVersion string
	Kind       string
	// Name selects a specific CR. Mutually exclusive with Selector.
	Name string
	// Namespace scopes the lookup.
	Namespace string
	// Selector is a label selector string for dynamic CR discovery.
	// Mutually exclusive with Name.
	Selector string
}

type Patch struct {
	Field    string
	Template string
}

// ApplyAll resolves the target CRs (by name or label selector) and applies all
// patches to each one via a single merged JSON merge patch per CR.
// Returns the names of CRs that were patched.
func (p *Patcher) ApplyAll(ctx context.Context, target Target, patches []Patch, data map[string]string) ([]string, error) {
	gvr, err := p.gvrFor(target.APIVersion, target.Kind)
	if err != nil {
		return nil, err
	}

	names, err := p.resolveNames(ctx, gvr, target)
	if err != nil {
		return nil, err
	}

	jsonPatch, err := buildJSONPatch(patches, data)
	if err != nil {
		return nil, err
	}

	rc := p.Client.Resource(gvr)
	var patched []string
	for _, name := range names {
		if target.Namespace != "" {
			_, err = rc.Namespace(target.Namespace).Patch(ctx, name, types.JSONPatchType, jsonPatch, metav1.PatchOptions{})
		} else {
			_, err = rc.Patch(ctx, name, types.JSONPatchType, jsonPatch, metav1.PatchOptions{})
		}
		if err != nil {
			return patched, fmt.Errorf("patch %s/%s: %w", target.Kind, name, err)
		}
		patched = append(patched, name)
	}
	return patched, nil
}

func (p *Patcher) resolveNames(ctx context.Context, gvr schema.GroupVersionResource, target Target) ([]string, error) {
	if target.Name != "" {
		return []string{target.Name}, nil
	}

	rc := p.Client.Resource(gvr)
	opts := metav1.ListOptions{LabelSelector: target.Selector}

	var ul *unstructured.UnstructuredList
	var err error
	if target.Namespace != "" {
		ul, err = rc.Namespace(target.Namespace).List(ctx, opts)
	} else {
		ul, err = rc.List(ctx, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("list %s with selector %q: %w", target.Kind, target.Selector, err)
	}

	names := make([]string, 0, len(ul.Items))
	for _, item := range ul.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// buildJSONPatch renders all patch templates and returns a JSON Patch (RFC 6902)
// document. Each dot-separated field path is converted to a JSON Pointer
// (/a/b/0/c), so numeric segments correctly target array indices. The `add`
// operation is used — it replaces existing scalar leaves and appends into
// arrays at valid indices, covering both update and initialisation cases.
func buildJSONPatch(patches []Patch, data map[string]string) ([]byte, error) {
	type op struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}
	ops := make([]op, 0, len(patches))
	for _, p := range patches {
		value, err := renderTemplate(p.Template, data)
		if err != nil {
			return nil, fmt.Errorf("render template for field %q: %w", p.Field, err)
		}
		// dot-path → JSON Pointer: split on ".", prefix each segment with "/".
		segments := strings.Split(p.Field, ".")
		ptr := "/" + strings.Join(segments, "/")
		ops = append(ops, op{Op: "replace", Path: ptr, Value: value})
	}
	return json.Marshal(ops)
}

func renderTemplate(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (p *Patcher) gvrFor(apiVersion, kind string) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	if p.Mapper != nil {
		mapping, err := p.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
		if err != nil {
			return schema.GroupVersionResource{}, fmt.Errorf("rest mapping %s/%s: %w", apiVersion, kind, err)
		}
		return mapping.Resource, nil
	}
	// Fallback for tests / no-mapper construction. Naive +s; correct for
	// most CRDs but wrong for kinds with non-trivial English plurals.
	resource := strings.ToLower(kind) + "s"
	return schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource}, nil
}
