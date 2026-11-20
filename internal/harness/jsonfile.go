package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/tailscale/hujson"

	"github.com/bluesky-social/diderot/internal/client"
)

// readJSONEntry returns the value at path (object keys from the root) in a
// JSON or JSON-with-comments file, or nil when the file or the key is
// missing.
func readJSONEntry(file string, path ...string) (any, error) {
	data, err := os.ReadFile(file) //nolint:gosec // a harness's own configuration file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	var doc any
	if len(strings.TrimSpace(string(std))) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(std, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	for _, key := range path {
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, nil
		}
		doc = obj[key]
	}
	return doc, nil
}

// writeJSONEntry sets the value at path in file, creating the objects above
// it, and leaves every other byte of the file as it was: comments,
// formatting and other keys survive. A new file is created 0600. When the
// value holds a credential, an existing file other users can read is
// refused rather than written.
func writeJSONEntry(file string, value any, secret bool, path ...string) error {
	data, err := os.ReadFile(file) //nolint:gosec // a harness's own configuration file
	perm := os.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = nil
	case err != nil:
		return err
	default:
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		perm = info.Mode().Perm()
		if secret && perm&0o077 != 0 {
			return fmt.Errorf("%s can be read by other users and would hold the API token; run chmod 600 %s and setup again", file, file)
		}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		doc := value
		for i := len(path) - 1; i >= 0; i-- {
			doc = map[string]any{path[i]: doc}
		}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		return client.WriteFileAtomic(file, append(out, '\n'), perm)
	}
	v, err := hujson.Parse(data)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	// "add" needs its parent: add the first missing object on the path with
	// the rest of the path nested inside it.
	depth := 0
	for depth < len(path)-1 && v.Find(pointer(path[:depth+1])) != nil {
		depth++
	}
	nested := value
	for i := len(path) - 1; i > depth; i-- {
		nested = map[string]any{path[i]: nested}
	}
	patch, err := json.Marshal([]map[string]any{{"op": "add", "path": pointer(path[:depth+1]), "value": nested}})
	if err != nil {
		return err
	}
	if err := v.Patch(patch); err != nil {
		return fmt.Errorf("update %s: %w", file, err)
	}
	indentMember(&v, path[:depth], path[depth], nested)
	return client.WriteFileAtomic(file, v.Pack(), perm)
}

// indentMember lays the patched member out like its siblings: on its own
// line at their indentation, its value indented one more step. A patch
// inserts it compactly; in a file written on one line it stays that way.
func indentMember(root *hujson.Value, parentPath []string, key string, value any) {
	parent := root.Find(pointer(parentPath))
	if parent == nil {
		return
	}
	obj, ok := parent.Value.(*hujson.Object)
	if !ok {
		return
	}
	idx := -1
	for i, m := range obj.Members {
		if name, ok := m.Name.Value.(hujson.Literal); ok && name.String() == key {
			idx = i
		}
	}
	if idx < 0 {
		return
	}
	member := &obj.Members[idx]
	if !bytes.Contains(member.Name.BeforeExtra, []byte("\n")) {
		var sibling hujson.Extra
		for i, m := range obj.Members {
			if i != idx && bytes.Contains(m.Name.BeforeExtra, []byte("\n")) {
				sibling = m.Name.BeforeExtra
				break
			}
		}
		if sibling == nil {
			return
		}
		member.Name.BeforeExtra = append(hujson.Extra("\n"), afterNewline(sibling)...)
	}
	indent := afterNewline(member.Name.BeforeExtra)
	// The closing brace's indentation is in the extra before it: the
	// object's own, after a trailing comma, or else the last value's.
	closing := afterNewline(obj.AfterExtra)
	if last := obj.Members[len(obj.Members)-1].Value.AfterExtra; bytes.Contains(last, []byte("\n")) {
		closing = afterNewline(last)
	}
	unit := "  "
	if step, ok := strings.CutPrefix(indent, closing); ok && step != "" && strings.TrimSpace(step) == "" {
		unit = step
	}
	b, err := json.MarshalIndent(value, indent, unit)
	if err != nil {
		return
	}
	formatted, err := hujson.Parse(b)
	if err != nil {
		return
	}
	formatted.BeforeExtra = hujson.Extra(" ")
	formatted.AfterExtra = member.Value.AfterExtra
	member.Value = formatted
}

// afterNewline is the text after the last newline: a line's indentation.
func afterNewline(b []byte) string {
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return string(b[i+1:])
	}
	return ""
}

// pointer is the JSON pointer (RFC 6901) for keys.
func pointer(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(k))
	}
	return b.String()
}
