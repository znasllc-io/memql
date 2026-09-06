package memql

// render_query_args_parse_test.go runs every statement the guest-invitation
// and auth-session handlers build with renderQueryArgs through the REAL MemQL
// front end, with no database (memql#4256).
//
// WHY THIS EXISTS. Those handlers are covered against a fake engine that
// records query strings and parses nothing -- the failure mode
// voice_agent_real_engine_test.go describes -- which is how every one of them
// shipped rendering the legacy `name({k: v})` object-literal wrapper the
// parser has rejected since Story 9 of memql#2335. SendGuestInviteMsg, guest
// accept / kick / token rotation, createAuthSession and revokeAuthSession all
// failed at parse in production while their unit tests stayed green.
// component/grpc/deploy_control_parse_test.go is the same guard for the
// identical defect in component/deploycontrol (memql#4209).
//
// ONE LEVEL NOW. This used to be two: the five guest mutations were named by
// this package and declared in no .memql file anywhere, so the engine's parser
// could not resolve them and only their SYNTAX could be checked. memql#4258
// declared them -- four in dsl/identity/mutations.memql beside the kind="user"
// twins, one in dsl/cognition/mutations.memql where the space lives -- so all
// seven now go through the whole front end, name resolution included.
//
// That upgrade is the point of the change, not a side effect of it. Syntax
// alone would still have passed while every guest-invite write failed at
// execute on any cluster running the embedded tree, which is exactly what was
// happening.
//
// RESOLUTION IS NOT ARGUMENT CHECKING, and assuming it was would have left
// half the bug uncovered. Measured here: adding an undeclared `tokenHash` to
// the markAccepted call site still parses and still resolves. CLAUDE.md says
// why -- validateFunctionArgs iterates DECLARED fields, and rejectUnknownArgs
// is gated behind the MCP boundary -- so an argument a handler invents is
// dropped on the floor rather than refused. That is precisely how
// revokeAuthSession came to be called with seven arguments against a
// two-argument declaration, in two places, for as long as it has existed.
// TestRenderedArgumentsAreDeclared below is the gate for that half.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// awkwardName exercises the string shapes a display name really carries: an
// apostrophe, embedded double quotes, angle brackets and an ampersand, a
// backslash, a newline, and a non-ASCII letter.
const awkwardName = "O'Brien \"the\" <admin> & co \\ line\nbreak é"

// renderedCallSites mirrors every production call site of renderQueryArgs,
// with the argument map each one builds. Keep this table in step with the
// handlers: a new call site that is not here is not covered.
func renderedCallSites() []struct {
	fn   string
	args map[string]any
} {
	hash := strings.Repeat("ab", 32)
	expires := time.Date(2026, 8, 21, 12, 34, 56, 0, time.UTC).Format(time.RFC3339)
	return []struct {
		fn   string
		args map[string]any
	}{
		// guest_handlers.go. The three update-shaped call sites carry only the
		// auth_session_handlers.go
		{"revokeAuthSession", map[string]any{
			"sessionId": "sess-1", "revokedReason": "user_action",
		}},
		{"createAuthSession", map[string]any{
			"sessionId": "sess-1", "userId": "u-1", "identityId": "id-1",
			"subject": "sub-1", "tokenHash": hash, "source": "bff_exchange",
			"clientLabel": awkwardName, "expiresAt": expires,
		}},
	}
}

// statementFor renders one call site exactly as the handler does.
func statementFor(t *testing.T, fn string, args map[string]any) string {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("%s: marshal args: %v", fn, err)
	}
	return fmt.Sprintf("%s(%s)", fn, renderQueryArgs(argsJSON))
}

// TestRenderedStatementsAreSyntacticallyValid parses every call site's
// rendered statement as an expression -- syntax only, no name resolution, so
// it covers the five product-supplied guest mutations too. Before memql#4256
// every one of them failed with "object-literal call args are removed".
func TestRenderedStatementsAreSyntacticallyValid(t *testing.T) {
	for _, site := range renderedCallSites() {
		stmt := statementFor(t, site.fn, site.args)
		if _, err := langparser.ParseExpression(stmt); err != nil {
			t.Errorf("%s: the parser refused the rendered statement:\n  %s\n  --> %v", site.fn, stmt, err)
		}
	}
}

// TestRenderedStatementsResolve drives EVERY call site through the real front
// end, so the construct name, the argument names and the enum values are all
// checked -- not merely that the text parses.
//
// This is the test the five guest mutations could not be in until memql#4258
// declared them. Their absence was invisible precisely because the syntax test
// above was green: the statements were well-formed and named nothing.
func TestRenderedStatementsResolve(t *testing.T) {
	eng := newRealDSLEngine(t)
	var checked int
	for _, site := range renderedCallSites() {
		stmt := statementFor(t, site.fn, site.args)
		checked++
		if _, err := eng.Parse(stmt); err != nil {
			t.Errorf("%s: the engine refused the rendered statement:\n  %s\n  --> %v", site.fn, stmt, err)
		}
	}
	// A loop that skipped everything would pass. Assert it ran.
	if want := len(renderedCallSites()); checked != want {
		t.Fatalf("resolved %d call sites, want %d", checked, want)
	}
}

// TestRenderQueryArgsShape pins the rendering contract: sorted keys, no
// braces, the parser's own quoting for strings (so the escape grammar is the
// lexer's, never Go's %q), JSON spelling for the other scalars, nested values
// as MemQL literals, and "" for an empty or unreadable object -- which is the
// empty call `name()`.
func TestRenderQueryArgsShape(t *testing.T) {
	got := renderQueryArgs([]byte(`{"zeta":"z","alpha":"a\"q","count":3,"on":true,"none":null,"tags":["x","y"],"obj":{"k":"v"}}`))
	want := `alpha: "a\"q", count: 3, none: null, obj: {k: "v"}, on: true, tags: ["x", "y"], zeta: "z"`
	if got != want {
		t.Fatalf("rendered\n  %s\nwant\n  %s", got, want)
	}
	for _, empty := range []string{`{}`, ``, `not json`, `[1,2]`} {
		if got := renderQueryArgs([]byte(empty)); got != "" {
			t.Errorf("renderQueryArgs(%q) = %q, want the empty call", empty, got)
		}
	}
	// QuoteString sets EscapeHTML(false); the lexer decodes the escapes it
	// does emit, so an HTML-significant character must survive as itself.
	if html := renderQueryArgs([]byte(`{"a":"<b>&"}`)); !strings.Contains(html, `<b>&`) {
		t.Errorf("HTML-significant characters must not be escaped away: got %s", html)
	}
}

// TestRenderedArgumentsAreDeclared checks the OTHER direction of memql#3626's
// "declared and used, in both directions" rule, at the Go call boundary: every
// argument name a handler passes must be a field the mutation declares.
//
// Resolution does not cover this. `eng.Parse` binds the construct name and
// stops; validateFunctionArgs then iterates the DECLARED fields, so a name the
// caller invented is never looked at, and rejectUnknownArgs -- the check that
// would refuse it -- is gated behind the MCP boundary. An extra argument is
// therefore silently discarded, which is not a loud enough failure to notice
// over two years: revokeAuthSession was called with seven arguments against a
// two-argument declaration from two separate packages, each carrying a comment
// explaining why the extras were required, and each wrong since memql#1628
// replaced re-supply with a read-merge.
//
// The failure this prevents is not a crash. It is a handler that believes it
// wrote a field, an operator reading the call site and believing the same, and
// a row that never received it.
func TestRenderedArgumentsAreDeclared(t *testing.T) {
	eng := newRealDSLEngine(t)
	var checked int
	for _, site := range renderedCallSites() {
		fn, err := eng.Functions().Get(site.fn)
		if err != nil || fn == nil {
			t.Errorf("%s: not in the function registry: %v", site.fn, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block; the call site passes %d argument(s)",
				site.fn, len(site.args))
			continue
		}
		checked++
		for name := range site.args {
			if declared[name] {
				continue
			}
			t.Errorf("%s: the handler passes %q, which the mutation does not declare. "+
				"It is not refused -- rejectUnknownArgs is gated behind the MCP boundary, "+
				"so the value is silently discarded and the row never receives it. Either "+
				"declare the field or stop sending it (memql#3626, memql#4258).",
				site.fn, name)
		}
	}
	if want := len(renderedCallSites()); checked != want {
		t.Fatalf("checked %d call sites, want %d", checked, want)
	}
}
