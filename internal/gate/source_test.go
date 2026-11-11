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

func TestOnDeleteAcknowledgesDesiredRevisionWithoutRequiringPodRotation(t *testing.T) {
	s, _ := Compile(v1.GateSpec{Preset: "statefulset", Name: "web", Namespace: "web"})
	obj := workload("StatefulSet", "", "v2", 2, int64(2))
	_ = unstructured.SetNestedField(obj.Object, "OnDelete", "spec", "updateStrategy", "type")
	_ = unstructured.SetNestedField(obj.Object, "old", "status", "currentRevision")
	_ = unstructured.SetNestedField(obj.Object, "desired-revision", "status", "updateRevision")
	_ = unstructured.SetNestedField(obj.Object, int64(0), "status", "updatedReplicas")
	if d, h, f, _ := s.Evaluate(obj, "v2"); !d || !h || f {
		t.Fatal("ready OnDelete pods waiting for operator rotation blocked acknowledgement")
	}
	if _, h, _, _ := s.Evaluate(obj, ""); !h {
		t.Fatal("ready OnDelete baseline incorrectly unhealthy")
	}
	if d, h, f, _ := s.Evaluate(obj, "v1"); d || h || f {
		t.Fatal("OnDelete acknowledged a tag absent from its desired template")
	}
	_ = unstructured.SetNestedField(obj.Object, int64(1), "status", "readyReplicas")
	if d, h, f, _ := s.Evaluate(obj, "v2"); !d || h || f {
		t.Fatal("OnDelete availability loss was reported healthy or as release failure")
	}
	_ = unstructured.SetNestedField(obj.Object, int64(2), "status", "readyReplicas")
	unstructured.RemoveNestedField(obj.Object, "status", "updateRevision")
	if d, h, f, _ := s.Evaluate(obj, "v2"); d || h || f {
		t.Fatal("OnDelete acknowledged before the StatefulSet controller computed a revision")
	}
	_ = unstructured.SetNestedField(obj.Object, "desired-revision", "status", "updateRevision")
	_ = unstructured.SetNestedField(obj.Object, int64(1), "status", "observedGeneration")
	if d, h, f, _ := s.Evaluate(obj, "v2"); d || h || f {
		t.Fatal("OnDelete acknowledged stale status")
	}
}

func TestStatefulSetRollingUpdateWaitsForReadyNewReplicas(t *testing.T) {
	s, _ := Compile(v1.GateSpec{Preset: "statefulset", Name: "web", Namespace: "web"})
	obj := workload("StatefulSet", "", "v2", 2, int64(2))
	_ = unstructured.SetNestedField(obj.Object, "old", "status", "currentRevision")
	_ = unstructured.SetNestedField(obj.Object, "new", "status", "updateRevision")
	_ = unstructured.SetNestedField(obj.Object, int64(1), "status", "updatedReplicas")
	if d, h, f, _ := s.Evaluate(obj, "v2"); d || h || f {
		t.Fatal("stalled StatefulSet rollout disabled its deployment timeout")
	}
	_ = unstructured.SetNestedField(obj.Object, int64(2), "status", "updatedReplicas")
	_ = unstructured.SetNestedField(obj.Object, "new", "status", "currentRevision")
	_ = unstructured.SetNestedField(obj.Object, int64(1), "status", "readyReplicas")
	if d, h, f, _ := s.Evaluate(obj, "v2"); d || h || f {
		t.Fatal("unready updated StatefulSet acknowledged deployment")
	}
	_ = unstructured.SetNestedField(obj.Object, int64(2), "status", "readyReplicas")
	if d, h, f, _ := s.Evaluate(obj, "v2"); !d || !h || f {
		t.Fatal("ready complete StatefulSet rollout not acknowledged")
	}
	if d, h, f, _ := s.Evaluate(obj, ""); d || !h || f {
		t.Fatal("ready StatefulSet baseline did not pass health independently of a tag")
	}
	unstructured.RemoveNestedField(obj.Object, "status", "currentRevision")
	unstructured.RemoveNestedField(obj.Object, "status", "updateRevision")
	if d, h, f, _ := s.Evaluate(obj, ""); d || h || f {
		t.Fatal("missing StatefulSet revisions were treated as a healthy baseline")
	}
}

func TestRolloutAnalysisErrorIsPendingWhileFailedAnalysisRejectsRelease(t *testing.T) {
	s, _ := Compile(v1.GateSpec{Preset: "argo-rollout", Name: "web", Namespace: "web"})
	for _, phase := range []string{"Degraded", "Healthy"} {
		t.Run(phase, func(t *testing.T) {
			obj := workload("Rollout", phase, "v2", 2, int64(2))
			_ = unstructured.SetNestedField(obj.Object, true, "status", "abort")
			_ = unstructured.SetNestedField(obj.Object, "Error", "status", "currentStepAnalysisRunStatus", "status")
			_ = unstructured.SetNestedSlice(obj.Object, []interface{}{map[string]interface{}{"type": "Progressing", "status": "False", "reason": "RolloutAborted"}}, "status", "conditions")
			if d, h, f, _ := s.Evaluate(obj, "v2"); !d || h || f {
				t.Fatal("analysis infrastructure failure was healthy or blamed the release")
			}
			if d, h, f, _ := s.Evaluate(obj, "v1"); d || h || f {
				t.Fatal("analysis error acknowledged a different tag")
			}
			_ = unstructured.SetNestedField(obj.Object, "Failed", "status", "currentStepAnalysisRunStatus", "status")
			_ = unstructured.SetNestedField(obj.Object, "Degraded", "status", "phase")
			if d, h, f, _ := s.Evaluate(obj, "v2"); !d || h || !f {
				t.Fatal("explicit failed analysis no longer rejects release")
			}
		})
	}
}

func TestDaemonSetAcknowledgesOnlyObservedCompleteRollouts(t *testing.T) {
	s, err := Compile(v1.GateSpec{Preset: "daemonset", Name: "web", Namespace: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := PresetGVK("daemonset"); got != "apps/v1/DaemonSet" {
		t.Fatalf("wrong DaemonSet GVK: %s", got)
	}
	for _, test := range []struct {
		name                               string
		desired, updated, ready, available int64
		generation, observed               int64
		tag                                string
		wantDeployed, wantHealthy          bool
	}{
		{name: "complete", desired: 2, updated: 2, ready: 2, available: 2, generation: 2, observed: 2, tag: "v2", wantDeployed: true, wantHealthy: true},
		{name: "old-pods", desired: 2, updated: 1, ready: 2, available: 2, generation: 2, observed: 2, tag: "v2"},
		{name: "unready", desired: 2, updated: 2, ready: 1, available: 1, generation: 2, observed: 2, tag: "v2"},
		{name: "not-yet-available", desired: 2, updated: 2, ready: 2, available: 1, generation: 2, observed: 2, tag: "v2"},
		{name: "never-observed", desired: 2, updated: 2, ready: 2, available: 2, generation: 2, observed: 0, tag: "v2"},
		{name: "stale-observation", desired: 2, updated: 2, ready: 2, available: 2, generation: 2, observed: 1, tag: "v2"},
		{name: "zero-generation", desired: 2, updated: 2, ready: 2, available: 2, generation: 0, observed: 0, tag: "v2"},
		{name: "different-tag", desired: 2, updated: 2, ready: 2, available: 2, generation: 2, observed: 2, tag: "v1"},
		{name: "empty-schedule", generation: 2, observed: 2, tag: "v2"},
		{name: "healthy-baseline", desired: 2, updated: 2, ready: 2, available: 2, generation: 2, observed: 2, tag: "", wantHealthy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			obj := workload("DaemonSet", "", "v2", test.generation, test.observed)
			for field, value := range map[string]int64{
				"desiredNumberScheduled": test.desired,
				"updatedNumberScheduled": test.updated,
				"numberReady":            test.ready,
				"numberAvailable":        test.available,
			} {
				_ = unstructured.SetNestedField(obj.Object, value, "status", field)
			}
			if d, h, f, _ := s.Evaluate(obj, test.tag); d != test.wantDeployed || h != test.wantHealthy || f {
				t.Fatalf("DaemonSet gate = deployed:%v healthy:%v failed:%v; want deployed:%v healthy:%v", d, h, f, test.wantDeployed, test.wantHealthy)
			}
		})
	}
}
