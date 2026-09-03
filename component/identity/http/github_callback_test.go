package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// github_callback_test.go -- the acceptance criteria of epic memql#4912's
// identity half, section F:
//
//	"The callback against a fake GitHub token endpoint: a valid state, an
//	 expired one, a replayed one, a code exchange that fails, an installation
//	 setup landing; each writes or refuses exactly as stated."
//
// THE ENGINE IS FAKED AND GITHUB IS FAKED, and the boundary between them is
// where the assertions live: every test below asserts on the MemQL statements
// the handler issued, because "wrote nothing" is the property four of the five
// cases are about, and a store that merely returned an error would satisfy a
// test that only checked the redirect.
//
// The fake answers the four constructs the flow uses and records anything else
// in `unknown`, so a construct the handler starts issuing without this file
// learning about it fails loudly instead of silently returning zero rows --
// which would read as "no such state" and make the refusal tests pass for the
// wrong reason.

// -----------------------------------------------------------------------------
// The fake engine
// -----------------------------------------------------------------------------

type githubFakeEngine struct {
	mu sync.Mutex

	// state is the one connect-state row, or nil for "no such state".
	state map[string]string
	// grant is the caller's existing grant row, or nil for "none yet".
	grant map[string]string

	consumes int
	creates  int
	updates  int
	// statements is every MemQL string the handler issued, in order. The
	// no-token-leak test greps it.
	statements []string
	unknown    []string
}

var (
	fakeStateByHashRe  = regexp.MustCompile(`^query githubConnectStateByHash\(stateHash: "([^"]*)"\)$`)
	fakeStateConsumeRe = regexp.MustCompile(`^mutation consumeGithubConnectState\(stateId: "([^"]*)"`)
	fakeGrantByExtRe   = regexp.MustCompile(`^query githubAppGrantByExternalId\(externalId: "([^"]*)"\)$`)
	fakeGrantCreateRe  = regexp.MustCompile(`^mutation createGithubAppGrant\(`)
	fakeGrantUpdateRe  = regexp.MustCompile(`^mutation updateGithubAppGrant\(`)
	fakeClusterSetsRe  = regexp.MustCompile(`^query clusterSettingsCurrent\(\)$`)
)

func (f *githubFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statements = append(f.statements, q)

	switch {
	case fakeStateByHashRe.MatchString(q):
		if f.state == nil {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return bundleOf(f.state), nil

	case fakeStateConsumeRe.MatchString(q):
		f.consumes++
		if f.state != nil {
			f.state["consumedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case fakeGrantByExtRe.MatchString(q):
		if f.grant == nil {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return bundleOf(f.grant), nil

	case fakeGrantCreateRe.MatchString(q):
		f.creates++
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case fakeGrantUpdateRe.MatchString(q):
		f.updates++
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case fakeClusterSetsRe.MatchString(q):
		// No /setup wizard has run, so the OS origin is derived from the
		// identity base URL. Answering "no row" is the state most clusters are
		// in for this read.
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}

	f.unknown = append(f.unknown, q)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (f *githubFakeEngine) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates + f.updates
}

func bundleOf(row map[string]string) *memqlengine.ExecuteResult {
	fields := map[string]*structpb.Value{}
	for k, v := range row {
		if v == "" {
			continue
		}
		fields[k] = structpb.NewStringValue(v)
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{
			Id:      row["id"],
			Payload: &structpb.Struct{Fields: fields},
		}},
	}}
}

// -----------------------------------------------------------------------------
// The fake GitHub
// -----------------------------------------------------------------------------

// fakeGitHub answers the three calls the callback makes. `tokenStatus` and
// `tokenBody` let one test make the exchange fail the way GitHub really does:
// an HTTP 200 carrying an `error` key.
type fakeGitHub struct {
	tokenStatus int
	tokenBody   string
	userBody    string
	instBody    string

	tokenHits int
	userHits  int
	instHits  int
}

const (
	testAccessToken  = "gho_testaccesstoken0000000000000000000000"
	testRefreshToken = "ghr_testrefreshtoken000000000000000000000"
)

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		tokenStatus: 200,
		tokenBody: `{"access_token":"` + testAccessToken + `","refresh_token":"` + testRefreshToken +
			`","token_type":"bearer","expires_in":28800,"refresh_token_expires_in":15811200}`,
		userBody: `{"login":"octocat","id":583231}`,
		instBody: `{"total_count":2,"installations":[{"id":11111},{"id":22222}]}`,
	}
}

func (g *fakeGitHub) start(t *testing.T) *githubconnect.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			g.tokenHits++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(g.tokenStatus)
			_, _ = w.Write([]byte(g.tokenBody))
		case r.URL.Path == "/user":
			g.userHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(g.userBody))
		case r.URL.Path == "/user/installations":
			g.instHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(g.instBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &githubconnect.Client{OAuthBaseURL: srv.URL, APIBaseURL: srv.URL}
}

// -----------------------------------------------------------------------------
// The harness
// -----------------------------------------------------------------------------

type githubAuditRecorder struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (r *githubAuditRecorder) Log(_ context.Context, ev identity.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *githubAuditRecorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Action)
	}
	return out
}

const testStateValue = "the-plaintext-state-value"

// newGitHubCallbackServer wires a Server whose whole GitHub Connect path is
// under test: a configured app, a fake engine, a fake GitHub, a master key so
// the seal is real rather than stubbed, and a slog sink so the leak scan can
// read the log stream as well as the writes.
func newGitHubCallbackServer(t *testing.T, eng *githubFakeEngine, gh *fakeGitHub) (*Server, *githubAuditRecorder, *bytes.Buffer) {
	t.Helper()
	// 64 hex characters -- component/secret's master key. Built rather than
	// written as one literal for the reason the signing-key fixtures are: a
	// high-entropy-looking constant in source is what a secret scanner hunts
	// for, and history is append-only.
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))

	audit := &githubAuditRecorder{}
	// DEBUG level, deliberately: a leak scan that read only warnings would
	// miss the line a future debug statement adds, which is the shape this
	// test exists to catch.
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := &Server{
		Cfg: identity.Config{
			BaseURL: "https://identity.example.test",
			GitHubApp: githubconnect.Config{
				AppID:         "123456",
				AppSlug:       "memql-example",
				ClientID:      "Iv1.exampleclientid",
				ClientSecret:  "example-client-secret",
				PrivateKeyB64: "LS0tLS1CRUdJTiBFWEFNUExF",
				WebhookSecret: "example-webhook-secret",
			},
		},
		Store:        &identity.Store{Engine: eng, Logger: logger},
		Audit:        audit,
		Logger:       logger,
		GitHubClient: gh.start(t),
	}
	return s, audit, logs
}

// liveState is a connect state that has not been consumed and has not expired.
func liveState() map[string]string {
	return map[string]string{
		"id":         "v1:identity:githubConnectState:live",
		"userId":     "v1:identity:user:asked",
		"stateHash":  identity.HashConnectState(testStateValue),
		"returnPath": "/packages/new",
		"expiresAt":  time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
	}
}

func callbackRequest(query string) *http.Request {
	r := httptest.NewRequest("GET", "https://identity.example.test/auth/github/callback?"+query, nil)
	// The binary sits behind a TLS-terminating ingress in every environment
	// MemQL runs in, so this is what a real secure request looks like here.
	r.Header.Set("X-Forwarded-Proto", "https")
	return r
}

func run(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGitHubCallback(rec, callbackRequest(query))
	return rec
}

func assertNoUnknownConstructs(t *testing.T, eng *githubFakeEngine) {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.unknown) > 0 {
		t.Fatalf("the handler issued constructs this fake does not model: %s.\n"+
			"An unmodelled construct silently returns zero rows, which reads as 'no such row' and "+
			"would make the refusal tests below pass for the wrong reason.",
			strings.Join(eng.unknown, "; "))
	}
}

// -----------------------------------------------------------------------------
// A valid state writes exactly one owned row
// -----------------------------------------------------------------------------

func TestValidStateWritesExactlyOneOwnedGrant(t *testing.T) {
	eng := &githubFakeEngine{state: liveState()}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "code=the-code&state="+testStateValue)
	assertNoUnknownConstructs(t, eng)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://os.example.test/packages/new?") {
		t.Errorf("Location = %q; the browser must land back in the OS at the return path the "+
			"begin call stored, on the os.<domain> origin derived from this service's own base URL", loc)
	}
	if !strings.Contains(loc, "github=connected") {
		t.Errorf("Location = %q, want the connected marker", loc)
	}

	if eng.creates != 1 || eng.updates != 0 {
		t.Fatalf("creates=%d updates=%d, want exactly one create", eng.creates, eng.updates)
	}
	if eng.consumes != 1 {
		t.Errorf("consumes=%d, want 1 -- the state is single-use", eng.consumes)
	}

	create := findStatement(t, eng, "mutation createGithubAppGrant(")
	for _, want := range []string{
		`login: "octocat"`,
		`externalId: "583231"`,
		`host: "github.com"`,
		`installationIds: ["11111", "22222"]`,
	} {
		if !strings.Contains(create, want) {
			t.Errorf("the grant write is missing %s:\n  %s", want, create)
		}
	}
	// The GRANT is stamped from the STATE ROW's user, never from anything the
	// request carried. The mutation stamps ownerUserId from the actor, so what
	// this asserts is that the call ran under one -- the write itself never
	// names the field.
	if gh.tokenHits != 1 || gh.userHits != 1 || gh.instHits < 1 {
		t.Errorf("GitHub calls: token=%d user=%d installations=%d, want 1/1/>=1",
			gh.tokenHits, gh.userHits, gh.instHits)
	}

	if got := audit.actions(); len(got) != 1 || got[0] != "github_connected" {
		t.Errorf("audit actions = %v, want exactly [github_connected]", got)
	}
	if audit.events[0].TargetType != githubGrantTargetType {
		t.Errorf("audit targetType = %q, want %q", audit.events[0].TargetType, githubGrantTargetType)
	}
	if audit.events[0].ActorUserId != "v1:identity:user:asked" {
		t.Errorf("audit actorUserId = %q, want the state row's user", audit.events[0].ActorUserId)
	}
}

// TestAReconnectUpdatesInPlace pins the other half of the insert-versus-update
// decision: a person who reconnects must not accumulate one grant per connect.
func TestAReconnectUpdatesInPlace(t *testing.T) {
	eng := &githubFakeEngine{
		state: liveState(),
		grant: map[string]string{
			"id":          "v1:platform:sourceCredential:existing",
			"ownerUserId": "v1:identity:user:asked",
			"externalId":  "583231",
			"kind":        "github_app",
		},
	}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "code=the-code&state="+testStateValue)
	assertNoUnknownConstructs(t, eng)

	if eng.creates != 0 || eng.updates != 1 {
		t.Fatalf("creates=%d updates=%d, want exactly one update -- a reconnect keyed on externalId "+
			"updates the existing row rather than minting a second grant to choose between",
			eng.creates, eng.updates)
	}
	update := findStatement(t, eng, "mutation updateGithubAppGrant(")
	if !strings.Contains(update, `credentialId: "v1:platform:sourceCredential:existing"`) {
		t.Errorf("the update does not name the existing row:\n  %s", update)
	}
	if !strings.Contains(rec.Header().Get("Location"), "github=reconnected") {
		t.Errorf("Location = %q, want the reconnected marker", rec.Header().Get("Location"))
	}
	if got := audit.actions(); len(got) != 1 || got[0] != "github_reconnected" {
		t.Errorf("audit actions = %v, want exactly [github_reconnected]", got)
	}
}

// -----------------------------------------------------------------------------
// The four refusals, and what each one must NOT write
// -----------------------------------------------------------------------------

// TestAnExpiredStateIsRefusedAndWritesNothing.
//
// "Writes nothing" is the assertion, not "redirects with an error": a handler
// that refused after landing a grant would satisfy a redirect-only test and
// leave an authorization row this cluster is not entitled to.
func TestAnExpiredStateIsRefusedAndWritesNothing(t *testing.T) {
	st := liveState()
	st["expiresAt"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	eng := &githubFakeEngine{state: st}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "code=the-code&state="+testStateValue)
	assertNoUnknownConstructs(t, eng)

	assertRefused(t, rec, eng, gh, audit, "state_expired")
}

// TestAReplayedStateIsRefusedAndWritesNothing -- the second click on a
// callback URL. The row exists and is already spent.
func TestAReplayedStateIsRefusedAndWritesNothing(t *testing.T) {
	st := liveState()
	st["consumedAt"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	eng := &githubFakeEngine{state: st}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "code=the-code&state="+testStateValue)
	assertNoUnknownConstructs(t, eng)

	assertRefused(t, rec, eng, gh, audit, "state_already_consumed")
}

// TestAnUnknownStateIsRefusedAndWritesNothing -- a forged or stale value this
// cluster never issued.
func TestAnUnknownStateIsRefusedAndWritesNothing(t *testing.T) {
	eng := &githubFakeEngine{state: nil}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "code=the-code&state=some-value-nobody-issued")
	assertNoUnknownConstructs(t, eng)

	assertRefused(t, rec, eng, gh, audit, "state_unknown")
}

// assertRefused is the shared shape of the three state refusals: the redirect
// carries connect_state_invalid, NOTHING was written, and GITHUB WAS NEVER
// CALLED -- the state is consumed before the exchange precisely so a replayed
// callback cannot reach it.
func assertRefused(t *testing.T, rec *httptest.ResponseRecorder, eng *githubFakeEngine,
	gh *fakeGitHub, audit *githubAuditRecorder, wantReason string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "github=connect_state_invalid") {
		t.Errorf("Location = %q, want the connect_state_invalid marker", rec.Header().Get("Location"))
	}
	if n := eng.writes(); n != 0 {
		t.Errorf("%d grant write(s) reached the engine on a refused callback, want 0", n)
	}
	if gh.tokenHits != 0 {
		t.Errorf("the code was exchanged %d time(s) on a refused callback. The state is consumed "+
			"BEFORE the exchange so a replay cannot reach GitHub at all", gh.tokenHits)
	}
	acts := audit.actions()
	if len(acts) != 1 || acts[0] != "github_connect_refused" {
		t.Fatalf("audit actions = %v, want exactly [github_connect_refused]", acts)
	}
	if got := audit.events[0].Detail["reason"]; got != wantReason {
		t.Errorf("audit reason = %v, want %q. The three refusals share one code for the person and "+
			"stay apart in the trail: expired is somebody who walked away, consumed is a replay, "+
			"unknown is a value never issued", got, wantReason)
	}
	if audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Errorf("audit outcome = %q, want failure", audit.events[0].Outcome)
	}
}

// TestAFailedCodeExchangeWritesNothing.
//
// GitHub answers a bad verification code with HTTP 200 and an `error` key, so
// this is also the regression test for a status-only check: that would read the
// refusal as a success and seal an empty token onto a real grant row.
func TestAFailedCodeExchangeWritesNothing(t *testing.T) {
	eng := &githubFakeEngine{state: liveState()}
	gh := newFakeGitHub()
	gh.tokenStatus = 200
	gh.tokenBody = `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "code=the-code&state="+testStateValue)
	assertNoUnknownConstructs(t, eng)

	if n := eng.writes(); n != 0 {
		t.Errorf("%d grant write(s) after a failed exchange, want 0 -- there must be no half-grant "+
			"claiming an authorization this cluster does not hold", n)
	}
	if eng.consumes != 1 {
		t.Errorf("consumes=%d; the state is spent even when the exchange fails, because it was "+
			"single-use and this WAS its use", eng.consumes)
	}
	if !strings.Contains(rec.Header().Get("Location"), "github=exchange_failed") {
		t.Errorf("Location = %q, want the exchange_failed marker", rec.Header().Get("Location"))
	}
	acts := audit.actions()
	if len(acts) != 1 || acts[0] != "github_connect_failed" {
		t.Fatalf("audit actions = %v, want exactly [github_connect_failed]", acts)
	}
}

// -----------------------------------------------------------------------------
// The setup landing
// -----------------------------------------------------------------------------

// TestASetupLandingWithNoStateUpdatesNothing is the case GitHub's own
// documentation warns about: `installation_id` on this URL can be supplied by
// anyone who visits it. A landing with no state is somebody who installed the
// app from its GitHub page, which is not an error and must not read as one.
func TestASetupLandingWithNoStateUpdatesNothing(t *testing.T) {
	eng := &githubFakeEngine{state: liveState()}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)

	rec := run(t, s, "installation_id=99999&setup_action=install")
	assertNoUnknownConstructs(t, eng)

	if n := eng.writes(); n != 0 {
		t.Errorf("%d write(s) from an unauthenticated setup landing, want 0. installation_id on "+
			"this URL is a value anyone who can open it supplies; acting on it would let them "+
			"attach an installation to a grant they chose", n)
	}
	if eng.consumes != 0 {
		t.Errorf("consumes=%d; a landing that names no state must spend none", eng.consumes)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "github=installed") {
		t.Errorf("Location = %q, want the neutral installed marker -- the installation SUCCEEDED, "+
			"and rendering a failure for it would send the person hunting a problem that is not there", loc)
	}
	acts := audit.actions()
	if len(acts) != 1 || acts[0] != "github_connect_setup_landing" {
		t.Fatalf("audit actions = %v, want exactly [github_connect_setup_landing]", acts)
	}
	if audit.events[0].Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("a setup landing is audited as %q; it is not a failure", audit.events[0].Outcome)
	}
}

// TestASetupLandingWithAValidStateUpdatesInstallationIds.
//
// The acceptance criterion says "a setup landing updates installation ids".
// It does so through the ordinary connect path -- GitHub sends `code` +
// `state` on the setup URL when the flow began here, and the grant write
// re-reads GET /user/installations, so the ids on the row are whatever GitHub
// says they are now. That is what makes an installation somebody just added
// reachable without a second flow.
func TestASetupLandingWithAValidStateUpdatesInstallationIds(t *testing.T) {
	eng := &githubFakeEngine{
		state: liveState(),
		grant: map[string]string{
			"id":              "v1:platform:sourceCredential:existing",
			"ownerUserId":     "v1:identity:user:asked",
			"externalId":      "583231",
			"kind":            "github_app",
			"installationIds": "11111",
		},
	}
	gh := newFakeGitHub()
	gh.instBody = `{"total_count":3,"installations":[{"id":11111},{"id":22222},{"id":33333}]}`
	s, _, _ := newGitHubCallbackServer(t, eng, gh)

	run(t, s, "code=the-code&state="+testStateValue+"&installation_id=33333&setup_action=install")
	assertNoUnknownConstructs(t, eng)

	update := findStatement(t, eng, "mutation updateGithubAppGrant(")
	if !strings.Contains(update, `installationIds: ["11111", "22222", "33333"]`) {
		t.Errorf("the installation ids were not replaced with what GitHub reports:\n  %s\n"+
			"The list is REPLACED rather than merged, because a merge could never remove one -- "+
			"and an installation that stays after it was uninstalled turns a clear "+
			"repository_not_installed into an unexplained 404", update)
	}
}

// -----------------------------------------------------------------------------
// No token in any log line or reply
// -----------------------------------------------------------------------------

// TestNoTokenReachesAStatementOrAnAuditRow is the memql#4912 section E
// property: "the reply to every capability carries login and fingerprint at
// most".
//
// IT CARRIES ITS OWN POSITIVE CONTROL. A grep that finds nothing proves
// nothing until the instrument could have moved: the control asserts that the
// SEALED form of the same token IS present in the create statement, so a test
// that scanned an empty corpus, or scanned for a token the fake never issued,
// fails here rather than passing silently.
func TestNoTokenReachesAStatementOrAnAuditRow(t *testing.T) {
	eng := &githubFakeEngine{state: liveState()}
	gh := newFakeGitHub()
	// An installations failure so the WARN path really runs: a log stream with
	// nothing in it is a corpus a grep cannot fail on.
	gh.instBody = "not json"
	s, audit, logs := newGitHubCallbackServer(t, eng, gh)

	run(t, s, "code=the-code&state="+testStateValue)
	assertNoUnknownConstructs(t, eng)

	create := findStatement(t, eng, "mutation createGithubAppGrant(")

	// THE POSITIVE CONTROL. Ciphertext for the access token really is in the
	// write, so the corpus below is non-empty and this test can fail.
	if !strings.Contains(create, `encryptedValue: "`) || !strings.Contains(create, `refreshToken: "`) {
		t.Fatalf("the grant write carries no sealed token at all, so the leak scan below has "+
			"nothing to scan and would pass vacuously:\n  %s", create)
	}
	if !strings.Contains(create, `fingerprint: "...0000"`) {
		t.Errorf("the fingerprint is not the token's last four characters:\n  %s", create)
	}

	eng.mu.Lock()
	corpus := strings.Join(eng.statements, "\n")
	eng.mu.Unlock()
	auditJSON, err := json.Marshal(audit.events)
	if err != nil {
		t.Fatalf("marshal audit events: %v", err)
	}
	corpus += "\n" + string(auditJSON) + "\n" + logs.String()

	// THE SECOND POSITIVE CONTROL, for the log half. The installations read
	// was made to fail above so this line exists; without it the slog buffer
	// would be empty and the grep over it would pass whatever the handler
	// logged.
	if !strings.Contains(logs.String(), "could not read the person's installations") {
		t.Fatalf("the log stream carries no line from this flow, so scanning it proves nothing:\n%s",
			logs.String())
	}

	for _, secretValue := range []struct {
		name  string
		value string
	}{
		{"the access token", testAccessToken},
		{"the refresh token", testRefreshToken},
		{"the plaintext connect state", testStateValue},
		{"the OAuth client secret", s.Cfg.GitHubApp.ClientSecret},
	} {
		if strings.Contains(corpus, secretValue.value) {
			t.Errorf("%s appears in a MemQL statement or an audit row.\n"+
				"Tokens are sealed under MEMQL_MASTER_KEY and never stored or logged in the clear; "+
				"the state value exists only inside the authorize URL and as a digest on its row.",
				secretValue.name)
		}
	}
}

// -----------------------------------------------------------------------------
// The route is inert on a cluster with no App
// -----------------------------------------------------------------------------

// TestTheCallbackIs404WithNoAppConfigured pins the OIDC pair's rule: the route
// is registered unconditionally so the route table does not vary with
// configuration, and it 404s when the feature is off.
func TestTheCallbackIs404WithNoAppConfigured(t *testing.T) {
	eng := &githubFakeEngine{state: liveState()}
	gh := newFakeGitHub()
	s, audit, _ := newGitHubCallbackServer(t, eng, gh)
	s.Cfg.GitHubApp = githubconnect.Config{}

	rec := run(t, s, "code=the-code&state="+testStateValue)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on a cluster with no GitHub App", rec.Code)
	}
	if n := eng.writes(); n != 0 || eng.consumes != 0 {
		t.Errorf("an unconfigured cluster touched the engine: writes=%d consumes=%d", n, eng.consumes)
	}
	if len(audit.actions()) != 0 {
		t.Errorf("an unconfigured cluster audited %v", audit.actions())
	}
}

// TestAPlaintextCallbackIsRefused. A code and a state in a plaintext query
// string are a grant anyone on the path can complete, so the transport check
// runs before anything else.
func TestAPlaintextCallbackIsRefused(t *testing.T) {
	eng := &githubFakeEngine{state: liveState()}
	gh := newFakeGitHub()
	s, _, _ := newGitHubCallbackServer(t, eng, gh)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://identity.example.test/auth/github/callback?code=c&state="+testStateValue, nil)
	s.handleGitHubCallback(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 over plaintext", rec.Code)
	}
	if eng.consumes != 0 || eng.writes() != 0 {
		t.Errorf("a plaintext callback reached the engine: consumes=%d writes=%d", eng.consumes, eng.writes())
	}
}

// findStatement returns the one recorded statement starting with prefix.
func findStatement(t *testing.T, eng *githubFakeEngine, prefix string) string {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	for _, q := range eng.statements {
		if strings.HasPrefix(q, prefix) {
			return q
		}
	}
	t.Fatalf("no statement starting %q was issued. Recorded:\n  %s", prefix, strings.Join(eng.statements, "\n  "))
	return ""
}
