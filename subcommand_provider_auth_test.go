package main

import (
	"bytes"
	"errors"
	"fmt"
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
		Recorded:          true,
		SignInAt:          expires.Add(-time.Hour),
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
		// Said every time: the reader must know whose sign-in this is, and
		// that running the command did not spend the pod's identity token.
		"engine's own sign-in",
		"contacts no vendor",
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

// TestProviderAuthReportUnavailablePath replaces TestProviderAuthReportAPIKeyPath.
//
// The api-key path is gone (epic memql#5088): there is no manually entered
// vendor key anywhere, so a node has either federation or nothing. Reporting
// "unavailable" is what the command must do on a cluster whose runbook is
// unfinished and on EVERY local cluster, and it is the most likely reading the
// command ever produces -- so the sentence has to be right and has to not read
// as a fault.
func TestProviderAuthReportUnavailablePath(t *testing.T) {
	var buf bytes.Buffer
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider:       "chat54Mini",
		Vendor:         "openai",
		Type:           "OpenAI",
		CredentialPath: "unavailable",
	})
	out := buf.String()

	if !strings.Contains(out, "unavailable") {
		t.Errorf("the report does not name the credential path:\n%s", out)
	}
	if !strings.Contains(out, "federation is not configured") {
		t.Errorf("the report does not say WHY there is no credential:\n%s", out)
	}
	// It must not read as a fault: a local cluster is in this state
	// permanently and by design.
	if !strings.Contains(out, "normal state of a local cluster") {
		t.Errorf("the report does not say this state is normal:\n%s", out)
	}
	// Federation-only rows must not appear as empty labels.
	for _, absent := range []string{"federationRuleId", "tokenSubject", "exchange:", "workspaceId"} {
		if strings.Contains(out, absent) {
			t.Errorf("the unavailable report carries the federation-only row %q:\n%s", absent, out)
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

// A refusal the vendor gave is exit 1, which the installer reads as "the
// federation configuration is wrong". Anything the check cannot say yet must
// not share that code, or a node that has only just started would be reported
// to an operator as a broken rule.
func TestProviderAuthExitCodes(t *testing.T) {
	if got := providerAuthExitCode(nil); got != 0 {
		t.Errorf("an accepted sign-in exits %d, want 0", got)
	}
	if got := providerAuthExitCode(errors.New("the vendor refused the engine's sign-in (status 401): match_subject_prefix")); got != 1 {
		t.Errorf("a refusal exits %d, want 1", got)
	}
	notYet := fmt.Errorf("%w: nothing recorded", memql.ErrSignInNotVerified)
	if got := providerAuthExitCode(notYet); got != 4 {
		t.Errorf("an unverified sign-in exits %d, want 4", got)
	}
}

// A report with no record says so, and still prints the configuration.
func TestProviderAuthReportWithNothingRecorded(t *testing.T) {
	var buf bytes.Buffer
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider:         "claudeSonnet",
		Vendor:           "anthropic",
		CredentialPath:   "federation",
		FederationRuleID: "fdrl_abc",
		TokenSubject:     "system:serviceaccount:memql:memql-engine",
	})
	out := buf.String()
	for _, want := range []string{"fdrl_abc", "nothing recorded by the engine", "concept storage", "contacts no vendor"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report with no record does not mention %q:\n%s", want, out)
		}
	}
}

// A refusal that a restart alone explains is reported as such, with the time
// the next identity token is due.
func TestProviderAuthReportExplainsAPossibleReplay(t *testing.T) {
	var buf bytes.Buffer
	due := time.Date(2026, 11, 22, 10, 8, 0, 0, time.UTC)
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider:        "claudeSonnet",
		Vendor:          "anthropic",
		CredentialPath:  "federation",
		Recorded:        true,
		ExchangeOutcome: "denied",
		Detail:          "jti_reused",
		MayBeReplay:     true,
		NextTokenDue:    due,
	})
	out := buf.String()
	for _, want := range []string{"denied", "jti_reused", "issued before this engine process started", "2026-11-22T10:08:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
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
