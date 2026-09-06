package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"context"
)

// MemQLExecutor runs raw MemQL queries. The HTTP handlers hold this rather
// than the concrete engine so they can be tested against a fake.
type MemQLExecutor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// dslCall renders a named construct invocation with its arguments.
//
// Arguments are sorted by name so the rendered call is deterministic -- two
// handlers building the same call produce the same string, which is what
// makes a recorded-call test assert something. Values are JSON-encoded, which
// is the engine's own literal syntax for scalars, lists and objects alike.
func dslCall(fn string, args map[string]any) (string, error) {
	if len(args) == 0 {
		return fn + "()", nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fn)
	b.WriteByte('(')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v, err := json.Marshal(args[k])
		if err != nil {
			return "", fmt.Errorf("marshal %s arg %q: %w", fn, k, err)
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.Write(v)
	}
	b.WriteByte(')')
	return b.String(), nil
}
