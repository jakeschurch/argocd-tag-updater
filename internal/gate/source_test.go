package gate

import (
	"testing"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func workload(kind, phase, tag string, generation int64, observed interface{}) *unstructured.Unstructured {
	version := "apps/v1"
	if kind == "Rollout" {
		version = "argoproj.io/v1alpha1"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": version, "kind": kind,
		"metadata": map[string]interface{}{"name": "web", "namespace": "web", "generation": generation},
		"spec":     map[string]interface{}{"replicas": int64(2), "template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "web", "image": "registry/web:" + tag}}}}},
		"status":   map[string]interface{}{"phase": phase, "observedGeneration": observed, "stableRS": "hash", "currentPodHash": "hash", "readyReplicas": int64(2), "updatedReplicas": int64(2), "availableReplicas": int64(2), "replicas": int64(2)},
	}}
}

func TestPresetsNeverBlameStaleStatusOrDifferentTag(t *testing.T) {
	for _, preset := range []string{"deployment", "statefulset", "argo-rollout"} {
		s, err := Compile(v1.GateSpec{Preset: preset, Namespace: "web", Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		obj := workload(s.GVK().Kind, "Degraded", "v2", 2, "1")
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{map[string]interface{}{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}, "status", "conditions")
		for _, tag := range []string{"v2", "v1"} {
			d, h, f, _ := s.Evaluate(obj, tag)
			if d || h || f {
				t.Fatalf("%s blamed stale status: %v %v %v", preset, d, h, f)
			}
		}
		obj.Object["status"].(map[string]interface{})["observedGeneration"] = "2"
		if d, _, f, _ := s.Evaluate(obj, "v1"); d || f {
			t.Fatal("different tag blamed")
		}
	}
}

func TestRolloutAbortIsFailureOnlyAfterAcknowledgement(t *testing.T) {
	s, _ := Compile(v1.GateSpec{Preset: "argo-rollout", Name: "web", Namespace: "web"})
	obj := workload("Rollout", "Degraded", "v2", 2, "2")
	if d, h, f, _ := s.Evaluate(obj, "v2"); !d || h || !f {
		t.Fatal("acknowledged abort did not fail")
	}
	obj.Object["status"].(map[string]interface{})["phase"] = "Healthy"
	if d, h, f, _ := s.Evaluate(obj, "v2"); !d || !h || f {
		t.Fatal("healthy Rollout did not pass")
	}
}

func TestCELCompilationEvaluationAndFailClosed(t *testing.T) {
	spec := v1.GateSpec{APIVersion: "example.org/v1", Kind: "Build", Name: "web", Namespace: "web", Deployed: "object.status.observedGeneration == object.metadata.generation && object.spec.tag == tag", Healthy: "object.status.phase == 'Healthy'", Failed: "object.status.phase == 'Failed'"}
	s, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"generation": int64(2)}, "spec": map[string]interface{}{"tag": "v2"}, "status": map[string]interface{}{"observedGeneration": int64(1), "phase": "Failed"}}}
	if d, h, f, _ := s.Evaluate(obj, "v2"); d || h || f {
		t.Fatal("CEL stale status blamed")
	}
	obj.Object["status"].(map[string]interface{})["observedGeneration"] = int64(2)
	if d, _, f, _ := s.Evaluate(obj, "v2"); !d || !f {
		t.Fatal("CEL failure lost")
	}
	delete(obj.Object, "status")
	if d, h, f, _ := s.Evaluate(obj, "v2"); d || h || f {
		t.Fatal("missing CEL field passed")
	}
	for _, expression := range []string{"1", "object.", ""} {
		spec.Deployed = expression
		if _, err := Compile(spec); err == nil {
			t.Fatalf("invalid CEL accepted: %q", expression)
		}
	}
}

func TestOnDeleteOldPodsCannotBeHealthy(t *testing.T) {
	s, _ := Compile(v1.GateSpec{Preset: "statefulset", Name: "web", Namespace: "web"})
	obj := workload("StatefulSet", "", "v2", 2, int64(2))
	obj.Object["status"].(map[string]interface{})["currentRevision"] = "old"
	obj.Object["status"].(map[string]interface{})["updateRevision"] = "new"
	if d, h, _, _ := s.Evaluate(obj, "v2"); !d || h {
		t.Fatal("old ready OnDelete pods passed")
	}
}
