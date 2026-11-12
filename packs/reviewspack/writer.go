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
// internal origin for exactly that one call.
//
// # The stamp skips the row gate, so the builtin's check is the gate
//
// A write at internal origin is not owner-checked
// (component/memql/rowauthz_write_guard.go). The caller's actor still runs the
// mutation and stamps what it stamps, but nothing in the engine asks whether
// this caller may write this row. So the check of who may write is the pack's
// own, made BEFORE the stamp, in each builtin: the part's organization gate,
// the reads it makes under the caller, and its own refusals. That is why the
// stamp is put on one call, inline as the argument to its Execute, and never
// carried in a context (server_written_db_test.go,
// TestEachWritingBuiltinRefusesBeforeItWritesAndWritesNothing).
//
// # The only way in for a client
//
// Every concept the pack declares is @serverWritten (dsl/concepts.memql): a
// write of any of its rows without internal origin -- the public insert()
// included, which never consults a mutation's @serverOnly -- is refused by the
// engine. So no client writes them except through a builtin that judged it.
//
// Not the only writer in the engine. @serverWritten closes every way a CLIENT
// can reach these rows. The engine's own code still can, and four writers of
// them exist outside this file: a step of an automation in the loaded DSL tree
// (it runs with internal origin, for any store); a DSL seed, materialized as
// the system actor; the identity integration's ownership transfer
// (transferRowOwnership, a cluster owner's act), which rewrites a row's owner;
// and this pack's erasure (erase.go), which rewrites stored versions by direct
// SQL.

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
