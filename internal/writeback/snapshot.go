package writeback

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

func manifestRoots(input []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(input))
	var roots []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err == io.EOF {
			return roots, nil
		}
		if err != nil {
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		root, err := documentMap(&doc)
		if err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
}

func snapshotNodeAt(root *yaml.Node, field string) (*yaml.Node, error) {
	node := root
	for _, part := range strings.Split(field, ".") {
		if node == nil {
			return nil, nil
		}
		switch node.Kind {
		case yaml.MappingNode:
			node = mapValue(node, part)
		case yaml.SequenceNode:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node.Content) {
				return nil, fmt.Errorf("invalid index %q in %s", part, field)
			}
			node = node.Content[i]
		default:
			return nil, fmt.Errorf("non-container in %s", field)
		}
	}
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.ScalarNode || node.Anchor != "" || (node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!float" && node.Tag != "!!bool" && node.Tag != "!!null") {
		return nil, fmt.Errorf("rollback field %s must be a plain scalar", field)
	}
	return node, nil
}

func snapshotScalarAt(root *yaml.Node, field string) (*string, error) {
	node, err := snapshotNodeAt(root, field)
	if err != nil || node == nil {
		return nil, err
	}
	value := node.Value
	return &value, nil
}

func scalarTypeAt(root *yaml.Node, field string) string {
	node, _ := snapshotNodeAt(root, field)
	if node == nil || node.Tag == "!!str" {
		return ""
	}
	return node.Tag
}

func snapshotManifest(before, after []byte, path string, targets []v1.TargetSpec) ([]v1.RollbackField, error) {
	oldRoots, err := manifestRoots(before)
	if err != nil {
		return nil, err
	}
	newRoots, err := manifestRoots(after)
	if err != nil {
		return nil, err
	}
	var fields []v1.RollbackField
	for _, target := range targets {
		for i, root := range oldRoots {
			matches, err := matchesTarget(root, target)
			if err != nil {
				return nil, err
			}
			if !matches {
				continue
			}
			metadata := mapValue(root, "metadata")
			for _, patch := range target.Patches {
				oldValue, err := snapshotScalarAt(root, patch.Field)
				if err != nil {
					return nil, err
				}
				newValue, err := snapshotScalarAt(newRoots[i], patch.Field)
				if err != nil {
					return nil, err
				}
				beforeType, afterType := scalarTypeAt(root, patch.Field), scalarTypeAt(newRoots[i], patch.Field)
				if sameScalar(oldValue, newValue) && beforeType == afterType {
					continue
				}
				field := v1.RollbackField{Path: path, APIVersion: target.APIVersion, Kind: target.Kind, Name: mapValue(metadata, "name").Value, Field: patch.Field, Before: oldValue, After: newValue}
				field.BeforeType, field.AfterType = beforeType, afterType
				if ns := mapValue(metadata, "namespace"); ns != nil {
					field.Namespace = ns.Value
				}
				fields = append(fields, field)
			}
		}
	}
	return fields, nil
}

func sameScalar(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// Restore only fields this release changed, checking current values on every
// retry. An unrelated writer's fields survive; a conflicting edit blocks rollback.
func restoreManifest(input []byte, path string, fields []v1.RollbackField) ([]byte, bool, error) {
	roots, err := manifestRoots(input)
	if err != nil {
		return nil, false, err
	}
	var targets []v1.TargetSpec
	data := map[string]string{}
	for i, field := range fields {
		if field.Path != path {
			continue
		}
		target := v1.TargetSpec{APIVersion: field.APIVersion, Kind: field.Kind, Name: field.Name, Namespace: field.Namespace}
		found := false
		for _, root := range roots {
			matches, err := matchesTarget(root, target)
			if err != nil {
				return nil, false, err
			}
			if !matches {
				continue
			}
			found = true
			current, err := snapshotScalarAt(root, field.Field)
			if err != nil {
				return nil, false, err
			}
			currentType := scalarTypeAt(root, field.Field)
			if sameScalar(current, field.Before) && currentType == field.BeforeType {
				continue
			}
			if !sameScalar(current, field.After) || currentType != field.AfterType {
				return nil, false, fmt.Errorf("rollback conflict: %s %s/%s field %s changed externally", path, field.Kind, field.Name, field.Field)
			}
			patch := v1.PatchSpec{Field: field.Field}
			if field.Before == nil {
				patch.Operation = "remove"
				patch.ExpectedValue = field.After
			} else {
				key := fmt.Sprintf("value%d", i)
				data[key] = *field.Before
				patch.Template = "{{ ." + key + " }}"
			}
			target.Patches = []v1.PatchSpec{patch}
			targets = append(targets, target)
		}
		if !found {
			return nil, false, fmt.Errorf("rollback target %s/%s missing", field.Kind, field.Name)
		}
	}
	output, changed, err := updateManifest(input, targets, data, nil, true)
	if err != nil {
		return nil, false, err
	}
	// 2026-11-11: tag: 20261111 is legal YAML. Preserve its scalar type on
	// rollback while retaining compare guards against external type changes.
	restoredRoots, err := manifestRoots(output)
	if err != nil {
		return nil, false, err
	}
	typed := map[*yaml.Node]string{}
	for _, field := range fields {
		if field.Path != path || field.Before == nil || field.BeforeType == "" {
			continue
		}
		for _, root := range restoredRoots {
			match, _ := matchesTarget(root, v1.TargetSpec{APIVersion: field.APIVersion, Kind: field.Kind, Name: field.Name, Namespace: field.Namespace})
			if !match {
				continue
			}
			node, err := snapshotNodeAt(root, field.Field)
			if err != nil || node == nil {
				return nil, false, fmt.Errorf("restore typed scalar %s", field.Field)
			}
			if node.Tag != field.BeforeType {
				node.Tag = field.BeforeType
				typed[node] = *field.Before
			}
		}
	}
	if len(typed) != 0 {
		output, err = applyYAMLEdits(output, typed, nil, nil)
		changed = true
	}
	return output, changed, err
}
