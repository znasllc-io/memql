package liveknowledge

// memql_connector_control_byte_test.go -- regression guard for memql#3192 on
// the Live Knowledge connector path.
//
// core/liveknowledge's memqlQuote rendered a string arg with
// fmt.Sprintf("%q") and substituted it into the source's queryTemplate, which
// then went to engine.Execute. Go's %q escape set and the MemQL lexer's do not
// agree, and the disagreement is a hard error at TOKENIZE time rather than a
// fallback: scanString implements the JSON escapes and only those, so the
// `\x00` / `\a` / `\v` %q emits are `invalid escape character`. One control
// byte in an arg made the whole live-knowledge read fail with an error about
// query text.
//
// # Why the guard lives HERE and not in core/liveknowledge
//
// Because the fix does. core/liveknowledge is an L0 leaf with zero in-repo
// imports -- the property memql#3164 moved it out of component/memql to get,
// and the reason the area graph is a DAG -- so it cannot import
// component/language/parser to reach the one correct quoter. Rather than grow
// a second definition of the escape set down there (which is the defect this
// issue is about, not a fix for it), the connector takes the quoter as an
// injected dependency, exactly as it already takes EngineAccess. This package
// is where that injection happens, so this is where it can be checked against
// the real lexer end to end.

import (
	"context"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	lk "github.com/znasllc-io/memql/core/liveknowledge"
)

// captureEngine records the query the connector built instead of running it.
type captureEngine struct{ query string }

func (c *captureEngine) Execute(_ context.Context, query string) (any, error) {
	c.query = query
	return map[string]any{"data": []any{}}, nil
}

func TestMemqlConnector_ControlByteArgLexes(t *testing.T) {
	const dirty = "sku \x00 \a \v end"

	eng := &captureEngine{}
	conn := newDefaultMemqlConnector(eng)

	src := lk.Source{
		Id:            "v1:knowledge:liveSource:test",
		Name:          "inventory.skuLevels",
		QueryTemplate: `query skuLevels(sku: {args.sku})`,
		ConnectorKind: "memql",
	}
	if _, err := conn.Query(context.Background(), src, map[string]any{"sku": dirty}); err != nil {
		t.Fatalf("connector query: %v", err)
	}

	toks, err := langparser.NewLexer(eng.query).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected the substituted query (the #3192 defect): %v\nquery: %#v", err, eng.query)
	}

	var literal string
	for _, tok := range toks {
		if tok.Type == langparser.TokenString {
			literal = tok.Literal
			break
		}
	}
	if literal != dirty {
		t.Errorf("arg did not round-trip: got %#v, want %#v", literal, dirty)
	}
}

// TestMemqlConnector_QuoterIsTheOneDefinition pins the injection itself: the
// connector this package registers must quote through langparser.QuoteString,
// not through a second escape set that can drift from the lexer.
func TestMemqlConnector_QuoterIsTheOneDefinition(t *testing.T) {
	eng := &captureEngine{}
	conn := newDefaultMemqlConnector(eng)

	const dirty = "a\x00b"
	src := lk.Source{
		Name:          "t",
		QueryTemplate: `query f(x: {args.x})`,
		ConnectorKind: "memql",
	}
	if _, err := conn.Query(context.Background(), src, map[string]any{"x": dirty}); err != nil {
		t.Fatalf("connector query: %v", err)
	}

	want := `query f(x: ` + langparser.QuoteString(dirty) + `)`
	if eng.query != want {
		t.Errorf("substituted query:\n  got:  %#v\n  want: %#v", eng.query, want)
	}
}

// TestMemqlConnector_MissingQuoterFailsLoudly pins the failure mode of the
// injection: a connector built without a quoter must refuse to run rather than
// fall back to a built-in escape set. A silent fallback is how this defect
// comes back.
func TestMemqlConnector_MissingQuoterFailsLoudly(t *testing.T) {
	conn := &lk.MemqlConnector{Engine: &captureEngine{}}
	src := lk.Source{Name: "t", QueryTemplate: `query f(x: {args.x})`, ConnectorKind: "memql"}
	if _, err := conn.Query(context.Background(), src, map[string]any{"x": "plain"}); err == nil {
		t.Fatal("expected an error from a connector with no quoter, got nil")
	}
}
