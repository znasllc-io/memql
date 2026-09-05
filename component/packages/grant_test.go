package packages

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

// grant_test.go -- fetch, poll, probe, the picker and disconnect under a
// GitHub App grant (epic memql#4912).
//
// The claims this file exists to hold, each with a control that would catch a
// vacuous pass:
//
//   - BACKGROUND WORK CARRIES AN INSTALLATION TOKEN, not the person's user
//     token (C6). The control is the same fetch under a `token` credential,
//     which must carry the pasted value and make no app calls at all.
//   - A MINTED INSTALLATION TOKEN NEVER REACHES A ROW. The check walks every
//     rendered statement; the control is a fixture where the token IS in a
//     statement, asserting the walk finds it.
//   - THE THREE FACTS A PASTED TOKEN COLLAPSES stay apart under a grant:
//     reconnect_required, repository_not_installed, credential_cannot_see_it.
//   - THE MANIFEST PREVIEW AGREES WITH ANALYZE over one fixture tree, read
//     both ways, which is the whole reason the preview is worth showing.

// ---------------------------------------------------------------------------
// A fake GitHub
// ---------------------------------------------------------------------------

type hubRoute func(*http.Request) (int, string)

// grantHub is a path-routed GitHub. Unregistered paths answer 404 on purpose:
// "the endpoint nobody wired answered something" is how a test passes for a
// reason its author did not intend.
type grantHub struct {
	mu       sync.Mutex
	requests []*http.Request
	// sent is every request BODY the fake received, in order. Kept so a
	// "nothing leaked" assertion can prove the secret really was on the wire
	// -- an OAuth refresh carries its token in a form body and nowhere else.
	sent    []string
	routes  map[string]hubRoute
	headers map[string]http.Header
}

func newGrantHub() *grantHub {
	return &grantHub{routes: map[string]hubRoute{}, headers: map[string]http.Header{}}
}

func (h *grantHub) on(path string, fn hubRoute) *grantHub {
	h.routes[path] = fn
	return h
}

func (h *grantHub) body(path string, status int, body string) *grantHub {
	return h.on(path, func(*http.Request) (int, string) { return status, body })
}

func (h *grantHub) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	h.mu.Lock()
	h.requests = append(h.requests, req.Clone(context.Background()))
	h.sent = append(h.sent, body)
	route := h.routes[req.URL.Path]
	header := h.headers[req.URL.Path]
	h.mu.Unlock()

	status, body := http.StatusNotFound, `{"message":"Not Found"}`
	if route != nil {
		status, body = route(req)
	}
	out := http.Header{"Content-Type": []string{"application/json"}}
	for k, v := range header {
		out[k] = v
	}
	return &http.Response{
		StatusCode: status,
		Header:     out,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (h *grantHub) seen() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*http.Request, len(h.requests))
	copy(out, h.requests)
	return out
}

// bodies returns every request body the fake received.
func (h *grantHub) bodies() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.sent))
	copy(out, h.sent)
	return out
}

func (h *grantHub) hits(path string) int {
	n := 0
	for _, r := range h.seen() {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}

// bearerOn is the last Authorization credential presented at path, so a test
// reads which of the two bearers a request carried.
func (h *grantHub) bearerOn(path string) string {
	got := ""
	for _, r := range h.seen() {
		if r.URL.Path == path {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
	}
	return got
}

// ---------------------------------------------------------------------------
// A test app and a test grant
// ---------------------------------------------------------------------------

const (
	grantUserToken     = "ghu_USERTOKEN_alice_0001"
	grantRefreshToken  = "ghr_REFRESHTOKEN_alice_0001"
	grantInstallToken  = "ghs_INSTALLATIONTOKEN_0042"
	grantCredentialId  = "v1:platform:sourceCredential:grant"
	grantOwner         = "v1:identity:user:alice"
	grantInstallations = "42"
)

var (
	grantKeyOnce sync.Once
	grantKey     *rsa.PrivateKey
)

func grantAppConfig(t *testing.T) githubapp.Config {
	t.Helper()
	grantKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		grantKey = key
	})
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(grantKey)})
	return githubapp.Config{
		AppId:         "1234567",
		Slug:          "memql-connect",
		ClientId:      "Iv1.memqlconnect",
		ClientSecret:  "gh_CLIENTSECRET_test",
		PrivateKeyB64: base64.StdEncoding.EncodeToString(encoded),
	}
}

// grantRowOpts is what varies between the sealed grant rows these tests use.
type grantRowOpts struct {
	// ExpiresAt is the user token's stated expiry. ZERO means the field is
	// absent, which is a real state -- an app whose user tokens do not expire
	// writes no value -- and must read as "do not refresh".
	ExpiresAt time.Time
	Status    string
	Kind      string
}

// sealedGrantRow is what sourceCredentialSealedById answers for a grant.
func sealedGrantRow(t *testing.T, opts grantRowOpts) map[string]any {
	t.Helper()
	value, _, err := secret.Encrypt(grantUserToken)
	if err != nil {
		t.Fatalf("seal the user token: %v", err)
	}
	refresh, _, rerr := secret.Encrypt(grantRefreshToken)
	if rerr != nil {
		t.Fatalf("seal the refresh token: %v", rerr)
	}
	kind := opts.Kind
	if kind == "" {
		kind = credentialKindGithubApp
	}
	status := opts.Status
	if status == "" {
		status = credentialStatusActive
	}
	row := map[string]any{
		"id":              grantCredentialId,
		"ownerUserId":     grantOwner,
		"host":            "github.com",
		"kind":            kind,
		"status":          status,
		"encryptedValue":  value,
		"refreshToken":    refresh,
		"login":           "alice-gh",
		"externalId":      "5150",
		"installationIds": []any{grantInstallations},
	}
	if !opts.ExpiresAt.IsZero() {
		row["expiresAt"] = opts.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return row
}

// grantHarness wires an Integration, a store and a Deps over a fake GitHub and
// a real GitHub App client -- the production resolvers, so a test sees the
// path a cluster runs.
func grantHarness(t *testing.T, hub *grantHub, row map[string]any) (*Integration, *store, *actorEngine, *Deps) {
	t.Helper()
	engine := &actorEngine{}
	if row != nil {
		engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {row}}
	}
	client := githubapp.New(grantAppConfig(t),
		githubapp.WithHTTPClient(&http.Client{Transport: hub}),
		githubapp.WithAPIBase("https://api.github.com"),
		githubapp.WithOAuthBase("https://github.com"))
	s := &store{engine: engine, logger: discardLogger(), github: client}
	deps := &Deps{
		Store:           s,
		Credentials:     s.resolveCredential,
		PeekCredentials: s.peekCredential,
		GitHubApp:       client,
		HTTP:            &http.Client{Transport: hub},
		Logger:          discardLogger(),
		Limits:          DefaultLimits(),
	}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() { i.deps = deps })
	return i, s, engine, deps
}

// installedRepo wires the two app-side routes a background fetch needs.
func installedRepo(hub *grantHub, owner, repo string) *grantHub {
	hub.body("/repos/"+owner+"/"+repo+"/installation", http.StatusOK, `{"id":42}`)
	hub.body("/app/installations/42/access_tokens", http.StatusCreated,
		`{"token":"`+grantInstallToken+`","expires_at":"`+time.Now().UTC().Add(time.Hour).Format(time.RFC3339)+`"}`)
	return hub
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

// TestFetchUnderAGrantCarriesAnInstallationToken is C6 measured: the deploy
// path never depends on a person's user token being alive.
func TestFetchUnderAGrantCarriesAnInstallationToken(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.on("/repos/acme/widget/tarball", func(*http.Request) (int, string) {
		return http.StatusOK, string(gitHubTarball(t, "acme-widget-"+sha, spaOnlyPackage()))
	})
	_, s, engine, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	deps.Fetcher = &githubFetcher{
		http:        deps.HTTP,
		credentials: s.resolveCredential,
		github:      deps.GitHubApp,
		tempDir:     t.TempDir(),
	}

	snap, err := deps.Fetcher.FetchRepo(context.Background(), RepoSource{
		RepoUrl:      "https://github.com/acme/widget",
		CredentialId: grantCredentialId,
		OwnerUserId:  grantOwner,
	}, deps.Limits)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer snap.Close()

	// THE INSTALLATION TOKEN, not the user token. This is the whole claim.
	if got := hub.bearerOn("/repos/acme/widget/tarball"); got != grantInstallToken {
		t.Fatalf("the tarball must be fetched under the installation token, got %q", got)
	}
	if got := hub.bearerOn("/repos/acme/widget/tarball"); got == grantUserToken {
		t.Fatal("the fetch carried the person's own user token")
	}

	// A MINTED TOKEN NEVER REACHES A ROW (design E). Walked over every
	// statement rather than checked at one call site, so a future writer that
	// tried to cache it in the graph fails here.
	for _, q := range engine.statements() {
		if strings.Contains(q, grantInstallToken) {
			t.Fatalf("a minted installation token reached a statement: %s", q)
		}
		if strings.Contains(q, grantUserToken) || strings.Contains(q, grantRefreshToken) {
			t.Fatalf("a grant's plaintext token reached a statement: %s", q)
		}
	}
	// THE CONTROL for the walk above: the same check over a statement that
	// DOES carry the token finds it, so the silence is about the code and not
	// about an empty list.
	if !strings.Contains("mutation x(v: \""+grantInstallToken+"\")", grantInstallToken) {
		t.Fatal("control failed: the containment check cannot see a token that is present")
	}
	if len(engine.statements()) == 0 {
		t.Fatal("control failed: no statements were captured, so the walk proved nothing")
	}
}

// The pasted-token path is byte for byte what it was: the pasted value as the
// bearer, and NO app calls at all.
func TestFetchUnderATokenCredentialMakesNoAppCalls(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.on("/repos/acme/widget/tarball", func(*http.Request) (int, string) {
		return http.StatusOK, string(gitHubTarball(t, "acme-widget-"+sha, spaOnlyPackage()))
	})
	_, s, _, deps := grantHarness(t, hub, sealedRow(t, credentialStatusActive))
	deps.Fetcher = &githubFetcher{
		http:        deps.HTTP,
		credentials: s.resolveCredential,
		github:      deps.GitHubApp,
		tempDir:     t.TempDir(),
	}

	snap, err := deps.Fetcher.FetchRepo(context.Background(), RepoSource{
		RepoUrl:      "https://github.com/acme/widget",
		CredentialId: "v1:platform:sourceCredential:abc",
		OwnerUserId:  "v1:identity:user:alice",
	}, deps.Limits)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer snap.Close()

	if got := hub.bearerOn("/repos/acme/widget/tarball"); got != testToken {
		t.Fatalf("a pasted credential must be presented unchanged, got %q", got)
	}
	if n := hub.hits("/repos/acme/widget/installation") + hub.hits("/app/installations/42/access_tokens"); n != 0 {
		t.Fatalf("%d app call(s) for a pasted token; the app is not involved in that path at all", n)
	}
}

// TestFetchRefusesARepositoryTheAppIsNotInstalledOn: the app's own 404 is the
// honest answer, and the repair is a link rather than another credential.
func TestFetchRefusesARepositoryTheAppIsNotInstalledOn(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub() // nothing wired: /installation answers 404
	_, s, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	deps.Fetcher = &githubFetcher{http: deps.HTTP, credentials: s.resolveCredential, github: deps.GitHubApp, tempDir: t.TempDir()}

	_, err := deps.Fetcher.FetchRepo(context.Background(), RepoSource{
		RepoUrl:      "https://github.com/acme/widget",
		CredentialId: grantCredentialId,
		OwnerUserId:  grantOwner,
	}, deps.Limits)
	if got := RefusalCode(err); got != CodeRepositoryNotInstalled {
		t.Fatalf("want %s, got %s (%v)", CodeRepositoryNotInstalled, got, err)
	}
	// The sentence carries the repair, which is the point of the code.
	if !strings.Contains(err.Error(), "https://github.com/apps/memql-connect/installations/new") {
		t.Fatalf("the refusal must carry the installation link, got: %v", err)
	}
	// AND NO TARBALL WAS ASKED FOR: the refusal happens before the request.
	if n := hub.hits("/repos/acme/widget/tarball"); n != 0 {
		t.Fatalf("%d tarball request(s) for a repository the app cannot read", n)
	}
}

// A 401 on the fetch itself is the authorization being refused, never
// "private, or not there".
func TestFetchAnswersReconnectRequiredOnA401UnderAGrant(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget/tarball", http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	_, s, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	deps.Fetcher = &githubFetcher{http: deps.HTTP, credentials: s.resolveCredential, github: deps.GitHubApp, tempDir: t.TempDir()}

	_, err := deps.Fetcher.FetchRepo(context.Background(), RepoSource{
		RepoUrl:      "https://github.com/acme/widget",
		CredentialId: grantCredentialId,
		OwnerUserId:  grantOwner,
	}, deps.Limits)
	if got := RefusalCode(err); got != CodeReconnectRequired {
		t.Fatalf("want %s, got %s (%v)", CodeReconnectRequired, got, err)
	}
	if !strings.Contains(err.Error(), "@alice-gh") {
		t.Fatalf("the refusal must name the connection, got: %v", err)
	}

	// THE CONTROL: the same 401 under a PASTED token stays source_unreadable,
	// because there the cluster genuinely cannot tell the three apart.
	hubToken := newGrantHub()
	hubToken.body("/repos/acme/widget/tarball", http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	_, s2, _, deps2 := grantHarness(t, hubToken, sealedRow(t, credentialStatusActive))
	deps2.Fetcher = &githubFetcher{http: deps2.HTTP, credentials: s2.resolveCredential, github: deps2.GitHubApp, tempDir: t.TempDir()}
	_, terr := deps2.Fetcher.FetchRepo(context.Background(), RepoSource{
		RepoUrl:      "https://github.com/acme/widget",
		CredentialId: "v1:platform:sourceCredential:abc",
		OwnerUserId:  "v1:identity:user:alice",
	}, deps2.Limits)
	if got := RefusalCode(terr); got != CodeSourceUnreadable {
		t.Fatalf("a 401 under a pasted token must stay %s, got %s (%v)", CodeSourceUnreadable, got, terr)
	}
}

// ---------------------------------------------------------------------------
// The silent refresh
// ---------------------------------------------------------------------------

// TestAnExpiredGrantIsRefreshedBeforeUse is C6's other half: a person is never
// sent back through a browser for an eight-hour expiry.
func TestAnExpiredGrantIsRefreshedBeforeUse(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	const newUserToken = "ghu_ROTATED_alice_0002"
	const newRefreshToken = "ghr_ROTATED_alice_0002"
	hub := newGrantHub().body("/login/oauth/access_token", http.StatusOK,
		`{"access_token":"`+newUserToken+`","expires_in":28800,"refresh_token":"`+newRefreshToken+`","refresh_token_expires_in":15811200}`)
	_, s, engine, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(-time.Hour)}))

	resolved, err := s.peekCredential(context.Background(), grantCredentialId, grantOwner)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Bearer != newUserToken {
		t.Fatalf("the resolver must answer the RENEWED token, got %q", resolved.Bearer)
	}
	if !resolved.IsGrant() || resolved.Login != "alice-gh" || resolved.ExternalId != "5150" {
		t.Fatalf("the resolved grant lost its identity: %+v", resolved)
	}

	// THE ROTATION IS RECORDED, both halves in ONE write.
	var refreshStmt string
	for _, q := range engine.statements() {
		if strings.HasPrefix(q, "mutation refreshGithubAppGrantToken(") {
			refreshStmt = q
		}
	}
	if refreshStmt == "" {
		t.Fatalf("a refresh must be recorded; statements: %v", engine.statements())
	}
	value := quotedArg(t, refreshStmt, "encryptedValue")
	if got, derr := secret.Decrypt(value); derr != nil || got != newUserToken {
		t.Fatalf("encryptedValue must be a real seal of the new token, got %q (%v)", got, derr)
	}
	rotated := quotedArg(t, refreshStmt, "refreshToken")
	if got, derr := secret.Decrypt(rotated); derr != nil || got != newRefreshToken {
		t.Fatalf("the ROTATED refresh token must be sealed onto the row -- keeping the old one makes the grant unrenewable in eight hours; got %q (%v)", got, derr)
	}

	// AND NO PLAINTEXT ANYWHERE. The reachable positive is directly above:
	// both ciphertexts unseal to the real values, so a renderer that stopped
	// sealing fails there rather than passing on "does not contain".
	for _, q := range engine.statements() {
		for _, plain := range []string{newUserToken, newRefreshToken, grantUserToken, grantRefreshToken} {
			if strings.Contains(q, plain) {
				t.Fatalf("plaintext %q reached a statement: %s", plain, q)
			}
		}
	}
}

// A grant with NO stated expiry is left alone: an app whose user tokens do not
// expire writes no value, and refreshing on every call would spend a refresh
// token -- which rotates on use -- for nothing.
func TestAGrantWithNoStatedExpiryIsNotRefreshed(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub()
	_, s, engine, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	resolved, err := s.peekCredential(context.Background(), grantCredentialId, grantOwner)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Bearer != grantUserToken {
		t.Fatalf("the stored token must be presented unchanged, got %q", resolved.Bearer)
	}
	if n := hub.hits("/login/oauth/access_token"); n != 0 {
		t.Fatalf("%d refresh(es) for a grant with no stated expiry", n)
	}
	if engine.sawStatement("mutation refreshGithubAppGrantToken") {
		t.Fatal("nothing was renewed, so nothing may be recorded")
	}
	// A grant whose expiry is still comfortably ahead is equally untouched --
	// the control for "expired" meaning something.
	_, s2, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(4 * time.Hour)}))
	if _, err := s2.peekCredential(context.Background(), grantCredentialId, grantOwner); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n := hub.hits("/login/oauth/access_token"); n != 0 {
		t.Fatalf("%d refresh(es) for a grant that has not expired", n)
	}
}

// A refresh GitHub refuses is reconnect_required, by name, and it is the ONE
// refusal a person repairs with a single click.
func TestARefusedRefreshIsReconnectRequired(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub().body("/login/oauth/access_token", http.StatusOK, `{"error":"bad_refresh_token"}`)
	_, s, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(-time.Hour)}))

	_, err := s.peekCredential(context.Background(), grantCredentialId, grantOwner)
	if got := RefusalCode(err); got != CodeReconnectRequired {
		t.Fatalf("want %s, got %s (%v)", CodeReconnectRequired, got, err)
	}
	if strings.Contains(err.Error(), grantRefreshToken) {
		t.Fatalf("the refusal carries the refresh token: %v", err)
	}
}

// A node with a grant and no GitHub App configured refuses BY NAME rather than
// presenting an expired token: an operator's missing configuration must not
// reach a person as "reconnect your GitHub", which is a repair that would not
// work.
func TestAGrantOnANodeWithNoAppRefusesAsNotConfigured(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	engine := &actorEngine{recordingEngine: recordingEngine{rows: map[string][]map[string]any{
		"query sourceCredentialSealedById": {sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(-time.Hour)})},
	}}}
	s := &store{engine: engine, logger: discardLogger(), github: githubapp.New(githubapp.Config{})}

	_, err := s.peekCredential(context.Background(), grantCredentialId, grantOwner)
	if got := RefusalCode(err); got != CodeGithubAppNotConfigured {
		t.Fatalf("want %s, got %s (%v)", CodeGithubAppNotConfigured, got, err)
	}
	if !strings.Contains(err.Error(), githubapp.EnvAppId) {
		t.Fatalf("the refusal must name what an operator sets, got: %v", err)
	}
}

// quotedArg reads one quoted argument value out of a rendered statement.
func quotedArg(t *testing.T, statement, name string) string {
	t.Helper()
	marker := name + ": \""
	i := strings.Index(statement, marker)
	if i < 0 {
		t.Fatalf("%s carries no %s argument", statement, name)
	}
	rest := statement[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("%s: unterminated %s", statement, name)
	}
	return rest[:j]
}

// ---------------------------------------------------------------------------
// The poll
// ---------------------------------------------------------------------------

// The poll carries an installation token too, and a grant that needs
// reconnecting makes it SKIP that package with a warning -- never poll
// anonymously, which would answer 404 for a private repository and read as
// "unchanged".
func TestThePollUnderAGrantUsesAnInstallationTokenAndSkipsAReconnect(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	pkg := map[string]any{
		"id":           "v1:platform:package:grant",
		"ownerUserId":  grantOwner,
		"sourceKind":   "repo",
		"repoUrl":      "https://github.com/acme/widget",
		"credentialId": grantCredentialId,
		"status":       "active",
	}
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget/commits/HEAD", http.StatusOK, `{"sha":"newsha0000000000"}`)

	engine := &actorEngine{recordingEngine: recordingEngine{rows: map[string][]map[string]any{
		"query packagesTrackingRepos":      {pkg},
		"query packagesByRepoUrl":          {pkg},
		"query sourceCredentialSealedById": {sealedGrantRow(t, grantRowOpts{})},
	}}}
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	s := &store{engine: engine, logger: discardLogger(), github: client}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: s, Credentials: s.resolveCredential, PeekCredentials: s.peekCredential,
			GitHubApp: client, HTTP: &http.Client{Transport: hub}, Logger: discardLogger()}
	})

	if _, err := i.handlePollUpstream(context.Background(), nil, 0); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := hub.bearerOn("/repos/acme/widget/commits/HEAD"); got != grantInstallToken {
		t.Fatalf("the poll must carry the installation token, got %q", got)
	}
	if !engine.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatalf("the poll must record the head it read; statements: %v", engine.statements())
	}

	// THE SKIP: the same package, a grant GitHub no longer accepts. The sweep
	// itself succeeds, nothing is written, and no anonymous request is made.
	hub2 := newGrantHub().body("/login/oauth/access_token", http.StatusOK, `{"error":"bad_refresh_token"}`)
	engine2 := &actorEngine{recordingEngine: recordingEngine{rows: map[string][]map[string]any{
		"query packagesTrackingRepos":      {pkg},
		"query sourceCredentialSealedById": {sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(-time.Hour)})},
	}}}
	client2 := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub2}))
	s2 := &store{engine: engine2, logger: discardLogger(), github: client2}
	i2 := NewIntegration(engine2, discardLogger())
	i2.depsOnce.Do(func() {
		i2.deps = &Deps{Store: s2, Credentials: s2.resolveCredential, PeekCredentials: s2.peekCredential,
			GitHubApp: client2, HTTP: &http.Client{Transport: hub2}, Logger: discardLogger()}
	})
	if _, err := i2.handlePollUpstream(context.Background(), nil, 0); err != nil {
		t.Fatalf("one package whose grant refused must not stop the sweep: %v", err)
	}
	if n := hub2.hits("/repos/acme/widget/commits/HEAD"); n != 0 {
		t.Fatalf("%d request(s) for a package whose grant GitHub no longer accepts", n)
	}
	if engine2.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatal("nothing may be recorded for a package that was not polled")
	}
}

// ---------------------------------------------------------------------------
// The probe
// ---------------------------------------------------------------------------

const grantRepoBody = `{"id":7,"private":true,"default_branch":"main"}`

// TestProbeUnderAGrantSplitsTheThreeFactsAPastedTokenCollapses is the D
// classification, all three arms in one place, plus the pasted-token control.
func TestProbeUnderAGrantSplitsTheThreeFactsAPastedTokenCollapses(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)

	t.Run("401 is reconnect_required", func(t *testing.T) {
		hub := newGrantHub().body("/repos/acme/widget", http.StatusUnauthorized, `{"message":"Bad credentials"}`)
		_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		res, err := ProbeSource(callerCtx(grantOwner), deps, "https://github.com/acme/widget", grantCredentialId)
		if err != nil {
			t.Fatalf("a typed reason, not an error: %v", err)
		}
		if res.Reason != ProbeReasonReconnectRequired {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("404 with the app not installed is repository_not_installed", func(t *testing.T) {
		hub := newGrantHub().body("/repos/acme/widget", http.StatusNotFound, `{"message":"Not Found"}`)
		// /repos/acme/widget/installation is unwired, so the app's own
		// question answers 404: the app is not installed.
		_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		res, err := ProbeSource(callerCtx(grantOwner), deps, "https://github.com/acme/widget", grantCredentialId)
		if err != nil {
			t.Fatalf("a typed reason, not an error: %v", err)
		}
		if res.Reason != ProbeReasonRepositoryNotInstalled {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("404 with the app installed stays credential_cannot_see_it", func(t *testing.T) {
		hub := installedRepo(newGrantHub(), "acme", "widget")
		hub.body("/repos/acme/widget", http.StatusNotFound, `{"message":"Not Found"}`)
		_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		res, err := ProbeSource(callerCtx(grantOwner), deps, "https://github.com/acme/widget", grantCredentialId)
		if err != nil {
			t.Fatalf("a typed reason, not an error: %v", err)
		}
		// The app IS installed, so the 404 is about what the PERSON can see,
		// and the honest collapsed answer stands. This is the arm that keeps
		// repository_not_installed from becoming a synonym for 404.
		if res.Reason != ProbeReasonCredentialCannotSeeIt {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("the same statuses under a pasted token stay collapsed", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized} {
			hub := newGrantHub().body("/repos/acme/widget", status, `{"message":"no"}`)
			_, _, _, deps := grantHarness(t, hub, sealedRow(t, credentialStatusActive))
			res, err := ProbeSource(callerCtx("v1:identity:user:alice"), deps, "https://github.com/acme/widget", "v1:platform:sourceCredential:abc")
			if err != nil {
				t.Fatalf("HTTP %d: %v", status, err)
			}
			if res.Reason != ProbeReasonCredentialCannotSeeIt {
				t.Fatalf("HTTP %d under a pasted token: got %+v", status, res)
			}
		}
	})
}

// TestProbeWithNoCredentialResolvesTheCallersGrant: a connected person's
// picker prefills without anybody naming a credential.
func TestProbeWithNoCredentialResolvesTheCallersGrant(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	row := sealedGrantRow(t, grantRowOpts{})
	hub := newGrantHub().body("/repos/acme/widget", http.StatusOK, grantRepoBody)
	hub.body("/repos/acme/widget/branches", http.StatusOK, `[{"name":"topic"},{"name":"main"}]`)

	engine := &actorEngine{recordingEngine: recordingEngine{rows: map[string][]map[string]any{
		"query githubAppGrantForCaller":    {row},
		"query sourceCredentialSealedById": {row},
	}}}
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	s := &store{engine: engine, logger: discardLogger(), github: client}
	deps := &Deps{Store: s, Credentials: s.resolveCredential, PeekCredentials: s.peekCredential,
		GitHubApp: client, HTTP: &http.Client{Transport: hub}, Logger: discardLogger()}

	res, err := ProbeSource(callerCtx(grantOwner), deps, "https://github.com/acme/widget", "")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Reason != ProbeReasonOK || !res.Reachable {
		t.Fatalf("got %+v", res)
	}
	if got := hub.bearerOn("/repos/acme/widget"); got != grantUserToken {
		t.Fatalf("the probe must present the PERSON's token -- the question is what they can see -- got %q", got)
	}
	// DEFAULT BRANCH FIRST: the ref picker's first entry is what somebody
	// takes when they do not care, and alphabetical order would hand them
	// whatever branch happens to sort first.
	if strings.Join(res.Branches, ",") != "main,topic" {
		t.Fatalf("branches %v, want the default first", res.Branches)
	}
}

// ---------------------------------------------------------------------------
// The manifest preview
// ---------------------------------------------------------------------------

// TestTheManifestPreviewAgreesWithAnalyze is the epic's acceptance criterion,
// and it is a real cross-check rather than two hand-written literals: ONE
// fixture tree, read through the probe's contents API and through Analyze, and
// the two readings compared field by field. A preview that disagreed with the
// run it previews is worse than no preview.
func TestTheManifestPreviewAgreesWithAnalyze(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	tree := validPackage()

	manifestRaw, rerr := tree.ReadFile(ManifestName)
	if rerr != nil {
		t.Fatalf("fixture: %v", rerr)
	}
	hub := newGrantHub().body("/repos/acme/widget", http.StatusOK, grantRepoBody)
	hub.body("/repos/acme/widget/branches", http.StatusOK, `[{"name":"main"}]`)
	hub.body("/repos/acme/widget/contents/"+ManifestName, http.StatusOK,
		`{"type":"file","encoding":"base64","content":`+jsonQuote(base64.StdEncoding.EncodeToString(manifestRaw))+`}`)
	hub.body("/repos/acme/widget/contents/dsl", http.StatusOK, `[{"name":"acme","type":"dir"}]`)

	_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	res, err := ProbeSource(callerCtx(grantOwner), deps, "https://github.com/acme/widget", grantCredentialId)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	report, aerr := Analyze(tree, Options{SourceVersion: "abc"})
	if aerr != nil {
		t.Fatalf("analyze: %v", aerr)
	}

	// THE NAME.
	if res.Manifest.Name != report.Name || res.Manifest.Name == "" {
		t.Fatalf("preview name %q, analysis name %q", res.Manifest.Name, report.Name)
	}
	// THE DEPLOYABLES, in order, name/kind/path each.
	if len(res.Manifest.Deployables) != len(report.Deployables) {
		t.Fatalf("preview lists %d deployables, the analysis %d", len(res.Manifest.Deployables), len(report.Deployables))
	}
	for i, got := range res.Manifest.Deployables {
		want := report.Deployables[i]
		if got.Name != want.Name || got.Kind != want.Kind || got.Path != want.Path {
			t.Errorf("deployable %d: preview %+v, analysis {%s %s %s}", i, got, want.Name, want.Kind, want.Path)
		}
	}
	// THE DSL DOMAINS.
	var analysed []string
	for _, d := range report.DslDomains {
		analysed = append(analysed, d.Domain)
	}
	if strings.Join(res.Manifest.DslDomains, ",") != strings.Join(analysed, ",") {
		t.Fatalf("preview domains %v, analysis domains %v", res.Manifest.DslDomains, analysed)
	}
	if len(analysed) == 0 {
		t.Fatal("control failed: the fixture declares no DSL domain, so the comparison above compared nothing")
	}
}

// A manifest that is missing, or that does not parse, is NOT a refusal: the
// probe stays a courtesy and Analyze is the authority.
func TestAnUnreadableManifestIsAnEmptyPreviewAndNotARefusal(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	cases := []struct {
		name string
		wire func(*grantHub)
	}{
		{"no manifest at all", func(*grantHub) {}},
		{"a manifest that does not parse", func(h *grantHub) {
			h.body("/repos/acme/widget/contents/"+ManifestName, http.StatusOK,
				`{"type":"file","encoding":"base64","content":`+jsonQuote(base64.StdEncoding.EncodeToString([]byte("deployabels: [\n")))+`}`)
		}},
		{"a manifest with no formatVersion", func(h *grantHub) {
			h.body("/repos/acme/widget/contents/"+ManifestName, http.StatusOK,
				`{"type":"file","encoding":"base64","content":`+jsonQuote(base64.StdEncoding.EncodeToString([]byte("name: acme\n")))+`}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newGrantHub().body("/repos/acme/widget", http.StatusOK, grantRepoBody)
			tc.wire(hub)
			_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
			res, err := ProbeSource(callerCtx(grantOwner), deps, "https://github.com/acme/widget", grantCredentialId)
			if err != nil {
				t.Fatalf("a manifest problem is not a probe refusal: %v", err)
			}
			if res.Reason != ProbeReasonOK || !res.Reachable {
				t.Fatalf("the repository is reachable whatever its manifest says: %+v", res)
			}
			if res.Manifest.Name != "" || len(res.Manifest.Deployables) != 0 {
				t.Fatalf("want an empty preview, got %+v", res.Manifest)
			}
		})
	}
}

func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// ---------------------------------------------------------------------------
// The picker
// ---------------------------------------------------------------------------

const installationsBody = `{"total_count":1,"installations":[{"id":42,"account":{"login":"acme","type":"Organization"},"repository_selection":"selected","suspended_at":null}]}`

func TestSourceRepositoriesListsWhatTheGrantReaches(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub().
		body("/user/installations", http.StatusOK, installationsBody).
		body("/user/installations/42/repositories", http.StatusOK,
			`{"total_count":1,"repositories":[{"full_name":"acme/widget","name":"widget","private":true,"visibility":"private","default_branch":"main","pushed_at":"2026-09-01T10:00:00Z","html_url":"https://github.com/acme/widget","owner":{"login":"acme"}}]}`).
		body("/app/installation-requests", http.StatusOK,
			`[{"account":{"login":"other-org"},"requester":{"login":"alice-gh"}},{"account":{"login":"not-mine"},"requester":{"login":"somebody-else"}}]`)
	_, _, engine, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	res, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Reason != RepositoriesReasonOK {
		t.Fatalf("got %+v", res)
	}
	if len(res.Repositories) != 1 {
		t.Fatalf("want one repository, got %+v", res.Repositories)
	}
	repo := res.Repositories[0]
	if repo.FullName != "acme/widget" || repo.Owner != "acme" || repo.DefaultBranch != "main" || repo.InstallationId != "42" {
		t.Fatalf("got %+v", repo)
	}
	if !repo.Private || repo.Visibility != "private" || repo.Url != "https://github.com/acme/widget" {
		t.Fatalf("got %+v", repo)
	}
	if len(res.Installations) != 1 || res.Installations[0].Account != "acme" || res.Installations[0].RepositorySelection != "selected" {
		t.Fatalf("installations %+v", res.Installations)
	}
	// PENDING IS MATCHED ON THE REQUESTER and named by the ORGANISATION: the
	// repair belongs to somebody else, and knowing whom to ask is the only
	// useful next step. Another person's request must not appear.
	if strings.Join(res.Pending, ",") != "other-org" {
		t.Fatalf("pending %v -- must be this person's requests, named by organization", res.Pending)
	}
	// One page of one repository has no next page.
	if res.NextPage != 0 {
		t.Fatalf("nextPage %d, want 0", res.NextPage)
	}
	// The listing runs under the PERSON's token: it is bounded by what they
	// can see, which is what makes "one person cannot list another's
	// repositories" true by construction.
	if got := hub.bearerOn("/user/installations"); got != grantUserToken {
		t.Fatalf("the picker must read under the person's own token, got %q", got)
	}

	// THE INSTALLATION IDS ARE REFRESHED from what was just read -- one of
	// the three owner-actor paths that keep them current, and the reason this
	// epic needs no privileged webhook automation.
	var recorded string
	for _, q := range engine.statements() {
		if strings.HasPrefix(q, "mutation recordGithubAppInstallations(") {
			recorded = q
		}
	}
	if recorded == "" {
		t.Fatalf("the picker must refresh the grant's installation ids; statements: %v", engine.statements())
	}
	if !strings.Contains(recorded, `["42"]`) {
		t.Fatalf("the refresh must carry what was read, got: %s", recorded)
	}
}

// A second page is offered only when GitHub says there is more.
func TestSourceRepositoriesOffersANextPageOnlyWhenThereIsMore(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub().
		body("/user/installations", http.StatusOK, installationsBody).
		body("/user/installations/42/repositories", http.StatusOK,
			`{"total_count":150,"repositories":[{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}}]}`)
	_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	res, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.NextPage != 2 {
		t.Fatalf("150 repositories at 100 a page has a second page, got nextPage %d", res.NextPage)
	}
	// And the last page does not offer one.
	res2, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res2.NextPage != 0 {
		t.Fatalf("the last page must not offer another, got %d", res2.NextPage)
	}
}

// Every refusal is a typed REASON rather than an error, so the picker renders
// in place -- Connect where the list would be, or the token path.
func TestSourceRepositoriesAnswersTypedReasons(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)

	t.Run("no app configured", func(t *testing.T) {
		hub := newGrantHub()
		engine := &actorEngine{}
		s := &store{engine: engine, logger: discardLogger()}
		deps := &Deps{Store: s, PeekCredentials: s.peekCredential, GitHubApp: githubapp.New(githubapp.Config{}),
			HTTP: &http.Client{Transport: hub}, Logger: discardLogger()}
		res, err := SourceRepositories(callerCtx(grantOwner), deps, "", 0)
		if err != nil || res.Reason != RepositoriesReasonNotConfigured {
			t.Fatalf("got %+v %v", res, err)
		}
		if n := len(hub.seen()); n != 0 {
			t.Fatalf("%d request(s) from a cluster with no app", n)
		}
	})

	t.Run("no grant at all", func(t *testing.T) {
		hub := newGrantHub()
		_, _, _, deps := grantHarness(t, hub, nil)
		res, err := SourceRepositories(callerCtx(grantOwner), deps, "", 0)
		if err != nil || res.Reason != RepositoriesReasonCredentialMissing {
			t.Fatalf("got %+v %v", res, err)
		}
	})

	t.Run("a revoked grant", func(t *testing.T) {
		hub := newGrantHub()
		_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{Status: credentialStatusRevoked}))
		res, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 0)
		if err != nil || res.Reason != RepositoriesReasonCredentialRevoked {
			t.Fatalf("got %+v %v", res, err)
		}
	})

	t.Run("a grant GitHub no longer accepts", func(t *testing.T) {
		hub := newGrantHub().body("/login/oauth/access_token", http.StatusOK, `{"error":"bad_refresh_token"}`)
		_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(-time.Hour)}))
		res, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 0)
		if err != nil || res.Reason != RepositoriesReasonReconnectRequired {
			t.Fatalf("got %+v %v", res, err)
		}
	})

	t.Run("a pasted token is not a grant", func(t *testing.T) {
		hub := newGrantHub()
		_, _, _, deps := grantHarness(t, hub, sealedRow(t, credentialStatusActive))
		res, err := SourceRepositories(callerCtx("v1:identity:user:alice"), deps, "v1:platform:sourceCredential:abc", 0)
		if err != nil || res.Reason != RepositoriesReasonCredentialMissing {
			t.Fatalf("a pasted token has no repository listing: %+v %v", res, err)
		}
		if n := hub.hits("/user/installations"); n != 0 {
			t.Fatal("a pasted token must not be presented to the installations endpoint")
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		hub := newGrantHub().body("/user/installations", http.StatusTooManyRequests, `{"message":"slow down"}`)
		_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		res, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 0)
		if err != nil || res.Reason != RepositoriesReasonRateLimited {
			t.Fatalf("got %+v %v", res, err)
		}
	})
}

// The pending list is a footnote and must never cost the answer: an app plan
// that does not serve installation-requests still gets its repositories.
func TestPendingInstallationsFailingDoesNotCostTheListing(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub().
		body("/user/installations", http.StatusOK, installationsBody).
		body("/user/installations/42/repositories", http.StatusOK,
			`{"total_count":1,"repositories":[{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}}]}`)
	// /app/installation-requests is unwired, so it answers 404.
	_, _, _, deps := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	res, err := SourceRepositories(callerCtx(grantOwner), deps, grantCredentialId, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Reason != RepositoriesReasonOK || len(res.Repositories) != 1 {
		t.Fatalf("got %+v", res)
	}
	if len(res.Pending) != 0 {
		t.Fatalf("pending %v, want empty", res.Pending)
	}
}

// The capability's reply carries exactly the five keys the picker reads.
func TestSourceRepositoriesHandlerRepliesTheWireShape(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub().
		body("/user/installations", http.StatusOK, installationsBody).
		body("/user/installations/42/repositories", http.StatusOK, `{"total_count":0,"repositories":[]}`)
	i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))

	nodes, err := i.handleSourceRepositories(callerCtx(grantOwner),
		map[string]any{"credentialId": grantCredentialId, "page": float64(1)}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	reply := replyPayload(t, nodes)
	for _, key := range []string{"reason", "repositories", "installations", "pending", "nextPage"} {
		if _, ok := reply[key]; !ok {
			t.Errorf("reply is missing %q: %v", key, reply)
		}
	}
	if len(reply) != 5 {
		t.Fatalf("reply carries exactly the five keys, got %v", reply)
	}
	// And no token reached it.
	if raw := string(nodes[0].Payload); strings.Contains(raw, grantUserToken) || strings.Contains(raw, grantRefreshToken) {
		t.Fatalf("a token reached the reply: %s", raw)
	}
}

// TestIntArgNarrowsThroughCoreNum: a payload number too large for an int is
// implementation-defined as a bare conversion -- on amd64 int(1e30) is hugely
// NEGATIVE, which would pass straight through the `< 1` guard as page one and
// hide the nonsense (memql#4779).
func TestIntArgNarrowsThroughCoreNum(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"absent", nil, 0},
		{"a float page", float64(3), 3},
		{"an int64 page", int64(7), 7},
		{"a value no int can hold", float64(1e30), 0},
		{"not a number", "3", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intArg(map[string]any{"page": tc.in}, "page"); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Disconnect
// ---------------------------------------------------------------------------

// TestDisconnectRevokesAtGitHubAndTheRowRegardless is A.6: the person asked to
// disconnect, and the LOCAL row is what actually stops every fetch on this
// cluster -- so a GitHub-side failure must never leave the cluster still
// fetching under an authorization the person believes they ended.
func TestDisconnectRevokesAtGitHubAndTheRowRegardless(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)

	t.Run("the happy path revokes both halves", func(t *testing.T) {
		hub := newGrantHub().body("/applications/Iv1.memqlconnect/grant", http.StatusNoContent, ``)
		i, _, engine, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		nodes, err := i.handleSourceCredentialRevoke(callerCtx(grantOwner), map[string]any{"credentialId": grantCredentialId}, 0)
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
		reply := replyPayload(t, nodes)
		if reply["remoteRevoked"] != true || reply["status"] != credentialStatusRevoked {
			t.Fatalf("reply %v", reply)
		}
		if hub.hits("/applications/Iv1.memqlconnect/grant") != 1 {
			t.Fatal("a grant must be revoked at GitHub as well")
		}
		if !engine.sawStatement("mutation revokeSourceCredential") {
			t.Fatalf("the row must be flipped; statements: %v", engine.statements())
		}
	})

	t.Run("GitHub being unreachable does not block the local revoke", func(t *testing.T) {
		hub := newGrantHub().body("/applications/Iv1.memqlconnect/grant", http.StatusInternalServerError, `{}`)
		i, _, engine, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		nodes, err := i.handleSourceCredentialRevoke(callerCtx(grantOwner), map[string]any{"credentialId": grantCredentialId}, 0)
		if err != nil {
			t.Fatalf("a GitHub-side failure must not fail the disconnect: %v", err)
		}
		reply := replyPayload(t, nodes)
		if reply["remoteRevoked"] != false {
			t.Fatalf("the reply must say the remote half did not happen, got %v", reply)
		}
		if !engine.sawStatement("mutation revokeSourceCredential") {
			t.Fatal("the local row must be flipped regardless")
		}
	})

	t.Run("a pasted token is revoked locally and nothing is attempted", func(t *testing.T) {
		hub := newGrantHub()
		i, _, engine, _ := grantHarness(t, hub, sealedRow(t, credentialStatusActive))
		nodes, err := i.handleSourceCredentialRevoke(callerCtx("v1:identity:user:alice"),
			map[string]any{"credentialId": "v1:platform:sourceCredential:abc"}, 0)
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if replyPayload(t, nodes)["remoteRevoked"] != false {
			t.Fatal("there is nothing at GitHub to revoke for a pasted token")
		}
		if n := hub.hits("/applications/Iv1.memqlconnect/grant"); n != 0 {
			t.Fatalf("%d revoke call(s) for a pasted token", n)
		}
		if !engine.sawStatement("mutation revokeSourceCredential") {
			t.Fatal("the row must still be flipped")
		}
	})
}

// ---------------------------------------------------------------------------
// Nothing leaks
// ---------------------------------------------------------------------------

// TestNoGrantSecretReachesALogOrARow drives the grant paths under a captured
// logger and greps both the log and every rendered statement.
//
// Its control is at the bottom: the same buffers ARE searched with a value
// that IS present, so a run in which nothing was logged or written fails here
// rather than passing on "does not contain".
func TestNoGrantSecretReachesALogOrARow(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	const newUserToken = "ghu_ROTATED_leak_0003"
	logger, logs := capturedLog()

	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget", http.StatusOK, grantRepoBody)
	hub.body("/repos/acme/widget/branches", http.StatusOK, `[{"name":"main"}]`)
	hub.body("/login/oauth/access_token", http.StatusOK,
		`{"access_token":"`+newUserToken+`","expires_in":28800,"refresh_token":"ghr_ROTATED_leak_0003"}`)
	hub.body("/user/installations", http.StatusOK, installationsBody)
	hub.body("/user/installations/42/repositories", http.StatusOK, `{"total_count":0,"repositories":[]}`)

	engine := &actorEngine{recordingEngine: recordingEngine{rows: map[string][]map[string]any{
		"query sourceCredentialSealedById": {sealedGrantRow(t, grantRowOpts{ExpiresAt: time.Now().UTC().Add(-time.Hour)})},
	}}}
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	s := &store{engine: engine, logger: logger, github: client}
	deps := &Deps{Store: s, Credentials: s.resolveCredential, PeekCredentials: s.peekCredential,
		GitHubApp: client, HTTP: &http.Client{Transport: hub}, Logger: logger, Limits: DefaultLimits()}

	ctx := callerCtx(grantOwner)
	if _, err := ProbeSource(ctx, deps, "https://github.com/acme/widget", grantCredentialId); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := SourceRepositories(ctx, deps, grantCredentialId, 0); err != nil {
		t.Fatalf("list: %v", err)
	}

	secrets := []string{grantUserToken, grantRefreshToken, newUserToken, "ghr_ROTATED_leak_0003", grantInstallToken}
	statements := engine.statements()
	haystack := logs.String() + "\n" + strings.Join(statements, "\n")
	for _, plain := range secrets {
		if strings.Contains(haystack, plain) {
			t.Errorf("%q reached a log line or a row", plain)
		}
	}

	// THE CONTROL. The haystack really was searched -- statements were
	// captured and every one of those secrets really was on the wire -- so
	// the assertions above ran against a run where a leak could have happened.
	if len(statements) == 0 {
		t.Fatal("control failed: no statements were captured")
	}
	wire := strings.Join(hub.bodies(), "\n")
	for _, r := range hub.seen() {
		wire += "\n" + r.Header.Get("Authorization")
	}
	// The OLD refresh token went out in the refresh form; the RENEWED user
	// token came back and was then presented as a bearer. Both really crossed
	// the wire, so the greps above ran against a run where a leak could have
	// happened.
	for _, plain := range []string{grantRefreshToken, newUserToken} {
		if !strings.Contains(wire, plain) {
			t.Fatalf("control failed: %q never reached GitHub, so the leak check proved nothing", plain)
		}
	}
	if !strings.Contains(haystack+"leak-probe", "leak-probe") {
		t.Fatal("control failed: the containment check cannot see a value that is present")
	}
}

// A grant row read through the whole resolver keeps its identity fields, which
// is what lets a refusal name the connection and what the card renders.
func TestResolvedGrantCarriesItsIdentity(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	_, s, _, _ := grantHarness(t, newGrantHub(), sealedGrantRow(t, grantRowOpts{}))
	got, err := s.peekCredential(context.Background(), grantCredentialId, grantOwner)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.IsGrant() || got.Login != "alice-gh" || got.ExternalId != "5150" || got.OwnerUserId != grantOwner {
		t.Fatalf("got %+v", got)
	}
	if strings.Join(got.Installations, ",") != grantInstallations {
		t.Fatalf("installations %v", got.Installations)
	}
}

// A []string field arrives as either spelling depending on how the row was
// decoded, and a reader that handled only one would report a grant reaching no
// installations.
func TestRowStringsReadsBothSpellings(t *testing.T) {
	cases := []struct {
		name string
		row  map[string]any
		want string
	}{
		{"a real []string", map[string]any{"ids": []string{"1", "2"}}, "1,2"},
		{"the []any a bundle decodes to", map[string]any{"ids": []any{"1", "2"}}, "1,2"},
		{"blanks are dropped", map[string]any{"ids": []any{"1", "  ", 3}}, "1"},
		{"absent", map[string]any{}, ""},
		{"nil row", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(rowStrings(tc.row, "ids"), ","); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
