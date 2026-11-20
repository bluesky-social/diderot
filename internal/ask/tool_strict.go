package ask

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// The Responses API reads a function tool whose "strict" field is absent as
// strict whenever it can rewrite the schema to fit, and that rewrite marks
// every property required. A filter the schema calls optional then has to be
// given a value on every call, and the model invents one: a start date, a
// time window, a page size. The Messages API has no such default; there a
// property left out of "required" may be left out of the call.
//
// So on the Responses protocol Diderot states the contract itself. A tool
// whose schema strict mode can express is sent strict, with each optional
// property made nullable: the model writes null for "no value", and the
// arguments are still checked against the schema. Any other tool is sent
// with "strict": false, where optional properties may simply be left out.
// dropUnsetArguments then removes the nulls, so a tool sees an unset
// property as absent on either protocol.
//
// A strict tool the API does not accept fails the whole request, so the
// conversion takes only the plain schemas it is sure of and leaves the rest
// non-strict, and the transport falls back to non-strict if the API still
// refuses one.

// strictSchemaKeywords are the JSON Schema keywords a strict function tool
// may carry besides the structural ones strictSchemaNode handles itself.
// OpenAI documents most of them as supported; default, title, examples,
// const, $schema, minLength and maxLength are undocumented and were accepted
// by the API when tried. A keyword not listed here (anyOf, oneOf, allOf,
// $ref, pattern, uniqueItems, patternProperties and the rest) keeps its tool
// non-strict.
var strictSchemaKeywords = map[string]struct{}{
	"$schema": {}, "const": {}, "default": {}, "description": {}, "enum": {},
	"examples": {}, "exclusiveMaximum": {}, "exclusiveMinimum": {},
	"maxItems": {}, "maxLength": {}, "maximum": {}, "minItems": {},
	"minLength": {}, "minimum": {}, "multipleOf": {}, "title": {},
}

// strictStringFormats are the string formats strict mode accepts. Any other
// format (uri, for one) is rejected there.
var strictStringFormats = map[string]struct{}{
	"date": {}, "date-time": {}, "duration": {}, "email": {}, "hostname": {},
	"ipv4": {}, "ipv6": {}, "time": {}, "uuid": {},
}

// strictMaxDepth is the nesting strict mode allows.
const strictMaxDepth = 10

// strictParameters rewrites a tool's parameter schema into the form strict
// mode requires: every object closed to other properties, every property
// listed as required, and each property the schema left optional given null
// as a second type. It reports false, and the tool stays non-strict, when
// the schema is anything but plain: an unsupported keyword, a union, an
// object with no declared properties, a list with no item schema, a schema
// with no single type, an optional property that cannot be opened to null.
func strictParameters(parameters map[string]any) (map[string]any, bool) {
	if declared, _ := schemaType(parameters); declared != "object" {
		return nil, false
	}
	return strictSchemaNode(parameters, 1)
}

func strictSchemaNode(node map[string]any, depth int) (map[string]any, bool) {
	if depth > strictMaxDepth {
		return nil, false
	}
	declared, ok := schemaType(node)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(node)+2)
	for key, value := range node {
		switch key {
		case "type":
			out[key] = value
		case "properties", "required", "additionalProperties", "items":
			// Structural: rebuilt below.
		case "format":
			format, _ := value.(string)
			if _, ok := strictStringFormats[format]; !ok {
				return nil, false
			}
			out[key] = value
		default:
			if _, ok := strictSchemaKeywords[key]; !ok {
				return nil, false
			}
			out[key] = value
		}
	}

	items, hasItems := node["items"]
	if declared == "array" != hasItems {
		// A list must say what it holds, and only a list may.
		return nil, false
	}
	if !requirementsReadable(node) {
		return nil, false
	}
	if hasItems {
		itemNode, ok := items.(map[string]any)
		if !ok {
			return nil, false
		}
		strictItems, ok := strictSchemaNode(itemNode, depth+1)
		if !ok {
			return nil, false
		}
		out["items"] = strictItems
	}

	if declared != "object" {
		_, hasProperties := node["properties"]
		_, hasRequired := node["required"]
		_, hasAdditional := node["additionalProperties"]
		return out, !hasProperties && !hasRequired && !hasAdditional
	}
	properties, ok := node["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		// An object that declares no properties takes any, which strict
		// mode has no way to say.
		return nil, false
	}
	if additional, has := node["additionalProperties"]; has {
		if open, isBool := additional.(bool); !isBool || open {
			return nil, false
		}
	}
	required, ok := schemaStringSet(node["required"])
	if !ok {
		return nil, false
	}
	for name := range required {
		if _, declared := properties[name]; !declared {
			return nil, false
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	strictProperties := make(map[string]any, len(properties))
	allRequired := make([]any, len(names))
	for i, name := range names {
		propertyNode, ok := properties[name].(map[string]any)
		if !ok {
			return nil, false
		}
		strictProperty, ok := strictSchemaNode(propertyNode, depth+1)
		if !ok {
			return nil, false
		}
		if _, isRequired := required[name]; !isRequired {
			if strictProperty, ok = nullableSchema(strictProperty); !ok {
				return nil, false
			}
		}
		strictProperties[name] = strictProperty
		allRequired[i] = name
	}
	out["properties"] = strictProperties
	out["required"] = allRequired
	out["additionalProperties"] = false
	return out, true
}

// nullableSchema adds null to what a schema accepts. An enum gains null too:
// without it the only values the model may write are the listed ones, and an
// optional choice becomes a forced one. A const cannot be opened to null and
// reports false.
func nullableSchema(node map[string]any) (map[string]any, bool) {
	if _, fixed := node["const"]; fixed {
		return nil, false
	}
	declared, ok := schemaType(node)
	if !ok {
		return nil, false
	}
	if declared != "null" {
		node["type"] = []any{declared, "null"}
	}
	if values, isEnum := node["enum"].([]any); isEnum {
		hasNull := false
		for _, value := range values {
			if value == nil {
				hasNull = true
			}
		}
		if !hasNull {
			node["enum"] = append(append(make([]any, 0, len(values)+1), values...), nil)
		}
	}
	return node, true
}

// schemaType returns the one type a schema declares: "string" for
// "type": "string" and for "type": ["string", "null"]. It reports false for
// a schema with no type or with a wider union, which strictSchemaNode does
// not take on.
func schemaType(node map[string]any) (string, bool) {
	switch declared := node["type"].(type) {
	case string:
		return declared, declared != ""
	case []any:
		name := ""
		for _, item := range declared {
			text, ok := item.(string)
			if !ok || text == "" {
				return "", false
			}
			if text == "null" {
				continue
			}
			if name != "" && name != text {
				return "", false
			}
			name = text
		}
		if name == "" {
			name = "null"
		}
		return name, len(declared) > 0 && len(declared) <= 2
	}
	return "", false
}

// schemaStringSet reads a schema's "required" list. A missing list is an
// empty set; a list of anything but strings is not a schema this code reads.
func schemaStringSet(value any) (map[string]struct{}, bool) {
	if value == nil {
		return map[string]struct{}{}, true
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, false
		}
		set[name] = struct{}{}
	}
	return set, true
}

// strictToolsTransport states each function tool's strictness on a Responses
// request before it is sent. The Eino Responses adapter builds the tool list
// and has no field for it.
type strictToolsTransport struct {
	base http.RoundTripper
	// refused remembers tool lists the API would not take as strict, by
	// digest, so that each is refused once and sent non-strict from then on.
	refused sync.Map
}

// strictSchemaRefusal is how the API begins its complaint about a strict
// tool's schema.
const strictSchemaRefusal = "Invalid schema for function"

func (t *strictToolsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Method != http.MethodPost || req.Body == nil || !strings.HasSuffix(req.URL.Path, "/responses") {
		return base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	send := func(body []byte) (*http.Response, error) {
		// A RoundTripper must not change the request it was given.
		sent := req.Clone(req.Context())
		sent.Body = io.NopCloser(bytes.NewReader(body))
		sent.ContentLength = int64(len(body))
		sent.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		return base.RoundTrip(sent)
	}

	stated, strictTools := withStatedToolStrictness(body, true)
	if strictTools == nil {
		return send(stated)
	}
	digest := sha256.Sum256(strictTools)
	if _, refused := t.refused.Load(digest); refused {
		loose, _ := withStatedToolStrictness(body, false)
		return send(loose)
	}
	resp, err := send(stated)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		return resp, err
	}
	// The conversion takes only schemas the API is known to accept, but the
	// API's rules are its own and have changed before. If it refuses one,
	// the turn is worth more than the validation: send the tools non-strict.
	complaint, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if readErr != nil || !bytes.Contains(complaint, []byte(strictSchemaRefusal)) {
		resp.Body = io.NopCloser(bytes.NewReader(complaint))
		return resp, nil
	}
	t.refused.Store(digest, struct{}{})
	slog.Default().Warn("ask: the model API refused a strict tool schema; these tools are sent non-strict from now on",
		"response", string(complaint[:min(len(complaint), 600)]))
	loose, _ := withStatedToolStrictness(body, false)
	return send(loose)
}

// withStatedToolStrictness returns a Responses request body in which every
// function tool says whether it is strict. With allowStrict a tool
// strictParameters can convert is sent strict with its schema rewritten;
// every other tool, and every tool without allowStrict, is sent
// "strict": false with its schema as written. A tool that already says is
// left alone, as is a body this code cannot read. The second result is the
// tool list as sent when it holds a tool this call made strict, and nil
// otherwise.
func withStatedToolStrictness(body []byte, allowStrict bool) ([]byte, []byte) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return body, nil
	}
	rawTools, ok := request["tools"]
	if !ok {
		return body, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(rawTools))
	decoder.UseNumber()
	var tools []any
	if err := decoder.Decode(&tools); err != nil {
		return body, nil
	}
	changed, anyStrict := false, false
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok || tool["type"] != "function" {
			continue
		}
		if stated, has := tool["strict"]; has && stated != nil {
			continue
		}
		parameters, _ := tool["parameters"].(map[string]any)
		strict, convertible := map[string]any(nil), false
		if allowStrict {
			strict, convertible = strictParameters(parameters)
		}
		if convertible {
			tool["parameters"] = strict
			anyStrict = true
		}
		tool["strict"] = convertible
		changed = true
	}
	if !changed {
		return body, nil
	}
	encodedTools, err := encodeJSON(tools)
	if err != nil {
		return body, nil
	}
	request["tools"] = encodedTools
	encoded, err := encodeJSON(request)
	if err != nil {
		return body, nil
	}
	if !anyStrict {
		return encoded, nil
	}
	return encoded, encodedTools
}

// encodeJSON marshals without escaping <, > and &, so text the request
// already carried is sent as it was.
func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// dropUnsetArguments removes from a tool call's arguments every property
// whose value is null and which the tool's schema declares and does not
// require, so that "null" from a strict tool and "left out" from a
// non-strict one reach the tool as the same thing: no value. Everything else
// is passed on as it came: a required property keeps its null, for the tool
// to reject as it always has; so does a property the schema does not
// declare, and anything under a schema this code cannot read in full (see
// requirementsReadable). Arguments that are not a JSON object are returned
// unchanged.
func dropUnsetArguments(arguments string, parameters map[string]any) string {
	if !strings.Contains(arguments, "null") {
		return arguments
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return arguments
	}
	// Anything after the object makes the arguments malformed, which is the
	// tool's decoder's to report, not this code's to repair.
	if _, err := decoder.Token(); err != io.EOF {
		return arguments
	}
	object, ok := decoded.(map[string]any)
	if !ok || !dropUnsetValues(object, parameters) {
		return arguments
	}
	encoded, err := encodeJSON(object)
	if err != nil {
		return arguments
	}
	return string(encoded)
}

// plainSchemaKeywords are, with strictSchemaKeywords, the keywords that say
// nothing about which properties an object must have beyond its own
// "required" list.
var plainSchemaKeywords = map[string]struct{}{
	"$comment": {}, "$id": {}, "additionalProperties": {}, "deprecated": {},
	"format": {}, "items": {}, "pattern": {}, "properties": {}, "readOnly": {},
	"required": {}, "type": {}, "writeOnly": {},
}

// requirementsReadable reports whether "required" is all a schema says about
// which properties must be present. Any keyword outside the two lists might
// say more (a reference, a combinator, dependencies, minProperties), or be
// broken by a removal (uniqueItems: two items that differed only by a null
// become the same), and an enum or a const on an object or a list fixes
// whole values.
// A schema carrying one is left alone. strictSchemaNode converts only
// schemas that pass, which is what guarantees that a null the strict form
// invited is always removed.
func requirementsReadable(schema map[string]any) bool {
	_, hasProperties := schema["properties"]
	_, hasItems := schema["items"]
	if hasProperties || hasItems {
		for _, fixing := range []string{"enum", "const"} {
			if _, fixed := schema[fixing]; fixed {
				return false
			}
		}
	}
	for keyword := range schema {
		if _, plain := plainSchemaKeywords[keyword]; plain {
			continue
		}
		if _, strict := strictSchemaKeywords[keyword]; !strict {
			return false
		}
	}
	return true
}

// dropUnsetValues walks a value beside its schema, through objects and
// lists to any depth, and deletes each null property the schema declares
// and does not require. It reports whether it deleted anything.
func dropUnsetValues(value any, schema map[string]any) bool {
	if !requirementsReadable(schema) {
		return false
	}
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		required, ok := schemaStringSet(schema["required"])
		if !ok {
			return false
		}
		for name, propertyValue := range typed {
			propertySchema, declared := properties[name].(map[string]any)
			if !declared {
				continue
			}
			if propertyValue == nil {
				if _, isRequired := required[name]; !isRequired {
					delete(typed, name)
					changed = true
				}
				continue
			}
			if dropUnsetValues(propertyValue, propertySchema) {
				changed = true
			}
		}
	case []any:
		itemSchema, described := schema["items"].(map[string]any)
		if !described {
			return false
		}
		for _, item := range typed {
			if dropUnsetValues(item, itemSchema) {
				changed = true
			}
		}
	}
	return changed
}
