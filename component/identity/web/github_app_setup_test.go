package web

import (
	"context"
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// github_app_setup_test.go -- the page that posts the manifest to GitHub
// (design record 2026-09-20-github-app-setup, D4).
//
// It is an HTTP route the owner approved for one narrow job, so what is pinned
// is the narrowness: it serves only to a live setup state, it writes nothing,
// what it posts comes from the cluster and not from the request, and the policy
// it carries admits GitHub and nobody else.

const setupStatePlain = "the-plaintext-setup-state"

// setupEngine answers the two reads the page makes and records everything, so a
// test can assert that nothing was WRITTEN.
type setupEngine struct {
	mu         sync.Mutex
	statements []string
	state      map[string]string
	domain     string
}

func (e *setupEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statements = append(e.statements, q)
	row := func(id string, fields map[string]string) *memqlengine.ExecuteResult {
		payload := map[string]*structpb.Value{}
		for k, v := range fields {
			payload[k] = structpb.NewStringValue(v)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: id, Payload: &structpb.Struct{Fields: payload}}},
		}}
	}
	switch {
	case strings.HasPrefix(q, "query githubConnectStateByHash(") && e.state != nil:
		return row(e.state["id"], e.state), nil
	case strings.Contains(q, "clusterSettingsCurrent") && e.domain != "":
		return row("cluster", map[string]string{"clusterDomain": e.domain}), nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (e *setupEngine) writes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, q := range e.statements {
		if strings.HasPrefix(q, "mutation ") {
			out = append(out, q)
		}
	}
	return out
}

func liveSetupState(over map[string]string) map[string]string {
	row := map[string]string{
		"id":           "v1:identity:githubConnectState:setup",
		"userId":       "v1:identity:user:owner",
		"stateHash":    identity.HashConnectState(setupStatePlain),
		"returnPath":   "/?connect=deployables",
		"expiresAt":    time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
		"purpose":      githubconnect.PurposeAppSetup,
		"organization": "",
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func newSetupServer(eng *setupEngine) *Server {
	return &Server{
		Cfg:    identity.Config{BaseURL: "https://identity.lab.example.com"},
		Logger: slog.Default(),
		Store:  &identity.Store{Engine: eng, Logger: slog.Default()},
	}
}

func getSetupPage(s *Server, query string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.Mount(mux)
	req := httptest.NewRequest(http.MethodGet, githubconnect.AppSetupStartPath+"?"+query, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var (
	formAction    = regexp.MustCompile(`<form[^>]*\saction="([^"]*)"`)
	manifestValue = regexp.MustCompile(`name="manifest"\s+value="([^"]*)"`)
)

func TestALiveSetupStateGetsTheFormThatPostsTheManifestToGitHub(t *testing.T) {
	eng := &setupEngine{state: liveSetupState(nil)}
	rec := getSetupPage(newSetupServer(eng), "state="+setupStatePlain)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	action := formAction.FindStringSubmatch(body)
	if action == nil || html.UnescapeString(action[1]) != "https://github.com/settings/apps/new?state="+setupStatePlain {
		t.Fatalf("form action = %v", action)
	}
	if !strings.Contains(body, `method="post"`) {
		t.Error("the form is not a POST, and GitHub's manifest endpoint takes nothing else")
	}

	raw := manifestValue.FindStringSubmatch(body)
	if raw == nil {
		t.Fatalf("no manifest field:\n%s", body)
	}
	var manifest githubconnect.Manifest
	if err := json.Unmarshal([]byte(html.UnescapeString(raw[1])), &manifest); err != nil {
		t.Fatalf("the manifest field does not hold the JSON GitHub reads: %v", err)
	}
	if manifest.CallbackURLs[0] != "https://identity.lab.example.com/auth/github/callback" ||
		manifest.RedirectURL != "https://identity.lab.example.com/auth/github/app/callback" ||
		manifest.URL != "https://os.lab.example.com" {
		t.Errorf("manifest = %+v", manifest)
	}
	if len(manifest.DefaultPermissions) != 2 || manifest.DefaultPermissions["contents"] != "read" {
		t.Errorf("permissions = %v", manifest.DefaultPermissions)
	}
	if !strings.Contains(body, "github-app-setup.js") {
		t.Error("the page does not load the script that submits the form")
	}

	// IT READS A STATE AND WRITES NOTHING. Spending is the callback's.
	if w := eng.writes(); len(w) != 0 {
		t.Errorf("the page wrote %d statement(s): %v", len(w), w)
	}
}

// TestThePagePolicyAdmitsGitHubAndNobodyElse. The package's default policy
// admits the registered clients' origins in form-action; this page has no
// business posting to them, and the default has no business admitting GitHub.
func TestThePagePolicyAdmitsGitHubAndNobodyElse(t *testing.T) {
	eng := &setupEngine{state: liveSetupState(nil)}
	s := newSetupServer(eng)
	s.Cfg.RegisteredClients = []identity.RegisteredClient{
		{ClientId: "portal", RedirectURIs: []string{"https://portal.memql.example/auth/callback"}},
	}
	rec := getSetupPage(s, "state="+setupStatePlain)
	policy := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "form-action 'self' https://github.com;") {
		t.Errorf("the form cannot post to GitHub under this policy, so the page does nothing:\n  %s", policy)
	}
	if strings.Contains(policy, "portal.memql.example") {
		t.Errorf("this page's policy admits an origin it never posts to:\n  %s", policy)
	}
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Errorf("the page policy lost %q:\n  %s", directive, policy)
		}
	}

	// THE CONTROL: no other page gained GitHub.
	other := httptest.NewRecorder()
	mux := http.NewServeMux()
	s.Mount(mux)
	mux.ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/login", nil))
	if strings.Contains(other.Header().Get("Content-Security-Policy"), "github.com") {
		t.Errorf("/login's policy admits GitHub:\n  %s", other.Header().Get("Content-Security-Policy"))
	}
}

func TestAnOrganizationOnTheStateDecidesWhereTheFormPosts(t *testing.T) {
	eng := &setupEngine{state: liveSetupState(map[string]string{"organization": "znasllc-io"})}
	// An organization in the QUERY is ignored: where the form posts was
	// validated at begin and lives on the row.
	rec := getSetupPage(newSetupServer(eng), "state="+setupStatePlain+"&organization=evil-corp")
	body := rec.Body.String()
	action := formAction.FindStringSubmatch(body)
	if action == nil || html.UnescapeString(action[1]) != "https://github.com/organizations/znasllc-io/settings/apps/new?state="+setupStatePlain {
		t.Fatalf("form action = %v", action)
	}
	if strings.Contains(body, "evil-corp") {
		t.Error("the page echoed an organization the request supplied")
	}
	if !strings.Contains(body, "the znasllc-io organization") {
		t.Error("the page does not say where the app will be registered")
	}
}

// TestNothingInTheRequestReachesTheManifest. The only thing a request chooses
// is which state it presents.
func TestNothingInTheRequestReachesTheManifest(t *testing.T) {
	eng := &setupEngine{state: liveSetupState(nil)}
	rec := getSetupPage(newSetupServer(eng),
		"state="+setupStatePlain+"&manifest=%7B%22default_permissions%22%3A%7B%22contents%22%3A%22write%22%7D%7D&callback_urls=https%3A%2F%2Fevil.example")
	body := rec.Body.String()
	if strings.Contains(body, "evil.example") || strings.Contains(html.UnescapeString(body), `"contents":"write"`) {
		t.Errorf("something the request supplied reached the page:\n%s", body)
	}
}

// Every refusal is the same refusal: back to MemQL OS to start again, with
// nothing served and nothing written.
func TestAStateThatIsNotLiveGetsNoPage(t *testing.T) {
	for name, state := range map[string]map[string]string{
		"unknown":                 nil,
		"already spent":           liveSetupState(map[string]string{"consumedAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}),
		"expired":                 liveSetupState(map[string]string{"expiresAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}),
		"a CONNECT state":         liveSetupState(map[string]string{"purpose": githubconnect.PurposeConnect}),
		"a state with no purpose": liveSetupState(map[string]string{"purpose": ""}),
	} {
		eng := &setupEngine{state: state}
		rec := getSetupPage(newSetupServer(eng), "state="+setupStatePlain)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want a 303 back to MemQL OS", name, rec.Code)
			continue
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "https://os.lab.example.com/") || !strings.Contains(loc, "github=github_app_setup_state_invalid") {
			t.Errorf("%s: Location = %q", name, loc)
		}
		if strings.Contains(rec.Body.String(), "manifest") {
			t.Errorf("%s: a manifest was served anyway", name)
		}
		if w := eng.writes(); len(w) != 0 {
			t.Errorf("%s: %d statement(s) written", name, len(w))
		}
	}
	// And no state at all.
	if rec := getSetupPage(newSetupServer(&setupEngine{}), ""); rec.Code != http.StatusSeeOther {
		t.Errorf("no state: status = %d", rec.Code)
	}
}

func TestAPlaintextRequestIsRefused(t *testing.T) {
	eng := &setupEngine{state: liveSetupState(nil)}
	mux := http.NewServeMux()
	newSetupServer(eng).Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://identity.lab.example.com"+githubconnect.AppSetupStartPath+"?state="+setupStatePlain, nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: the state in this URL finishes a registration", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `name="manifest"`) {
		t.Error("the form was served over plaintext")
	}
}

// A cluster whose domain cannot be named cannot describe its app, and sending
// the owner to GitHub with a manifest of blanks is an error page they cannot
// act on. Back to the OS instead, with the return path they came with.
func TestAClusterThatCannotNameItsDomainSendsTheOwnerBack(t *testing.T) {
	eng := &setupEngine{state: liveSetupState(nil)}
	s := newSetupServer(eng)
	s.Cfg.BaseURL = "https://login.corp.example.com"
	rec := getSetupPage(s, "state="+setupStatePlain)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "github=github_app_setup_failed") {
		t.Errorf("status = %d Location = %q", rec.Code, rec.Header().Get("Location"))
	}
	// ...unless the /setup wizard recorded one.
	eng.domain = "lab.example.com"
	if rec := getSetupPage(s, "state="+setupStatePlain); rec.Code != http.StatusOK {
		t.Errorf("with a recorded cluster domain: status = %d", rec.Code)
	}
}
