package liveknowledge

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// QuoteFunc renders a Go string as a MemQL string literal, quotes and
// escapes included.
//
// It is injected rather than implemented here, and the reason is the whole
// design of this file (memql#3192). There is exactly ONE correct definition of
// "a MemQL string literal" -- langparser.QuoteString, which lives beside the
// lexer whose escape set it targets -- and any second definition drifts from
// it the moment that set changes. This package cannot reach it: it is an L0
// leaf with zero in-repo imports, which is the property memql#3164 moved it
// out of component/memql to get and the reason the area dependency graph is a
// DAG. Growing a local escape table to work around that would be committing
// the defect this type exists to prevent, so the quoter comes in from the
// caller that can name it -- integrations/liveknowledge, exactly as
// EngineAccess already does for the engine.
type QuoteFunc func(string) string

// MemqlConnector is the built-in connector for kind='memql'. The
// queryTemplate is a MemQL query string with {args.x} placeholders;
// the connector substitutes args into placeholders, dispatches via
// the engine, and returns the rows as the result.
type MemqlConnector struct {
	Engine EngineAccess

	// Quote renders string args as MemQL literals. Required: Query refuses
	// to run without it rather than falling back to a built-in escape set.
	// A silent fallback is how the escape-set disagreement comes back.
	Quote QuoteFunc
}

// NewMemqlConnector pins the connector to an engine adapter and the MemQL
// string quoter. Use the same engine you pass to NewDispatcher, and
// langparser.QuoteString as the quoter -- see QuoteFunc for why it is a
// parameter.
func NewMemqlConnector(engine EngineAccess, quote QuoteFunc) *MemqlConnector {
	return &MemqlConnector{Engine: engine, Quote: quote}
}

// Kind returns "memql" -- the liveConnector.kind value this connector
// claims.
func (c *MemqlConnector) Kind() string { return "memql" }

// Query substitutes args into the queryTemplate placeholders and
// dispatches via engine.Execute. Returns the rows wrapped under a
// "rows" key in the result map -- callers reading via
// integration.liveknowledge.query get a consistent shape regardless of
// the underlying connector.
func (c *MemqlConnector) Query(ctx context.Context, src Source, args map[string]any) (map[string]any, error) {
	if c.Engine == nil {
		return nil, fmt.Errorf("memql connector: engine not configured")
	}
	if src.QueryTemplate == "" {
		return nil, fmt.Errorf("memql connector: source %q has empty queryTemplate", src.Name)
	}
	if c.Quote == nil {
		return nil, fmt.Errorf("memql connector: quoter not configured")
	}
	q, err := substituteArgs(src.QueryTemplate, args, c.Quote)
	if err != nil {
		return nil, fmt.Errorf("memql connector: substitute args: %w", err)
	}
	res, err := c.Engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("memql connector: engine execute: %w", err)
	}
	rows := asRows(res)
	return map[string]any{
		"rows":  rows,
		"count": len(rows),
	}, nil
}

// argPlaceholder matches {args.fieldName} in a queryTemplate. Field
// names follow the MemQL identifier convention (letter / digit /
// underscore).
var argPlaceholder = regexp.MustCompile(`\{args\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// substituteArgs walks the template and replaces each {args.X}
// placeholder with the corresponding args[X] value, MemQL-quoted as
// appropriate (strings go through quote; numbers / bools pass
// through). Missing args fail loudly -- the dispatcher should have validated
// args against the source's argsSchema before calling, so a missing field
// here is a bug.
func substituteArgs(template string, args map[string]any, quote QuoteFunc) (string, error) {
	var missing []string
	out := argPlaceholder.ReplaceAllStringFunc(template, func(match string) string {
		// match looks like {args.name}; extract name
		sub := argPlaceholder.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		name := sub[1]
		v, ok := args[name]
		if !ok {
			missing = append(missing, name)
			return match
		}
		return memqlQuote(v, quote)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing args: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// memqlQuote renders a Go value as a MemQL literal. Strings go through the
// injected quoter; bools / numbers pass through; other types are stringified
// with %v and then quoted. Note: this is intentionally narrow -- the
// queryTemplate authoring guidance is "use simple scalars in placeholders;
// build complex inputs via argsSchema preprocessing if you need them."
//
// The string arms used fmt.Sprintf("%q") until memql#3192. Go's %q escape set
// and the MemQL lexer's do not agree, and the disagreement is a hard error at
// tokenize time rather than a fallback -- %q emits `\x00` / `\a` / `\v`, and
// the lexer implements the JSON escapes and only those -- so one control byte
// in a live-knowledge arg made the substituted query unparseable.
//
// Quoted faithfully, never substituted. This is a READ path: the rendered
// value lands in a filter, is compared, and is discarded. Nothing here writes
// a row, so the NUL-into-jsonb second layer that the outbound and inbound
// paths have to decide about does not arise -- and rewriting a byte would
// silently change which rows match, turning "no results" into the wrong
// results.
func memqlQuote(v any, quote QuoteFunc) string {
	switch x := v.(type) {
	case string:
		return quote(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", x)
	default:
		return quote(fmt.Sprintf("%v", x))
	}
}
