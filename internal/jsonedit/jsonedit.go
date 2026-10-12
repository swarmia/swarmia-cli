// Package jsonedit rewrites a JSON document after a program has edited it,
// keeping the original file's formatting wherever the content didn't change,
// so that editing someone's settings file produces a diff of just the edit.
//
// Parse decodes a document into plain values (objects map[string]any, arrays
// []any, numbers json.Number) for the caller to edit, and a Layout recording
// how each value was written. Marshal writes the edited values back:
//
//   - a value equal to the one originally there is copied byte for byte;
//   - a changed object keeps its keys' original order, with new keys after
//     them (in the order the caller prefers, then alphabetically);
//   - changed and new values are written one element per line, indented like
//     the original file, without HTML escaping (& stays &, not &).
package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Layout is how a document was formatted.
type Layout struct {
	root    *node
	indent  string
	eol     string // "\n" or "\r\n"
	trailer string // what follows the top-level value
}

// node is one value in the original document: its source text, its decoded
// value, and for objects and arrays, its members'.
type node struct {
	raw    []byte
	value  any
	keys   []string // object keys in source order
	fields map[string]*node
	elems  []*node
}

// Parse decodes data for editing and records its layout. The value is
// independent of the layout: editing it in place doesn't change what Marshal
// compares against.
func Parse(data []byte) (any, *Layout, error) {
	root, err := parseDocument(data)
	if err != nil {
		return nil, nil, err
	}
	edit, err := parseDocument(data) // a second, unshared copy of the values
	if err != nil {
		return nil, nil, err
	}
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	trailer := string(data[len(bytes.TrimRight(data, " \t\r\n")):]) // only whitespace follows the value
	return edit.value, &Layout{root: root, indent: detectIndent(data), eol: eol, trailer: trailer}, nil
}

func parseDocument(data []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseValue(dec, data)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("unexpected data after the top-level value")
	}
	return n, nil
}

func parseValue(dec *json.Decoder, data []byte) (*node, error) {
	// The decoder consumes the separators before a value lazily, with the
	// value's first token.
	start := int(dec.InputOffset())
	for start < len(data) && strings.IndexByte(" \t\r\n,:", data[start]) >= 0 {
		start++
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	n := &node{}
	switch tok {
	case json.Delim('{'):
		obj := map[string]any{}
		n.fields = map[string]*node{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			child, err := parseValue(dec, data)
			if err != nil {
				return nil, err
			}
			if _, dup := n.fields[k]; !dup {
				n.keys = append(n.keys, k)
			}
			n.fields[k], obj[k] = child, child.value // the last duplicate wins, as in json.Unmarshal
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		n.value = obj
	case json.Delim('['):
		arr := []any{}
		for dec.More() {
			child, err := parseValue(dec, data)
			if err != nil {
				return nil, err
			}
			n.elems = append(n.elems, child)
			arr = append(arr, child.value)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		n.value = arr
	default:
		n.value = tok
	}
	n.raw = data[start:dec.InputOffset()]
	return n, nil
}

// detectIndent is the indentation of the first indented line, or two spaces.
func detectIndent(data []byte) string {
	for _, line := range strings.Split(string(data), "\n")[1:] {
		trimmed := strings.TrimLeft(line, " \t\r")
		if trimmed != "" && len(trimmed) < len(line) {
			return line[:len(line)-len(trimmed)]
		}
	}
	return "  "
}

// Marshal writes v as JSON, formatted like the document l was parsed from
// where v didn't change. l may be nil, for a new file. New object keys found
// in keyOrder come first, in that order, then the rest alphabetically.
func Marshal(v any, l *Layout, keyOrder []string) ([]byte, error) {
	if l == nil {
		l = &Layout{indent: "  ", eol: "\n", trailer: "\n"}
	}
	w := writer{indent: l.indent, eol: l.eol, keyOrder: keyOrder}
	if err := w.value(v, l.root, 0); err != nil {
		return nil, err
	}
	w.buf.WriteString(l.trailer)
	return w.buf.Bytes(), nil
}

type writer struct {
	buf      bytes.Buffer
	indent   string
	eol      string
	keyOrder []string
}

// value writes v at depth, where orig (if not nil) was in the original.
func (w *writer) value(v any, orig *node, depth int) error {
	if orig != nil && reflect.DeepEqual(orig.value, v) {
		w.buf.Write(orig.raw)
		return nil
	}
	switch v := v.(type) {
	case map[string]any:
		return w.object(v, orig, depth)
	case []any:
		return w.array(v, orig, depth)
	}
	return w.scalar(v)
}

func (w *writer) object(m map[string]any, orig *node, depth int) error {
	var keys []string
	if orig != nil {
		for _, k := range orig.keys {
			if _, ok := m[k]; ok {
				keys = append(keys, k)
			}
		}
	}
	var added []string
	for k := range m {
		if orig == nil || orig.fields[k] == nil {
			added = append(added, k)
		}
	}
	slices.SortFunc(added, w.compareKeys)
	keys = append(keys, added...)

	if len(keys) == 0 {
		w.buf.WriteString("{}")
		return nil
	}
	w.buf.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			w.buf.WriteString(",")
		}
		w.newline(depth + 1)
		if err := w.scalar(k); err != nil {
			return err
		}
		w.buf.WriteString(": ")
		var child *node
		if orig != nil {
			child = orig.fields[k]
		}
		if err := w.value(m[k], child, depth+1); err != nil {
			return err
		}
	}
	w.newline(depth)
	w.buf.WriteString("}")
	return nil
}

// compareKeys orders new keys: those in keyOrder first, then alphabetically.
func (w *writer) compareKeys(a, b string) int {
	ia, ib := slices.Index(w.keyOrder, a), slices.Index(w.keyOrder, b)
	switch {
	case ia >= 0 && ib >= 0:
		return ia - ib
	case ia >= 0:
		return -1
	case ib >= 0:
		return 1
	}
	return strings.Compare(a, b)
}

func (w *writer) array(arr []any, orig *node, depth int) error {
	if len(arr) == 0 {
		w.buf.WriteString("[]")
		return nil
	}
	used := make([]bool, len(arr))
	if orig != nil {
		used = make([]bool, len(orig.elems))
	}
	w.buf.WriteString("[")
	for i, e := range arr {
		if i > 0 {
			w.buf.WriteString(",")
		}
		w.newline(depth + 1)
		if err := w.value(e, w.match(e, i, orig, used), depth+1); err != nil {
			return err
		}
	}
	w.newline(depth)
	w.buf.WriteString("]")
	return nil
}

// match finds the original of element e at index i: an unused original
// element equal to it (elements before it may have been removed), else the
// one at the same index, whose layout an edited element mostly keeps.
func (w *writer) match(e any, i int, orig *node, used []bool) *node {
	if orig == nil {
		return nil
	}
	for j, o := range orig.elems {
		if !used[j] && reflect.DeepEqual(o.value, e) {
			used[j] = true
			return o
		}
	}
	if i < len(orig.elems) && !used[i] {
		used[i] = true
		return orig.elems[i]
	}
	return nil
}

func (w *writer) scalar(v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	w.buf.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return nil
}

func (w *writer) newline(depth int) {
	w.buf.WriteString(w.eol)
	w.buf.WriteString(strings.Repeat(w.indent, depth))
}
