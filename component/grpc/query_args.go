package memql

import (
	"encoding/json"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// renderQueryArgs turns a flat JSON object of call arguments into the
// NAMED-ARGUMENT list a MemQL call takes -- `key: "val", key2: 3` -- for the
// caller to place directly inside the parens: `name(` + renderQueryArgs(b) +
// `)`. Keys are emitted in sorted order so the rendered statement is
// deterministic (stable logs + testability); string values go through the
// parser's own QuoteString so the escape grammar is the lexer's, never Go's
// %q or raw JSON; nested objects render with bare keys, lists as lists, and
// the remaining scalars (numbers, booleans, null) as the JSON spelling MemQL
// literals share. An empty or unparseable object renders "", which is the
// empty call `name()`.
//
// It used to wrap the list in braces, rendering `name({k: v})` -- the legacy
// object-literal argument wrapper the parser REJECTS since Story 9 of
// memql#2335 ("object-literal call args are removed; pass named args
// directly"). Every caller had been failing at parse in production,
// invisibly: the handler tests drive a fake engine that records query strings
// and parses nothing (memql#4256). The DB-free guard that runs every caller's
// rendered statement through the real parser is render_query_args_parse_test.go,
// the same shape as deploy_control_parse_test.go, which found the identical
// defect in component/deploycontrol (memql#4209).
//
// It lived in the guest-invitation handlers, which were its first caller and
// went with the space concept (epic memql#4988); the auth-session handlers use
// it and the parse guard still gates it.
func renderQueryArgs(argsJSON []byte) string {
	var m map[string]any
	if err := json.Unmarshal(argsJSON, &m); err != nil || len(m) == 0 {
		return ""
	}
	return renderNamedArgs(m)
}

// renderNamedArgs renders one object level as `k: v, k2: v2` with sorted keys.
func renderNamedArgs(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(renderQueryValue(m[k]))
	}
	return b.String()
}

// renderQueryValue renders one argument value as a MemQL literal.
func renderQueryValue(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case map[string]any:
		return "{" + renderNamedArgs(t) + "}"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = renderQueryValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		vb, err := json.Marshal(t)
		if err != nil {
			return "null"
		}
		return string(vb)
	}
}
