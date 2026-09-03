package githubconnect

import (
	"strings"
	"testing"
)

// config_test.go -- the all-or-none rule (epic memql#4912, design section B).
//
// Three states and only three: none of six leaves Connect absent, all six turn
// it on, anything between REFUSES BOOT naming what the operator has as well as
// what they lack. The middle case is the one worth a test, because it is the
// only one where a green boot would be a lie -- a Connect button that appears
// and then fails per person.

func fullConfig() Config {
	return Config{
		AppID:         "123456",
		AppSlug:       "memql-example",
		ClientID:      "Iv1.exampleclientid",
		ClientSecret:  "example-client-secret",
		PrivateKeyB64: "LS0tLS1CRUdJTiBFWEFNUExF",
		WebhookSecret: "example-webhook-secret",
	}
}

func TestNoneOfSixLeavesConnectAbsent(t *testing.T) {
	var c Config
	if err := c.Validate(); err != nil {
		t.Fatalf("an unconfigured cluster must boot: %v", err)
	}
	if c.Configured() {
		t.Fatal("the zero Config reports itself configured")
	}
	if got := c.AuthorizeURL("https://identity.example.test/auth/github/callback", "state"); got != "" {
		t.Errorf("an unconfigured cluster composed an authorize URL: %q", got)
	}
	if got := c.InstallURL(); got != "" {
		t.Errorf("an unconfigured cluster composed an install URL: %q", got)
	}
}

func TestAllSixIsAccepted(t *testing.T) {
	c := fullConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("a fully configured app was refused: %v", err)
	}
	if !c.Configured() {
		t.Fatal("a fully configured app does not report itself configured")
	}
	if len(c.Missing()) != 0 {
		t.Errorf("Missing() = %v on a full config", c.Missing())
	}
}

// TestFiveOfSixIsRefusedNamingBothHalves is the acceptance criterion: "five of
// six values refuses the identity node at boot NAMING the missing ones".
//
// It asserts both halves, because the operator is mid-setup. Naming only what
// is missing tells somebody who set four values that two are absent without
// telling them which four this process actually saw -- and a value that is set
// in the shell but not in the pod's environment is the failure this message
// exists to diagnose.
func TestFiveOfSixIsRefusedNamingBothHalves(t *testing.T) {
	full := fullConfig()
	for _, tc := range []struct {
		name    string
		blank   func(*Config)
		missing string
	}{
		{"no app id", func(c *Config) { c.AppID = "" }, EnvAppID},
		{"no slug", func(c *Config) { c.AppSlug = "" }, EnvAppSlug},
		{"no client id", func(c *Config) { c.ClientID = "" }, EnvClientID},
		{"no client secret", func(c *Config) { c.ClientSecret = "" }, EnvClientSecret},
		{"no private key", func(c *Config) { c.PrivateKeyB64 = "" }, EnvPrivateKeyB64},
		{"no webhook secret", func(c *Config) { c.WebhookSecret = "" }, EnvWebhookSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := full
			tc.blank(&c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("five of six was accepted. A half-configured app boots, shows Connect, and "+
					"fails for every person who presses it -- which reports the operator's mistake to "+
					"everybody except the operator. (%s was blank.)", tc.missing)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.missing) {
				t.Errorf("the refusal does not name the MISSING value %s:\n  %s", tc.missing, msg)
			}
			for _, present := range c.Present() {
				if !strings.Contains(msg, present) {
					t.Errorf("the refusal does not name the PRESENT value %s. An operator mid-setup "+
						"needs to know which values this process actually saw, not only which it "+
						"lacks -- a value set in the shell and absent from the pod is exactly the "+
						"failure this message diagnoses.\n  %s", present, msg)
				}
			}
			if c.Configured() {
				t.Error("a five-of-six config reports itself configured")
			}
		})
	}
}

// TestAuthorizeURLCarriesTheThreeParameters pins the shape the browser is sent
// to. The state travels in the URL and only its digest is stored, so this is
// the one place the plaintext appears.
func TestAuthorizeURLCarriesTheThreeParameters(t *testing.T) {
	c := fullConfig()
	const redirect = "https://identity.example.test/auth/github/callback"
	got := c.AuthorizeURL(redirect, "the-state-value")

	if !strings.HasPrefix(got, "https://github.com/login/oauth/authorize?") {
		t.Fatalf("authorize URL does not point at GitHub's authorize endpoint: %q", got)
	}
	for _, want := range []string{
		"client_id=" + c.ClientID,
		"state=the-state-value",
		"redirect_uri=https%3A%2F%2Fidentity.example.test%2Fauth%2Fgithub%2Fcallback",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authorize URL is missing %q:\n  %s", want, got)
		}
	}
	if c.AuthorizeURL("", "state") != "" || c.AuthorizeURL(redirect, "") != "" {
		t.Error("an authorize URL was composed with no redirect URI or no state; both are required " +
			"and a URL missing either sends the person to a page GitHub refuses")
	}
}

// TestRedirectURIIsDerivedFromTheServicesOwnBaseURL pins the rule the OIDC
// callback already follows: a value taken from the request is a value an
// attacker chooses, so this one comes from configuration.
func TestRedirectURIIsDerivedFromTheServicesOwnBaseURL(t *testing.T) {
	const want = "https://identity.example.test" + CallbackPath
	for _, base := range []string{
		"https://identity.example.test",
		"https://identity.example.test/",
	} {
		if got := RedirectURI(base); got != want {
			t.Errorf("RedirectURI(%q) = %q, want %q", base, got, want)
		}
	}
	if got := RedirectURI(""); got != "" {
		t.Errorf("RedirectURI(\"\") = %q; a cluster that cannot name its own origin must compose "+
			"no redirect URI rather than a relative one GitHub would refuse", got)
	}
}

func TestInstallURLNamesTheApp(t *testing.T) {
	c := fullConfig()
	want := "https://github.com/apps/memql-example/installations/new"
	if got := c.InstallURL(); got != want {
		t.Errorf("InstallURL() = %q, want %q", got, want)
	}
}
