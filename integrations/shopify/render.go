package shopify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// parseExpression is the front end's own expression parser, re-exported for
// the call-shape gate. Named here rather than imported at the test site so
// the production package carries the dependency the gate is about.
func parseExpression(s string) (any, error) { return langparser.ParseExpression(s) }

// render.go -- turning a fetched object into a MemQL call.
//
// The connector's only write channel is engine.Execute, which takes MemQL
// text, so every mapped value has to become a literal. Getting that wrong is
// not a compile error: a malformed literal is a PARSE failure at execute
// time, on a live delivery, in a path whose unit tests can pass without ever
// parsing anything (memql#4256 is the same defect one domain over). So the
// rules are narrow and the renderer is shared.

// renderCall builds `name(k: v, ...)`, arguments in sorted order so a failed
// call logs identically twice and a diff between two attempts is meaningful.
func renderCall(name string, args map[string]any) string {
	if len(args) == 0 {
		return name + "()"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+renderValue(args[k]))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// renderValue emits one value as a MemQL literal.
//
// OBJECT KEYS ARE ALWAYS QUOTED, and that is the rule worth stating. A
// mirrored payload's keys come from Shopify, not from an author: a metafield
// is keyed "namespace.key", an attribute key can be anything a merchant typed,
// and a nested object can carry a field named `default` or `return`. Every one
// of those is a bare-identifier parse failure and a quoted-string success, so
// the renderer never asks whether a key happens to be safe.
func renderValue(v any) string {
	var b strings.Builder
	writeValue(&b, v)
	return b.String()
}

func writeValue(b *strings.Builder, v any) {
	switch val := v.(type) {
	case nil:
		b.WriteString("null")
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(jsonString(k))
			b.WriteString(": ")
			writeValue(b, val[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				b.WriteString(", ")
			}
			writeValue(b, item)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(jsonString(item))
		}
		b.WriteByte(']')
	case json.Number:
		b.WriteString(val.String())
	case string:
		b.WriteString(jsonString(val))
	default:
		// Scalars lean on encoding/json: MemQL accepts the same literal
		// shapes for numbers and booleans, and json.Marshal is what makes
		// a float render without a locale or an exponent surprise.
		out, err := json.Marshal(v)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(out)
	}
}

// extractPath reads a value out of a fetched object through the small path
// language the generated model records.
//
//	"field"               the field itself
//	"field.id"            an object's key
//	"field[].id"          each element of an array of objects
//	"field.nodes[].id"    each node of a connection
//	"field.nodes[]"       the connection's nodes, whole
//
// A missing step yields nil rather than an error: Shopify legitimately
// returns null for a field an unapproved scope cannot see, and the caller's
// job is to write null, not to fail.
func extractPath(obj map[string]any, path string) any {
	if obj == nil || path == "" {
		return nil
	}
	var cur any = obj
	for _, step := range strings.Split(path, ".") {
		if cur == nil {
			return nil
		}
		explode := strings.HasSuffix(step, "[]")
		key := strings.TrimSuffix(step, "[]")
		if key != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		if !explode {
			continue
		}
		items, ok := cur.([]any)
		if !ok {
			return nil
		}
		// The remainder of the path applies to EACH element. Recursing on
		// the rest is what makes "field.nodes[].id" mean a list of ids
		// rather than the id of a list.
		rest := strings.TrimPrefix(strings.TrimPrefix(path, step), ".")
		if idx := strings.Index(path, step); idx >= 0 {
			rest = strings.TrimPrefix(path[idx+len(step):], ".")
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			if rest == "" {
				out = append(out, item)
				continue
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if v := extractPath(m, rest); v != nil {
				out = append(out, v)
			}
		}
		return out
	}
	return cur
}

// stringsOf coerces an extracted value into a []string, dropping anything
// that is not one. Used for GID lists, where a non-string element means the
// selection changed shape and the safe answer is to carry the ids we can
// read rather than to fail the whole row.
func stringsOf(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// metafieldMap folds a metafield connection's nodes into the
// "namespace.key" -> {type, value, jsonValue} shape spec 4.2 asks for.
func metafieldMap(v any) map[string]any {
	items, ok := v.([]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ns, _ := m["namespace"].(string)
		key, _ := m["key"].(string)
		if ns == "" || key == "" {
			continue
		}
		entry := map[string]any{}
		for _, f := range []string{"type", "value", "jsonValue"} {
			if val, present := m[f]; present && val != nil {
				entry[f] = val
			}
		}
		out[ns+"."+key] = entry
	}
	return out
}

// firstString reads the first non-empty string at any of the given keys.
// Webhook payloads spell the same id three ways depending on the topic's
// age, and a connector that knew only one of them would silently ignore
// every delivery of the others.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		case json.Number:
			return v.String()
		}
	}
	return ""
}
