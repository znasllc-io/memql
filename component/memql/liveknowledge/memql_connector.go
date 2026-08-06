package liveknowledge

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// MemqlConnector is the built-in connector for kind='memql'. The
// queryTemplate is a MemQL query string with {args.x} placeholders;
// the connector substitutes args into placeholders, dispatches via
// the engine, and returns the rows as the result.
type MemqlConnector struct {
	Engine EngineAccess
}

// NewMemqlConnector pins the connector to an engine adapter. Use the
// same engine you pass to NewDispatcher.
func NewMemqlConnector(engine EngineAccess) *MemqlConnector {
	return &MemqlConnector{Engine: engine}
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
	q, err := substituteArgs(src.QueryTemplate, args)
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
// appropriate (strings get %q-style quoting; numbers / bools pass
// through; maps + slices marshal as JSON). Missing args fail loudly --
// the dispatcher should have validated args against the source's
// argsSchema before calling, so a missing field here is a bug.
func substituteArgs(template string, args map[string]any) (string, error) {
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
		return memqlQuote(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing args: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// memqlQuote renders a Go value as a MemQL literal. Strings get
// %q-style double quoting with backslash escapes; bools / numbers
// pass through; other types use Go's %v fmt fallback. Note: this
// is intentionally narrow -- the queryTemplate authoring guidance
// is "use simple scalars in placeholders; build complex inputs via
// argsSchema preprocessing if you need them."
func memqlQuote(v any) string {
	switch x := v.(type) {
	case string:
		// QuoteString, not %q -- the escape sets disagree and the lexer treats
		// the difference as a parse error (memql#3099).
		return langparser.QuoteString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", x)
	default:
		return langparser.QuoteString(fmt.Sprintf("%v", x))
	}
}
