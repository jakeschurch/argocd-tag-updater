// Package gate observes release health without changing watched workloads.
package gate

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type Matcher struct {
	Namespace, Name string
	Selector        labels.Selector
}

func (m Matcher) Matches(obj *unstructured.Unstructured) bool {
	return obj.GetNamespace() == m.Namespace && (m.Name == "" || obj.GetName() == m.Name) && m.Selector.Matches(labels.Set(obj.GetLabels()))
}

type GateSource interface {
	GVK() schema.GroupVersionKind
	Match(v1.GateSpec) (Matcher, error)
	Evaluate(obj *unstructured.Unstructured, tag string) (deployed, healthy, failed bool, reason string)
}

// Kind is the exact discovery/RBAC allowlist entry; no plural guessing or wildcard.
type Kind struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Resource   string `json:"resource"`
}

var Defaults = []Kind{
	{"argoproj.io/v1alpha1", "Application", "applications"},
	{"argoproj.io/v1alpha1", "Rollout", "rollouts"},
	{"argoproj.io/v1alpha1", "AnalysisRun", "analysisruns"},
	{"apps/v1", "Deployment", "deployments"},
	{"apps/v1", "StatefulSet", "statefulsets"},
	{"kargo.akuity.io/v1alpha1", "Promotion", "promotions"},
}

var presets = map[string]Kind{
	"argocd-application": Defaults[0], "argo-rollout": Defaults[1], "analysisrun": Defaults[2],
	"deployment": Defaults[3], "statefulset": Defaults[4], "kargo-promotion": Defaults[5],
}

func PresetGVK(preset string) string {
	kind := presets[preset]
	return kind.APIVersion + "/" + kind.Kind
}

type source struct {
	kind     schema.GroupVersionKind
	preset   string
	programs []cel.Program
}

func Compile(spec v1.GateSpec) (GateSource, error) {
	kind := Kind{APIVersion: spec.APIVersion, Kind: spec.Kind}
	if spec.Preset != "" {
		var ok bool
		kind, ok = presets[spec.Preset]
		if !ok {
			return nil, fmt.Errorf("unknown gate preset %q", spec.Preset)
		}
		if spec.APIVersion != "" || spec.Kind != "" || spec.Deployed != "" || spec.Healthy != "" || spec.Failed != "" {
			return nil, fmt.Errorf("preset and custom gate fields are mutually exclusive")
		}
	}
	gv, err := schema.ParseGroupVersion(kind.APIVersion)
	if err != nil || kind.APIVersion == "" || kind.Kind == "" {
		return nil, fmt.Errorf("gate requires apiVersion and kind")
	}
	s := &source{kind: gv.WithKind(kind.Kind), preset: spec.Preset}
	if _, err := s.Match(spec); err != nil {
		return nil, err
	}
	if s.preset == "" {
		env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("tag", cel.StringType))
		if err != nil {
			return nil, err
		}
		for _, expression := range []string{spec.Deployed, spec.Healthy, spec.Failed} {
			if expression == "" {
				return nil, fmt.Errorf("custom gate requires deployed, healthy and failed CEL")
			}
			ast, issues := env.Compile(expression)
			if issues.Err() != nil {
				return nil, issues.Err()
			}
			if ast.OutputType() != cel.BoolType {
				return nil, fmt.Errorf("gate CEL must return bool")
			}
			program, err := env.Program(ast, cel.CostLimit(10000))
			if err != nil {
				return nil, err
			}
			s.programs = append(s.programs, program)
		}
	}
	return s, nil
}

func (s *source) GVK() schema.GroupVersionKind { return s.kind }
func (s *source) Match(spec v1.GateSpec) (Matcher, error) {
	if spec.Namespace == "" || (spec.Name == "") == (spec.Selector == nil) {
		return Matcher{}, fmt.Errorf("gate requires namespace and exactly one of name or selector")
	}
	selector := labels.Everything()
	if spec.Selector != nil {
		var err error
		selector, err = metav1.LabelSelectorAsSelector(spec.Selector)
		if err != nil {
			return Matcher{}, err
		}
	}
	return Matcher{spec.Namespace, spec.Name, selector}, nil
}

func text(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedFieldNoCopy(obj.Object, fields...)
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
func number(obj *unstructured.Unstructured, fields ...string) int64 {
	value, _, _ := unstructured.NestedInt64(obj.Object, fields...)
	return value
}
func condition(obj *unstructured.Unstructured, kind, status, reason string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		c, ok := item.(map[string]interface{})
		if ok && c["type"] == kind && c["status"] == status && (reason == "" || c["reason"] == reason) {
			return true
		}
	}
	return false
}
func containsTag(value interface{}, tag string) bool {
	if tag == "" {
		return false
	}
	switch item := value.(type) {
	case string:
		// Tags embedded in image/flake references must have token boundaries:
		// release v2 must never claim status belonging to v20.
		word := func(b byte) bool {
			return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
		}
		for start := 0; start < len(item); {
			index := strings.Index(item[start:], tag)
			if index < 0 {
				break
			}
			index += start
			end := index + len(tag)
			if (index == 0 || !word(item[index-1])) && (end == len(item) || !word(item[end])) {
				return true
			}
			start = index + 1
		}
	case map[string]interface{}:
		for _, child := range item {
			if containsTag(child, tag) {
				return true
			}
		}
	case []interface{}:
		for _, child := range item {
			if containsTag(child, tag) {
				return true
			}
		}
	}
	return false
}

func (s *source) Evaluate(obj *unstructured.Unstructured, tag string) (bool, bool, bool, string) {
	if len(s.programs) != 0 {
		values := make([]bool, 3)
		for i, program := range s.programs {
			if tag == "" && i != 1 {
				continue
			}
			value, _, err := program.Eval(map[string]interface{}{"object": obj.Object, "tag": tag})
			if err != nil || types.IsError(value) || types.IsUnknown(value) {
				return false, false, false, fmt.Sprintf("CEL evaluation failed: %v", value)
			}
			values[i] = value == types.True
		}
		return values[0], (tag == "" || values[0]) && values[1], values[0] && values[2], "CEL"
	}
	observed := text(obj, "status", "observedGeneration") == strconv.FormatInt(obj.GetGeneration(), 10) && obj.GetGeneration() > 0
	deployed := containsTag(obj.Object["spec"], tag) && observed
	healthy, failed := false, false
	switch s.preset {
	case "deployment", "statefulset":
		replicas := int64(1)
		if value, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); found {
			replicas = value
		}
		healthy = observed && number(obj, "status", "updatedReplicas") == replicas && number(obj, "status", "readyReplicas") == replicas
		if s.preset == "deployment" {
			healthy = healthy && number(obj, "status", "availableReplicas") == replicas && number(obj, "status", "replicas") == replicas
			failed = condition(obj, "Progressing", "False", "ProgressDeadlineExceeded") || condition(obj, "ReplicaFailure", "True", "")
		} else {
			healthy = healthy && text(obj, "status", "currentRevision") == text(obj, "status", "updateRevision") && text(obj, "status", "updateRevision") != ""
		}
	case "argo-rollout":
		phase := text(obj, "status", "phase")
		healthy = observed && phase == "Healthy" && text(obj, "status", "stableRS") == text(obj, "status", "currentPodHash") && text(obj, "status", "stableRS") != ""
		failed = phase == "Degraded" || text(obj, "status", "abort") == "true" || condition(obj, "Progressing", "False", "RolloutAborted")
	case "analysisrun":
		// AnalysisRun specs are immutable while running; startedAt proves execution.
		deployed = containsTag(obj.Object["spec"], tag) && text(obj, "status", "startedAt") != ""
		phase := text(obj, "status", "phase")
		healthy, failed = phase == "Successful", phase == "Failed" || phase == "Error"
	case "kargo-promotion":
		// Promotions are immutable execution requests; terminal phase is their acknowledgement.
		phase := text(obj, "status", "phase")
		deployed = containsTag(obj.Object["spec"], tag) && phase != "" && phase != "Pending"
		healthy, failed = phase == "Succeeded", phase == "Failed" || phase == "Errored" || phase == "Aborted"
	case "argocd-application":
		// Application has no observedGeneration. comparedTo proves the status is
		// for the desired source; time freshness is also enforced by the controller.
		compared, _, _ := unstructured.NestedMap(obj.Object, "status", "sync", "comparedTo")
		spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
		fresh := false
		for _, key := range []string{"source", "sources"} {
			if spec[key] != nil && reflect.DeepEqual(spec[key], compared[key]) {
				fresh = true
			}
		}
		deployed = fresh && containsTag(spec, tag)
		inFlight := text(obj, "status", "operationState", "phase") == "Running" || text(obj, "status", "operationState", "phase") == "Terminating"
		healthy = text(obj, "status", "health", "status") == "Healthy" && text(obj, "status", "sync", "status") == "Synced" && !inFlight
		failed = text(obj, "status", "health", "status") == "Degraded" && !inFlight
	}
	// tag="" is used only before a write to inspect baseline health.
	return deployed, (deployed || tag == "") && healthy, deployed && failed, s.preset
}
