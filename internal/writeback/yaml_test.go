package writeback

import (
	"bytes"
	"strings"
	"testing"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func applicationTarget(value string) []v1alpha1.TargetSpec {
	return []v1alpha1.TargetSpec{{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "foundry-operator", Namespace: "argocd",
		Patches: []v1alpha1.PatchSpec{
			{Field: "spec.sources.0.targetRevision", Template: "{{ .tag }}"},
			{Field: "spec.sources.0.helm.valuesObject.image.tag", Template: value},
		},
	}}
}

func TestUpdateManifestListIndicesAndComments(t *testing.T) {
	input := []byte(`# file header
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: foundry-operator # identity comment
  namespace: argocd
spec:
  # sources comment
  sources:
    - repoURL: https://example.invalid/chart.git
      targetRevision: old # revision comment
      helm:
        valuesObject:
          image:
            tag: old-image # image comment
  destination:
    server: https://kubernetes.default.svc # untouched
`)
	out, changed, err := UpdateManifest(input, applicationTarget("image-{{ .rev }}"), map[string]string{"tag": "v2", "rev": "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected manifest change")
	}
	wantExact := bytes.ReplaceAll(input, []byte("targetRevision: old"), []byte("targetRevision: v2"))
	wantExact = bytes.ReplaceAll(wantExact, []byte("tag: old-image"), []byte("tag: image-abc123"))
	if !bytes.Equal(out, wantExact) {
		t.Errorf("unrelated manifest bytes changed:\n%s", out)
	}
	for _, want := range [][]byte{
		[]byte("# file header"), []byte("# identity comment"), []byte("# sources comment"),
		[]byte("targetRevision: v2 # revision comment"), []byte("tag: image-abc123 # image comment"),
		[]byte("server: https://kubernetes.default.svc # untouched"),
	} {
		if !bytes.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUpdateManifestAlreadyCorrectIsNoOp(t *testing.T) {
	input := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: foundry-operator
  namespace: argocd
spec:
  sources:
    - targetRevision: v2
      helm:
        valuesObject:
          image:
            tag: image-abc123
`)
	out, changed, err := UpdateManifest(input, applicationTarget("image-{{ .rev }}"), map[string]string{"tag": "v2", "rev": "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("already-correct manifest reported changed")
	}
	if !bytes.Equal(out, input) {
		t.Fatal("no-op did not return original bytes")
	}
}

func TestUpdateManifestCreatesMissingMappingsAndLeaf(t *testing.T) {
	input := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: foundry-operator
  namespace: argocd
spec:
  sources:
    - helm:
        valuesObject:
          image:
            tag: old # keep image comment
          # gatewayFlakeRef intentionally unset
  destination:
    namespace: foundry # keep destination
`)
	target := []v1alpha1.TargetSpec{{
		APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "foundry-operator", Namespace: "argocd",
		Patches: []v1alpha1.PatchSpec{
			{Field: "spec.sources.0.helm.valuesObject.nixMount.storePath", Template: "/nix/store/new"},
			{Field: "spec.sources.0.helm.valuesObject.gatewayFlakeRef", Template: "github:org/repo/rev"},
		},
	}}
	out, changed, err := UpdateManifest(input, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("missing fields were not created")
	}
	for _, want := range []string{
		"          nixMount:\n            storePath: /nix/store/new\n",
		"          gatewayFlakeRef: github:org/repo/rev\n",
		"# keep image comment", "# gatewayFlakeRef intentionally unset", "namespace: foundry # keep destination",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUpdateManifestNumericLookingScalarNoOp(t *testing.T) {
	input := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: foundry-operator
  namespace: argocd
spec:
  sources:
    - targetRevision: v2
      helm:
        valuesObject:
          image:
            tag: 12345
`)
	out, changed, err := UpdateManifest(input, applicationTarget("12345"), map[string]string{"tag": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if changed || !bytes.Equal(out, input) {
		t.Fatalf("numeric-looking no-op changed manifest:\n%s", out)
	}
}
