package http

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// github_app_callback_test.go -- GitHub's return from creating the cluster's
// app (design record 2026-09-20-github-app-setup, D4).
//
// What this route writes is the DEPLOYMENT's credentials, so the tests are
// mostly about the ways it must refuse -- each asserted with nothing written,
// because a refusal that still stored a key is a refusal in the redirect only.

const (
	appSetupState   = "the-plaintext-app-setup-state"
	appSetupCode    = "c0de0000c0de0000c0de0000c0de0000c0de0000"
	appClientSecret = "callback-test-client-secret-VALUE"
	appHookSecret   = "callback-test-webhook-secret-VALUE"
	appKeyMaterial  = "MIIEcallbacktestkeymaterialVALUE"
)

func appConversionBody(permissions string) string {
	return `{"id":515151,"slug":"memql-on-example-test","name":"MemQL on example.test",` +
		`"html_url":"https://github.com/apps/memql-on-example-test","owner":{"login":"znasllc-io"},` +
		`"client_id":"Iv1.callbacktest","client_secret":"` + appClientSecret + `","webhook_secret":"` + appHookSecret + `",` +
		`"pem":"-----BEGIN RSA PRIVATE KEY-----\n` + appKeyMaterial + `\n-----END RSA PRIVATE KEY-----",` +
		`"permissions":` + permissions + `}`
}

// appFakeEngine models every construct the setup callback issues. Anything else
// lands in `unknown` and fails the test: an unmodelled construct answers zero
// rows, which reads as "no such row" and would let a refusal test pass for the
// wrong reason (githubFakeEngine's rule).
type appFakeEngine struct {
	mu         sync.Mutex
	statements []string
	unknown    []string
	state      map[string]string
	consumed   bool
	user       map[string]string
	failWrites bool
}

func (e *appFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statements = append(e.statements, q)
	row := func(fields map[string]string) *memqlengine.ExecuteResult {
		payload := map[string]*structpb.Value{}
		for k, v := range fields {
			if k == "active" {
				payload[k] = structpb.NewBoolValue(v == "true")
				continue
			}
			payload[k] = structpb.NewStringValue(v)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: fields["id"], Payload: &structpb.Struct{Fields: payload}}},
		}}
	}
	empty := &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}
	switch {
	case strings.HasPrefix(q, "query githubConnectStateByHash("):
		if e.state == nil {
			return empty, nil
		}
		state := map[string]string{}
		for k, v := range e.state {
			state[k] = v
		}
		if e.consumed {
			state["consumedAt"] = time.Now().UTC().Format(time.RFC3339)
		}
		return row(state), nil
	case strings.HasPrefix(q, "mutation consumeGithubConnectState("):
		e.consumed = true
		return empty, nil
	case strings.HasPrefix(q, "query userByIdSystem("):
		if e.user == nil {
			return empty, nil
		}
		return row(e.user), nil
	case strings.HasPrefix(q, "mutation setGlobalVariable("), strings.HasPrefix(q, "mutation setGlobalSecret("):
		if e.failWrites {
			return nil, context.DeadlineExceeded
		}
		return empty, nil
	case strings.HasPrefix(q, "mutation createGithubConnectState("),
		strings.HasPrefix(q, "builtin readinessRecompute()"),
		strings.Contains(q, "clusterSettingsCurrent"):
		return empty, nil
	}
	e.unknown = append(e.unknown, q)
	return empty, nil
}

func (e *appFakeEngine) all(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, q := range e.statements {
		if strings.HasPrefix(q, prefix) {
			out = append(out, q)
		}
	}
	return out
}

func (e *appFakeEngine) storedRows() int {
	return len(e.all("mutation setGlobalVariable(")) + len(e.all("mutation setGlobalSecret("))
}

func liveAppState(over map[string]string) map[string]string {
	row := map[string]string{
		"id":           "v1:identity:githubConnectState:setup",
		"userId":       "v1:identity:user:owner",
		"stateHash":    identity.HashConnectState(appSetupState),
		"returnPath":   "/?connect=deployables",
		"expiresAt":    time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
		"purpose":      githubconnect.PurposeAppSetup,
		"organization": "znasllc-io",
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func ownerRow() map[string]string {
	return map[string]string{"id": "v1:identity:user:owner", "role": "owner", "active": "true", "primaryEmail": "owner@example.test"}
}

type appFakeGitHub struct {
	status int
	body   string
	hits   int
	paths  []string
}

func (g *appFakeGitHub) start(t *testing.T) *githubconnect.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits++
		g.paths = append(g.paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		_, _ = w.Write([]byte(g.body))
	}))
	t.Cleanup(srv.Close)
	return &githubconnect.Client{OAuthBaseURL: srv.URL, APIBaseURL: srv.URL}
}

func newAppCallbackServer(t *testing.T, eng *appFakeEngine, gh *appFakeGitHub) (*Server, *githubAuditRecorder, *bytes.Buffer) {
	t.Helper()
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	// The environment decides nothing here unless a test says so.
	for _, name := range githubconnect.EnvNames() {
		t.Setenv(name, "")
	}
	audit := &githubAuditRecorder{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Server{
		Cfg:          identity.Config{BaseURL: "https://identity.example.test"},
		Store:        &identity.Store{Engine: eng, Logger: logger, GithubGate: githubHTTPUnitGate},
		Audit:        audit,
		Logger:       logger,
		GitHubClient: gh.start(t),
		GitHubApp:    &githubconnect.Resolver{},
	}, audit, logs
}

func runApp(s *Server, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://identity.example.test"+githubconnect.AppSetupCallbackPath+"?"+query, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.handleGitHubAppSetupCallback(rec, r)
	return rec
}

func okGitHub() *appFakeGitHub {
	return &appFakeGitHub{status: http.StatusCreated, body: appConversionBody(`{"contents":"read","metadata":"read"}`)}
}

// ---------------------------------------------------------------------------
// The registration
// ---------------------------------------------------------------------------

func TestARegistrationIsStoredSealedAndTheOwnerIsSentOnToInstall(t *testing.T) {
	eng := &appFakeEngine{state: liveAppState(nil), user: ownerRow()}
	gh := okGitHub()
	s, audit, _ := newAppCallbackServer(t, eng, gh)

	rec := runApp(s, "code="+appSetupCode+"&state="+appSetupState)
	if len(eng.unknown) > 0 {
		t.Fatalf("unmodelled constructs: %v", eng.unknown)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if gh.hits != 1 || gh.paths[0] != "POST /app-manifests/"+appSetupCode+"/conversions" {
		t.Fatalf("GitHub saw %v", gh.paths)
	}
	if eng.storedRows() != 6 {
		t.Fatalf("%d rows stored, want the six", eng.storedRows())
	}
	for _, q := range eng.all("mutation setGlobalSecret(") {
		if !strings.Contains(q, `addedBy: "v1:identity:user:owner"`) {
			t.Errorf("a credential row does not name the owner who registered it:\n  %s", q)
		}
	}
	if len(eng.all("mutation consumeGithubConnectState(")) != 1 {
		t.Error("the setup state was not spent exactly once")
	}
	if len(eng.all("builtin readinessRecompute()")) != 1 {
		t.Error("the githubApp readiness module was not asked to look again")
	}

	// Continue to installation; it cannot mint a grant without a subsequent
	// explicit, session-bound PKCE authorization.
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "github.com" || loc.Path != "/apps/memql-on-example-test/installations/new" {
		t.Fatalf("Location = %q, want the app's installation page", rec.Header().Get("Location"))
	}
	if loc.Query().Get("state") != "" || len(eng.all("mutation createGithubConnectState(")) != 0 {
		t.Fatal("installation must return neutral installed; explicit Connect establishes browser binding and PKCE")
	}

	if got := audit.actions(); len(got) != 1 || got[0] != "github_app_registered" {
		t.Fatalf("audit = %v", got)
	}
	ev := audit.events[0]
	if ev.TargetType != "config" || ev.Category != identity.AuditCategoryConfiguration || ev.ActorUserId != "v1:identity:user:owner" {
		t.Errorf("audit event = %+v", ev)
	}
	if ev.Detail["slug"] != "memql-on-example-test" || ev.Detail["owner"] != "znasllc-io" {
		t.Errorf("audit detail = %v", ev.Detail)
	}
}

// TestNoCredentialReachesAStatementAnAuditRowOrALog. The reply this route
// handles is the most credential-dense in the package, so everything it emits
// is scanned -- with the control that the secrets really were received.
func TestNoAppCredentialReachesAStatementAnAuditRowOrALog(t *testing.T) {
	eng := &appFakeEngine{state: liveAppState(nil), user: ownerRow()}
	gh := okGitHub()
	s, audit, logs := newAppCallbackServer(t, eng, gh)
	rec := runApp(s, "code="+appSetupCode+"&state="+appSetupState)

	var haystack strings.Builder
	eng.mu.Lock()
	haystack.WriteString(strings.Join(eng.statements, "\n"))
	eng.mu.Unlock()
	for _, ev := range audit.events {
		for k, v := range ev.Detail {
			haystack.WriteString(k + "=" + toText(v) + "\n")
		}
	}
	haystack.WriteString(logs.String())
	haystack.WriteString(rec.Header().Get("Location"))
	for name, value := range map[string]string{
		"client secret": appClientSecret, "webhook secret": appHookSecret, "private key": appKeyMaterial, "manifest code": appSetupCode, "setup state": appSetupState,
	} {
		if strings.Contains(haystack.String(), value) {
			t.Errorf("the %s reached a statement, an audit row, a log line or the redirect", name)
		}
	}
	// THE CONTROL: the credentials were stored, sealed, and open back.
	opened := map[string]bool{}
	for _, q := range eng.all("mutation setGlobalSecret(") {
		start := strings.Index(q, `encryptedValue: "`) + len(`encryptedValue: "`)
		end := strings.Index(q[start:], `"`)
		if plain, err := secret.Decrypt(q[start : start+end]); err == nil {
			opened[plain] = true
		}
	}
	if !opened[appClientSecret] || !opened[appHookSecret] {
		t.Fatal("the credentials were not among the sealed values, so the scan above proved nothing")
	}
}

func toText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// The refusals, each with nothing stored
// ---------------------------------------------------------------------------

func TestASetupCallbackRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		query  string
		state  map[string]string
		user   map[string]string
		env    bool
		gh     *appFakeGitHub
		result string
		audit  string
		spent  bool
		asked  int
	}{
		"no state at all": {
			query: "code=" + appSetupCode, state: liveAppState(nil), user: ownerRow(), gh: okGitHub(),
			result: "github_app_setup_state_invalid", audit: "github_app_setup_refused",
		},
		"an unknown state": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: nil, user: ownerRow(), gh: okGitHub(),
			result: "github_app_setup_state_invalid", audit: "github_app_setup_refused",
		},
		"an expired state": {
			query: "code=" + appSetupCode + "&state=" + appSetupState,
			state: liveAppState(map[string]string{"expiresAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}),
			user:  ownerRow(), gh: okGitHub(),
			result: "github_app_setup_state_invalid", audit: "github_app_setup_refused",
		},
		// THE ONE THAT MATTERS MOST. Anybody who can press Connect can mint a
		// connect state; it must not finish a registration, and it must be left
		// unspent for the callback it does belong to.
		"a CONNECT state": {
			query: "code=" + appSetupCode + "&state=" + appSetupState,
			state: liveAppState(map[string]string{"purpose": githubconnect.PurposeConnect}),
			user:  ownerRow(), gh: okGitHub(),
			result: "github_app_setup_state_invalid", audit: "github_app_setup_refused",
		},
		"somebody demoted since they began": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: liveAppState(nil),
			user: map[string]string{"id": "v1:identity:user:owner", "role": "developer", "active": "true"}, gh: okGitHub(),
			result: "github_app_setup_forbidden", audit: "github_app_setup_refused", spent: true,
		},
		"somebody deactivated since they began": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: liveAppState(nil),
			user: map[string]string{"id": "v1:identity:user:owner", "role": "owner", "active": "false"}, gh: okGitHub(),
			result: "github_app_setup_forbidden", audit: "github_app_setup_refused", spent: true,
		},
		"a person whose row cannot be read": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: liveAppState(nil), user: nil, gh: okGitHub(),
			result: "github_app_setup_forbidden", audit: "github_app_setup_refused", spent: true,
		},
		"an environment that manages the app": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: liveAppState(nil), user: ownerRow(), env: true, gh: okGitHub(),
			result: "github_app_managed_by_environment", audit: "github_app_setup_refused", spent: true,
		},
		"no code": {
			query: "state=" + appSetupState, state: liveAppState(nil), user: ownerRow(), gh: okGitHub(),
			result: "github_app_setup_failed", audit: "github_app_setup_failed", spent: true,
		},
		"a code GitHub does not know": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: liveAppState(nil), user: ownerRow(),
			gh:     &appFakeGitHub{status: http.StatusNotFound, body: `{"message":"Not Found"}`},
			result: "github_app_setup_failed", audit: "github_app_setup_failed", spent: true, asked: 1,
		},
		// The owner can edit only the NAME on GitHub's page, so this is not the
		// app this cluster's manifest described.
		"an app with more than was asked for": {
			query: "code=" + appSetupCode + "&state=" + appSetupState, state: liveAppState(nil), user: ownerRow(),
			gh:     &appFakeGitHub{status: http.StatusCreated, body: appConversionBody(`{"contents":"write","metadata":"read"}`)},
			result: "github_app_setup_failed", audit: "github_app_setup_failed", spent: true, asked: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			eng := &appFakeEngine{state: tc.state, user: tc.user}
			s, audit, _ := newAppCallbackServer(t, eng, tc.gh)
			if tc.env {
				t.Setenv(githubconnect.EnvAppID, "123456")
			}
			rec := runApp(s, tc.query)
			if len(eng.unknown) > 0 {
				t.Fatalf("unmodelled constructs: %v", eng.unknown)
			}
			loc := rec.Header().Get("Location")
			if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "https://os.example.test/") || !strings.Contains(loc, "github="+tc.result) {
				t.Errorf("status = %d Location = %q, want a 303 to MemQL OS carrying %s", rec.Code, loc, tc.result)
			}
			if eng.storedRows() != 0 {
				t.Errorf("%d row(s) stored by a callback that refused", eng.storedRows())
			}
			if n := len(eng.all("mutation createGithubConnectState(")); n != 0 {
				t.Errorf("a refused registration chained into a connect (%d state(s) written)", n)
			}
			if spent := len(eng.all("mutation consumeGithubConnectState(")) == 1; spent != tc.spent {
				t.Errorf("state spent = %v, want %v", spent, tc.spent)
			}
			if tc.gh.hits != tc.asked {
				t.Errorf("GitHub was asked %d time(s), want %d", tc.gh.hits, tc.asked)
			}
			if got := audit.actions(); len(got) != 1 || got[0] != tc.audit {
				t.Errorf("audit = %v, want [%s]", got, tc.audit)
			}
		})
	}
}

func TestAReplayedSetupCallbackIsRefused(t *testing.T) {
	eng := &appFakeEngine{state: liveAppState(nil), user: ownerRow()}
	gh := okGitHub()
	s, _, _ := newAppCallbackServer(t, eng, gh)
	if rec := runApp(s, "code="+appSetupCode+"&state="+appSetupState); !strings.Contains(rec.Header().Get("Location"), "github.com/apps/") {
		t.Fatalf("the first callback did not register: %q", rec.Header().Get("Location"))
	}
	rec := runApp(s, "code="+appSetupCode+"&state="+appSetupState)
	if !strings.Contains(rec.Header().Get("Location"), "github=github_app_setup_state_invalid") {
		t.Errorf("the replay's Location = %q", rec.Header().Get("Location"))
	}
	if gh.hits != 1 || eng.storedRows() != 6 {
		t.Errorf("the replay reached GitHub (%d calls) or wrote again (%d rows)", gh.hits, eng.storedRows())
	}
}

// A store that refuses the write is a registration that did not happen, and
// must not be reported as one -- nor chained into an installation of an app
// this cluster does not hold.
func TestARegistrationThatCannotBeStoredIsNotReportedAsOne(t *testing.T) {
	eng := &appFakeEngine{state: liveAppState(nil), user: ownerRow(), failWrites: true}
	s, audit, _ := newAppCallbackServer(t, eng, okGitHub())
	rec := runApp(s, "code="+appSetupCode+"&state="+appSetupState)
	if !strings.Contains(rec.Header().Get("Location"), "github=github_app_setup_failed") {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	if got := audit.actions(); len(got) != 1 || got[0] != "github_app_setup_failed" {
		t.Errorf("audit = %v", got)
	}
	if n := len(eng.all("mutation createGithubConnectState(")); n != 0 {
		t.Error("an unstored registration chained into a connect")
	}
}

func TestAPlaintextSetupCallbackIsRefused(t *testing.T) {
	eng := &appFakeEngine{state: liveAppState(nil), user: ownerRow()}
	gh := okGitHub()
	s, _, _ := newAppCallbackServer(t, eng, gh)
	r := httptest.NewRequest("GET", "http://identity.example.test"+githubconnect.AppSetupCallbackPath+"?code="+appSetupCode+"&state="+appSetupState, nil)
	rec := httptest.NewRecorder()
	s.handleGitHubAppSetupCallback(rec, r)
	if rec.Code != http.StatusForbidden || gh.hits != 0 || eng.storedRows() != 0 {
		t.Errorf("status = %d, GitHub calls = %d, rows = %d", rec.Code, gh.hits, eng.storedRows())
	}
}

func TestBothSetupRoutesAreMounted(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{Cfg: identity.Config{BaseURL: "https://identity.example.test"}, Store: &identity.Store{Engine: &appFakeEngine{}}, Logger: slog.Default()}).Mount(mux)
	r := httptest.NewRequest("GET", "https://identity.example.test"+githubconnect.AppSetupCallbackPath, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET %s answered %d, want the handler's 303 (a 404 means the route is not registered)", githubconnect.AppSetupCallbackPath, rec.Code)
	}
}
