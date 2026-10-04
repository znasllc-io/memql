package inbound

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
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
	// Lower-cased, trimmed, blanks dropped: the allowlist is compared against
	// canonical header names, and a trailing comma is not a header.
	t.Setenv("MEMQL_INBOUND_SOURCE_ACME_FORWARD_HEADERS", "X-Acme-Topic, X-Acme-Id,")
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
	if want := []string{"x-acme-topic", "x-acme-id"}; !reflect.DeepEqual(acme.ForwardHeaders, want) {
		t.Errorf("FORWARD_HEADERS did not resolve from env: got %q, want %q", acme.ForwardHeaders, want)
	}
	if _, ok := cfg.Sources["big-corp"]; !ok {
		t.Errorf("a source with an explicit scheme=none must be admitted -- it is a deliberate "+
			"configuration, and the '-' -> '_' env mapping is what this checks: %v", cfg.Sources)
	}
}

// An env source named after a DECLARED connector is dropped at boot, with an
// ERROR naming the env vars (memql#5707 residual). resolveSource already
// refuses it per request, but only once a request arrives -- so a deploy
// configured that way booted clean and answered every delivery 404 with
// nothing at boot saying why. The other sources in the same allowlist are
// untouched: the name is reserved, not the receiver.
func TestLoadConfigDropsAnEnvSourceNamedAfterADeclaredConnector(t *testing.T) {
	memqlsync.Declare("shopify")
	t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "shopify,acme")
	for _, suffix := range []string{"SHOPIFY", "ACME"} {
		t.Setenv("MEMQL_INBOUND_SOURCE_"+suffix+"_SIGNATURE_SCHEME", SchemeHMACSHA256Base64)
		t.Setenv("MEMQL_INBOUND_SOURCE_"+suffix+"_SECRET", "shh")
		t.Setenv("MEMQL_INBOUND_SOURCE_"+suffix+"_SIGNATURE_HEADER", "X-Shopify-Hmac-Sha256")
	}

	var logs bytes.Buffer
	cfg := LoadConfig(slog.New(slog.NewTextHandler(&logs, nil)))
	if src, ok := cfg.Sources["shopify"]; ok {
		t.Fatalf("an env source under a connector's own name was admitted at boot: %+v", src)
	}
	if _, ok := cfg.Sources["acme"]; !ok {
		t.Fatalf("an unrelated source in the same allowlist was dropped too: %v", cfg.Sources)
	}
	out := logs.String()
	for _, want := range []string{"level=ERROR", "source=shopify", "MEMQL_INBOUND_SOURCE_SHOPIFY_", "connector"} {
		if !strings.Contains(out, want) {
			t.Errorf("the boot log does not say %q:\n%s", want, out)
		}
	}
}

// A HYPHENATED source name reaches every one of its settings, FORWARD_HEADERS
// included, through the '-' -> '_' env spelling. The case above proves the
// mapping only for a source with scheme=none, which reads nothing but the
// scheme, so a mapping that broke for the other keys would still pass it.
//
// The variables are the ones docs/public/operate/inbound-delivery.md tells an
// operator to set for `shopify-custom`, spelled exactly as printed there. If
// the mapping drifted, that block would configure a source that is dropped at
// boot (no SIGNATURE_SCHEME under the name the loader reads) or, worse, one
// that verifies and stages no metadata, so the topic never reaches a
// connector.
func TestLoadConfigMapsAHyphenatedSourcesForwardHeaders(t *testing.T) {
	t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "shopify-custom")
	t.Setenv("MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_SIGNATURE_SCHEME", "hmac-sha256-base64")
	t.Setenv("MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_SIGNATURE_HEADER", "X-Shopify-Hmac-Sha256")
	t.Setenv("MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_DEDUPE_HEADER", "X-Shopify-Webhook-Id")
	t.Setenv("MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_FORWARD_HEADERS", "X-Shopify-Topic,X-Shopify-Shop-Domain,X-Shopify-Webhook-Id")
	t.Setenv("MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_SECRET", "shh")

	src, ok := LoadConfig(quietLogger()).Sources["shopify-custom"]
	if !ok {
		t.Fatal("the documented shopify-custom configuration was dropped: its settings did not " +
			"resolve under MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_*")
	}
	if src.Scheme != SchemeHMACSHA256Base64 || src.Secret != "shh" ||
		src.SignatureHeader != "X-Shopify-Hmac-Sha256" || src.DedupeHeader != "X-Shopify-Webhook-Id" {
		t.Errorf("source policy did not resolve from the hyphenated name's env spelling: %+v", src)
	}
	want := []string{"x-shopify-topic", "x-shopify-shop-domain", "x-shopify-webhook-id"}
	if !reflect.DeepEqual(src.ForwardHeaders, want) {
		t.Errorf("MEMQL_INBOUND_SOURCE_SHOPIFY_CUSTOM_FORWARD_HEADERS resolved to %q, want %q",
			src.ForwardHeaders, want)
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
