package patcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	Name       string
	Namespace  string
	Field      string // dot-notation: "spec.nixExpr"
}

// Apply renders tmpl with data, then JSON-patches the field on the target CR.
// Returns the rendered value.
func (p *Patcher) Apply(ctx context.Context, target Target, tmpl string, data map[string]string) (string, error) {
	t, err := template.New("value").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	value := buf.String()

	// Build a JSON merge patch from the dot-notation field path. Merge patch
	// deep-merges at every level, so intermediate keys don't need to exist and
	// sibling fields are preserved. This handles both simple scalar fields like
	// spec.flakeRef and deeply nested paths like
	// spec.source.helm.valuesObject.image.tag without any special-casing.
	patch, err := buildMergePatch(target.Field, value)
	if err != nil {
		return "", err
	}

	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return "", fmt.Errorf("parse apiVersion %q: %w", target.APIVersion, err)
	}
	// Naive plural: lowercase kind + "s". Sufficient for our known targets;
	// extend with a REST mapper if needed for arbitrary CRDs.
	resource := strings.ToLower(target.Kind) + "s"
	gvr := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource}

	rc := p.Client.Resource(gvr)
	if target.Namespace != "" {
		_, err = rc.Namespace(target.Namespace).Patch(ctx, target.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	} else {
		_, err = rc.Patch(ctx, target.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("patch %s/%s .%s: %w", target.Kind, target.Name, target.Field, err)
	}
	return value, nil
}

// buildMergePatch converts a dot-notation field path and value into a JSON
// merge patch document. "spec.source.helm.valuesObject.image.tag" with value
// "abc" produces {"spec":{"source":{"helm":{"valuesObject":{"image":{"tag":"abc"}}}}}}.
func buildMergePatch(field string, value string) ([]byte, error) {
	parts := strings.Split(field, ".")
	var obj any = value
	for i := len(parts) - 1; i >= 0; i-- {
		obj = map[string]any{parts[i]: obj}
	}
	return json.Marshal(obj)
}
