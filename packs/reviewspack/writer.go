package reviewspack

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// writer.go -- HOW THIS PACK PERSISTS.
//
// A BUILTIN COMPUTES; IT DOES NOT WRITE. The engine returns a builtin
// handler's nodes as the expression's VALUE and persists nothing, so a
// capability whose write was "the node it returned" reports success, writes
// nothing, and reads back empty -- which is what this pack's first builtins
// did, and what the wholesale pack's live suite found before it. So: THE
// BUILTIN DECIDES, THE MUTATION WRITES. Each capability makes the cross-row
// reads a mutation body cannot, judges the write, and then runs one of the
// pack's own mutations.
//
// # The write mutations are @serverOnly
//
// They are reached ONLY from the builtins that judge them. A decision written
// straight through its mutation would skip the checks that make it a decision
// -- above all that the cluster's own operators never record one -- so a
// client-originated call to it is refused by the engine. The builtin stamps
// internal origin for exactly that one call, and the CALLER'S OWN ACTOR still
// runs it, so the row gate still decides whether this caller may write this
// row.

type rowWriter interface {
	Write(ctx context.Context, mutation string, args map[string]any) error
}

// engineWriter is the production implementation.
type engineWriter struct {
	engine memql.IntegrationEngineAccess
}

func (w *engineWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	if w == nil || w.engine == nil {
		return fmt.Errorf("reviews: the pack has no engine handle, so %q cannot be written", mutation)
	}
	// INLINE, so the internal stamp dies with this one call (auth.ContextWithInternalOrigin).
	if _, err := w.engine.Execute(auth.ContextWithInternalOrigin(ctx), renderMutationCall(mutation, args)); err != nil {
		return fmt.Errorf("reviews: %s: %w", mutation, err)
	}
	return nil
}

// renderMutationCall writes the invocation a pack mutation becomes: NO
// keyword, which is the mutation form (`builtin ` and `query ` are the other
// two), arguments in a stable order.
func renderMutationCall(mutation string, args map[string]any) string {
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(mutation)
	b.WriteByte('(')
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(renderLiteral(args[k]))
	}
	b.WriteByte(')')
	return b.String()
}

// renderLiteral renders one argument with the engine's own string escaping,
// never Go's: review text is shopper input.
func renderLiteral(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case int:
		return strconv.Itoa(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []string:
		parts := make([]string, 0, len(t))
		for _, s := range t {
			parts = append(parts, langparser.QuoteString(s))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// An object literal, keys in a stable order, every key and value
		// through the engine's own escaping.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, langparser.QuoteString(k)+": "+renderLiteral(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return langparser.QuoteString(asString(v))
	}
}

// slugOf renders an identifier as a bare slug a derived row id may carry:
// the engine refuses a row id with a colon in it, and a store id may itself
// be a v1:-prefixed row id.
func slugOf(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
