package writeback

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

// UpdateManifest renders and applies only configured target fields to matching
// YAML documents. yaml.Node retains comments, mapping order, anchors, and style.
func UpdateManifest(input []byte, targets []v1alpha1.TargetSpec, data map[string]string) ([]byte, bool, error) {
	return updateManifestWithSources(input, targets, data, nil)
}

func updateManifestWithSources(input []byte, targets []v1alpha1.TargetSpec, data map[string]string, sources map[string]map[string]string) ([]byte, bool, error) {
	return updateManifest(input, targets, data, sources, false)
}

func updateManifest(input []byte, targets []v1alpha1.TargetSpec, data map[string]string, sources map[string]map[string]string, allowEmpty bool) ([]byte, bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(input))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, false, fmt.Errorf("decode manifest: %w", err)
		}
		if len(doc.Content) != 0 {
			docs = append(docs, &doc)
		}
	}

	edits := map[*yaml.Node]string{}
	insertions := map[*yaml.Node][]*yaml.Node{}
	removals := map[*yaml.Node]*yaml.Node{}
	for _, target := range targets {
		targetData := data
		if target.Source != "" {
			var ok bool
			targetData, ok = sources[target.Source]
			if !ok {
				return nil, false, fmt.Errorf("unknown target source %q", target.Source)
			}
		}
		matched := 0
		for _, doc := range docs {
			root, err := documentMap(doc)
			if err != nil {
				return nil, false, err
			}
			ok, err := matchesTarget(root, target)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue
			}
			matched++
			for _, p := range target.Patches {
				switch p.Operation {
				case "", "set":
					if p.ExpectedValue != nil {
						return nil, false, fmt.Errorf("field %q: expectedValue is only supported for remove", p.Field)
					}
				case "remove":
					if p.ExpectedValue == nil || p.Template != "" {
						return nil, false, fmt.Errorf("field %q: remove requires expectedValue and no template", p.Field)
					}
					key, value, err := removalTarget(root, strings.Split(p.Field, "."))
					if err != nil {
						return nil, false, fmt.Errorf("field %q: %w", p.Field, err)
					}
					if value != nil {
						if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value != *p.ExpectedValue {
							return nil, false, fmt.Errorf("field %q: remove expectedValue does not match the existing string scalar", p.Field)
						}
						removals[key] = value
					}
					continue
				default:
					return nil, false, fmt.Errorf("field %q: unsupported operation %q", p.Field, p.Operation)
				}
				value, err := renderTemplate(p.Template, targetData)
				if err != nil {
					return nil, false, fmt.Errorf("render template for field %q: %w", p.Field, err)
				}
				if strings.TrimSpace(value) == "" && !allowEmpty {
					continue
				}
				node, fieldChanged, err := setScalar(root, strings.Split(p.Field, "."), value, insertions)
				if err != nil {
					return nil, false, fmt.Errorf("target %s/%s field %q: %w", target.Kind, target.Name, p.Field, err)
				}
				if fieldChanged {
					if node.Line > 0 {
						edits[node] = value
					}
					node.Value = value
					node.Tag = "!!str"
				}
			}
		}
		if matched == 0 {
			return nil, false, fmt.Errorf("target %s/%s not found in manifest", target.Kind, target.Name)
		}
	}
	if len(edits) == 0 && len(insertions) == 0 && len(removals) == 0 {
		return input, false, nil
	}
	output, err := applyYAMLEdits(input, edits, insertions, removals)
	if err != nil {
		return nil, false, err
	}
	return output, !bytes.Equal(output, input), nil
}

func removalTarget(node *yaml.Node, path []string) (*yaml.Node, *yaml.Node, error) {
	for index, segment := range path {
		if segment == "" {
			return nil, nil, fmt.Errorf("empty field path segment")
		}
		switch node.Kind {
		case yaml.MappingNode:
			matched := -1
			for entry := 0; entry < len(node.Content); entry += 2 {
				if node.Content[entry].Value != segment {
					continue
				}
				if matched >= 0 {
					return nil, nil, fmt.Errorf("remove path contains a duplicate mapping key")
				}
				matched = entry
			}
			if matched < 0 {
				return nil, nil, nil
			}
			if index == len(path)-1 {
				if node.Style&yaml.FlowStyle != 0 || len(node.Content) == 2 {
					return nil, nil, fmt.Errorf("remove requires a block mapping with another retained entry")
				}
				return node.Content[matched], node.Content[matched+1], nil
			}
			node = node.Content[matched+1]
		case yaml.SequenceNode:
			position, err := strconv.Atoi(segment)
			if err != nil || position < 0 || position >= len(node.Content) || index == len(path)-1 {
				return nil, nil, fmt.Errorf("remove path has an invalid list index or selects a list element")
			}
			node = node.Content[position]
		default:
			return nil, nil, fmt.Errorf("remove path traverses a non-mapping value")
		}
	}
	return nil, nil, fmt.Errorf("remove path must select a mapping entry")
}

func renderTemplate(text string, data map[string]string) (string, error) {
	tmpl, err := template.New("value").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func documentMap(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("manifest document must contain a mapping")
	}
	return doc.Content[0], nil
}

func matchesTarget(root *yaml.Node, target v1alpha1.TargetSpec) (bool, error) {
	if scalarAt(root, "apiVersion") != target.APIVersion || scalarAt(root, "kind") != target.Kind {
		return false, nil
	}
	metadata := mapValue(root, "metadata")
	if metadata == nil {
		return false, nil
	}
	if target.Name != "" && scalarAt(metadata, "name") != target.Name {
		return false, nil
	}
	if target.Namespace != "" && scalarAt(metadata, "namespace") != target.Namespace {
		return false, nil
	}
	if target.Selector == nil {
		return target.Name != "", nil
	}
	sel, err := metav1.LabelSelectorAsSelector(target.Selector)
	if err != nil {
		return false, fmt.Errorf("target %s selector: %w", target.Kind, err)
	}
	labelNode := mapValue(metadata, "labels")
	set := labels.Set{}
	if labelNode != nil {
		for i := 0; i < len(labelNode.Content); i += 2 {
			set[labelNode.Content[i].Value] = labelNode.Content[i+1].Value
		}
	}
	return sel.Matches(set), nil
}

func setScalar(node *yaml.Node, path []string, value string, insertions map[*yaml.Node][]*yaml.Node) (*yaml.Node, bool, error) {
	cur := node
	for i, segment := range path {
		last := i == len(path)-1
		switch cur.Kind {
		case yaml.MappingNode:
			parent := cur
			cur = mapValue(parent, segment)
			if cur == nil {
				key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}
				if last {
					cur = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
				} else {
					cur = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				}
				parent.Content = append(parent.Content, key, cur)
				if parent.Line > 0 {
					insertions[parent] = append(insertions[parent], key, cur)
				}
			}
		case yaml.SequenceNode:
			idx, err := strconv.Atoi(segment)
			if err != nil || idx < 0 || idx >= len(cur.Content) {
				return nil, false, fmt.Errorf("path segment %q is not a valid list index", strings.Join(path[:i+1], "."))
			}
			cur = cur.Content[idx]
		default:
			return nil, false, fmt.Errorf("path segment %q traverses a scalar", strings.Join(path[:i], "."))
		}
		if last {
			if cur.Kind != yaml.ScalarNode {
				return nil, false, fmt.Errorf("target is not a scalar")
			}
			return cur, true, nil
		}
	}
	return nil, false, fmt.Errorf("empty field path")
}

type scalarEdit struct {
	start, end int
	value      string
}

// applyYAMLEdits uses yaml.Node solely for structure and source positions,
// then replaces scalar token spans in the original byte stream. Unlike a YAML
// encode round trip, unrelated whitespace, comments, directives, and document
// separators remain byte-for-byte identical.
func applyYAMLEdits(input []byte, values map[*yaml.Node]string, insertions map[*yaml.Node][]*yaml.Node, removals map[*yaml.Node]*yaml.Node) ([]byte, error) {
	lineStarts := []int{0}
	for i, b := range input {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	var edits []scalarEdit
	for key, value := range removals {
		if key.Line < 1 || key.Line > len(lineStarts) || value.Line != key.Line || key.Column < 1 || value.Column < 1 || value.Anchor != "" || key.Anchor != "" || value.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return nil, fmt.Errorf("remove requires an unanchored single-line mapping entry")
		}
		start := lineStarts[key.Line-1]
		end := len(input)
		if key.Line < len(lineStarts) {
			end = lineStarts[key.Line]
		}
		keyStart := start + key.Column - 1
		valueStart := start + value.Column - 1
		if keyStart > end || valueStart >= end || strings.TrimSpace(string(input[start:keyStart])) != "" {
			return nil, fmt.Errorf("remove requires a standalone mapping entry")
		}
		valueEnd, err := scalarTokenEnd(input, valueStart, value.Style)
		if err != nil || valueEnd > end {
			return nil, fmt.Errorf("remove requires a single-line scalar")
		}
		trailing := strings.TrimSpace(string(input[valueEnd:end]))
		if trailing != "" && !strings.HasPrefix(trailing, "#") {
			return nil, fmt.Errorf("remove would affect another YAML token")
		}
		var singleLine yaml.Node
		if err := yaml.Unmarshal(input[start:end], &singleLine); err != nil || len(singleLine.Content) != 1 {
			return nil, fmt.Errorf("remove requires a complete single-line mapping entry")
		}
		parsed := mapValue(singleLine.Content[0], key.Value)
		if parsed == nil || parsed.Kind != value.Kind || parsed.Tag != value.Tag || parsed.Value != value.Value {
			return nil, fmt.Errorf("remove cannot truncate a multiline value")
		}
		edits = append(edits, scalarEdit{start: start, end: end})
	}
	for node, value := range values {
		if node.Line < 1 || node.Line > len(lineStarts) || node.Column < 1 {
			return nil, fmt.Errorf("invalid YAML source position for edited scalar")
		}
		if node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return nil, fmt.Errorf("block scalar targets are not supported")
		}
		start := lineStarts[node.Line-1] + node.Column - 1
		end, err := scalarTokenEnd(input, start, node.Style)
		if err != nil {
			return nil, err
		}
		rendered, err := renderScalar(value, node.Style)
		if err != nil {
			return nil, err
		}
		edits = append(edits, scalarEdit{start: start, end: end, value: rendered})
	}
	for parent, content := range insertions {
		start, indent, err := mappingInsertionPoint(input, lineStarts, parent)
		if err != nil {
			return nil, err
		}
		rendered, err := renderMappingEntries(content, indent)
		if err != nil {
			return nil, err
		}
		if start > 0 && input[start-1] != '\n' {
			rendered = "\n" + rendered
		}
		edits = append(edits, scalarEdit{start: start, end: start, value: rendered})
	}
	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if edits[i].start < edits[j].start {
				edits[i], edits[j] = edits[j], edits[i]
			}
		}
	}
	out := append([]byte(nil), input...)
	for index, edit := range edits {
		if index > 0 && edit.end > edits[index-1].start {
			return nil, fmt.Errorf("configured YAML edits overlap")
		}
		out = append(out[:edit.start], append([]byte(edit.value), out[edit.end:]...)...)
	}
	return out, nil
}

func mappingInsertionPoint(input []byte, lineStarts []int, node *yaml.Node) (int, int, error) {
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
		return 0, 0, fmt.Errorf("cannot insert into an empty or non-mapping YAML node")
	}
	indent := node.Content[0].Column - 1
	startLine := node.Content[0].Line - 1
	for line := startLine + 1; line < len(lineStarts); line++ {
		start := lineStarts[line]
		end := len(input)
		if line+1 < len(lineStarts) {
			end = lineStarts[line+1]
		}
		text := strings.TrimRight(string(input[start:end]), "\r\n")
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lineIndent := len(text) - len(strings.TrimLeft(text, " "))
		if lineIndent < indent {
			return start, indent, nil
		}
	}
	return len(input), indent, nil
}

func renderMappingEntries(content []*yaml.Node, indent int) (string, error) {
	node := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return "", fmt.Errorf("encode inserted YAML mapping: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("close inserted YAML mapping encoder: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	prefix := strings.Repeat(" ", indent)
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func scalarTokenEnd(input []byte, start int, style yaml.Style) (int, error) {
	if start >= len(input) {
		return 0, fmt.Errorf("edited scalar starts past end of manifest")
	}
	quote := byte(0)
	if style&yaml.DoubleQuotedStyle != 0 {
		quote = '"'
	} else if style&yaml.SingleQuotedStyle != 0 {
		quote = '\''
	}
	if quote != 0 {
		for i := start + 1; i < len(input); i++ {
			if quote == '"' && input[i] == '\\' {
				i++
				continue
			}
			if input[i] == quote {
				if quote == '\'' && i+1 < len(input) && input[i+1] == '\'' {
					i++
					continue
				}
				return i + 1, nil
			}
		}
		return 0, fmt.Errorf("unterminated quoted scalar")
	}
	end := start
	for end < len(input) && input[end] != '\n' && input[end] != '\r' {
		if input[end] == '#' && end > start && (input[end-1] == ' ' || input[end-1] == '\t') {
			break
		}
		end++
	}
	for end > start && (input[end-1] == ' ' || input[end-1] == '\t') {
		end--
	}
	return end, nil
}

func renderScalar(value string, style yaml.Style) (string, error) {
	if style == 0 && safePlainScalar(value) {
		return value, nil
	}
	node := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	if err := enc.Encode(&node); err != nil {
		return "", fmt.Errorf("encode replacement scalar: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("close scalar encoder: %w", err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func safePlainScalar(value string) bool {
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return false
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("value: "+value+"\n"), &doc); err != nil {
		return false
	}
	if len(doc.Content) != 1 {
		return false
	}
	node := mapValue(doc.Content[0], "value")
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == value
}

func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func scalarAt(node *yaml.Node, key string) string {
	value := mapValue(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}
