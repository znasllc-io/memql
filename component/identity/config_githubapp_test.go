package identity

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// config_githubapp_test.go -- the boot half of the all-or-none rule
// (epic memql#4912, issue memql#4913, design section F: "Boot: five of six
// values refuses the identity node with the missing names").
//
// The unit test next door (component/identity/githubconnect/config_test.go)
// pins the rule on the Config type. THIS one pins that the rule is actually
// REACHED: identity.Config.Validate is what app/integrations_identity.go calls,
// and it fatals the node when it errors, so a rule the type enforces and this
// function never consults is a rule that never fires.

// githubAppEnv sets all six to the values given, blanking any whose value is
// the empty string. t.Setenv restores the ambient environment afterwards, so a
// developer with a real app exported cannot make these pass or fail.
func githubAppEnv(t *testing.T, appID, slug, clientID, clientSecret, key, webhook string) {
	t.Helper()
	t.Setenv(githubconnect.EnvAppID, appID)
	t.Setenv(githubconnect.EnvAppSlug, slug)
	t.Setenv(githubconnect.EnvClientID, clientID)
	t.Setenv(githubconnect.EnvClientSecret, clientSecret)
	t.Setenv(githubconnect.EnvPrivateKeyB64, key)
	t.Setenv(githubconnect.EnvWebhookSecret, webhook)
}

// bootableConfig is an identity config that Validate accepts, so the GitHub
// clause is the only thing under test. Loopback base URL: the ephemeral-key
// guard and the key-encryption requirement both exempt a single-process host,
// which is exactly what a unit test is.
func bootableConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("MEMQL_IDENTITY_ENABLED", "true")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "http://localhost:8085")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	return cfg
}

// TestIdentityBootsWithNoGitHubApp is the ordinary install: Connect is absent,
// nothing is refused, and the Source stop offers the pasted token alone.
func TestIdentityBootsWithNoGitHubApp(t *testing.T) {
	githubAppEnv(t, "", "", "", "", "", "")
	cfg := bootableConfig(t)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a cluster with no GitHub App was refused at boot: %v.\n"+
			"None of six is a supported install -- most clusters have no App at all -- and "+
			"refusing it would make the feature mandatory rather than optional.", err)
	}
	if cfg.GitHubApp.Configured() {
		t.Error("Config.GitHubApp reports itself configured with nothing set")
	}
}

// TestIdentityBootsWithAllSixGitHubAppValues is the other end.
func TestIdentityBootsWithAllSixGitHubAppValues(t *testing.T) {
	githubAppEnv(t, "123456", "memql-example", "Iv1.exampleclientid",
		"example-client-secret", "LS0tLS1CRUdJTiBFWEFNUExF", "example-webhook-secret")
	cfg := bootableConfig(t)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a fully configured GitHub App was refused at boot: %v", err)
	}
	if !cfg.GitHubApp.Configured() {
		t.Fatal("Config.GitHubApp does not report itself configured with all six set")
	}
	if cfg.GitHubApp.AppSlug != "memql-example" {
		t.Errorf("the slug did not reach the config: %q", cfg.GitHubApp.AppSlug)
	}
}

// TestFiveOfSixRefusesTheIdentityNodeAtBoot is the acceptance criterion.
//
// It goes through identity.Config.Validate rather than
// githubconnect.Config.Validate on purpose: that is the function
// app/integrations_identity.go calls, and a.fatal() on its error is what turns
// this into a refusal to start. A rule enforced only on the leaf type would be
// a rule nothing consults.
func TestFiveOfSixRefusesTheIdentityNodeAtBoot(t *testing.T) {
	// The private key is the one missing value, chosen deliberately: it is the
	// one the IDENTITY node never reads. If the refusal were written from
	// "what does this node need", this is the case it would wave through --
	// and the cluster would then connect people to a grant the engine cannot
	// fetch under, failing in the background where nobody is watching.
	githubAppEnv(t, "123456", "memql-example", "Iv1.exampleclientid",
		"example-client-secret", "", "example-webhook-secret")
	cfg := bootableConfig(t)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("five of six booted. The node comes up, the OS shows Connect, and every person " +
			"who presses it fails -- which reports the operator's mistake to everybody except " +
			"the operator.")
	}
	msg := err.Error()
	if !strings.Contains(msg, githubconnect.EnvPrivateKeyB64) {
		t.Errorf("the boot refusal does not name the missing value %s:\n  %s",
			githubconnect.EnvPrivateKeyB64, msg)
	}
	for _, present := range []string{
		githubconnect.EnvAppID, githubconnect.EnvAppSlug, githubconnect.EnvClientID,
		githubconnect.EnvClientSecret, githubconnect.EnvWebhookSecret,
	} {
		if !strings.Contains(msg, present) {
			t.Errorf("the boot refusal does not name the present value %s:\n  %s", present, msg)
		}
	}
}

// TestOneOfSixRefusesToo pins the other end of the partial range. One value is
// the likeliest half-setup -- somebody pasted the app id and went to find the
// rest -- and it must fail the same way rather than reading as "nearly none".
func TestOneOfSixRefusesToo(t *testing.T) {
	githubAppEnv(t, "123456", "", "", "", "", "")
	cfg := bootableConfig(t)

	if err := cfg.Validate(); err == nil {
		t.Fatal("one of six booted; the rule is all six or none, not a majority")
	}
}
