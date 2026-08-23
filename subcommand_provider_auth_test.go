package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// The report is what an operator reads in the middle of a cutover, so what it
// must NOT say matters as much as what it must (memql#4335).

func TestProviderAuthReportFederationPath(t *testing.T) {
	var buf bytes.Buffer
	expires := time.Date(2026, 8, 23, 18, 4, 11, 0, time.UTC)
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider:          "streamClaudeSonnet",
		Type:              "AnthropicStream",
		Model:             "claude-sonnet-4-5",
		CredentialPath:    "federation",
		FederationRuleID:  "fdrl_abc",
		OrganizationID:    "11111111-2222-3333-4444-555555555555",
		ServiceAccountID:  "svac_xyz",
		IdentityTokenFile: "/var/run/secrets/anthropic.com/token",
		TokenSubject:      "system:serviceaccount:memql:memql-engine",
		TokenAudience:     []string{"https://api.anthropic.com"},
		ExchangeOutcome:   "ok",
		TokenExpiresAt:    expires,
		TokenExpiresIn:    time.Hour,
		ModelsListed:      12,
	})
	out := buf.String()

	for _, want := range []string{
		"credential:", "federation",
		"fdrl_abc", "svac_xyz",
		// The subject is what the rule's subject_prefix matches on, so it is
		// the first thing an operator compares against the Console when a
		// denial says match_subject_prefix. Omitting it would make the most
		// common failure the hardest one to diagnose.
		"system:serviceaccount:memql:memql-engine",
		"https://api.anthropic.com",
		"2026-08-23T18:04:11Z",
		"12 models returned",
		"concept storage",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the federation report does not mention %q:\n%s", want, out)
		}
	}

	// An empty workspace id is a legitimate configuration, not a gap; the
	// report says which so nobody "fixes" it into a denial.
	if !strings.Contains(out, "Anthropic picks the rule's workspace") {
		t.Errorf("the report does not explain an empty workspaceId:\n%s", out)
	}
}

func TestProviderAuthReportAPIKeyPath(t *testing.T) {
	var buf bytes.Buffer
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider:       "streamClaudeSonnet",
		Type:           "AnthropicStream",
		CredentialPath: "api-key",
		ModelsListed:   12,
	})
	out := buf.String()

	if !strings.Contains(out, "api-key") {
		t.Errorf("the report does not name the credential path:\n%s", out)
	}
	// Saying so is the point of running it after step 5 of the cutover: a
	// cluster that still reports api-key has something leaning on the key.
	if !strings.Contains(out, "long-lived API key") {
		t.Errorf("the api-key report does not say the credential is long-lived:\n%s", out)
	}
	// Federation-only rows must not appear as empty labels.
	for _, absent := range []string{"federationRuleId", "tokenSubject", "exchange:"} {
		if strings.Contains(out, absent) {
			t.Errorf("the api-key report carries the federation-only row %q:\n%s", absent, out)
		}
	}
}

// TestProviderAuthReportPrintsNoCredential is the one thing this output must
// never do. Every field it renders is an id that NAMES something; the bearer
// the exchange returns is not among them and must not become one.
func TestProviderAuthReportPrintsNoCredential(t *testing.T) {
	var buf bytes.Buffer
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider:         "streamClaudeSonnet",
		CredentialPath:   "federation",
		FederationRuleID: "fdrl_abc",
		ServiceAccountID: "svac_xyz",
		TokenSubject:     "system:serviceaccount:memql:memql-engine",
	})
	out := buf.String()
	for _, secretish := range []string{"sk-ant-", "Bearer ", "eyJ"} {
		if strings.Contains(out, secretish) {
			t.Fatalf("the report contains something credential-shaped (%q):\n%s", secretish, out)
		}
	}
}

// TestProviderAuthDispatch proves the subcommand is reachable, which is the
// half a report test cannot cover: the runbook and verify-provider-key.sh both
// invoke it by name through `kubectl exec`, and an unrouted name exits 0 into
// the server bootstrap instead of failing.
func TestProviderAuthDispatch(t *testing.T) {
	handled, code := dispatchSubcommand([]string{"provider-auth"})
	if !handled {
		t.Fatal("`memql provider-auth` is not routed; it would fall through to the server bootstrap")
	}
	if code != 2 {
		t.Fatalf("bare `provider-auth` exited %d, want 2 (usage)", code)
	}
	handled, code = dispatchSubcommand([]string{"provider-auth", "--help"})
	if !handled || code != 0 {
		t.Fatalf("`provider-auth --help` handled=%v code=%d, want true/0", handled, code)
	}
	handled, code = dispatchSubcommand([]string{"provider-auth", "nonsense"})
	if !handled || code != 2 {
		t.Fatalf("`provider-auth nonsense` handled=%v code=%d, want true/2", handled, code)
	}
}
