package patcher

import (
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
		"a/b":                                         "/a~1b",
		"a~b":                                         "/a~0b",
	}
	for field, want := range cases {
		if got := fieldToJSONPointer(splitField(field)); got != want {
			t.Errorf("fieldToJSONPointer(%q) = %q, want %q", field, got, want)
		}
	}
}

func splitField(f string) []string { return strings.Split(f, ".") }
