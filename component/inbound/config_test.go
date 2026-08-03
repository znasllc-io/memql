package inbound

import (
	"log/slog"
	"testing"
)

// quietLogger keeps LoadConfig's operator-facing warnings out of test output
// while still exercising the logging branches.
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestLoadConfigAdmitsNothingByDefault is the deny-by-default claim, and it is
// the one property of this package that must hold unconditionally: the route is
// mounted on every bff, so an operator who has configured nothing must get a
// receiver that accepts nothing rather than one that accepts everything.
func TestLoadConfigAdmitsNothingByDefault(t *testing.T) {
	cfg := LoadConfig(quietLogger())
	if len(cfg.Sources) != 0 {
		t.Errorf("an unconfigured deployment resolved %d source(s); the allowlist is empty, so "+
			"the receiver must admit nothing: %v", len(cfg.Sources), cfg.Sources)
	}
}

// TestLoadConfigFailsClosedOnMisconfiguration pins the direction that matters:
// a source an operator LISTED but did not finish configuring must be dropped,
// never admitted unverified. Admitting it would turn a typo into a public,
// unauthenticated write endpoint.
func TestLoadConfigFailsClosedOnMisconfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		why  string
	}{
		{
			name: "no scheme at all",
			env:  map[string]string{"SECRET": "s", "SIGNATURE_HEADER": "X-Sig"},
			why:  "an unset scheme must not default to accepting anything",
		},
		{
			name: "unknown scheme",
			env: map[string]string{
				"SIGNATURE_SCHEME": "hmac-md5", "SECRET": "s", "SIGNATURE_HEADER": "X-Sig",
			},
			why: "a scheme this build does not implement cannot be checked",
		},
		{
			name: "scheme but no secret",
			env:  map[string]string{"SIGNATURE_SCHEME": SchemeHMACSHA256Hex, "SIGNATURE_HEADER": "X-Sig"},
			why:  "there is no key to verify against",
		},
		{
			name: "scheme and secret but no header",
			env:  map[string]string{"SIGNATURE_SCHEME": SchemeHMACSHA256Hex, "SECRET": "s"},
			why:  "there is nothing to read the signature from",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "acme")
			for k, v := range tc.env {
				t.Setenv("MEMQL_INBOUND_SOURCE_ACME_"+k, v)
			}
			cfg := LoadConfig(quietLogger())
			if _, ok := cfg.Sources["acme"]; ok {
				t.Errorf("a misconfigured source was admitted -- %s.\n  resolved: %+v",
					tc.why, cfg.Sources["acme"])
			}
		})
	}
}

// A name that is not a single safe path segment is refused, so a listed
// "../admin" or "a/b" can never become a mounted source.
func TestLoadConfigRejectsUnsafeSourceNames(t *testing.T) {
	for _, name := range []string{"a/b", "..", "../admin", "A", "-lead", "with space", ""} {
		t.Run("name="+name, func(t *testing.T) {
			t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", name)
			cfg := LoadConfig(quietLogger())
			if len(cfg.Sources) != 0 {
				t.Errorf("%q was admitted as a source name: %v", name, cfg.Sources)
			}
		})
	}
}

// The happy path, plus the explicit-none path -- because "fails closed" is only
// meaningful next to evidence that a correct configuration is admitted.
func TestLoadConfigAdmitsAConfiguredSource(t *testing.T) {
	t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "acme, big-corp ")
	t.Setenv("MEMQL_INBOUND_SOURCE_ACME_SIGNATURE_SCHEME", SchemeHMACSHA256Base64)
	t.Setenv("MEMQL_INBOUND_SOURCE_ACME_SECRET", "shh")
	t.Setenv("MEMQL_INBOUND_SOURCE_ACME_SIGNATURE_HEADER", "X-Acme-Hmac")
	// The '-' in the name maps to '_' in the env spelling.
	t.Setenv("MEMQL_INBOUND_SOURCE_BIG_CORP_SIGNATURE_SCHEME", SchemeNone)

	cfg := LoadConfig(quietLogger())
	acme, ok := cfg.Sources["acme"]
	if !ok {
		t.Fatalf("a fully configured source was not admitted: %v", cfg.Sources)
	}
	if acme.Scheme != SchemeHMACSHA256Base64 || acme.Secret != "shh" || acme.SignatureHeader != "X-Acme-Hmac" {
		t.Errorf("source policy did not resolve from env: %+v", acme)
	}
	if _, ok := cfg.Sources["big-corp"]; !ok {
		t.Errorf("a source with an explicit scheme=none must be admitted -- it is a deliberate "+
			"configuration, and the '-' -> '_' env mapping is what this checks: %v", cfg.Sources)
	}
}

// Enabled=false must take the whole receiver out, not merely stop new sources
// resolving.
func TestLoadConfigRespectsTheKillSwitch(t *testing.T) {
	t.Setenv("MEMQL_INBOUND_ENABLED", "false")
	if LoadConfig(quietLogger()).Enabled {
		t.Error("MEMQL_INBOUND_ENABLED=false did not disable the receiver")
	}
}
