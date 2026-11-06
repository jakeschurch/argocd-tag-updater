package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

// mergeCheckResult is the first check that did not pass, or passed=true.
type mergeCheckResult struct {
	passed  bool
	reason  string
	message string
}

// evaluateMergeChecks reads each check's object and compares its field. A check
// that cannot be read or is misconfigured holds the tag rather than letting it
// through: a merge check exists to stop a rollout at a bad moment.
func (r *TagUpdaterReconciler) evaluateMergeChecks(ctx context.Context, checks []v1alpha1.MergeCheck) mergeCheckResult {
	for _, check := range checks {
		if check.Object.APIVersion == "" || check.Object.Kind == "" || check.Object.Name == "" || check.Field == "" {
			return mergeCheckResult{reason: "InvalidMergeCheck", message: fmt.Sprintf("merge check %q needs object apiVersion, kind and name, and a field", check.Name)}
		}
		gv, err := schema.ParseGroupVersion(check.Object.APIVersion)
		if err != nil {
			return mergeCheckResult{reason: "InvalidMergeCheck", message: fmt.Sprintf("merge check %q: %v", check.Name, err)}
		}
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(gv.WithKind(check.Object.Kind))
		key := types.NamespacedName{Namespace: check.Object.Namespace, Name: check.Object.Name}
		if err := r.Get(ctx, key, object); err != nil {
			return mergeCheckResult{reason: "MergeCheckError", message: fmt.Sprintf("merge check %q: read %s %s: %v", check.Name, check.Object.Kind, key, err)}
		}
		value, found := lookupField(object.Object, check.Field)
		passed, err := compareMergeCheck(check, value, found)
		if err != nil {
			return mergeCheckResult{reason: "InvalidMergeCheck", message: fmt.Sprintf("merge check %q: %v", check.Name, err)}
		}
		if !passed {
			return mergeCheckResult{reason: "Holding", message: fmt.Sprintf("merge check %q: %s of %s %s is %s, want %s", check.Name, check.Field, check.Object.Kind, key, describe(value, found), want(check))}
		}
	}
	return mergeCheckResult{passed: true, reason: "Passed", message: fmt.Sprintf("%d merge checks passed", len(checks))}
}

// lookupField follows a dot path through maps and, for numeric segments, lists.
func lookupField(object map[string]interface{}, path string) (interface{}, bool) {
	var current interface{} = object
	for _, segment := range strings.Split(path, ".") {
		switch node := current.(type) {
		case map[string]interface{}:
			next, ok := node[segment]
			if !ok {
				return nil, false
			}
			current = next
		case []interface{}:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func empty(value interface{}, found bool) bool {
	if !found || value == nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return typed == ""
	case map[string]interface{}:
		return len(typed) == 0
	case []interface{}:
		return len(typed) == 0
	}
	return false
}

func compareMergeCheck(check v1alpha1.MergeCheck, value interface{}, found bool) (bool, error) {
	switch check.Operator {
	case v1alpha1.MergeCheckEmpty:
		return empty(value, found), nil
	case v1alpha1.MergeCheckNotEmpty:
		return !empty(value, found), nil
	case v1alpha1.MergeCheckEquals, v1alpha1.MergeCheckNotEquals:
		equal := false
		if found && value != nil {
			switch value.(type) {
			case map[string]interface{}, []interface{}:
				return false, fmt.Errorf("%s is a map or list; Equals compares only strings, numbers and booleans", check.Field)
			}
			equal = fmt.Sprint(value) == check.Value
		}
		return equal == (check.Operator == v1alpha1.MergeCheckEquals), nil
	}
	return false, fmt.Errorf("unknown operator %q", check.Operator)
}

func describe(value interface{}, found bool) string {
	switch {
	case !found:
		return "missing"
	case empty(value, found):
		return "empty"
	}
	switch value.(type) {
	case map[string]interface{}, []interface{}:
		return "set"
	}
	return strconv.Quote(fmt.Sprint(value))
}

func want(check v1alpha1.MergeCheck) string {
	switch check.Operator {
	case v1alpha1.MergeCheckEmpty:
		return "empty"
	case v1alpha1.MergeCheckNotEmpty:
		return "not empty"
	case v1alpha1.MergeCheckEquals:
		return strconv.Quote(check.Value)
	}
	return "not " + strconv.Quote(check.Value)
}
