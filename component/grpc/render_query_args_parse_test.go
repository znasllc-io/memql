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
// TWO LEVELS, because the callers are not all resolvable here. The five
// guest mutations are named by this package but declared nowhere in the
// engine's own dsl/ tree -- a product bundle mounted at MEMQL_DSL_PATH is
// expected to supply them (memql#4258) -- so the engine's parser cannot
// resolve them and only their SYNTAX can be checked here. The two
// auth-session mutations are engine constructs (dsl/identity/mutations.memql),
// so those go through the whole front end, name resolution included.

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
	invite := map[string]any{
		"invitationId": "inv-1", "partitionId": "p-1", "spaceName": awkwardName,
		"inviterId": "u-1", "inviterName": awkwardName,
		"inviteeEmail": "guest+tag@example.com", "inviteeName": awkwardName,
		"tokenHash": hash,
	}
	create := map[string]any{"expiresAt": expires}
	for k, v := range invite {
		create[k] = v
	}
	return []struct {
		fn   string
		args map[string]any
	}{
		// guest_handlers.go
		{"mutationMarkGuestInvitationKicked", invite},
		{"mutationCreateGuestInvitation", create},
		{"mutationMarkGuestInvitationAccepted", invite},
		{"mutationCreateGuestParticipant", map[string]any{
			"participantId": "part-1", "partitionId": "p-1", "displayName": awkwardName,
		}},
		{"mutationRotateGuestInvitationToken", map[string]any{
			"invitationId": "inv-1", "tokenHash": strings.Repeat("cd", 32),
			"previousTokenHash": hash,
		}},
		// auth_session_handlers.go
		{"revokeAuthSession", map[string]any{
			"sessionId": "sess-1", "userId": "u-1", "subject": "sub-1",
			"tokenHash": hash, "source": "bff_exchange", "expiresAt": expires,
			"revokedReason": "user_action",
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

// TestRenderedAuthSessionStatementsResolve drives the two call sites whose
// mutations the engine itself declares through the real front end, so the
// argument names and enum values are checked as well as the syntax.
func TestRenderedAuthSessionStatementsResolve(t *testing.T) {
	eng := newRealDSLEngine(t)
	for _, site := range renderedCallSites() {
		if site.fn != "revokeAuthSession" && site.fn != "createAuthSession" {
			continue
		}
		stmt := statementFor(t, site.fn, site.args)
		if _, err := eng.Parse(stmt); err != nil {
			t.Errorf("%s: the engine refused the rendered statement:\n  %s\n  --> %v", site.fn, stmt, err)
		}
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
