package packages

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/secret"
)

// probe_test.go -- the two compose probes (epic memql#4885, D11) against a
// fake GitHub: public, private without a credential, private with one, a
// credential the caller cannot read, a revoked one, rate-limited, a non-GitHub
// host, and a GitHub this cluster cannot reach. And the property every case
// shares: a probe WRITES NOTHING AND STAMPS NOTHING.

// probeGitHub answers GET /repos/{owner}/{repo} with one canned response and
// records every request it is handed, so a test reads which requests were
// made, what bearer they carried -- and, for a credential that did not
// resolve, that none was made at all.
type probeGitHub struct {
	status  int
	body    string
	headers http.Header
	// err, when set, is what the transport answers instead of a response --
	// the "GitHub is unreachable from this cluster" case.
	err error

	mu       sync.Mutex
	requests []*http.Request
}

func (g *probeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.requests = append(g.requests, req.Clone(context.Background()))
	g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	h := http.Header{"Content-Type": []string{"application/json"}}
	for k, v := range g.headers {
		h[k] = v
	}
	return &http.Response{
		StatusCode: g.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(g.body)),
		Request:    req,
	}, nil
}

func (g *probeGitHub) seen() []*http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*http.Request, len(g.requests))
	copy(out, g.requests)
	return out
}

// probeHarness wires Deps over an actorEngine and a fake GitHub, with the
// store's own two resolvers -- the one that stamps and the one that does not
// -- so a test can see which of them the probe reached for.
func probeHarness(t *testing.T, gh *probeGitHub) (*Deps, *actorEngine) {
	t.Helper()
	engine := &actorEngine{}
	s := &store{engine: engine, logger: discardLogger()}
	return &Deps{
		Store:           s,
		Credentials:     s.resolveCredential,
		PeekCredentials: s.peekCredential,
		HTTP:            &http.Client{Transport: gh},
		Logger:          discardLogger(),
	}, engine
}

const publicRepoBody = `{"id":1,"private":false,"default_branch":"main"}`
const privateRepoBody = `{"id":2,"private":true,"default_branch":"trunk"}`

// ---------------------------------------------------------------------------
// sourceProbe
// ---------------------------------------------------------------------------

func TestSourceProbeAnswersAPublicRepository(t *testing.T) {
	gh := &probeGitHub{status: http.StatusOK, body: publicRepoBody}
	d, engine := probeHarness(t, gh)

	res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/widget", "")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	want := SourceProbeResult{Host: "github.com", Reachable: true, Private: false, DefaultBranch: "main", Reason: ProbeReasonOK}
	if res != want {
		t.Fatalf("got %+v, want %+v", res, want)
	}
	reqs := gh.seen()
	if len(reqs) != 1 {
		t.Fatalf("want exactly one request, got %d", len(reqs))
	}
	if got := reqs[0].URL.String(); got != "https://api.github.com/repos/acme/widget" {
		t.Fatalf("the probe asks the repository endpoint and nothing else, got %s", got)
	}
	if reqs[0].Method != http.MethodGet {
		t.Fatalf("a probe is a GET, got %s", reqs[0].Method)
	}
	if auth := reqs[0].Header.Get("Authorization"); auth != "" {
		t.Fatalf("a probe with no credential sends no bearer, got %q", auth)
	}
	// The same headers the fetcher sends, so GitHub answers the probe and the
	// fetch the same way.
	for k, want := range map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "memql-packages"} {
		if got := reqs[0].Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	// NOTHING WRITTEN, NOTHING READ: no credential was named, so the engine
	// was not consulted at all.
	if stmts := engine.statements(); len(stmts) != 0 {
		t.Fatalf("a probe of a public repository touches the graph for nothing, got %v", stmts)
	}
}

func TestSourceProbeAnswersNotFoundOrPrivateWithoutACredential(t *testing.T) {
	gh := &probeGitHub{status: http.StatusNotFound, body: `{"message":"Not Found"}`}
	d, _ := probeHarness(t, gh)

	res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/secret", "")
	if err != nil {
		t.Fatalf("a 404 is a typed reason, not an error: %v", err)
	}
	// GitHub answers 404 for a private repository and for one that does not
	// exist alike, and the reason says exactly that rather than guessing.
	if res.Reason != ProbeReasonNotFoundOrPrivate || res.Reachable || res.Private {
		t.Fatalf("got %+v", res)
	}
	if res.Host != "github.com" {
		t.Fatalf("the host is answered even when the repository is not: %+v", res)
	}
}

// TestSourceProbeUnderACredentialPresentsTheBearerAndStampsNothing is the
// probe's whole difference from a fetch: the SAME sealed read under the SAME
// rule, and NO heartbeat -- a probe is a question, and lastUsedAt is the
// record of a fetch.
func TestSourceProbeUnderACredentialPresentsTheBearerAndStampsNothing(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	gh := &probeGitHub{status: http.StatusOK, body: privateRepoBody}
	d, engine := probeHarness(t, gh)
	engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {sealedRow(t, credentialStatusActive)}}

	res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/secret", "v1:platform:sourceCredential:abc")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	want := SourceProbeResult{Host: "github.com", Reachable: true, Private: true, DefaultBranch: "trunk", Reason: ProbeReasonOK}
	if res != want {
		t.Fatalf("got %+v, want %+v", res, want)
	}
	reqs := gh.seen()
	if len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("the probe must present the resolved credential as its bearer, got %v", authHeaders(reqs))
	}

	// THE ONE STATEMENT is the sealed read; no mutation of any kind, and in
	// particular no touchSourceCredential. The resolver the fetcher uses
	// would have stamped one, which is why the probe has a resolver of its
	// own.
	stmts := engine.statements()
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "query sourceCredentialSealedById(") {
		t.Fatalf("a probe issues exactly the sealed read and nothing else, got %v", stmts)
	}
	if engine.sawStatement("mutation ") {
		t.Fatalf("a probe WRITES NOTHING, got %v", stmts)
	}
	// The sealed read runs under the CALLER's actor: the person composing is
	// choosing their own credential, so the caller is the owner here.
	if got := engine.actors["sourceCredentialSealedById"]; got != "v1:identity:user:alice" {
		t.Fatalf("the sealed read must run as the caller, got %q", got)
	}
	// And the value reached no reply, no row and no log line.
	for _, q := range stmts {
		if strings.Contains(q, testToken) {
			t.Fatalf("the token VALUE reached a statement: %s", q)
		}
	}
}

func TestSourceProbeSaysWhenTheCredentialCannotSeeIt(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		gh := &probeGitHub{status: status, body: `{"message":"no"}`}
		d, engine := probeHarness(t, gh)
		engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": {sealedRow(t, credentialStatusActive)}}

		res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/secret", "v1:platform:sourceCredential:abc")
		if err != nil {
			t.Fatalf("HTTP %d under a credential is a typed reason, not an error: %v", status, err)
		}
		if res.Reason != ProbeReasonCredentialCannotSeeIt || res.Reachable {
			t.Fatalf("HTTP %d: got %+v", status, res)
		}
		if len(gh.seen()) != 1 {
			t.Fatalf("HTTP %d: the request was made under the credential", status)
		}
		if engine.sawStatement("mutation ") {
			t.Fatalf("HTTP %d: a probe writes nothing", status)
		}
	}
}

// A credential the caller cannot read -- somebody else's, or none -- and a
// revoked one are answered as reasons BEFORE any request leaves the cluster,
// exactly as the fetcher refuses them before fetching.
func TestSourceProbeAnswersACredentialRefusalWithoutARequest(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	cases := []struct {
		name string
		rows []map[string]any
		want string
	}{
		{"zero rows is not found", nil, ProbeReasonCredentialNotFound},
		{"a revoked row is revoked", []map[string]any{sealedRow(t, credentialStatusRevoked)}, ProbeReasonCredentialRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &probeGitHub{status: http.StatusOK, body: privateRepoBody}
			d, engine := probeHarness(t, gh)
			if tc.rows != nil {
				engine.rows = map[string][]map[string]any{"query sourceCredentialSealedById": tc.rows}
			}
			res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/secret", "v1:platform:sourceCredential:abc")
			if err != nil {
				t.Fatalf("a credential refusal is a typed reason, not an error: %v", err)
			}
			if res.Reason != tc.want || res.Reachable {
				t.Fatalf("got %+v, want reason %s", res, tc.want)
			}
			if n := len(gh.seen()); n != 0 {
				t.Fatalf("%d request(s) left the cluster for a credential that did not resolve", n)
			}
			if engine.sawStatement("mutation ") {
				t.Fatal("a probe writes nothing")
			}
		})
	}
}

func TestSourceProbeAnswersRateLimited(t *testing.T) {
	cases := []struct {
		name string
		gh   *probeGitHub
	}{
		{"403 with the limit exhausted", &probeGitHub{status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`, headers: http.Header{"X-Ratelimit-Remaining": []string{"0"}}}},
		{"429", &probeGitHub{status: http.StatusTooManyRequests, body: `{"message":"slow down"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := probeHarness(t, tc.gh)
			res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/widget", "")
			if err != nil {
				t.Fatalf("rate limiting is a typed reason, not an error: %v", err)
			}
			if res.Reason != ProbeReasonRateLimited || res.Reachable {
				t.Fatalf("got %+v", res)
			}
		})
	}
	// The control: a 403 WITHOUT the exhausted-limit header and without a
	// credential is not rate limiting and must not be filed as it; it is a
	// refusal from GitHub this cluster cannot type, so it is an error the
	// stop shows and stays editable behind.
	gh := &probeGitHub{status: http.StatusForbidden, body: `{"message":"forbidden"}`}
	d, _ := probeHarness(t, gh)
	if _, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/widget", ""); RefusalCode(err) != CodeSourceUnreadable {
		t.Fatalf("an untyped GitHub refusal is source_unreadable, got %v", err)
	}
}

func TestSourceProbeAnswersANonGitHubHostAsAReason(t *testing.T) {
	gh := &probeGitHub{status: http.StatusOK, body: publicRepoBody}
	d, engine := probeHarness(t, gh)

	res, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://gitlab.com/acme/widget", "")
	if err != nil {
		t.Fatalf("a non-GitHub host is a typed reason, not an error: %v", err)
	}
	want := SourceProbeResult{Host: "gitlab.com", Reachable: false, Reason: ProbeReasonSourceHostUnsupported}
	if res != want {
		t.Fatalf("got %+v, want %+v", res, want)
	}
	if n := len(gh.seen()); n != 0 {
		t.Fatalf("%d request(s) left the cluster for a host it does not fetch from", n)
	}
	if stmts := engine.statements(); len(stmts) != 0 {
		t.Fatalf("nothing to resolve for a host nothing fetches from, got %v", stmts)
	}

	// The boundary: a URL that is not a URL, or names no repository, has no
	// host to be unsupported and stays an ERROR -- the stop says so.
	for _, raw := range []string{"", "https://github.com/acme", "::nope"} {
		if _, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, raw, ""); RefusalCode(err) != CodeSourceUnreadable {
			t.Errorf("%q: want %s, got %v", raw, CodeSourceUnreadable, err)
		}
	}
}

// A GitHub this cluster cannot reach IS an error, deliberately: the stop says
// so and stays editable, and it never blocks Analyze on a public repository,
// because the fetch is the authority and the probe is a courtesy (section H).
func TestSourceProbeReportsAnUnreachableGitHubAsAnError(t *testing.T) {
	gh := &probeGitHub{err: errors.New("dial tcp: no route to host")}
	d, _ := probeHarness(t, gh)

	_, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/widget", "")
	if got := RefusalCode(err); got != CodeSourceUnreadable {
		t.Fatalf("want %s, got %s (%v)", CodeSourceUnreadable, got, err)
	}
	if !strings.Contains(err.Error(), "could not reach GitHub") {
		t.Fatalf("the error must say what could not be reached, got: %v", err)
	}
}

// A node with no resolver wired refuses a credentialled probe rather than
// probing anonymously -- the direction the fetcher and the poll both take.
func TestSourceProbeRefusesACredentialOnANodeThatCannotResolve(t *testing.T) {
	gh := &probeGitHub{status: http.StatusOK, body: privateRepoBody}
	d, _ := probeHarness(t, gh)
	d.PeekCredentials = nil

	_, err := ProbeSource(callerCtx("v1:identity:user:alice"), d, "https://github.com/acme/secret", "v1:platform:sourceCredential:abc")
	if RefusalCode(err) != CodeSourceUnreadable {
		t.Fatalf("want %s, got %v", CodeSourceUnreadable, err)
	}
	if len(gh.seen()) != 0 {
		t.Fatal("a node that cannot resolve credentials must not probe anonymously in their place")
	}
}

// The handler's reply carries exactly the five keys the Source stop reads.
func TestSourceProbeHandlerRepliesTheWireShape(t *testing.T) {
	gh := &probeGitHub{status: http.StatusOK, body: publicRepoBody}
	d, engine := probeHarness(t, gh)
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() { i.deps = d })

	nodes, err := i.handleSourceProbe(callerCtx("v1:identity:user:alice"), map[string]any{"repoUrl": "https://github.com/acme/widget"}, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	reply := replyPayload(t, nodes)
	for _, key := range []string{"host", "reachable", "private", "defaultBranch", "reason"} {
		if _, ok := reply[key]; !ok {
			t.Errorf("reply is missing %q: %v", key, reply)
		}
	}
	if len(reply) != 5 {
		t.Fatalf("reply carries exactly the five keys, got %v", reply)
	}
	if reply["reason"] != ProbeReasonOK || reply["defaultBranch"] != "main" || reply["reachable"] != true {
		t.Fatalf("reply %v", reply)
	}

	if _, err := i.handleSourceProbe(callerCtx("v1:identity:user:alice"), map[string]any{}, 0); err == nil {
		t.Fatal("repoUrl is required")
	}
}

// ---------------------------------------------------------------------------
// artifactProbe
// ---------------------------------------------------------------------------

// zipOf builds a zip archive over a tree, the way a person's Files upload
// arrives.
func zipOf(t *testing.T, tree fstest.MapFS) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, f := range tree {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// artifactDeps wires the REAL fetcher over canned artifact bytes, so the probe
// is measured through OpenZip under the limits it will run under in
// production rather than through a fake tree.
func artifactDeps(raw []byte, limits Limits) *Deps {
	return &Deps{
		Fetcher: &githubFetcher{artifactBytes: func(context.Context, string) ([]byte, string, error) {
			return raw, "application/zip", nil
		}},
		Limits: limits,
		Logger: discardLogger(),
	}
}

func TestArtifactProbeTellsAPackageTreeFromABuiltSite(t *testing.T) {
	cases := []struct {
		name string
		tree fstest.MapFS
		want ArtifactProbeResult
	}{
		{
			name: "a package tree: the manifest at the root",
			tree: fstest.MapFS{
				ManifestName:              file(validManifest),
				"clients/docs/index.html": file("<!doctype html>\n"),
			},
			want: ArtifactProbeResult{IsPackage: true, IsBuiltSite: false, FileCount: 2, TotalBytes: int64(len(validManifest) + len("<!doctype html>\n"))},
		},
		{
			name: "a built site: index.html at the root and no manifest",
			tree: fstest.MapFS{
				"index.html":    file("<!doctype html><title>acme</title>\n"),
				"assets/app.js": file("console.log(1)\n"),
			},
			want: ArtifactProbeResult{IsPackage: false, IsBuiltSite: true, FileCount: 2, TotalBytes: int64(len("<!doctype html><title>acme</title>\n") + len("console.log(1)\n"))},
		},
		{
			// Both at the root is a PACKAGE: the manifest is the stronger
			// claim, and the package path analyzes the tree properly.
			name: "both at the root is a package",
			tree: fstest.MapFS{
				ManifestName: file(validManifest),
				"index.html": file("<!doctype html>\n"),
			},
			want: ArtifactProbeResult{IsPackage: true, IsBuiltSite: false, FileCount: 2, TotalBytes: int64(len(validManifest) + len("<!doctype html>\n"))},
		},
		{
			// index.html one level down is neither: a zip of a folder that
			// wraps the site is the common mistake, and saying "built site"
			// would publish a bundle whose root has no index.
			name: "neither: a wrapped site",
			tree: fstest.MapFS{
				"site/index.html": file("<!doctype html>\n"),
				"README.md":       file("# acme\n"),
			},
			want: ArtifactProbeResult{IsPackage: false, IsBuiltSite: false, FileCount: 2, TotalBytes: int64(len("<!doctype html>\n") + len("# acme\n"))},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := artifactDeps(zipOf(t, tc.tree), Limits{})
			got, err := ProbeArtifact(callerCtx("v1:identity:user:alice"), d, "v1:library:artifact:zip")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The probe opens the zip under the packages limits, so a zip the deploy would
// refuse is refused here, by the same code, before a person commits to it.
func TestArtifactProbeRunsUnderThePackagesLimits(t *testing.T) {
	raw := zipOf(t, fstest.MapFS{
		"index.html": file("<!doctype html>\n"),
		"a.txt":      file("aaaa"),
		"b.txt":      file("bbbb"),
	})
	d := artifactDeps(raw, Limits{MaxFileCount: 2})
	_, err := ProbeArtifact(callerCtx("v1:identity:user:alice"), d, "v1:library:artifact:zip")
	if got := RefusalCode(err); got != CodeSourceTooLarge {
		t.Fatalf("want %s, got %s (%v)", CodeSourceTooLarge, got, err)
	}
	// The reachable positive: the same zip under the default limits opens.
	if _, err := ProbeArtifact(callerCtx("v1:identity:user:alice"), artifactDeps(raw, Limits{}), "v1:library:artifact:zip"); err != nil {
		t.Fatalf("control failed: the zip must open under the defaults, got %v", err)
	}

	// And a zip that is not a zip is source_unreadable, as the fetch says.
	if _, err := ProbeArtifact(callerCtx("v1:identity:user:alice"), artifactDeps([]byte("not a zip"), Limits{}), "v1:library:artifact:zip"); RefusalCode(err) != CodeSourceUnreadable {
		t.Fatalf("want %s, got %v", CodeSourceUnreadable, err)
	}
}

func TestArtifactProbeHandlerRepliesTheWireShape(t *testing.T) {
	engine := &recordingEngine{}
	i := NewIntegration(engine, discardLogger())
	i.depsOnce.Do(func() {
		i.deps = artifactDeps(zipOf(t, fstest.MapFS{"index.html": file("<!doctype html>\n")}), Limits{})
	})

	nodes, err := i.handleArtifactProbe(callerCtx("v1:identity:user:alice"), map[string]any{"artifactId": "v1:library:artifact:zip"}, 0)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	reply := replyPayload(t, nodes)
	for _, key := range []string{"isPackage", "isBuiltSite", "fileCount", "totalBytes"} {
		if _, ok := reply[key]; !ok {
			t.Errorf("reply is missing %q: %v", key, reply)
		}
	}
	if len(reply) != 4 || reply["isBuiltSite"] != true || reply["isPackage"] != false {
		t.Fatalf("reply %v", reply)
	}
	// A probe reads and never writes.
	if engine.sawStatement("mutation ") {
		t.Fatalf("a probe writes nothing, got %v", engine.statements())
	}

	if _, err := i.handleArtifactProbe(callerCtx("v1:identity:user:alice"), map[string]any{}, 0); err == nil {
		t.Fatal("artifactId is required")
	}
}

// Both probes are on the surface the builtins in dsl/platform/builtins.memql
// name by executor.
func TestProbeCapabilitiesAreRegistered(t *testing.T) {
	i := NewIntegration(&recordingEngine{}, discardLogger())
	found := map[string]bool{}
	for _, c := range i.Capabilities() {
		found[c.Name] = true
	}
	for _, name := range []string{"sourceProbe", "artifactProbe"} {
		if !found[name] {
			t.Errorf("capability %q is not registered; dsl/platform/builtins.memql names integration.packages.%s", name, name)
		}
	}
}
