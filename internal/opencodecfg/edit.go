package opencodecfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Update describes the changes to make to an OpenCode configuration.
type Update struct {
	// ProviderID is the provider key to create or replace, for example
	// "rotator".
	ProviderID string
	// Provider is the provider object. It must not be nil.
	Provider any
	// DefaultModel is the model ID to use as the top-level default, for
	// example "deepseek-v4.1-flash-free". The provider prefix is added
	// automatically. Empty means leave the default alone.
	DefaultModel string
	// KeepDefault leaves an existing top-level model setting untouched.
	KeepDefault bool
}

// Apply edits src in place and returns a new document. Everything outside the
// changed values is preserved byte-for-byte, including comments and
// indentation. Applying the same update twice is idempotent.
func Apply(src []byte, u Update) ([]byte, error) {
	if strings.TrimSpace(u.ProviderID) == "" {
		return nil, fmt.Errorf("provider ID is required")
	}
	if u.Provider == nil {
		return nil, fmt.Errorf("provider value is required")
	}
	doc, err := Parse(src)
	if err != nil {
		return nil, err
	}
	nl := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		nl = "\r\n"
	}

	root := doc.Root
	rootIndent := childIndent(src, root, "")
	rootEmpty := len(root.Members) == 0

	providersMember := root.Member("providers")
	modelMember := root.Member("model")

	setModel := u.DefaultModel != "" && !u.KeepDefault
	needProviders := providersMember == nil
	needModel := setModel && modelMember == nil
	needProviderEntry := false

	if providersMember != nil {
		if providersMember.Value.Kind != ObjectKind {
			return nil, fmt.Errorf("providers must be an object, got %s", providersMember.Value.Kind)
		}
		if providersMember.Value.Member(u.ProviderID) == nil {
			needProviderEntry = true
		}
	}

	var edits []edit

	// Insert missing top-level members in one edit so the result is
	// deterministic and both values can share a single comma.
	if needProviders || needModel {
		var parts []string
		if needModel {
			model, err := json.Marshal(u.ProviderID + "/" + u.DefaultModel)
			if err != nil {
				return nil, err
			}
			parts = append(parts, fmt.Sprintf("%q: %s", "model", model))
		}
		if needProviders {
			providers := map[string]any{u.ProviderID: u.Provider}
			providerValue, err := formatValue(providers, rootIndent, nl)
			if err != nil {
				return nil, err
			}
			parts = append(parts, fmt.Sprintf("%q: %s", "providers", providerValue))
		}
		var b strings.Builder
		b.WriteString(nl)
		for i, part := range parts {
			b.WriteString(rootIndent)
			b.WriteString(part)
			if rootEmpty {
				if i < len(parts)-1 {
					b.WriteString(",")
				}
				b.WriteString(nl)
			} else {
				b.WriteString(",")
				if i < len(parts)-1 {
					b.WriteString(nl)
				}
			}
		}
		edits = append(edits, edit{start: root.Start + 1, end: root.Start + 1, text: b.String()})
	}

	// Replace an existing top-level model.
	if setModel && modelMember != nil {
		quoted, err := json.Marshal(u.ProviderID + "/" + u.DefaultModel)
		if err != nil {
			return nil, err
		}
		edits = append(edits, edit{
			start: modelMember.Value.Start,
			end:   modelMember.Value.End,
			text:  string(quoted),
		})
	}

	if providersMember != nil {
		if needProviderEntry {
			obj := providersMember.Value
			keyIndent := lineIndent(src, providersMember.KeyStart)
			value, err := formatValue(u.Provider, keyIndent, nl)
			if err != nil {
				return nil, err
			}
			insertAt, comma, empty := memberInsertPoint(src, obj)
			var b strings.Builder
			if empty {
				b.WriteString(nl)
				b.WriteString(childIndent(src, obj, keyIndent))
				b.WriteString(fmt.Sprintf("%q: %s", u.ProviderID, value))
				b.WriteString(nl)
				b.WriteString(keyIndent)
			} else {
				if !comma {
					b.WriteString(",")
				}
				b.WriteString(nl)
				b.WriteString(childIndent(src, obj, keyIndent))
				b.WriteString(fmt.Sprintf("%q: %s", u.ProviderID, value))
			}
			edits = append(edits, edit{start: insertAt, end: insertAt, text: b.String()})
		} else if m := providersMember.Value.Member(u.ProviderID); m != nil {
			keyIndent := lineIndent(src, m.KeyStart)
			value, err := formatValue(u.Provider, keyIndent, nl)
			if err != nil {
				return nil, err
			}
			edits = append(edits, edit{start: m.Value.Start, end: m.Value.End, text: value})
		}
	}

	out, err := applyEdits(src, edits)
	if err != nil {
		return nil, err
	}
	// Validate the result before handing it back.
	if _, err := Parse(out); err != nil {
		return nil, fmt.Errorf("internal error: edited configuration is invalid: %w", err)
	}
	return out, nil
}

// FindString returns the decoded string at the given object path. Path
// elements address object keys; the final element must point at a string.
func FindString(doc *Document, path ...string) (string, bool) {
	var n *Node = doc.Root
	for _, key := range path {
		if n == nil || n.Kind != ObjectKind {
			return "", false
		}
		m := n.Member(key)
		if m == nil {
			return "", false
		}
		n = m.Value
	}
	if n == nil || n.Kind != StringKind {
		return "", false
	}
	return n.Str, true
}

func formatValue(v any, baseIndent, nl string) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode provider: %w", err)
	}
	s := string(raw)
	if nl != "\n" {
		s = strings.ReplaceAll(s, "\n", nl)
	}
	if baseIndent == "" {
		return s, nil
	}
	return strings.ReplaceAll(s, nl, nl+baseIndent), nil
}

// childIndent finds the indentation of the first member (or item) inside obj.
func childIndent(src []byte, obj *Node, fallback string) string {
	if obj.Kind == ObjectKind && len(obj.Members) > 0 {
		if ind := lineIndent(src, obj.Members[0].KeyStart); ind != "" {
			return ind
		}
	}
	if obj.Kind == ArrayKind && len(obj.Items) > 0 {
		if ind := lineIndent(src, obj.Items[0].Start); ind != "" {
			return ind
		}
	}
	if fallback != "" {
		return fallback + "  "
	}
	return "  "
}

func lineIndent(src []byte, pos int) string {
	if pos > len(src) {
		pos = len(src)
	}
	start := bytes.LastIndexByte(src[:pos], '\n') + 1
	end := start
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return string(src[start:end])
}

// memberInsertPoint returns the offset where a new member can be appended and
// whether a comma already follows the current last member.
func memberInsertPoint(src []byte, obj *Node) (offset int, comma bool, empty bool) {
	if len(obj.Members) == 0 {
		return obj.Start + 1, false, true
	}
	last := obj.Members[len(obj.Members)-1]
	pos := last.Value.End
	// Look past whitespace and comments for an existing comma.
	for pos < obj.End {
		c := src[pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pos++
		case c == '/' && pos+1 < len(src) && src[pos+1] == '/':
			pos += 2
			for pos < len(src) && src[pos] != '\n' {
				pos++
			}
		case c == '/' && pos+1 < len(src) && src[pos+1] == '*':
			pos += 2
			for pos+1 < len(src) && !(src[pos] == '*' && src[pos+1] == '/') {
				pos++
			}
			if pos+1 < len(src) {
				pos += 2
			}
		case c == ',':
			return pos + 1, true, false
		default:
			return last.Value.End, false, false
		}
	}
	return last.Value.End, false, false
}

type edit struct {
	start int
	end   int
	text  string
}

func applyEdits(src []byte, edits []edit) ([]byte, error) {
	if len(edits) == 0 {
		out := make([]byte, len(src))
		copy(out, src)
		return out, nil
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end < edits[j].end
		}
		return edits[i].start > edits[j].start
	})
	chunks := make([][]byte, 0, len(edits)*2+1)
	prevStart := len(src)
	for _, e := range edits {
		if e.start < 0 || e.end < e.start || e.end > len(src) {
			return nil, fmt.Errorf("internal error: edit range [%d,%d) is outside the document", e.start, e.end)
		}
		if e.end > prevStart {
			return nil, fmt.Errorf("internal error: overlapping edits")
		}
		chunks = append(chunks, src[e.end:prevStart], []byte(e.text))
		prevStart = e.start
	}
	chunks = append(chunks, src[:prevStart])
	out := make([]byte, 0, len(src)+256)
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	return out, nil
}
