package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// client_test.go -- the GitHub App client against a fake GitHub.
//
// Three properties carry the weight here and each has a NEGATIVE CONTROL, an
// assertion that the same instrument DOES see the thing when it is present:
//
//   - an installation token is minted once and reused until it nearly expires
//     (the control: advance the clock and watch it mint again);
//   - no secret reaches an error string (the control: the fake asserts it
//     RECEIVED those secrets, so a run where nothing was sent would fail
//     rather than pass on "does not contain");
//   - rate limiting is recognised in both of GitHub's spellings (the control:
//     an ordinary 403 is NOT filed as rate limiting).

// ---------------------------------------------------------------------------
// A fake GitHub
// ---------------------------------------------------------------------------

// hubHandler answers one request with a status and a body. A function rather
// than canned bytes so a route can count its own hits and read what it was
// sent.
type hubHandler func(*http.Request) (int, string)

// fakeHub is a path-routed GitHub. Exact paths, and 404 for everything else --
// deliberately, because "the endpoint nobody registered answered something"
// is how a test passes for a reason its author did not intend.
type fakeHub struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   map[*http.Request]string
	handlers map[string]hubHandler
}

func newHub() *fakeHub {
	return &fakeHub{handlers: map[string]hubHandler{}, bodies: map[*http.Request]string{}}
}

func (h *fakeHub) on(path string, fn hubHandler) *fakeHub {
	h.handlers[path] = fn
	return h
}

func (h *fakeHub) json(path string, status int, body string) *fakeHub {
	return h.on(path, func(*http.Request) (int, string) { return status, body })
}

func (h *fakeHub) RoundTrip(req *http.Request) (*http.Response, error) {
	var sent string
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		sent = string(raw)
	}
	clone := req.Clone(context.Background())
	h.mu.Lock()
	h.requests = append(h.requests, clone)
	h.bodies[clone] = sent
	handler := h.handlers[req.URL.Path]
	h.mu.Unlock()

	status, body := http.StatusNotFound, `{"message":"Not Found"}`
	if handler != nil {
		status, body = handler(req)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (h *fakeHub) seen() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*http.Request, len(h.requests))
	copy(out, h.requests)
	return out
}

func (h *fakeHub) hits(path string) int {
	n := 0
	for _, r := range h.seen() {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}

// sentBodies returns every request body the fake received, so a test can prove
// a secret really was on the wire before asserting it is not in an error.
func (h *fakeHub) sentBodies() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.bodies))
	for _, r := range h.requests {
		out = append(out, h.bodies[r])
	}
	return out
}

// ---------------------------------------------------------------------------
// A test app
// ---------------------------------------------------------------------------

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

// appKey generates ONE 2048-bit key for the whole file. Generation is the
// slowest thing in this package's suite and every test wants the same key.
func appKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = key
	})
	return testKey
}

const (
	testClientId     = "Iv1.testclientid"
	testClientSecret = "gh_CLIENTSECRET_0123456789"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	der := x509.MarshalPKCS1PrivateKey(appKey(t))
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	return Config{
		AppId:         "1234567",
		Slug:          "memql-connect",
		ClientId:      testClientId,
		ClientSecret:  testClientSecret,
		PrivateKeyB64: base64.StdEncoding.EncodeToString(encoded),
	}
}

// testClient wires a configured client at a fake hub, with a clock the test
// drives.
func testClient(t *testing.T, hub *fakeHub, clock *time.Time) *Client {
	t.Helper()
	return New(testConfig(t),
		WithHTTPClient(&http.Client{Transport: hub}),
		WithAPIBase("https://api.github.com"),
		WithOAuthBase("https://github.com"),
		WithClock(func() time.Time { return *clock }))
}

// ---------------------------------------------------------------------------
// The app JWT
// ---------------------------------------------------------------------------

// TestAppJWTIsRS256OverTheAppsClaims verifies the assertion GitHub will
// verify: the same signature, over the same two segments, carrying the three
// claims and the backdated iat.
func TestAppJWTIsRS256OverTheAppsClaims(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	c := testClient(t, newHub(), &now)

	token, err := c.appJWT(now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a compact JWS has three segments, got %d: %q", len(parts), token)
	}

	// The SIGNATURE is what GitHub checks, so it is what this test checks:
	// RS256 over "<header>.<claims>", verified with the public half.
	sig, derr := base64.RawURLEncoding.DecodeString(parts[2])
	if derr != nil {
		t.Fatalf("the signature is not base64url: %v", derr)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if verr := rsa.VerifyPKCS1v15(&appKey(t).PublicKey, crypto.SHA256, sum[:], sig); verr != nil {
		t.Fatalf("the signature does not verify against the app's key: %v", verr)
	}

	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(header) != `{"alg":"RS256","typ":"JWT"}` {
		t.Fatalf("header %s", header)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if jerr := json.Unmarshal(raw, &claims); jerr != nil {
		t.Fatalf("claims: %v", jerr)
	}
	if claims.Iss != "1234567" {
		t.Fatalf("iss %q, want the app id", claims.Iss)
	}
	// BACKDATED sixty seconds: a node whose clock runs fast issues an
	// assertion GitHub reads as being from the future and rejects.
	if want := now.Add(-jwtBackdate).Unix(); claims.Iat != want {
		t.Fatalf("iat %d, want %d (sixty seconds of clock skew)", claims.Iat, want)
	}
	if want := now.Add(jwtLifetime).Unix(); claims.Exp != want {
		t.Fatalf("exp %d, want %d", claims.Exp, want)
	}
	// AND THE SPAN INCLUDES THE BACKDATE. GitHub measures exp against iat, so
	// a lifetime chosen without counting the backdated minute lands on the
	// 600-second boundary it rejects at -- which is a refusal that only
	// appears in production, against the real GitHub, with no local
	// reproduction.
	if span := claims.Exp - claims.Iat; span > 540 {
		t.Fatalf("the assertion spans %ds counting the backdate; GitHub refuses anything past 600 and this leaves no margin", span)
	}
}

func TestAPrivateKeyErrorNamesTheVariableAndNotTheMaterial(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"not base64", "%%%not base64%%%"},
		{"not PEM", base64.StdEncoding.EncodeToString([]byte("hello"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.PrivateKeyB64 = tc.key
			c := New(cfg, WithClock(func() time.Time { return now }))
			_, err := c.appJWT(now)
			if err == nil {
				t.Fatal("a key this node cannot parse must refuse")
			}
			if !strings.Contains(err.Error(), EnvPrivateKeyB64) {
				t.Fatalf("the error must name the variable an operator would fix, got: %v", err)
			}
			if strings.Contains(err.Error(), tc.key) {
				t.Fatalf("the error carries the configured material: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Installation tokens
// ---------------------------------------------------------------------------

const mintedToken = "ghs_INSTALLATIONTOKEN_abcdefgh"

// TestInstallationTokenIsCachedUntilItNearlyExpires is the caching claim and
// its own control in one test: two calls, one mint; then the clock moves past
// the skew window and the third call mints again. Without the second half,
// "no request was made" would also pass for a client that never asked at all.
func TestInstallationTokenIsCachedUntilItNearlyExpires(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	hub := newHub().json("/app/installations/42/access_tokens", http.StatusCreated,
		`{"token":"`+mintedToken+`","expires_at":"`+expires.Format(time.RFC3339)+`","repository_selection":"all"}`)
	c := testClient(t, hub, &now)

	first, err := c.InstallationToken(context.Background(), 42)
	if err != nil || first != mintedToken {
		t.Fatalf("mint: %q %v", first, err)
	}
	if got := hub.hits("/app/installations/42/access_tokens"); got != 1 {
		t.Fatalf("want one mint, got %d", got)
	}

	// THE CACHE: still well inside the hour, so nothing leaves the process.
	now = now.Add(30 * time.Minute)
	second, err := c.InstallationToken(context.Background(), 42)
	if err != nil || second != mintedToken {
		t.Fatalf("cached: %q %v", second, err)
	}
	if got := hub.hits("/app/installations/42/access_tokens"); got != 1 {
		t.Fatalf("the second call must not reach GitHub, got %d mints", got)
	}

	// THE CONTROL: inside the skew window the token is treated as expired and
	// re-minted, so the assertion above is about the cache and not about a
	// fake nobody reached.
	now = expires.Add(-tokenSkew / 2)
	if _, err := c.InstallationToken(context.Background(), 42); err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if got := hub.hits("/app/installations/42/access_tokens"); got != 2 {
		t.Fatalf("a token inside its skew window must be re-minted, got %d mints", got)
	}

	// The mint is asked under the APP JWT, never under anybody's token.
	for _, r := range hub.seen() {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ey") {
			t.Fatalf("the mint must present the app assertion, got %q", auth)
		}
	}
}

func TestInstallationLookupAnswersNotInstalledOnA404(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget/installation", http.StatusOK, `{"id":42}`)
	c := testClient(t, hub, &now)

	id, err := c.InstallationForRepo(context.Background(), "acme", "widget")
	if err != nil || id != 42 {
		t.Fatalf("installed: %d %v", id, err)
	}
	// Anything the fake does not serve is a 404, which is exactly what GitHub
	// answers for a repository the app is not installed on.
	if _, err := c.InstallationForRepo(context.Background(), "acme", "other"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("want ErrNotInstalled, got %v", err)
	}
	// A 200 naming no installation is the same fact and must answer the same.
	hub.json("/repos/acme/empty/installation", http.StatusOK, `{}`)
	if _, err := c.InstallationForRepo(context.Background(), "acme", "empty"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a 200 with no id must answer ErrNotInstalled, got %v", err)
	}
}

// TestAnUnconfiguredClientAnswersNotConfigured: an engine node with no app is
// a real deployment, and every path must answer the operator's condition
// rather than dereference a nil or attempt a call it cannot sign.
func TestAnUnconfiguredClientAnswersNotConfigured(t *testing.T) {
	hub := newHub()
	c := New(Config{}, WithHTTPClient(&http.Client{Transport: hub}))
	if c.Configured() {
		t.Fatal("an empty config must not read as configured")
	}
	if got := len(c.Missing()); got != 5 {
		t.Fatalf("Missing must name all five values, got %d: %v", got, c.Missing())
	}
	ctx := context.Background()
	if _, err := c.InstallationForRepo(ctx, "acme", "widget"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("InstallationForRepo: %v", err)
	}
	if _, err := c.InstallationToken(ctx, 42); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("InstallationToken: %v", err)
	}
	if _, err := c.RefreshUserToken(ctx, "ghr_something"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("RefreshUserToken: %v", err)
	}
	if err := c.RevokeGrant(ctx, "ghu_something"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("RevokeGrant: %v", err)
	}
	if _, err := c.PendingInstallationRequests(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("PendingInstallationRequests: %v", err)
	}
	if url := c.InstallURL(); url != "" {
		t.Errorf("an unconfigured client offers no installation link, got %q", url)
	}
	// THE CONTROL: nothing left the process for any of them.
	if n := len(hub.seen()); n != 0 {
		t.Fatalf("%d request(s) left an unconfigured client", n)
	}
	// And a NIL client is in the same position, because a node with no app
	// wired is a node with no app.
	var nilClient *Client
	if nilClient.Configured() || len(nilClient.Missing()) != 5 || nilClient.InstallURL() != "" {
		t.Fatal("a nil client must read as unconfigured")
	}
}

// ---------------------------------------------------------------------------
// The user token
// ---------------------------------------------------------------------------

func TestRefreshUserTokenRotatesBothHalves(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	hub := newHub().json("/login/oauth/access_token", http.StatusOK,
		`{"access_token":"ghu_NEW","expires_in":28800,"refresh_token":"ghr_NEW","refresh_token_expires_in":15811200}`)
	c := testClient(t, hub, &now)

	set, err := c.RefreshUserToken(context.Background(), "ghr_OLD")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// BOTH halves: a refresh that kept the old refresh token would work for
	// eight hours and then be unrenewable.
	if set.AccessToken != "ghu_NEW" || set.RefreshToken != "ghr_NEW" {
		t.Fatalf("got %+v", set)
	}
	if want := now.Add(8 * time.Hour); !set.ExpiresAt.Equal(want) {
		t.Fatalf("expiry %v, want %v", set.ExpiresAt, want)
	}
	// The form carries the old refresh token and the OAuth pair.
	body := hub.sentBodies()[0]
	for _, want := range []string{"grant_type=refresh_token", "refresh_token=ghr_OLD", "client_id=" + testClientId} {
		if !strings.Contains(body, want) {
			t.Errorf("the refresh form must carry %q, got %q", want, body)
		}
	}
}

// TestRefreshRefusalsAllAnswerReauthorize: GitHub spells a spent refresh token
// three ways, and every one of them means the same thing -- the authorization
// is over and only the person can repair it.
func TestRefreshRefusalsAllAnswerReauthorize(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"200 with an error object", http.StatusOK, `{"error":"bad_refresh_token","error_description":"The refresh token passed is incorrect or expired."}`},
		{"400", http.StatusBadRequest, `{"error":"bad_verification_code"}`},
		{"401", http.StatusUnauthorized, `{"message":"Bad credentials"}`},
		{"200 with no token at all", http.StatusOK, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			hub := newHub().json("/login/oauth/access_token", tc.status, tc.body)
			c := testClient(t, hub, &now)
			if _, err := c.RefreshUserToken(context.Background(), "ghr_SPENT"); !errors.Is(err, ErrReauthorize) {
				t.Fatalf("want ErrReauthorize, got %v", err)
			}
		})
	}
	// An EMPTY refresh token never leaves the process: there is nothing to
	// exchange, and the answer is the same repair.
	now := time.Now().UTC()
	hub := newHub()
	c := testClient(t, hub, &now)
	if _, err := c.RefreshUserToken(context.Background(), "  "); !errors.Is(err, ErrReauthorize) {
		t.Fatalf("want ErrReauthorize, got %v", err)
	}
	if n := len(hub.seen()); n != 0 {
		t.Fatalf("%d request(s) left for a grant with no refresh token", n)
	}
}

// A 401 from a USER-token call is the authorization being refused, and must
// never surface as an ordinary status the caller would read as "this
// credential cannot see it".
func TestA401UnderAUserTokenIsReauthorize(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().
		json("/user", http.StatusUnauthorized, `{"message":"Bad credentials"}`).
		json("/user/installations", http.StatusUnauthorized, `{"message":"Bad credentials"}`).
		json("/repos/acme/widget/branches", http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	c := testClient(t, hub, &now)
	ctx := context.Background()

	if _, err := c.User(ctx, "ghu_DEAD"); !errors.Is(err, ErrReauthorize) {
		t.Errorf("User: %v", err)
	}
	if _, err := c.UserInstallations(ctx, "ghu_DEAD"); !errors.Is(err, ErrReauthorize) {
		t.Errorf("UserInstallations: %v", err)
	}
	if _, err := c.Branches(ctx, "ghu_DEAD", "acme", "widget"); !errors.Is(err, ErrReauthorize) {
		t.Errorf("Branches: %v", err)
	}
	// THE CONTROL: a 404 is NOT lifted, because a repository that is not
	// there is a different fact with a different repair.
	if _, err := c.Branches(ctx, "ghu_LIVE", "acme", "missing"); errors.Is(err, ErrReauthorize) {
		t.Errorf("a 404 must not read as a dead authorization: %v", err)
	} else if StatusOf(err) != http.StatusNotFound {
		t.Errorf("want a 404 status, got %v", err)
	}
}

func TestRevokeGrantTreatsAMissingGrantAsSuccess(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/applications/"+testClientId+"/grant", http.StatusNoContent, ``)
	c := testClient(t, hub, &now)

	if err := c.RevokeGrant(context.Background(), "ghu_LIVE"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	req := hub.seen()[0]
	if req.Method != http.MethodDelete {
		t.Fatalf("revoke is a DELETE, got %s", req.Method)
	}
	// Basic auth with the OAuth pair: this endpoint authenticates the APP, not
	// the person whose token is being revoked.
	user, pass, ok := req.BasicAuth()
	if !ok || user != testClientId || pass != testClientSecret {
		t.Fatalf("revoke must authenticate as the app, got %q/%v", user, ok)
	}
	if body := hub.sentBodies()[0]; !strings.Contains(body, `"access_token":"ghu_LIVE"`) {
		t.Fatalf("the revoke body must name the token, got %q", body)
	}

	// A 404 is the state the call was trying to reach: there is no such grant.
	hub404 := newHub()
	c404 := testClient(t, hub404, &now)
	if err := c404.RevokeGrant(context.Background(), "ghu_ALREADY_GONE"); err != nil {
		t.Fatalf("a grant that is already gone is not an error: %v", err)
	}
	// THE CONTROL: a 500 IS one, so the 404 above is a deliberate reading
	// rather than every status being swallowed.
	hub500 := newHub().json("/applications/"+testClientId+"/grant", http.StatusInternalServerError, `{}`)
	if err := testClient(t, hub500, &now).RevokeGrant(context.Background(), "ghu_LIVE"); err == nil {
		t.Fatal("a 500 from the revoke endpoint must be reported")
	}
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

func TestRateLimitIsRecognisedInBothSpellings(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name   string
		status int
		header string
		want   bool
	}{
		{"429 is a secondary limit", http.StatusTooManyRequests, "", true},
		{"403 with nothing remaining is the primary limit", http.StatusForbidden, "0", true},
		{"a plain 403 is an ordinary refusal", http.StatusForbidden, "", false},
		{"a 403 with quota left is an ordinary refusal", http.StatusForbidden, "4999", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newHub()
			hub.on("/user/installations", func(*http.Request) (int, string) { return tc.status, `{"message":"no"}` })
			// The header has to come back on the response, which the fake's
			// json helper does not do, so this route is wired by hand.
			c := New(testConfig(t), WithHTTPClient(&http.Client{Transport: headerHub{hub: hub, header: tc.header}}),
				WithClock(func() time.Time { return now }))
			_, err := c.UserInstallations(context.Background(), "ghu_LIVE")
			if got := IsRateLimited(err); got != tc.want {
				t.Fatalf("IsRateLimited = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

// headerHub adds X-RateLimit-Remaining to whatever the inner fake answers.
type headerHub struct {
	hub    *fakeHub
	header string
}

func (h headerHub) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := h.hub.RoundTrip(req)
	if err == nil && h.header != "" {
		resp.Header.Set("X-RateLimit-Remaining", h.header)
	}
	return resp, err
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

func TestFileContentsDecodesGitHubsWrappedBase64(t *testing.T) {
	now := time.Now().UTC()
	raw := "formatVersion: 1\nname: acme\ndeployables: []\n"
	// GitHub wraps the base64 at 60 columns, which base64.StdEncoding refuses
	// as written -- a decoder that did not strip the newlines would answer
	// "no manifest" for every manifest longer than 45 bytes.
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	wrapped := ""
	for i := 0; i < len(encoded); i += 60 {
		end := i + 60
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped += encoded[i:end] + "\n"
	}
	hub := newHub().json("/repos/acme/widget/contents/memql-package.yaml", http.StatusOK,
		`{"type":"file","encoding":"base64","content":`+quote(wrapped)+`}`)
	c := testClient(t, hub, &now)

	got, err := c.FileContents(context.Background(), "ghu_LIVE", "acme", "widget", "", "memql-package.yaml")
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	if string(got) != raw {
		t.Fatalf("got %q, want %q", got, raw)
	}
}

func TestDirectoryNamesAnswersOnlyDirectories(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget/contents/dsl", http.StatusOK,
		`[{"name":"acme","type":"dir"},{"name":"README.md","type":"file"},{"name":"widgets","type":"dir"}]`)
	c := testClient(t, hub, &now)

	got, err := c.DirectoryNames(context.Background(), "ghu_LIVE", "acme", "widget", "", "dsl")
	if err != nil {
		t.Fatalf("dirs: %v", err)
	}
	if strings.Join(got, ",") != "acme,widgets" {
		t.Fatalf("got %v", got)
	}
	// A repository with no dsl/ answers a 404, and that is an ORDINARY
	// SPAs-only package rather than a problem -- the caller reads the error
	// and answers an empty list.
	if _, err := c.DirectoryNames(context.Background(), "ghu_LIVE", "acme", "nodsl", "", "dsl"); err == nil {
		t.Fatal("a missing directory must report itself so the caller can answer empty")
	}
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// ---------------------------------------------------------------------------
// Secrets never reach an error
// ---------------------------------------------------------------------------

// TestNoSecretReachesAnErrorString drives every failing path and greps the
// errors, and it carries its own control: the fake asserts the secrets WERE
// on the wire, so a run in which nothing was sent fails here rather than
// passing on "does not contain".
func TestNoSecretReachesAnErrorString(t *testing.T) {
	now := time.Now().UTC()
	const refreshToken = "ghr_REFRESHSECRET_zzz"
	const userToken = "ghu_USERSECRET_yyy"

	hub := newHub().
		json("/login/oauth/access_token", http.StatusInternalServerError, `{"message":"boom"}`).
		json("/applications/"+testClientId+"/grant", http.StatusInternalServerError, `{"message":"boom"}`).
		json("/user", http.StatusInternalServerError, `{"message":"boom"}`)
	c := testClient(t, hub, &now)
	ctx := context.Background()

	var errs []string
	if _, err := c.RefreshUserToken(ctx, refreshToken); err != nil {
		errs = append(errs, err.Error())
	}
	if err := c.RevokeGrant(ctx, userToken); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := c.User(ctx, userToken); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) != 3 {
		t.Fatalf("control failed: want three errors to inspect, got %d", len(errs))
	}
	for _, e := range errs {
		for _, secret := range []string{refreshToken, userToken, testClientSecret} {
			if strings.Contains(e, secret) {
				t.Errorf("a secret reached an error string: %s", e)
			}
		}
	}

	// THE REACHABLE POSITIVE. Every one of those secrets really did cross the
	// wire, so the greps above were looking at a run where they could have
	// leaked. Without this, a client that made no requests at all would pass.
	wire := strings.Join(hub.sentBodies(), "\n")
	for _, r := range hub.seen() {
		wire += "\n" + r.Header.Get("Authorization")
		if user, pass, ok := r.BasicAuth(); ok {
			wire += "\n" + user + ":" + pass
		}
	}
	for _, secret := range []string{refreshToken, userToken, testClientSecret} {
		if !strings.Contains(wire, secret) {
			t.Fatalf("control failed: %q never reached the fake, so the assertions above proved nothing", secret)
		}
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestConfigFromEnvReadsAllFiveAndTrims(t *testing.T) {
	t.Setenv(EnvAppId, "  1234567\n")
	t.Setenv(EnvSlug, "memql-connect")
	t.Setenv(EnvClientId, testClientId)
	t.Setenv(EnvClientSecret, testClientSecret)
	t.Setenv(EnvPrivateKeyB64, " a2V5\n")

	cfg := ConfigFromEnv()
	// TRIMMED, because a secret pasted into a manifest arrives with a newline
	// more often than not and a key that differs by trailing whitespace fails
	// at signature verification with a message about the signature.
	if cfg.AppId != "1234567" || cfg.PrivateKeyB64 != "a2V5" {
		t.Fatalf("got %+v", cfg)
	}
	if !cfg.Configured() {
		t.Fatalf("all five present must read as configured: missing %v", cfg.Missing())
	}

	// ALL FIVE OR NONE: four of five is treated as absent, not as most of an
	// app, and the missing one is NAMED.
	t.Setenv(EnvClientSecret, "")
	partial := ConfigFromEnv()
	if partial.Configured() {
		t.Fatal("a partial configuration must not read as configured")
	}
	if got := partial.Missing(); len(got) != 1 || got[0] != EnvClientSecret {
		t.Fatalf("Missing must name the absent value, got %v", got)
	}
}

func TestInstallURLNamesTheApp(t *testing.T) {
	now := time.Now().UTC()
	c := testClient(t, newHub(), &now)
	if got := c.InstallURL(); got != "https://github.com/apps/memql-connect/installations/new" {
		t.Fatalf("got %q", got)
	}
}
