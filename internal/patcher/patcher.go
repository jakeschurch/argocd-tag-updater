package patcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

type Patcher struct {
	Client dynamic.Interface
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
	gvr, err := gvrFor(target.APIVersion, target.Kind)
	if err != nil {
		return nil, err
	}

	names, err := p.resolveNames(ctx, gvr, target)
	if err != nil {
		return nil, err
	}

	mergePatch, err := buildMergePatch(patches, data)
	if err != nil {
		return nil, err
	}

	rc := p.Client.Resource(gvr)
	var patched []string
	for _, name := range names {
		if target.Namespace != "" {
			_, err = rc.Namespace(target.Namespace).Patch(ctx, name, types.MergePatchType, mergePatch, metav1.PatchOptions{})
		} else {
			_, err = rc.Patch(ctx, name, types.MergePatchType, mergePatch, metav1.PatchOptions{})
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

// buildMergePatch renders all patch templates and merges them into a single
// JSON merge patch document. Multiple patches targeting the same subtree are
// merged — later entries in the slice overwrite earlier ones at the leaf level.
func buildMergePatch(patches []Patch, data map[string]string) ([]byte, error) {
	root := map[string]any{}
	for _, p := range patches {
		value, err := renderTemplate(p.Template, data)
		if err != nil {
			return nil, fmt.Errorf("render template for field %q: %w", p.Field, err)
		}
		setNested(root, strings.Split(p.Field, "."), value)
	}
	return json.Marshal(root)
}

// setNested sets a scalar value at keys[last] inside m, creating intermediate
// maps along keys[:last]. Existing intermediate maps are reused so sibling
// keys under the same parent are preserved.
func setNested(m map[string]any, keys []string, value any) {
	for _, k := range keys[:len(keys)-1] {
		if next, ok := m[k].(map[string]any); ok {
			m = next
		} else {
			next = map[string]any{}
			m[k] = next
			m = next
		}
	}
	m[keys[len(keys)-1]] = value
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

func gvrFor(apiVersion, kind string) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	resource := strings.ToLower(kind) + "s"
	return schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource}, nil
}
