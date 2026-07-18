package patcher

import (
	"errors"
	"strings"
	"testing"
)

func TestGetNestedStringInUnstructured(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sources": []any{
				map[string]any{
					"helm": map[string]any{
						"valuesObject": map[string]any{
							"builders": []any{
								map[string]any{"image": "reg/builder:tag1"},
							},
							"image": map[string]any{"tag": "v2"},
						},
					},
				},
			},
		},
	}

	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"spec.sources.0.helm.valuesObject.builders.0.image", "reg/builder:tag1", true},
		{"spec.sources.0.helm.valuesObject.image.tag", "v2", true},
		{"spec.sources.1.helm.valuesObject.image.tag", "", false},
		{"spec.sources.0.helm.valuesObject.builders.0.missing", "", false},
		{"spec.missing", "", false},
	}
	for _, c := range cases {
		got, ok := getNestedStringInUnstructured(obj, splitPath(c.path))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestGetSetRoundTrip(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sources": []any{
				map[string]any{"helm": map[string]any{"valuesObject": map[string]any{
					"builders": []any{map[string]any{"image": "old"}},
				}}},
			},
		},
	}
	keys := splitPath("spec.sources.0.helm.valuesObject.builders.0.image")
	if err := setNestedStringInUnstructured(obj, keys, "new"); err != nil {
		t.Fatal(err)
	}
	got, ok := getNestedStringInUnstructured(obj, keys)
	if !ok || got != "new" {
		t.Errorf("round trip: got (%q,%v)", got, ok)
	}
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, c := range p {
		if c == '.' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	return append(out, cur)
}

func TestFieldToJSONPointer(t *testing.T) {
	cases := map[string]string{
		"spec.sources.0.helm.valuesObject.image.tag": "/spec/sources/0/helm/valuesObject/image/tag",
		"spec.source.helm.parameters.0.value":        "/spec/source/helm/parameters/0/value",
		"a/b":                                        "/a~1b",
		"a~b":                                        "/a~0b",
	}
	for field, want := range cases {
		if got := fieldToJSONPointer(splitField(field)); got != want {
			t.Errorf("fieldToJSONPointer(%q) = %q, want %q", field, got, want)
		}
	}
}

func splitField(f string) []string { return strings.Split(f, ".") }

// wfLike mimics an argo WorkflowTemplate: named templates, one carrying named
// input parameters — the exact shape whose positional indices rot on reorder.
func wfLike() map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"arguments": map[string]any{
				"parameters": []any{
					map[string]any{"name": "revision", "value": "abc"},
					map[string]any{"name": "build-image", "value": "old:1"},
				},
			},
			"templates": []any{
				map[string]any{"name": "cancel-stale"},
				map[string]any{"name": "build-and-push", "inputs": map[string]any{
					"parameters": []any{
						map[string]any{"name": "package"},
						map[string]any{"name": "build-image", "value": "old:1"},
					},
				}},
			},
		},
	}
}

func TestResolvePathNameSelector(t *testing.T) {
	obj := wfLike()
	cases := []struct {
		path string
		want string // dot-joined resolved (numeric) path
	}{
		{"spec.arguments.parameters.[name=build-image].value", "spec.arguments.parameters.1.value"},
		{"spec.templates.[name=build-and-push].inputs.parameters.[name=build-image].value", "spec.templates.1.inputs.parameters.1.value"},
		{"spec.arguments.parameters.[name=revision].value", "spec.arguments.parameters.0.value"},
		{"spec.templates.0.name", "spec.templates.0.name"}, // numeric passthrough
	}
	for _, c := range cases {
		resolved, err := resolvePath(obj, splitField(c.path))
		if err != nil {
			t.Errorf("%s: unexpected err %v", c.path, err)
			continue
		}
		if got := strings.Join(resolved, "."); got != c.want {
			t.Errorf("%s: resolved %q want %q", c.path, got, c.want)
		}
	}
}

func TestResolvePathSelectorMissIsPathError(t *testing.T) {
	obj := wfLike()
	cases := []string{
		"spec.arguments.parameters.[name=nonexistent].value",
		"spec.templates.[name=missing].inputs.parameters.[name=build-image].value",
		"spec.arguments.[name=x].value", // selector on a non-array (map)
	}
	for _, path := range cases {
		_, err := resolvePath(obj, splitField(path))
		if err == nil {
			t.Errorf("%s: want error, got nil", path)
			continue
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Errorf("%s: want *PathError, got %T", path, err)
		}
	}
}

// End-to-end through the setter: a name-keyed path resolves and writes to the
// correct element even though the template order would break a positional index.
func TestResolveThenSetNameKeyed(t *testing.T) {
	obj := wfLike()
	keys := splitField("spec.templates.[name=build-and-push].inputs.parameters.[name=build-image].value")
	resolved, err := resolvePath(obj, keys)
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if err := setNestedStringInUnstructured(obj, resolved, "new:2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, ok := getNestedStringInUnstructured(obj, resolved); !ok || got != "new:2" {
		t.Errorf("got (%q,%v) want (new:2,true)", got, ok)
	}
	// The JSON Pointer sent to the API server carries the resolved numeric index.
	if got := fieldToJSONPointer(resolved); got != "/spec/templates/1/inputs/parameters/1/value" {
		t.Errorf("pointer %q", got)
	}
}

// A raw positional index that overshoots the array still returns a PathError
// from the setter (permanent classification), as the ci-tools breakage did.
func TestSetOutOfRangeIsPathError(t *testing.T) {
	obj := wfLike()
	keys := splitField("spec.templates.1.inputs.parameters.5.value")
	err := setNestedStringInUnstructured(obj, keys, "x")
	if err == nil {
		t.Fatal("want out-of-range error")
	}
	var pe *PathError
	if !errors.As(err, &pe) {
		t.Errorf("want *PathError, got %T: %v", err, err)
	}
}

func TestRenderTemplateExposesRev(t *testing.T) {
	data := map[string]string{
		"owner": "acme",
		"repo":  "flake",
		"tag":   "platform.main.build-42.abc123",
		"rev":   "0123456789abcdef0123456789abcdef01234567",
	}
	tmpl := "github:{{ .owner }}/{{ .repo }}/{{ .tag }}?rev={{ .rev }}#packages.x86_64-linux.platform"
	got, err := renderTemplate(tmpl, data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	want := "github:acme/flake/platform.main.build-42.abc123?rev=0123456789abcdef0123456789abcdef01234567#packages.x86_64-linux.platform"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A template referencing .rev against data that never populated it renders the
// key as the empty string (text/template default). ApplyAll then skips the
// whole field rather than blanking it, so a rev-less reconcile is safe.
func TestRenderTemplateRevAbsentIsEmpty(t *testing.T) {
	data := map[string]string{"tag": "v1"}
	got, err := renderTemplate("{{ .rev }}", data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
