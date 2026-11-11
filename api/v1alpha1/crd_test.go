package v1alpha1

import (
	"os"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"sigs.k8s.io/yaml"
)

func TestCRDBackwardCompatibleGateAdmission(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/updater.argocd.io_tagupdaters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]interface{}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	get := func(value map[string]interface{}, path ...string) map[string]interface{} {
		for _, key := range path {
			value = value[key].(map[string]interface{})
		}
		return value
	}
	version := crd["spec"].(map[string]interface{})["versions"].([]interface{})[0].(map[string]interface{})
	spec := get(version, "schema", "openAPIV3Schema", "properties", "spec")
	for _, required := range spec["required"].([]interface{}) {
		if required == "gate" {
			t.Fatal("new gate required for legacy CRs")
		}
	}
	fields := spec["properties"].(map[string]interface{})
	for _, name := range []string{"argoCDApp", "managingApp", "rollback", "gate", "releaseSources"} {
		if fields[name] == nil {
			t.Fatalf("missing backward-compatible field %s", name)
		}
	}
	rules := get(fields, "gate", "properties", "sources", "items")["x-kubernetes-validations"].([]interface{})
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		spec  map[string]interface{}
		valid bool
	}{
		{"preset", map[string]interface{}{"preset": "argo-rollout", "namespace": "web", "name": "web"}, true},
		{"unknown GVK", map[string]interface{}{"apiVersion": "example.org/v1", "kind": "Unknown", "namespace": "web", "name": "web", "deployed": "true", "healthy": "true", "failed": "false"}, false},
		{"known custom GVK", map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "namespace": "web", "name": "web", "deployed": "true", "healthy": "true", "failed": "false"}, true},
		{"both matchers", map[string]interface{}{"preset": "deployment", "namespace": "web", "name": "web", "selector": map[string]interface{}{}}, false},
		{"mixed preset CEL", map[string]interface{}{"preset": "deployment", "namespace": "web", "name": "web", "deployed": "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := true
			for _, raw := range rules {
				rule := raw.(map[string]interface{})["rule"].(string)
				ast, issues := env.Compile(rule)
				if issues.Err() != nil {
					t.Fatal(issues.Err())
				}
				p, err := env.Program(ast)
				if err != nil {
					t.Fatal(err)
				}
				value, _, err := p.Eval(map[string]interface{}{"self": tc.spec})
				if err != nil || value != types.True {
					valid = false
				}
			}
			if valid != tc.valid {
				t.Fatalf("admission valid=%v want %v", valid, tc.valid)
			}
		})
	}
}
