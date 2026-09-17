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
	for _, target := range targets {
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
				value, err := renderTemplate(p.Template, data)
				if err != nil {
					return nil, false, fmt.Errorf("render template for field %q: %w", p.Field, err)
				}
				if strings.TrimSpace(value) == "" {
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
	if len(edits) == 0 && len(insertions) == 0 {
		return input, false, nil
	}
	output, err := applyYAMLEdits(input, edits, insertions)
	if err != nil {
		return nil, false, err
	}
	return output, !bytes.Equal(output, input), nil
}

func renderTemplate(text string, data map[string]string) (string, error) {
	tmpl, err := template.New("value").Parse(text)
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
func applyYAMLEdits(input []byte, values map[*yaml.Node]string, insertions map[*yaml.Node][]*yaml.Node) ([]byte, error) {
	lineStarts := []int{0}
	for i, b := range input {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	var edits []scalarEdit
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
	for _, edit := range edits {
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
