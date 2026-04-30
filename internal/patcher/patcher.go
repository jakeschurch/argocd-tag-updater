package patcher

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type Patcher struct {
	Client dynamic.Interface
	Mapper meta.RESTMapper
}

type Target struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	Selector   string
}

type Patch struct {
	Field    string
	Template string
}

// ApplyAll resolves the target CRs and applies all patches via GET-modify-PATCH.
// Gets the current object, applies field mutations via unstructured.SetNestedField
// (which handles both object creation and array-index navigation correctly), then
// diffs against the original to produce a minimal strategic merge patch.
func (p *Patcher) ApplyAll(ctx context.Context, target Target, patches []Patch, data map[string]string) ([]string, error) {
	gvr, err := p.gvrFor(target.APIVersion, target.Kind)
	if err != nil {
		return nil, err
	}

	names, err := p.resolveNames(ctx, gvr, target)
	if err != nil {
		return nil, err
	}

	rc := p.Client.Resource(gvr)
	var patched []string
	for _, name := range names {
		var obj *unstructured.Unstructured
		if target.Namespace != "" {
			obj, err = rc.Namespace(target.Namespace).Get(ctx, name, metav1.GetOptions{})
		} else {
			obj, err = rc.Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return patched, fmt.Errorf("get %s/%s: %w", target.Kind, name, err)
		}

		modified := obj.DeepCopy()
		changed := false
		for _, pp := range patches {
			value, err := renderTemplate(pp.Template, data)
			if err != nil {
				return patched, fmt.Errorf("render template for field %q: %w", pp.Field, err)
			}
			keys := strings.Split(pp.Field, ".")
			current, _, _ := unstructured.NestedString(modified.Object, keys...)
			if current == value {
				continue
			}
			if err := setNestedStringInUnstructured(modified.Object, keys, value); err != nil {
				return patched, fmt.Errorf("set field %q on %s/%s: %w", pp.Field, target.Kind, name, err)
			}
			changed = true
		}

		if !changed {
			patched = append(patched, name)
			continue
		}

		if target.Namespace != "" {
			_, err = rc.Namespace(target.Namespace).Update(ctx, modified, metav1.UpdateOptions{})
		} else {
			_, err = rc.Update(ctx, modified, metav1.UpdateOptions{})
		}
		if err != nil {
			return patched, fmt.Errorf("patch %s/%s: %w", target.Kind, name, err)
		}
		patched = append(patched, name)
	}
	return patched, nil
}

// setNestedStringInUnstructured sets a string value at the dot-split key path
// inside an unstructured object. Navigates arrays by numeric index. Creates
// intermediate maps as needed for object paths.
func setNestedStringInUnstructured(obj map[string]any, keys []string, value string) error {
	if len(keys) == 1 {
		obj[keys[0]] = value
		return nil
	}
	key := keys[0]
	rest := keys[1:]

	// Check if next key is a numeric array index.
	if idx, ok := parseIndex(rest[0]); ok && len(rest) >= 1 {
		// Navigate into array at key, then into element at idx.
		raw, exists := obj[key]
		if !exists {
			return fmt.Errorf("field %q not found (cannot index into non-existent array)", key)
		}
		arr, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("field %q is not an array", key)
		}
		if idx >= len(arr) {
			return fmt.Errorf("field %q index %d out of range (len %d)", key, idx, len(arr))
		}
		elem, ok := arr[idx].(map[string]any)
		if !ok {
			return fmt.Errorf("field %q[%d] is not an object", key, idx)
		}
		return setNestedStringInUnstructured(elem, rest[1:], value)
	}

	// Object navigation — create intermediate map if absent.
	child, ok := obj[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		obj[key] = child
	}
	return setNestedStringInUnstructured(child, rest, value)
}

func parseIndex(s string) (int, bool) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
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
	resource := strings.ToLower(kind) + "s"
	return schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource}, nil
}
