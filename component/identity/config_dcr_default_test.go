package identity

import "testing"

// config_dcr_default_test.go -- memql#3719.
//
// The default for MEMQL_IDENTITY_OAUTH_DCR_ENABLED is a SECURITY POSTURE
// DECISION taken by the repository owner, not an implementation detail, so it
// is pinned by a test rather than left to whatever the loader happens to pass
// as a fallback argument.
//
// WHAT IT DECIDES. When true, RFC 7591 dynamic client registration is open:
// POST /register is an UNAUTHENTICATED WRITE that anyone able to reach identity
// can use to create v1:identity:oauthClient rows, and to choose the client_name
// a human later sees on the consent screen. Under one-cluster-per-customer
// (#3700 D1) most clusters never route mcp.<domain> and have no DCR consumer at
// all, so on those it is a standing endpoint serving nothing. It was also one
// half of the CORS escalation in #3716 -- unauthenticated rows becoming an
// input to a different trust decision -- which is the durable argument for
// fewer trusted-by-accident inputs.
//
// It cannot be softened into an approval gate (the point of DCR is completing
// the flow with no human present) and it cannot be derived from whether the
// cluster routes MCP (MEMQL_MCP_PUBLIC_URL comes from MEMQL_DOMAIN since #3704,
// so it is set everywhere). So it is a per-cluster binary, and the safe side is
// the default.
//
// A silent flip back to true would re-open that endpoint on every cluster
// without anything failing, which is precisely the class of change that should
// have to break a test to happen.

// TestOAuthDCRDefaultsToDisabled is the assertion the owner decision comes
// down to.
func TestOAuthDCRDefaultsToDisabled(t *testing.T) {
	// Explicitly empty rather than merely unset: the loader must treat "not
	// configured" as off, and t.Setenv guarantees the ambient environment of
	// whoever runs the suite cannot decide this.
	t.Setenv("MEMQL_IDENTITY_OAUTH_DCR_ENABLED", "")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.OAuthDCREnabled {
		t.Error("OAuthDCREnabled defaults to TRUE.\n" +
			"RFC 7591 dynamic client registration is an unauthenticated write endpoint: " +
			"anyone who can reach identity can create v1:identity:oauthClient rows and pick " +
			"the client_name a human sees on the consent screen. Most clusters route no MCP " +
			"host and have no consumer for it (memql#3719, owner decision). Clusters that " +
			"expose MCP opt in with MEMQL_IDENTITY_OAUTH_DCR_ENABLED=true.")
	}
}

// TestOAuthDCROptInIsHonoured is the other half, and it is not a formality:
// a default that could not be overridden would take the capability away rather
// than move it behind a deliberate choice, and claude.ai / Claude Desktop's
// "add custom connector" flow is a real use case this change does not remove.
//
// The accepted spellings are envBool's, which deliberately tolerates the
// yes/no + on/off convention some env systems use as well as strconv.ParseBool's
// set. All of them are listed here because anything reading this flag OUTSIDE
// the identity Config loader has to agree with this set -- app/transport_mcp.go's
// boot warning reads the same variable and would otherwise fire spuriously on a
// cluster that had correctly enabled DCR with "yes".
func TestOAuthDCROptInIsHonoured(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "1", "yes", "y", "on"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("MEMQL_IDENTITY_OAUTH_DCR_ENABLED", v)

			cfg, err := LoadConfigFromEnv()
			if err != nil {
				t.Fatalf("LoadConfigFromEnv: %v", err)
			}
			if !cfg.OAuthDCREnabled {
				t.Errorf("MEMQL_IDENTITY_OAUTH_DCR_ENABLED=%q did not enable DCR. A cluster "+
					"exposing an MCP surface must be able to turn registration on with one "+
					"env var (memql#3719).", v)
			}
		})
	}
}

// TestOAuthDCRJunkValueDoesNotEnable pins the direction an unparseable value
// resolves in. envBool falls back to the default when it cannot read the value,
// and now that the default is off, that fallback is fail-closed -- "I could not
// understand this" cannot open an unauthenticated write endpoint.
//
// Worth noting because the property is INHERITED rather than stated: it holds
// only because the default is false. When the default was true, an
// unparseable value silently enabled DCR, and nothing said so.
//
// "yes" and "on" are deliberately NOT in this list -- envBool accepts them as
// valid opt-ins, and an earlier draft of this test asserted they were junk and
// failed. They belong in the opt-in test above.
func TestOAuthDCRJunkValueDoesNotEnable(t *testing.T) {
	for _, v := range []string{"enabled", "maybe", "sure", "TRUEISH", " "} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("MEMQL_IDENTITY_OAUTH_DCR_ENABLED", v)

			cfg, err := LoadConfigFromEnv()
			if err != nil {
				t.Fatalf("LoadConfigFromEnv: %v", err)
			}
			if cfg.OAuthDCREnabled {
				t.Errorf("MEMQL_IDENTITY_OAUTH_DCR_ENABLED=%q ENABLED dynamic client "+
					"registration. A value the loader cannot parse must not open an "+
					"unauthenticated write endpoint (memql#3719).", v)
			}
		})
	}
}
