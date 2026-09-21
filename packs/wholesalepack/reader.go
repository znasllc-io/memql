package wholesalepack

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
