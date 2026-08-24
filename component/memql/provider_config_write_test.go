package memql

import (
	"strconv"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/events"
)

// The two credential-WRITING builtins (epic memql#4440, task memql#4444).
//
// ============================================================================
// THE CALL IS A STRING, AND A STRING CAN BE MALFORMED
// ============================================================================
// `providerKeySet` reaches setGlobalSecret through `e.Execute(ctx, <MemQL
// text>)`, because engine.Execute is the only write channel available from
// inside a builtin. Every value it carries has to become a literal, and
// getting that wrong is NOT a compile error -- it is a PARSE failure at
// EXECUTE time, on an operator's first attempt to save a key.
//
// The values here are the worst possible inputs for that: base64 ciphertext
// from NaCl secretbox, which contains `+` and `/` and `=`, and an operator-
// typed filesystem path. This is the memql#4256 class, and the Shopify
// connector carries `call_parse_test.go` for exactly the same reason. So the
// rendered call is driven through the FRONT END's own parser rather than
// eyeballed.

func TestRenderedProviderConfigCallsParse(t *testing.T) {
	cases := []struct {
		name string
		fn   string
		args map[string]string
	}{
		{
			name: "a sealed key, with base64 ciphertext",
			fn:   "setGlobalSecret",
			args: map[string]string{
				"id":   "sec-memql-ai-anthropic-api-key",
				"name": envAnthropicAPIKey,
				// Real NaCl-secretbox output shape: +, / and = all appear, and
				// every one of them is a character a naive renderer breaks on.
				"encryptedValue": "YWJjZGVm+Z2hpams/bG1ub3A=Cg==",
				"fingerprint":    "9f2c",
				"kind":           "vendor_api_key",
				"description":    "Seeded from the portal's AI providers page.",
				"addedBy":        "user-owner",
			},
		},
		{
			name: "a federation id",
			fn:   "setGlobalVariable",
			args: map[string]string{
				"id":    "var-memql-ai-anthropic-federation-rule-id",
				"name":  envAnthropicFederationRuleID,
				"value": "fdrl_01HZX9Q2ABCDEF",
			},
		},
		{
			name: "a token FILE PATH, which an operator types by hand",
			fn:   "setGlobalVariable",
			args: map[string]string{
				"id":    "var-memql-ai-anthropic-identity-token-file",
				"name":  envAnthropicIdentityTokenFile,
				"value": "/var/run/secrets/anthropic.com/serviceaccount/token",
			},
		},
		{
			name: "values carrying the characters a renderer breaks on",
			fn:   "setGlobalVariable",
			args: map[string]string{
				"id":   "var-hostile",
				"name": "MEMQL_AI_TEST",
				// A quote, a backslash and a newline. None of these should
				// ever arrive, and the renderer must survive them anyway --
				// an operator pasting from a terminal can produce all three.
				"value": "a\"b\\c\nd",
			},
		},
		{
			// THE FOUR THAT strconv.Quote GETS WRONG. Go's quoting emits
			// \x00, \a, \v and \x7f; the MemQL lexer rejects every one, so
			// this case FAILS against the obvious implementation and passes
			// only with langparser.QuoteString's JSON escaping. Verified by
			// driving both forms through this same parser.
			//
			// These reach a value because the federation form's field is
			// operator-typed -- a rule id or a token path pasted from a
			// terminal -- and a stray control byte in a paste is ordinary.
			name: "control bytes Go quoting escapes in a form the lexer refuses",
			fn:   "setGlobalVariable",
			args: map[string]string{
				"id":    "var-control-bytes",
				"name":  "MEMQL_AI_TEST",
				"value": "nul\x00 bell\a vtab\v del\x7f",
			},
		},
		{
			name: "non-ASCII, which an operator's path or note can carry",
			fn:   "setGlobalVariable",
			args: map[string]string{
				"id":    "var-unicode",
				"name":  "MEMQL_AI_TEST",
				"value": "café — ñ 🔑",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := renderProviderConfigCall(tc.fn, tc.args)
			if _, err := langparser.ParseExpression(stmt); err != nil {
				t.Fatalf("the rendered call does not parse, so this would fail at EXECUTE time "+
					"on an operator's first save:\n  %s\n  %v", stmt, err)
			}
		})
	}
}

func TestRenderedCallDropsEmptyValuesRatherThanWritingThem(t *testing.T) {
	// An omitted optional writes NO argument at all. An empty string argument
	// would be a row whose value is "" -- which the resolver reads as absent,
	// so it means nothing and shadows nothing, and is worse than no row
	// because it looks configured in the concept browser.
	stmt := renderProviderConfigCall("setGlobalVariable", map[string]string{
		"id":    "var-x",
		"name":  "MEMQL_AI_TEST",
		"value": "",
	})
	if strings.Contains(stmt, "value:") {
		t.Errorf("an empty value was rendered into the call: %s", stmt)
	}
	if !strings.Contains(stmt, "name:") {
		t.Errorf("a non-empty value was dropped: %s", stmt)
	}
	if _, err := langparser.ParseExpression(stmt); err != nil {
		t.Fatalf("the trimmed call does not parse: %s: %v", stmt, err)
	}
}

// TestRendererUsesLexerCompatibleEscaping states the rule directly, so the
// reason survives even if the hostile fixture above is ever trimmed.
//
// strconv.Quote is the function anyone reaches for, and for these four bytes
// it produces a literal this engine cannot parse. The failure would land at
// EXECUTE time on an operator's save, with a parse error naming a column in a
// statement nobody logged.
func TestRendererUsesLexerCompatibleEscaping(t *testing.T) {
	for _, v := range []string{"a\x00b", "a\ab", "a\vb", "a\x7fb"} {
		goQuoted := strconv.Quote(v)
		if _, err := langparser.ParseExpression("f(x: " + goQuoted + ")"); err == nil {
			t.Errorf("strconv.Quote(%q) now parses; if the lexer has grown these escapes "+
				"this test is stale, but do NOT relax the renderer on that basis alone", v)
		}
		stmt := renderProviderConfigCall("setGlobalVariable", map[string]string{"value": v})
		if _, err := langparser.ParseExpression(stmt); err != nil {
			t.Errorf("the renderer produced an unparseable literal for %q: %s: %v", v, stmt, err)
		}
	}
}

func TestProviderKeySetIsOwnerOnly(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	_, err := e.evaluateProviderKeySetExpression(nonClusterOwnerCtx("user-writer"), map[string]any{
		"vendor": "anthropic", "apiKey": "sk-test",
	})
	if err == nil {
		t.Fatal("a non-owner sealed a vendor credential into the cluster")
	}
	if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("the refusal should say why: %v", err)
	}
}

func TestProviderFederationSetIsOwnerOnly(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	_, err := e.evaluateProviderFederationSetExpression(nonClusterOwnerCtx("user-writer"), nil)
	if err == nil {
		t.Fatal("a non-owner wrote the cluster's federation configuration")
	}
	if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("the refusal should say why: %v", err)
	}
}

func TestProviderKeySetRefusesAnUnknownVendor(t *testing.T) {
	// The row NAME is derived from the vendor rather than supplied, which is
	// the whole safety property: an operator cannot name a row the resolver
	// never tries. That only holds if an unknown vendor is REFUSED rather
	// than falling through to some default name.
	e := engineWithProviders(t, events.NewBus())
	_, err := e.evaluateProviderKeySetExpression(clusterOwnerCtx("user-owner"), map[string]any{
		"vendor": "gemini", "apiKey": "sk-test",
	})
	if err == nil {
		t.Fatal("an unknown vendor was accepted; the key would land under a name nothing reads")
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Errorf("the refusal should name what was asked for: %v", err)
	}
}

func TestProviderKeySetRefusesABlankKey(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	for _, blank := range []string{"", "   ", "\n"} {
		_, err := e.evaluateProviderKeySetExpression(clusterOwnerCtx("user-owner"), map[string]any{
			"vendor": "openai", "apiKey": blank,
		})
		if err == nil {
			t.Errorf("a blank key (%q) was sealed into a row", blank)
		}
	}
}

// TestFederationSetIsAllOrNone is the refusal that has to happen HERE rather
// than at the fleet's next restart.
//
// A partial federation set REFUSES BOOT (memql#4333, deliberately: zero config
// is legitimate, half config is a mistake). So a portal write that accepted a
// partial set would look like it worked, and take every node down the next
// time one restarted -- hours later, with nothing connecting the outage to the
// save that caused it.
func TestFederationSetIsAllOrNone(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	ctx := clusterOwnerCtx("user-owner")

	partial := map[string]any{
		"ruleId":         "fdrl_01HZX",
		"organizationId": "org-1",
		// serviceAccountId and identityTokenFile deliberately absent.
	}
	_, err := e.evaluateProviderFederationSetExpression(ctx, partial)
	if err == nil {
		t.Fatal("a PARTIAL federation set was accepted; it refuses BOOT, so this " +
			"would take the fleet down at its next restart")
	}
	if !strings.Contains(err.Error(), "all-or-none") {
		t.Errorf("the refusal should name the rule: %v", err)
	}
	// It must name BOTH halves -- what was given and what is missing -- or the
	// operator cannot tell which boxes to fill.
	for _, want := range []string{envAnthropicFederationRuleID, envAnthropicServiceAccountID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	// THE REACHABLE POSITIVE, and it is two-sided. An EMPTY set is legitimate
	// (it is what a cluster with no federation configured looks like), so the
	// refusal must be about PARTIAL specifically, not about "not complete".
	if _, err := e.evaluateProviderFederationSetExpression(ctx, map[string]any{}); err != nil &&
		strings.Contains(err.Error(), "all-or-none") {
		t.Errorf("an EMPTY federation set was refused as partial; zero config is legitimate: %v", err)
	}
}

// TestWorkspaceIdIsOutsideTheRequiredSet pins the one field Anthropic makes
// conditional. Anthropic needs it only when a rule spans more than one
// workspace, so demanding it would make the common single-workspace install
// carry a value it does not need -- and this file's required set must agree
// with `requiredFederationFields`, which is what the CONSTRUCTOR reads.
func TestWorkspaceIdIsOutsideTheRequiredSet(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	complete := map[string]any{
		"ruleId":            "fdrl_01HZX",
		"organizationId":    "org-1",
		"serviceAccountId":  "sa-1",
		"identityTokenFile": "/var/run/secrets/token",
		// No workspaceId.
	}
	if _, err := e.evaluateProviderFederationSetExpression(clusterOwnerCtx("user-owner"), complete); err != nil &&
		strings.Contains(err.Error(), "all-or-none") {
		t.Errorf("a complete four-field set was refused for want of the OPTIONAL workspace id: %v", err)
	}
}
