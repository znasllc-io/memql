package identity

import "testing"

// The vite dev-server origins are not domain-shaped, so they cannot be part of
// what MEMQL_DOMAIN derives -- folding them in would mean a staging deployment
// that forgot to set CORS silently admits localhost. They arrive as their own
// domain-free knob instead, appended to whatever the derived-or-explicit set is
// (memql#3593).
func TestCORSExtraOriginsAppend(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS", "https://cockpit.memql.localhost")
	t.Setenv("MEMQL_IDENTITY_CORS_EXTRA_ORIGINS", "http://localhost:8080,http://localhost:3000")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}

	want := []string{
		"https://cockpit.memql.localhost",
		"http://localhost:8080",
		"http://localhost:3000",
	}
	if len(cfg.CORSAllowedOrigins) != len(want) {
		t.Fatalf("CORSAllowedOrigins = %v, want %v", cfg.CORSAllowedOrigins, want)
	}
	for i, w := range want {
		if cfg.CORSAllowedOrigins[i] != w {
			t.Errorf("origin[%d] = %q, want %q", i, cfg.CORSAllowedOrigins[i], w)
		}
	}
}

// A client id in both lists is ONE client with more redirect URIs, not two
// clients. Two entries sharing an id would make FindClient's answer depend on
// list order, which is how a redirect URI silently stops being accepted.
func TestExtraRegisteredClientsMergeById(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_REGISTERED_CLIENTS",
		`[{"clientId":"portal","redirectURIs":["https://cockpit.memql.localhost/portal/auth/callback"]}]`)
	t.Setenv("MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS",
		`[{"clientId":"portal","redirectURIs":["http://localhost:3000/auth/callback"]},`+
			`{"clientId":"devtool","redirectURIs":["http://localhost:9999/cb"]}]`)

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}

	portal := cfg.FindClient("portal")
	if portal == nil {
		t.Fatal("portal client missing")
	}
	if len(portal.RedirectURIs) != 2 {
		t.Errorf("portal redirect URIs = %v, want both merged", portal.RedirectURIs)
	}
	if !cfg.AllowsRedirectURI("portal", "http://localhost:3000/auth/callback") {
		t.Error("the extra redirect URI is not accepted")
	}
	if !cfg.AllowsRedirectURI("portal", "https://cockpit.memql.localhost/portal/auth/callback") {
		t.Error("the original redirect URI stopped being accepted")
	}

	if cfg.FindClient("devtool") == nil {
		t.Error("devtool client missing -- an id present only in the extras must still register")
	}
}

// Malformed extras are a configuration error, not something to swallow: a
// client silently absent surfaces as a redirect being refused at sign-in,
// with nothing naming the cause.
func TestExtraRegisteredClientsRejectsMalformed(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS", "{not json")

	if _, err := LoadConfigFromEnv(); err == nil {
		t.Fatal("LoadConfigFromEnv() = nil error, want a refusal naming the variable")
	}
}
