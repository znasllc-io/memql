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
		Proof:             "ok",
		ProofAt:           expires.Add(-59 * time.Minute),
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
		// The line that makes "accepted" mean "usable": the vendor's API took
		// the bearer, and when.
		"took the bearer", "2026-08-23T17:05:11Z",
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
		Provider:        "chat54Mini",
		Vendor:          "openai",
		Type:            "OpenAI",
		CredentialPath:  "unavailable",
		Recorded:        true,
		ExchangeOutcome: "notConfigured",
		Detail:          "no federation ids are set for this vendor on this node",
	})
	out := buf.String()

	if !strings.Contains(out, "unavailable") {
		t.Errorf("the report does not name the credential path:\n%s", out)
	}
	if !strings.Contains(out, "federation is not configured") {
		t.Errorf("the report does not say WHY there is no credential:\n%s", out)
	}
	// It must not read as a fault: every cluster is installed in this state.
	if !strings.Contains(out, "normal state of a cluster nobody has set a vendor up on") {
		t.Errorf("the report does not say this state is normal:\n%s", out)
	}
	// And it must not tell a local cluster's operator that nothing can be done:
	// a local cluster federates by pasted keys (design 2026-11-21, A6).
	if strings.Contains(out, "cannot be federated") {
		t.Errorf("the report still says a local cluster cannot federate:\n%s", out)
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
	// A vendor nobody set up is not success: the installer read exit 0 as
	// "federation verified". It is not a refusal either, and not "try again".
	notSetUp := fmt.Errorf("%w: no federation ids are set for this vendor on this node", memql.ErrVendorNotSetUp)
	if got := providerAuthExitCode(notSetUp); got != 3 {
		t.Errorf("a vendor nobody set up exits %d, want 3", got)
	}
}

// The last line is what an operator reads first, and what a person skimming a
// terminal takes away. Each answer has words of its own.
func TestProviderAuthVerdictLine(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		wantCode int
		want     string
		absent   string
	}{
		"verified":     {nil, 0, "provider-auth check: OK", "FAILED"},
		"refused":      {errors.New("the vendor refused the engine's sign-in (status 401): match_subject_prefix"), 1, "FAILED: the vendor refused", "NOT"},
		"not set up":   {fmt.Errorf("%w: no federation ids are set for this vendor on this node", memql.ErrVendorNotSetUp), 3, "NOT SET UP", "FAILED"},
		"not verified": {fmt.Errorf("%w: nothing recorded", memql.ErrSignInNotVerified), 4, "NOT VERIFIED YET", "FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			code := writeProviderAuthVerdict(&buf, memql.ProviderAuthReport{Vendor: "openai"}, tc.err)
			out := buf.String()
			if code != tc.wantCode {
				t.Errorf("exit code %d, want %d", code, tc.wantCode)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the verdict does not say %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.absent) {
				t.Errorf("the verdict says %q:\n%s", tc.absent, out)
			}
			// Only a refusal sends the reader to the runbook, and to the right one.
			if gotRunbook := strings.Contains(out, "openai-federation.md"); gotRunbook != (tc.wantCode == 1) {
				t.Errorf("runbook named = %v for exit code %d:\n%s", gotRunbook, tc.wantCode, out)
			}
		})
	}
}

// An accepted sign-in is a bearer. The report says, on a line of its own,
// whether the vendor's API has taken it.
func TestProviderAuthReportSaysWhetherTheApiTookTheBearer(t *testing.T) {
	base := memql.ProviderAuthReport{
		Provider: "claudeSonnet", Vendor: "anthropic", CredentialPath: "federation",
		Recorded: true, ExchangeOutcome: "ok",
	}
	dropped := time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		mutate func(*memql.ProviderAuthReport)
		want   []string
	}{
		"refused": {func(r *memql.ProviderAuthReport) {
			r.Proof, r.ProofStatus, r.ProofDetail = "refused", 403, "permission_error"
		}, []string{"refused the bearer", "403", "permission_error"}},
		"no answer": {func(r *memql.ProviderAuthReport) {
			r.Proof, r.ProofDetail = "error", "context deadline exceeded"
		}, []string{"gave no answer", "context deadline exceeded"}},
		"not used yet": {func(*memql.ProviderAuthReport) {}, []string{"nothing has used the bearer yet"}},
		"dropped": {func(r *memql.ProviderAuthReport) {
			r.Proof, r.BearerDroppedAt = "ok", dropped
		}, []string{"bearerDropped", "2026-11-22T10:30:00Z"}},
	} {
		t.Run(name, func(t *testing.T) {
			report := base
			tc.mutate(&report)
			var buf bytes.Buffer
			writeProviderAuthReport(&buf, report)
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the report does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

// A bearer on a node with no traffic expires and is not renewed until a call
// needs it. The report says that, not "in -37m0s".
func TestProviderAuthReportDoesNotCountDownAnExpiredBearer(t *testing.T) {
	var buf bytes.Buffer
	expired := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider: "claudeSonnet", Vendor: "anthropic", CredentialPath: "federation",
		Recorded: true, ExchangeOutcome: "ok", Proof: "ok",
		TokenExpiresAt: expired, TokenExpiresIn: -37 * time.Minute,
	})
	out := buf.String()
	if strings.Contains(out, "in -") {
		t.Errorf("the report counts down a bearer that has expired:\n%s", out)
	}
	if !strings.Contains(out, "expired") || !strings.Contains(out, "signs in again") {
		t.Errorf("the report does not say the bearer expired and what happens next:\n%s", out)
	}
}

// With nothing recorded and no ids in this process's environment, the command
// does not know whether the vendor is set up: ids applied through Settings are
// the engine's to see. It must not print the "nobody set this up" sentence.
func TestProviderAuthReportClaimsNothingWithoutARecord(t *testing.T) {
	var buf bytes.Buffer
	writeProviderAuthReport(&buf, memql.ProviderAuthReport{
		Provider: "chat54Mini", Vendor: "openai", Type: "OpenAI", CredentialPath: "unavailable",
	})
	out := buf.String()
	if strings.Contains(out, "federation is not configured") {
		t.Errorf("with nothing recorded the report still claims the vendor is not configured:\n%s", out)
	}
	for _, want := range []string{"recorded nothing", "cannot say whether"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
}

// What the engine itself said about a vendor it holds no sign-in for is
// printed in its own words: a node type that signs in to nothing, and ids the
// engine refuses before any vendor is asked.
func TestProviderAuthReportCarriesTheEnginesOwnReason(t *testing.T) {
	t.Run("a node that signs in to nothing", func(t *testing.T) {
		var buf bytes.Buffer
		writeProviderAuthReport(&buf, memql.ProviderAuthReport{
			Provider: "claudeSonnet", Vendor: "anthropic", CredentialPath: "unavailable",
			Recorded: true, ExchangeOutcome: "notUsed",
			Detail: "this node type calls no models and does not sign in to a vendor",
		})
		out := buf.String()
		if !strings.Contains(out, "calls no models") {
			t.Errorf("the report does not say why this node has no sign-in:\n%s", out)
		}
		if strings.Contains(out, "nobody has set a vendor up") {
			t.Errorf("the report tells a node that signs in to nothing that nobody set the vendor up:\n%s", out)
		}
		// There is no sign-in to describe, and the report must not describe one.
		if strings.Contains(out, "engine's own sign-in") {
			t.Errorf("the report describes a sign-in this engine never made:\n%s", out)
		}
		if !strings.Contains(out, "what the engine in this pod recorded") || !strings.Contains(out, "contacts no vendor") {
			t.Errorf("the report does not say whose words these are:\n%s", out)
		}
	})
	t.Run("ids the engine refuses", func(t *testing.T) {
		var buf bytes.Buffer
		writeProviderAuthReport(&buf, memql.ProviderAuthReport{
			Provider: "claudeSonnet", Vendor: "anthropic", CredentialPath: "federation",
			Recorded: true, ExchangeOutcome: "misconfigured",
			Detail: "HALF-CONFIGURED: MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID set, MEMQL_AI_ANTHROPIC_ORGANIZATION_ID missing",
		})
		out := buf.String()
		for _, want := range []string{"engineSaid", "MEMQL_AI_ANTHROPIC_ORGANIZATION_ID missing"} {
			if !strings.Contains(out, want) {
				t.Errorf("the report does not mention %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "vendorSaid") {
			t.Errorf("the engine's own refusal is printed as something the vendor said:\n%s", out)
		}
	})
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
	// Two things make a refusal a possible repeat: a container that restarted in
	// its pod, and an attempt of this process the vendor never answered. The
	// note covers both and does not claim the first when it was the second.
	if strings.Contains(out, "issued before this engine process started") {
		t.Errorf("the note still says only a restart can explain the refusal:\n%s", out)
	}
	for _, want := range []string{"denied", "jti_reused", "already presented", "2026-11-22T10:08:00Z"} {
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
