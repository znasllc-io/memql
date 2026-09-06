package parser

// render.go -- the ONE renderer for a MemQL invocation composed in Go.
//
// WHY THIS IS HERE RATHER THAN IN EACH CALLER. Go code that has to hand the
// engine a call composes it as text, and there is exactly one accepted form:
// named arguments directly in the parens, `name(k: v, ...)`, empty as
// `name()`. The legacy wrapper `name({...})` -- a single positional object
// literal -- has been REFUSED at parse since Story 9 of memql#2335, and
// parseFunctionCallWithKind rejects it by name.
//
// That refusal is silent in the worst way. The rendered string is well-formed
// Go, the caller logs a warning at a level nobody watches, and every suite
// that drives a recording engine stays green because a recorder parses
// nothing. It has now shipped THREE times: component/deploycontrol
// (memql#4209), the guest-invitation and auth-session handlers (memql#4256),
// and all eight writes in component/worker (memql#5004) -- registration,
// heartbeat, revoke, invocation and the three app-session mutations, i.e. the
// whole Fleet surface, refused at parse on every cluster while its tests were
// green.
//
// Rendering the form is not the hard part; KNOWING which form is current is.
// So the renderer lives beside QuoteString, in the package that owns the
// grammar, and new Go that composes a call should call this rather than add a
// tenth private copy. The copies that predate it (component/proving,
// component/server/{fileversion,uploadsession}, integrations/release,
// integrations/shopify, component/memql, component/workjournal,
// component/safety/approval) already render the named form correctly and are
// left alone deliberately: their error and sorting contracts differ, and
// rewriting eight correct renderers is a change with risk and no defect
// behind it.
//
// WHAT THIS DOES NOT DO. It renders SYNTAX. It does not know whether `fn`
// exists, whether the argument names are declared, or whether the values fit
// their types -- a call may render, parse and still name nothing. The gate for
// that is a per-package test driving the real methods against a recording
// executor and resolving what they produced through a real engine; see
// component/worker/render_parses_test.go and component/packages/render_parse_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RenderCall renders `fn(k: v, ...)` with arguments in a stable order and
// every value JSON-encoded, so a value carrying a quote, a newline or a
// backslash can never break out of its literal and change the statement
// around it.
//
// Keys are SORTED. The engine does not care, but a caller's map iteration
// order is random, and a statement that differs run to run cannot be asserted
// on, diffed in a log, or hashed.
//
// An empty map renders `fn()`, which is the empty form the parser accepts --
// never `fn({})`, which is the removed wrapper's empty case and is rejected
// with the same error.
func RenderCall(fn string, args map[string]any) (string, error) {
	name := strings.TrimSpace(fn)
	if name == "" {
		return "", fmt.Errorf("parser.RenderCall: empty function name")
	}
	if len(args) == 0 {
		return name + "()", nil
	}

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v, err := renderValue(args[k])
		if err != nil {
			return "", fmt.Errorf("parser.RenderCall: %s argument %q: %w", name, k, err)
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
	}
	b.WriteByte(')')
	return b.String(), nil
}

// renderValue encodes one argument value.
//
// HTML escaping is OFF, matching QuoteString. encoding/json escapes `<`, `>`
// and `&` into <-style sequences by default -- a browser-safety default
// that has no meaning here and turns readable text into an unreadable
// statement in every log line and every test failure that prints one.
func renderValue(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	// Encode appends a newline Marshal does not; a value inside a call must
	// not carry one.
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
