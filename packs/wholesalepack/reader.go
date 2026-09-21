package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// reader.go -- THE NARROW ROW READ THE PACK'S GO HALF DECIDES OVER.
//
// AN INTERFACE RATHER THAN THE ENGINE, and the reason is testability of the
// parts that matter: the state fold, the transition legality check and the
// applications-open gate are decisions over ROWS, and a test that has to
// build an *ExecuteResult to exercise them is testing the engine's envelope
// instead. Behind this seam they are functions over values.
//
// This is packs/reviewspack/published.go's seam, kept deliberately
// identical rather than factored into a shared helper. A shared one would
// have to be in component/memql, which would make a convenience for two
// packs into a part of the engine's plugin contract -- and the contract is
// the thing a third pack has to live with for ever. Two forty-line
// implementations that can diverge are the cheaper mistake.

// rowReader is the read the pack's Go half needs.
type rowReader interface {
	Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error)
	// RowsWithList is Rows plus ONE list-valued argument, which the state
	// fold over a page of applications needs and no other read here does.
	RowsWithList(ctx context.Context, query string, args map[string]string,
		listName string, list []string) ([]map[string]any, error)
}

// engineRowReader is the production implementation.
type engineRowReader struct {
	engine memql.IntegrationEngineAccess
}

func (e *engineRowReader) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	return e.rows(ctx, query, args, "", nil)
}

func (e *engineRowReader) RowsWithList(ctx context.Context, query string, args map[string]string,
	listName string, list []string) ([]map[string]any, error) {
	return e.rows(ctx, query, args, listName, list)
}

func (e *engineRowReader) rows(ctx context.Context, query string, args map[string]string,
	listName string, list []string) ([]map[string]any, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("wholesale: the pack has no engine handle")
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("query ")
	b.WriteString(query)
	b.WriteByte('(')
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		// QuoteString, never Go quoting: it is the engine's own literal
		// escaping, and a company name is caller-supplied text.
		b.WriteString(langparser.QuoteString(args[k]))
	}
	if listName != "" {
		if len(names) > 0 {
			b.WriteString(", ")
		}
		b.WriteString(listName)
		b.WriteString(": [")
		for i, v := range list {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(langparser.QuoteString(v))
		}
		b.WriteString("]")
	}
	b.WriteByte(')')

	result, err := e.engine.Execute(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("wholesale: %s: %w", query, err)
	}
	return memql.MaterializeRows(result), nil
}

// rowsFor runs one of the pack's own queries through the seam.
func (p *Provider) rowsFor(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if p.reader == nil {
		return nil, fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	return p.reader.Rows(ctx, query, args)
}

// asString narrows a payload value to text.
func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// trimmed is asString plus the trim every caller here wants.
func trimmed(v any) string { return strings.TrimSpace(asString(v)) }

// Writer is how this pack PERSISTS. It renders one of the pack's own
// mutations and runs it under the caller's own actor.
//
// SEPARATE FROM Caller BECAUSE THE CALL FORMS DIFFER AND THE DIFFERENCE IS
// EASY TO LOSE: a mutation is `name(args)`, a builtin is `builtin
// name(args)`. Separate from rowReader because a write is not a read and a
// test that fakes one should not silently fake the other.
type rowWriter interface {
	Write(ctx context.Context, mutation string, args map[string]any) error
}

// engineWriter is the production implementation.
type engineWriter struct {
	engine memql.IntegrationEngineAccess
}

func (w *engineWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	if w == nil || w.engine == nil {
		return fmt.Errorf("wholesale: the pack has no engine handle, so %q cannot be written", mutation)
	}
	if _, err := w.engine.Execute(ctx, renderMutationCall(mutation, args)); err != nil {
		return fmt.Errorf("wholesale: %s: %w", mutation, err)
	}
	return nil
}

// renderMutationCall writes the invocation a pack mutation becomes.
//
// NO KEYWORD, which is the mutation form -- `builtin ` and `query ` are the
// other two and both would be wrong here. Named and asserted for the same
// reason renderBuiltinCall is: the engine is the only thing that can tell
// these apart, and a fake writer cannot.
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

// renderLiteral writes one argument as MemQL source.
//
// QuoteString FOR EVERY STRING, never Go quoting: it is the engine's own
// literal escaping, and a company name is caller-supplied text that reaches
// this from a public form.
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
	default:
		return langparser.QuoteString(asString(v))
	}
}

// slugOf renders any identifier as a BARE SLUG a derived row id may carry.
//
// THE ENGINE REFUSES A ROW ID WITH COLONS IN IT: a shortId "must be a bare
// slug/UUID (no colons) or the concept-prefixed form" (see
// docs/public/concepts/identifiers.md). A store id may itself be a
// v1:-prefixed row id, so composing one into a derived id without this
// produces an id every write refuses -- which the live e2e suite found and
// no fixture could, because a fixture never validates an id.
func slugOf(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// write runs one of the pack's own mutations through the seam.
func (p *Provider) write(ctx context.Context, mutation string, args map[string]any) error {
	if p.writer == nil {
		return fmt.Errorf("wholesale: the pack has no writer; %q cannot be persisted", mutation)
	}
	return p.writer.Write(ctx, mutation, args)
}

// confirm is the ONE node a write capability replies with.
//
// A RECEIPT, NOT THE ROW. The row is the mutation's, and echoing a copy of
// it here would be a second, unversioned answer to "what was written" that
// nothing keeps in step. What a caller needs back is that it happened and
// against what, so that is what this carries.
func confirm(kind string, fields map[string]any) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("wholesale: marshal %s: %w", kind, err)
	}
	return []memorynodes.MemoryNode{{
		ID:        kind + ":" + strconv.FormatInt(time.Now().UTC().UnixNano(), 36),
		Concept:   "v1:" + strings.ReplaceAll(kind, ":", ":"),
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}
