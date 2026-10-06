package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// verify_test.go -- memql-verify tested the way it is used: against a fake
// cluster of httptest servers, one per public host, with a fake clock so the
// retry loop runs without waiting. Nothing here leaves loopback.
//
// The tests read the command's two outputs and nothing else: what it prints
// and the evidence file it writes. The evidence is decoded into the types at
// the bottom of this file, which spell out the JSON contract and share no
// code with the command, so a renamed key in the command fails a test instead
// of silently renaming the contract.

const (
	testRelease = "v0.24.1"
	testCommit  = "1a2b3c4"
)

// ---------------------------------------------------------------------------
// The fake cluster
// ---------------------------------------------------------------------------

// served is one file a fake host serves.
type served struct {
	status      int // 0 serves 200
	contentType string
	body        []byte
}

// reply is one /healthz answer.
type reply struct {
	status int    // 0 answers 200
	body   string // the raw body
}

// healthy is what a node of the given release writes at /healthz. An empty
// version or commit is left out of the body, as the engine leaves them out.
func healthy(node, version, commit string) reply {
	body := map[string]any{"status": "ok", "nodeId": node, "activeStreams": 1}
	if version != "" {
		body["version"] = version
	}
	if commit != "" {
		body["commit"] = commit
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return reply{body: string(raw)}
}

func always(r reply) func(int) reply { return func(int) reply { return r } }

// hashOf is the manifest's spelling of a file. The tests compute it on their
// own rather than through the command, so a wrong hash in the command cannot
// agree with itself.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func manifestServed(files map[string]string) *served {
	raw, err := json.Marshal(map[string]any{"schema": 1, "algorithm": "sha256", "files": files})
	if err != nil {
		panic(err)
	}
	return &served{contentType: "application/json", body: raw}
}

// indexPage is the shell the OS host serves, as the edge serves it: Vite's
// references (an absolute path, a relative one, a dot-relative one), an icon,
// a script on another origin, and the refresh tag the edge injects into every
// document it serves.
//
// The icon, the foreign script and the edge's own script are each something
// the command must NOT fetch, and none of them can be fetched successfully
// here: the fake serves a 404 for the icon and the edge script, and a hit
// counter on the foreign origin lets a test see the request. A command that
// fetched any of them would fail the happy path.
func indexPage(foreignURL string) string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>MemQL OS</title>
<link rel="icon" href="/favicon.svg">
<script type="module" crossorigin src="/assets/index-a1.js"></script>
<link rel="modulepreload" crossorigin href="assets/chunk-b2.js">
<link rel="stylesheet" crossorigin href="./assets/index-a1.css">
<script src="` + foreignURL + `/analytics.js"></script>
<script src="/_memql/site-refresh.js" data-memql-version="abc123" defer></script>
</head>
<body><div id="root"></div></body>
</html>
`
}

func docsPageFor(version string) string {
	return `<!doctype html><html><head><title>Docs</title><meta name="memql-docs-version" content="` + version + `"/></head><body></body></html>`
}

// cluster is the three public hosts of one cluster, its docs site, and a
// foreign origin nothing should ever call.
type cluster struct {
	t *testing.T

	api, identity, osHost, docs, foreign *httptest.Server

	mu         sync.Mutex
	health     map[string]func(n int) reply // by role: api, identity, os
	healthHits map[string]int
	hits       map[string]int // "<role> <path>"
	redirects  map[string]string
	files      map[string]*served // the OS host, by URL path; "/" is the page
	entries    map[string]string  // what the default manifest lists
	docsPage   *served
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{
		t:          t,
		health:     map[string]func(int) reply{},
		healthHits: map[string]int{},
		hits:       map[string]int{},
		redirects:  map[string]string{},
		files:      map[string]*served{},
		entries:    map[string]string{},
	}
	assets := map[string]*served{
		"/assets/index-a1.js":  {contentType: "text/javascript; charset=utf-8", body: []byte(`console.log("memql os");`)},
		"/assets/chunk-b2.js":  {contentType: "text/javascript; charset=utf-8", body: []byte(`export const chunk = 1;`)},
		"/assets/index-a1.css": {contentType: "text/css; charset=utf-8", body: []byte(`body{margin:0}`)},
	}
	for path, a := range assets {
		c.files[path] = a
		c.entries[strings.TrimPrefix(path, "/")] = hashOf(a.body)
	}
	// The dist copy of the page is in the manifest and is never compared: the
	// edge changes the bytes it serves.
	c.entries["index.html"] = hashOf([]byte("<html>the dist copy</html>"))
	c.files["/memql-bundle.json"] = manifestServed(c.entries)
	c.docsPage = &served{contentType: "text/html; charset=utf-8", body: []byte(docsPageFor("0.24.1"))}
	for _, role := range []string{"api", "identity", "os"} {
		c.health[role] = always(healthy(role+"-node-1", testRelease, testCommit))
	}

	c.foreign = c.start("foreign", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	c.files["/"] = &served{contentType: "text/html; charset=utf-8", body: []byte(indexPage(c.foreign.URL))}
	c.api = c.start("api", c.serveHealthOnly("api"))
	c.identity = c.start("identity", c.serveHealthOnly("identity"))
	c.osHost = c.start("os", c.serveOS)
	c.docs = c.start("docs", c.serveDocs)
	return c
}

// start serves h as the named role, counting every request and answering any
// redirect a test installed for that path.
func (c *cluster) start(role string, h http.HandlerFunc) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := role + " " + r.URL.Path
		c.mu.Lock()
		c.hits[key]++
		to := c.redirects[key]
		c.mu.Unlock()
		if to != "" {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		h(w, r)
	}))
	c.t.Cleanup(srv.Close)
	return srv
}

func (c *cluster) serveHealthOnly(role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		c.serveHealth(role, w)
	}
}

func (c *cluster) serveHealth(role string, w http.ResponseWriter) {
	c.mu.Lock()
	n := c.healthHits[role]
	c.healthHits[role]++
	answer := c.health[role]
	c.mu.Unlock()

	rep := answer(n)
	w.Header().Set("Content-Type", "application/json")
	if rep.status != 0 {
		w.WriteHeader(rep.status)
	}
	_, _ = io.WriteString(w, rep.body)
}

func (c *cluster) serveOS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		c.serveHealth("os", w)
		return
	}
	c.mu.Lock()
	file := c.files[r.URL.Path]
	var copied served
	if file != nil {
		copied = *file
	}
	c.mu.Unlock()
	if file == nil {
		http.NotFound(w, r)
		return
	}
	writeServed(w, copied)
}

func (c *cluster) serveDocs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/docs/" {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	page := *c.docsPage
	c.mu.Unlock()
	writeServed(w, page)
}

func writeServed(w http.ResponseWriter, f served) {
	if f.contentType != "" {
		w.Header().Set("Content-Type", f.contentType)
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	_, _ = w.Write(f.body)
}

// edit changes the cluster under its lock, so a test can change what a host
// answers without racing the server goroutines.
func (c *cluster) edit(f func(c *cluster)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

func (c *cluster) setHealth(role string, answer func(n int) reply) {
	c.edit(func(c *cluster) {
		c.health[role] = answer
		c.healthHits[role] = 0
	})
}

func (c *cluster) setFile(path string, f *served) {
	c.edit(func(c *cluster) { c.files[path] = f })
}

func (c *cluster) redirect(role, path, to string) {
	c.edit(func(c *cluster) { c.redirects[role+" "+path] = to })
}

func (c *cluster) hitCount(role, path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[role+" "+path]
}

func (c *cluster) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.hits {
		total += n
	}
	return total
}

// ---------------------------------------------------------------------------
// Running the command
// ---------------------------------------------------------------------------

// fakeClock is the command's clock and its sleep: a sleep advances the clock
// by exactly what it was asked for and returns at once, so a fifteen minute
// wait costs nothing and its arithmetic is exact.
type fakeClock struct {
	t       time.Time
	sleeps  []time.Duration
	onSleep func(n int) // called with the 1-based count, after the clock moved
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) now() time.Time { return f.t }

func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.sleeps = append(f.sleeps, d)
	f.t = f.t.Add(d)
	if f.onSleep != nil {
		f.onSleep(len(f.sleeps))
	}
	return nil
}

type tweak func(*config, *fakeClock)

func withVersion(v string) tweak {
	return func(cfg *config, _ *fakeClock) { cfg.version = v }
}

func retrying(wait, interval time.Duration) tweak {
	return func(cfg *config, _ *fakeClock) { cfg.wait, cfg.interval = wait, interval }
}

type outcome struct {
	code     int
	stdout   string
	stderr   string
	evidence evidenceDoc
	path     string
	clock    *fakeClock
}

// verify runs the command against the cluster. By default it makes ONE
// attempt (--wait=0): a test about a persistent condition then sees it once,
// and only the tests about the retry loop wait.
func (c *cluster) verify(tweaks ...tweak) outcome {
	c.t.Helper()
	return c.verifyWith(context.Background(), tweaks...)
}

func (c *cluster) verifyWith(ctx context.Context, tweaks ...tweak) outcome {
	c.t.Helper()
	clock := newFakeClock()
	cfg := config{
		version:        testRelease,
		docsURL:        c.docs.URL + "/docs/",
		wait:           0,
		interval:       30 * time.Second,
		samples:        3,
		evidencePath:   filepath.Join(c.t.TempDir(), "verify-rollout.json"),
		requestTimeout: 5 * time.Second,
		apiURL:         c.api.URL,
		identityURL:    c.identity.URL,
		osURL:          c.osHost.URL,
		now:            clock.now,
		sleep:          clock.sleep,
	}
	for _, tw := range tweaks {
		tw(&cfg, clock)
	}

	var stdout, stderr bytes.Buffer
	code := run(ctx, cfg, &stdout, &stderr)
	o := outcome{code: code, stdout: stdout.String(), stderr: stderr.String(), path: cfg.evidencePath, clock: clock}
	if raw, err := os.ReadFile(cfg.evidencePath); err == nil {
		require.NoError(c.t, json.Unmarshal(raw, &o.evidence), "the evidence file is not JSON:\n%s", raw)
	}
	return o
}

// The evidence JSON, as a consumer reads it.
type evidenceDoc struct {
	Version    string     `json:"version"`
	Verified   bool       `json:"verified"`
	Attempts   int        `json:"attempts"`
	StartedAt  string     `json:"startedAt"`
	FinishedAt string     `json:"finishedAt"`
	Checks     []checkDoc `json:"checks"`
}

type checkDoc struct {
	Check    string       `json:"check"`
	URL      string       `json:"url"`
	Passed   bool         `json:"passed"`
	Skipped  bool         `json:"skipped"`
	Reason   string       `json:"reason"`
	Findings []findingDoc `json:"findings"`
	Samples  []sampleDoc  `json:"samples"`
}

type findingDoc struct {
	Code     string `json:"code"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	Detail   string `json:"detail"`
}

type sampleDoc struct {
	NodeID  string `json:"nodeId"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Status  string `json:"status"`
}

func (o outcome) check(t *testing.T, name string) checkDoc {
	t.Helper()
	for _, ch := range o.evidence.Checks {
		if ch.Check == name {
			return ch
		}
	}
	require.FailNow(t, "no such check in the evidence", "%q in %+v", name, o.evidence.Checks)
	return checkDoc{}
}

// failing is the names of the checks that have findings, in order.
func (o outcome) failing() []string {
	var names []string
	for _, ch := range o.evidence.Checks {
		if len(ch.Findings) > 0 {
			names = append(names, ch.Check)
		}
	}
	return names
}

func (o outcome) codes(t *testing.T, check string) []string {
	t.Helper()
	var codes []string
	for _, f := range o.check(t, check).Findings {
		codes = append(codes, f.Code)
	}
	return codes
}

// finding is the one finding of that code in that check.
func (o outcome) finding(t *testing.T, check, code string) findingDoc {
	t.Helper()
	var found []findingDoc
	for _, f := range o.check(t, check).Findings {
		if f.Code == code {
			found = append(found, f)
		}
	}
	require.Len(t, found, 1, "findings of %s in %q: %+v\nstdout:\n%s", code, check, o.check(t, check).Findings, o.stdout)
	return found[0]
}

func (o outcome) lines() []string {
	trimmed := strings.TrimRight(o.stdout, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func (o outcome) lastLine() string {
	lines := o.lines()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// ---------------------------------------------------------------------------
// The brief's fourteen
// ---------------------------------------------------------------------------

// 1. Everything passes: exit 0, verified, one attempt -- and what it took to
// pass is on the evidence: three samples a host naming the node that answered,
// and the OS bundle judged on exactly the files that belong to it.
func TestAllChecksPassAndTheRolloutIsVerified(t *testing.T) {
	c := newCluster(t)
	o := c.verify()

	require.Equal(t, 0, o.code, "stdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
	assert.True(t, o.evidence.Verified)
	assert.Equal(t, 1, o.evidence.Attempts)
	assert.Equal(t, testRelease, o.evidence.Version)
	started, err := time.Parse(time.RFC3339Nano, o.evidence.StartedAt)
	require.NoError(t, err)
	finished, err := time.Parse(time.RFC3339Nano, o.evidence.FinishedAt)
	require.NoError(t, err)
	assert.False(t, finished.Before(started))

	var names []string
	for _, ch := range o.evidence.Checks {
		names = append(names, ch.Check)
		assert.True(t, ch.Passed, "%s: %+v", ch.Check, ch.Findings)
		assert.False(t, ch.Skipped, ch.Check)
		assert.Empty(t, ch.Findings, ch.Check)
	}
	assert.Equal(t, []string{"api health", "identity health", "os health", "os bundle", "docs version"}, names)

	for role, name := range map[string]string{"api": "api health", "identity": "identity health", "os": "os health"} {
		ch := o.check(t, name)
		require.Len(t, ch.Samples, 3, name)
		assert.Equal(t, sampleDoc{NodeID: role + "-node-1", Version: testRelease, Commit: testCommit, Status: "ok"}, ch.Samples[0], name)
		assert.Equal(t, 3, c.hitCount(role, "/healthz"), "%s is sampled three times", role)
	}

	// The bundle: the three same-origin references the page names, each once...
	for _, path := range []string{"/assets/index-a1.js", "/assets/chunk-b2.js", "/assets/index-a1.css"} {
		assert.Equal(t, 1, c.hitCount("os", path), path)
	}
	assert.Equal(t, 1, c.hitCount("os", "/memql-bundle.json"))
	// ...and nothing else.
	assert.Zero(t, c.hitCount("os", "/_memql/site-refresh.js"), "the edge's own script is never in the manifest")
	assert.Zero(t, c.hitCount("os", "/favicon.svg"), "an icon is neither a script nor a stylesheet")
	assert.Zero(t, c.hitCount("foreign", "/analytics.js"), "a cross-origin reference is ignored")
	assert.Equal(t, 1, c.hitCount("docs", "/docs/"))

	assert.Equal(t, "verified: 5 of 5 checks passing after 1 attempts over 0s", o.lastLine())
}

// 2. A host running another release is a version mismatch that names what was
// expected and what was observed. Each of the three hosts is probed on its own.
func TestAHostRunningAnotherReleaseIsAVersionMismatch(t *testing.T) {
	for _, tc := range []struct{ role, check string }{
		{"api", "api health"},
		{"identity", "identity health"},
		{"os", "os health"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			c := newCluster(t)
			c.setHealth(tc.role, always(healthy("bff-1", "v0.24.0", "9f8e7d6")))
			o := c.verify()

			require.Equal(t, 1, o.code)
			assert.False(t, o.evidence.Verified)
			assert.Equal(t, []string{tc.check}, o.failing())

			f := o.finding(t, tc.check, "rollout_version_mismatch")
			assert.Equal(t, "v0.24.1", f.Expected)
			assert.Equal(t, "v0.24.0", f.Observed)
			assert.Contains(t, f.Detail, "bff-1")
			assert.Contains(t, o.stdout, "rollout_version_mismatch "+tc.check+": ")
			assert.Equal(t, "rollout_unverified: 1 of 5 checks failing after 1 attempts over 0s", o.lastLine())
		})
	}
}

// 3. One request reaches one replica, so three samples can see a rollout that
// is half done. The finding names the replica that is behind and a replica
// that is not, with both versions.
func TestAMixedRolloutNamesBothNodes(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", func(n int) reply {
		if n == 1 { // the second sample reaches a replica still on the old release
			return healthy("bff-old", "v0.24.0", "9f8e7d6")
		}
		return healthy("bff-new", testRelease, testCommit)
	})
	o := c.verify()

	require.Equal(t, 1, o.code)
	assert.Equal(t, []string{"api health"}, o.failing())
	f := o.finding(t, "api health", "rollout_version_mismatch")
	assert.Equal(t, "v0.24.0", f.Observed)
	for _, want := range []string{"bff-old", "bff-new", "v0.24.0", "v0.24.1"} {
		assert.Contains(t, f.Detail, want)
	}

	samples := o.check(t, "api health").Samples
	require.Len(t, samples, 3)
	assert.Equal(t, []string{"bff-new", "bff-old", "bff-new"}, []string{samples[0].NodeID, samples[1].NodeID, samples[2].NodeID})
}

// 4. A node that is draining is not serving. The finding is the health one:
// the version of a node that is leaving is not what the verdict rests on.
func TestADrainingNodeFailsHealth(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", always(reply{status: http.StatusServiceUnavailable, body: `{"status":"draining","nodeId":"bff-1","version":"v0.24.1","commit":"1a2b3c4"}`}))
	o := c.verify()

	require.Equal(t, 1, o.code)
	assert.Equal(t, []string{"rollout_health_failed"}, o.codes(t, "api health"))
	f := o.finding(t, "api health", "rollout_health_failed")
	assert.Contains(t, f.Detail, "503")
	assert.Contains(t, f.Detail, "draining")
	assert.Contains(t, f.Detail, "bff-1")

	// The 503 body still says who answered, and the evidence keeps it.
	samples := o.check(t, "api health").Samples
	require.NotEmpty(t, samples)
	assert.Equal(t, sampleDoc{NodeID: "bff-1", Version: testRelease, Commit: testCommit, Status: "draining"}, samples[0])
}

// Not 200-and-ok in other ways is the same finding.
func TestHealthThatIsNotOKFails(t *testing.T) {
	for name, rep := range map[string]reply{
		"degraded":         {body: `{"status":"degraded","nodeId":"bff-1","version":"v0.24.1"}`},
		"a bad gateway":    {status: http.StatusBadGateway, body: `<html>502 Bad Gateway</html>`},
		"not JSON at all":  {body: `ok`},
		"a JSON array":     {body: `["ok"]`},
		"an error status":  {status: http.StatusInternalServerError, body: `{"status":"ok","version":"v0.24.1"}`},
		"no status field":  {body: `{"nodeId":"bff-1","version":"v0.24.1"}`},
		"a wrong-typed id": {body: `{"status":"ok","nodeId":7,"version":"v0.24.1"}`},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.setHealth("identity", always(rep))
			o := c.verify()
			require.Equal(t, 1, o.code)
			assert.Equal(t, []string{"rollout_health_failed"}, o.codes(t, "identity health"))
		})
	}
}

// 5. A /healthz with no version is an engine from before releases were
// reported: it cannot be told from any other release, so it cannot be verified.
func TestHealthWithoutAVersionIsUnreported(t *testing.T) {
	t.Run("a release", func(t *testing.T) {
		c := newCluster(t)
		c.setHealth("api", always(healthy("bff-1", "", testCommit)))
		o := c.verify()
		require.Equal(t, 1, o.code)
		f := o.finding(t, "api health", "rollout_version_unreported")
		assert.Contains(t, f.Detail, "bff-1")
		assert.Contains(t, f.Detail, "version")
	})
	t.Run("a commit", func(t *testing.T) {
		c := newCluster(t)
		c.setHealth("api", always(healthy("bff-1", testRelease, "")))
		o := c.verify(withVersion("1a2b3c4d5e"))
		require.Equal(t, 1, o.code)
		f := o.finding(t, "api health", "rollout_version_unreported")
		assert.Contains(t, f.Detail, "commit")
	})
}

// 6. A redirect is refused with the place it points to, and not followed: a
// rollout is judged where it is served.
func TestARedirectOnTheOSPageIsRefused(t *testing.T) {
	c := newCluster(t)
	to := c.foreign.URL + "/login"
	c.redirect("os", "/", to)
	o := c.verify()

	require.Equal(t, 1, o.code)
	assert.Equal(t, []string{"os bundle"}, o.failing())
	f := o.finding(t, "os bundle", "rollout_redirect_refused")
	assert.Equal(t, to, f.Observed)
	assert.Contains(t, f.Detail, to)
	assert.Contains(t, f.Detail, "302")
	assert.Zero(t, c.hitCount("foreign", "/login"), "the redirect is not followed")
}

// A redirect anywhere is refused: a health endpoint, the manifest, an asset,
// the docs.
func TestRedirectsAreRefusedEverywhere(t *testing.T) {
	for _, tc := range []struct{ role, path, check string }{
		{"api", "/healthz", "api health"},
		{"os", "/memql-bundle.json", "os bundle"},
		{"os", "/assets/index-a1.js", "os bundle"},
		{"docs", "/docs/", "docs version"},
	} {
		t.Run(tc.role+tc.path, func(t *testing.T) {
			c := newCluster(t)
			c.redirect(tc.role, tc.path, c.foreign.URL+"/elsewhere")
			o := c.verify()
			require.Equal(t, 1, o.code)
			assert.Equal(t, []string{tc.check}, o.failing())
			assert.Equal(t, []string{"rollout_redirect_refused"}, o.codes(t, tc.check))
			assert.Zero(t, c.hitCount("foreign", "/elsewhere"))
		})
	}
}

// 7. After a rollout a hashed asset that is gone is not a 404: the edge falls
// back to index.html for any path it does not have. A page that references a
// script served as HTML is a blank screen, and that is what this catches.
func TestAHTMLFallbackServedAsAScriptIsInvalid(t *testing.T) {
	const asset = "/assets/index-a1.js"
	for name, f := range map[string]*served{
		"html content type and body":  {contentType: "text/html; charset=utf-8", body: []byte("<!doctype html><html><body>app</body></html>")},
		"a script type, an html body": {contentType: "text/javascript", body: []byte("  \r\n<!DOCTYPE HTML><html>")},
		"an html tag first":           {contentType: "text/javascript", body: []byte("<HTML><body>")},
		"a byte order mark first":     {contentType: "text/javascript", body: []byte("\ufeff<!doctype html>")},
		"an html type, a script body": {contentType: "text/html", body: []byte(`console.log(1)`)},
		"an empty body":               {contentType: "text/javascript", body: nil},
		"a 404":                       {status: http.StatusNotFound, contentType: "text/plain", body: []byte("not found")},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.setFile(asset, f)
			o := c.verify()

			require.Equal(t, 1, o.code)
			assert.Equal(t, []string{"os bundle"}, o.failing())
			assert.Equal(t, []string{"rollout_asset_invalid"}, o.codes(t, "os bundle"), "an invalid asset is not also a hash mismatch")
			assert.Contains(t, o.finding(t, "os bundle", "rollout_asset_invalid").Detail, "assets/index-a1.js")
		})
	}
}

// 8. The bytes the front door serves are the bytes the image was built with:
// a script that hashes to something else is a mismatch naming both hashes.
func TestAssetBytesThatDifferFromTheManifestAreAMismatch(t *testing.T) {
	c := newCluster(t)
	tampered := []byte(`console.log("another build");`)
	c.setFile("/assets/index-a1.js", &served{contentType: "text/javascript; charset=utf-8", body: tampered})
	o := c.verify()

	require.Equal(t, 1, o.code)
	assert.Equal(t, []string{"os bundle"}, o.failing())
	f := o.finding(t, "os bundle", "rollout_bundle_mismatch")
	assert.Equal(t, hashOf([]byte(`console.log("memql os");`)), f.Expected)
	assert.Equal(t, hashOf(tampered), f.Observed)
	assert.Contains(t, f.Detail, f.Expected)
	assert.Contains(t, f.Detail, f.Observed)
	assert.Contains(t, f.Detail, "assets/index-a1.js")
}

// An asset the page references and the manifest does not list is a mismatch
// too: the edge serves a file its image does not account for.
func TestAnAssetTheManifestDoesNotListIsAMismatch(t *testing.T) {
	c := newCluster(t)
	entries := maps.Clone(c.entries)
	delete(entries, "assets/chunk-b2.js")
	c.setFile("/memql-bundle.json", manifestServed(entries))
	o := c.verify()

	require.Equal(t, 1, o.code)
	f := o.finding(t, "os bundle", "rollout_bundle_mismatch")
	assert.Contains(t, f.Detail, "assets/chunk-b2.js")
	assert.Contains(t, f.Detail, "no entry")
	assert.Equal(t, hashOf([]byte(`export const chunk = 1;`)), f.Observed)
}

// 9. No manifest: an edge image from before the manifest existed. The edge
// answers an unknown path with 404, or with its index.html when it falls back,
// and both are "the bundle reports nothing".
func TestAMissingManifestIsUnreported(t *testing.T) {
	c := newCluster(t)
	index := c.files["/"]
	for name, f := range map[string]*served{
		"a 404":                    {status: http.StatusNotFound, contentType: "text/plain", body: []byte("not found")},
		"the single-page fallback": index,
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.setFile("/memql-bundle.json", f)
			o := c.verify()
			require.Equal(t, 1, o.code)
			assert.Equal(t, []string{"os bundle"}, o.failing())
			assert.Equal(t, []string{"rollout_bundle_unreported"}, o.codes(t, "os bundle"))
		})
	}
}

// A manifest that is there and that the command cannot read is a bad OS
// answer, not an absent one.
func TestAManifestThatCannotBeReadIsInvalid(t *testing.T) {
	good := hashOf([]byte("x"))
	for name, f := range map[string]*served{
		"a server error":       {status: http.StatusInternalServerError, contentType: "text/plain", body: []byte("boom")},
		"not JSON":             {contentType: "text/plain", body: []byte("sha256 of everything")},
		"another schema":       {contentType: "application/json", body: []byte(`{"schema":2,"algorithm":"sha256","files":{"a.js":"` + good + `"}}`)},
		"another algorithm":    {contentType: "application/json", body: []byte(`{"schema":1,"algorithm":"md5","files":{"a.js":"` + good + `"}}`)},
		"no files":             {contentType: "application/json", body: []byte(`{"schema":1,"algorithm":"sha256","files":{}}`)},
		"a list for the files": {contentType: "application/json", body: []byte(`{"schema":1,"algorithm":"sha256","files":["a.js"]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.setFile("/memql-bundle.json", f)
			o := c.verify()
			require.Equal(t, 1, o.code)
			assert.Equal(t, []string{"rollout_os_invalid"}, o.codes(t, "os bundle"))
		})
	}
}

// 10. The docs site carries the bare release in a meta tag; a docs site that
// has not moved on is a mismatch. A commit is not a release the docs are
// versioned by, so the check is skipped, and recorded as skipped: a verdict
// that does not say what it did not look at is a claim about the checker.
func TestDocsOnAnotherVersionAreAMismatch(t *testing.T) {
	c := newCluster(t)
	c.edit(func(c *cluster) {
		c.docsPage = &served{contentType: "text/html", body: []byte(docsPageFor("0.24.0"))}
	})
	o := c.verify()

	require.Equal(t, 1, o.code)
	assert.Equal(t, []string{"docs version"}, o.failing())
	f := o.finding(t, "docs version", "rollout_docs_version_mismatch")
	assert.Equal(t, "0.24.1", f.Expected)
	assert.Equal(t, "0.24.0", f.Observed)
}

func TestACommitVersionSkipsTheDocsCheckAndSaysSo(t *testing.T) {
	c := newCluster(t)
	c.edit(func(c *cluster) {
		c.docsPage = &served{contentType: "text/html", body: []byte(docsPageFor("0.0.0"))}
	})
	o := c.verify(withVersion("1a2b3c4d5e"))

	require.Equal(t, 0, o.code, "stdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
	assert.True(t, o.evidence.Verified)
	docs := o.check(t, "docs version")
	assert.True(t, docs.Skipped)
	assert.False(t, docs.Passed, "skipped is not passed")
	assert.Contains(t, docs.Reason, "commit")
	assert.Zero(t, c.hitCount("docs", "/docs/"), "a skipped check makes no request")
	assert.Equal(t, "verified: 4 of 4 checks passing after 1 attempts over 0s", o.lastLine(), "a skipped check is not counted")
	assert.Contains(t, o.stderr, "docs version skipped")
}

func TestNoDocsFlagSkipsTheDocsCheckAndSaysSo(t *testing.T) {
	c := newCluster(t)
	o := c.verify(func(cfg *config, _ *fakeClock) { cfg.docsURL = "" })

	require.Equal(t, 0, o.code, "stdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
	docs := o.check(t, "docs version")
	assert.True(t, docs.Skipped)
	assert.False(t, docs.Passed)
	assert.Contains(t, docs.Reason, "--docs")
}

func TestDocsThatCannotBeReadAreAMismatch(t *testing.T) {
	for name, tc := range map[string]struct {
		page   *served
		detail string
	}{
		"a 404":                {&served{status: http.StatusNotFound, contentType: "text/html", body: []byte("gone")}, "HTTP 404"},
		"no meta tag":          {&served{contentType: "text/html", body: []byte(`<html><head><title>Docs</title></head></html>`)}, "memql-docs-version"},
		"a leading v is wrong": {&served{contentType: "text/html", body: []byte(docsPageFor("v0.24.1"))}, "v0.24.1"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.edit(func(c *cluster) { c.docsPage = tc.page })
			o := c.verify()
			require.Equal(t, 1, o.code)
			f := o.finding(t, "docs version", "rollout_docs_version_mismatch")
			assert.Contains(t, f.Detail, tc.detail)
		})
	}
}

// 11. A commit matches /healthz's short commit as a prefix, from either side.
func TestACommitVersionMatchesTheHealthCommitByPrefix(t *testing.T) {
	c := newCluster(t)
	o := c.verify(withVersion("1a2b3c4d5e"))

	require.Equal(t, 0, o.code, "stdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
	assert.True(t, o.evidence.Verified)
	assert.Equal(t, "1a2b3c4d5e", o.evidence.Version)
	assert.Equal(t, testCommit, o.check(t, "api health").Samples[0].Commit)
}

func TestACommitThatIsNotRunningIsAMismatch(t *testing.T) {
	c := newCluster(t)
	o := c.verify(withVersion("ffffffffff"))

	require.Equal(t, 1, o.code)
	f := o.finding(t, "api health", "rollout_version_mismatch")
	assert.Equal(t, "ffffffffff", f.Expected)
	assert.Equal(t, testCommit, f.Observed)
}

// 12. The rollout lands between attempts: the first attempt fails, the second
// passes, and the evidence is the passing round.
func TestARolloutThatLandsBetweenAttemptsIsVerified(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", always(healthy("bff-1", "v0.24.0", "9f8e7d6")))
	o := c.verify(retrying(10*time.Minute, 30*time.Second), func(_ *config, clock *fakeClock) {
		clock.onSleep = func(int) { c.setHealth("api", always(healthy("bff-1", testRelease, testCommit))) }
	})

	require.Equal(t, 0, o.code, "stdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
	assert.True(t, o.evidence.Verified)
	assert.Equal(t, 2, o.evidence.Attempts)
	assert.Equal(t, []time.Duration{30 * time.Second}, o.clock.sleeps)
	assert.Empty(t, o.failing(), "the evidence is the round that passed")
	assert.Contains(t, o.stderr, "attempt 1", "a retry says why it is retrying")
	assert.Contains(t, o.stderr, "rollout_version_mismatch")
	assert.Equal(t, "verified: 5 of 5 checks passing after 2 attempts over 30s", o.lastLine())
}

// 13. A rollout that never lands: retried at the interval until the wait is
// over, the last attempt at the deadline itself, then the findings and the
// summary as the last lines of output.
func TestARolloutThatNeverLandsIsUnverifiedWhenTheWaitIsOver(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", always(healthy("bff-1", "v0.24.0", "9f8e7d6")))
	o := c.verify(retrying(2*time.Minute, 30*time.Second))

	require.Equal(t, 1, o.code)
	assert.False(t, o.evidence.Verified)
	assert.Equal(t, 5, o.evidence.Attempts, "attempts at 0, 30, 60, 90 and 120 seconds")
	assert.Equal(t, []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}, o.clock.sleeps)
	assert.Equal(t, []string{"api health"}, o.failing())

	lines := o.lines()
	require.GreaterOrEqual(t, len(lines), 2)
	assert.True(t, strings.HasPrefix(lines[len(lines)-2], "rollout_version_mismatch api health: "), lines[len(lines)-2])
	assert.Equal(t, "rollout_unverified: 1 of 5 checks failing after 5 attempts over 2m0s", lines[len(lines)-1])
}

// The last attempt lands on the deadline, not past it, when the wait is not a
// whole number of intervals.
func TestTheLastAttemptIsAtTheDeadline(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", always(healthy("bff-1", "v0.24.0", "9f8e7d6")))
	o := c.verify(retrying(100*time.Second, 30*time.Second))

	require.Equal(t, 1, o.code)
	assert.Equal(t, []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second, 10 * time.Second}, o.clock.sleeps[:4])
	assert.Equal(t, 5, o.evidence.Attempts)
	assert.Equal(t, "rollout_unverified: 1 of 5 checks failing after 5 attempts over 1m40s", o.lastLine())
}

// 14. Bad flags exit 2 and print nothing to stdout, which carries a verdict
// or nothing. Every row is invalid on its own: none of them may reach the
// network.
func TestBadFlagsExitTwo(t *testing.T) {
	const roles = "--api-url=http://127.0.0.1:1 --identity-url=http://127.0.0.1:1 --os-url=http://127.0.0.1:1"
	for name, args := range map[string]string{
		"no flags":                       "",
		"no version":                     "--domain=example.com",
		"an empty version":               "--domain=example.com --version=",
		"a version that is neither":      "--domain=example.com --version=banana",
		"a commit too short":             "--domain=example.com --version=1a2b3c",
		"no domain and no urls":          "--version=v0.24.1",
		"two urls and no domain":         "--version=v0.24.1 --api-url=http://127.0.0.1:1 --identity-url=http://127.0.0.1:1",
		"no samples":                     "--domain=example.com --version=v0.24.1 --samples=0",
		"a negative wait":                "--domain=example.com --version=v0.24.1 --wait=-1s",
		"a zero interval":                "--domain=example.com --version=v0.24.1 --interval=0s",
		"a zero request timeout":         "--domain=example.com --version=v0.24.1 --request-timeout=0s",
		"an empty evidence path":         "--domain=example.com --version=v0.24.1 --evidence=",
		"a domain that is a url":         "--domain=https://example.com --version=v0.24.1",
		"a url with no scheme":           "--version=v0.24.1 --api-url=127.0.0.1:1 --identity-url=http://127.0.0.1:1 --os-url=http://127.0.0.1:1",
		"a docs value that is not a url": "--domain=example.com --version=v0.24.1 --docs=not-a-url",
		"a stray argument":               "--domain=example.com --version=v0.24.1 extra",
		"an unknown flag":                "--nonsense",
		"a malformed duration":           "--domain=example.com --version=v0.24.1 --wait=soon",
		"a help request":                 "-h",
		"a version with the urls":        "--version= " + roles,
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), strings.Fields(args), &stdout, &stderr)
			assert.Equal(t, 2, code, "stderr:\n%s", stderr.String())
			assert.Empty(t, stdout.String())
			assert.NotEmpty(t, stderr.String(), "a refusal says why")
		})
	}
}

// ---------------------------------------------------------------------------
// What the brief specifies and does not number
// ---------------------------------------------------------------------------

// The domain may be omitted only when all three origins are given, and the
// flags reach the probes: this runs the whole command from its arguments.
func TestFlagsReachTheProbes(t *testing.T) {
	c := newCluster(t)
	path := filepath.Join(t.TempDir(), "flagged.json")
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{
		"--version=" + testRelease,
		"--api-url=" + c.api.URL,
		"--identity-url=" + c.identity.URL,
		"--os-url=" + c.osHost.URL,
		"--docs=" + c.docs.URL + "/docs/",
		"--samples=2",
		"--wait=0s",
		"--evidence=" + path,
	}, &stdout, &stderr)

	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	assert.Equal(t, 2, c.hitCount("api", "/healthz"), "--samples reaches the probes")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "--evidence is where the evidence goes")
	assert.Contains(t, string(raw), `"verified": true`)
}

func TestFlagDefaultsAndTheOriginsDerivedFromTheDomain(t *testing.T) {
	cfg, err := parseFlags([]string{"--domain=lab.example.com", "--version=v0.24.1"}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, cfg.wait)
	assert.Equal(t, 30*time.Second, cfg.interval)
	assert.Equal(t, 3, cfg.samples)
	assert.Equal(t, "verify-rollout.json", cfg.evidencePath)
	assert.Equal(t, 20*time.Second, cfg.requestTimeout)
	assert.Empty(t, cfg.docsURL)

	resolved, want, err := cfg.resolve()
	require.NoError(t, err)
	assert.Equal(t, "https://api.lab.example.com", resolved.apiURL)
	assert.Equal(t, "https://identity.lab.example.com", resolved.identityURL)
	assert.Equal(t, "https://os.lab.example.com", resolved.osURL)
	assert.True(t, want.release)

	// An override replaces its own role only, and a trailing slash goes.
	cfg, err = parseFlags([]string{"--domain=lab.example.com", "--version=v0.24.1", "--os-url=http://127.0.0.1:9/"}, io.Discard)
	require.NoError(t, err)
	resolved, _, err = cfg.resolve()
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9", resolved.osURL)
	assert.Equal(t, "https://api.lab.example.com", resolved.apiURL)
}

// The hosts the command probes by default are the front door's own. They are
// composed by component/frontdoor -- the one derivation the Ingress rules, the
// certificate and every node's issuer and CORS origins are written from -- and
// not spelled a second time here, because a second copy is the one that
// disagrees: the command would verify a cluster by asking a host nothing is
// served at, and say so with a reachable-looking error.
//
// The expected values below are the package's answers, not strings written out
// in this test, so a change to the rule that this command did not follow is a
// red test. (TestFlagDefaultsAndTheOriginsDerivedFromTheDomain pins the shapes
// the rule produces today.) The OS is a platform SITE and not a role, and the
// package composes the two differently.
func TestDefaultOriginsAreTheFrontDoorsOwnHosts(t *testing.T) {
	for _, domain := range []string{"lab.example.com", "example.org", "memql.localhost"} {
		cfg := config{
			version:        testRelease,
			domain:         domain,
			samples:        1,
			interval:       time.Second,
			requestTimeout: time.Second,
			evidencePath:   "verify-rollout.json",
		}
		resolved, _, err := cfg.resolve()
		require.NoError(t, err, domain)
		assert.Equal(t, "https://"+frontdoor.RoleHost(frontdoor.RoleAPI, domain), resolved.apiURL, domain)
		assert.Equal(t, "https://"+frontdoor.RoleHost(frontdoor.RoleIdentity, domain), resolved.identityURL, domain)
		assert.Equal(t, "https://"+frontdoor.OsHost(domain), resolved.osURL, domain)
	}
}

// A release compares with the leading v stripped on both sides.
func TestReleasesCompareWithoutTheLeadingV(t *testing.T) {
	for name, tc := range map[string]struct{ flag, reported string }{
		"the node leaves the v off": {"v0.24.1", "0.24.1"},
		"the flag leaves the v off": {"0.24.1", "v0.24.1"},
		"neither has it":            {"0.24.1", "0.24.1"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.setHealth("api", always(healthy("bff-1", tc.reported, testCommit)))
			o := c.verify(withVersion(tc.flag))
			assert.Equal(t, 0, o.code, "stdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
		})
	}
}

func TestVersionFlagForms(t *testing.T) {
	for _, tc := range []struct {
		in      string
		release bool
		bare    string
		commit  string
	}{
		{"v0.24.1", true, "0.24.1", ""},
		{"0.24.1", true, "0.24.1", ""},
		{" v0.24.1 ", true, "0.24.1", ""},
		{"v0.24.1-rc.1", true, "0.24.1-rc.1", ""},
		{"v0.24.1+build.5", true, "0.24.1+build.5", ""},
		{"1a2b3c4", false, "", "1a2b3c4"},
		{"1A2B3C4D5E6F", false, "", "1a2b3c4d5e6f"},
	} {
		want, err := parseExpectation(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.release, want.release, tc.in)
		assert.Equal(t, tc.bare, want.bare, tc.in)
		assert.Equal(t, tc.commit, want.commit, tc.in)
		assert.Equal(t, strings.TrimSpace(tc.in), want.raw, tc.in)
	}
	for _, bad := range []string{"", "   ", "banana", "1a2b3c", "v1.2", "1.2.3.4", "g1a2b3c4", "v1.2.x"} {
		_, err := parseExpectation(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestCommitMatching(t *testing.T) {
	for _, tc := range []struct {
		want, got string
		matches   bool
	}{
		{"1a2b3c4d5e", "1a2b3c4", true},   // the flag is longer than the node's short commit
		{"1a2b3c4", "1a2b3c4d5e6f", true}, // the node's is longer than the flag
		{"1a2b3c4d5e6f", "1a2b3c4d5e6f", true},
		{"1a2b3c4d5e", "1A2B3C4", true}, // case does not matter
		{"1a2b3c4d5e", "1a2b3c5", false},
		{"1a2b3c4d5e", "1a2b3c", false}, // under seven characters never matches, however it begins
		{"1a2b3c4d5e", "", false},
		{"1a2b3c4", "1a2b3c4-dirty", false}, // a build from a modified tree is not that commit
		{"1a2b3c4", "dev+1a2b3c4", false},
	} {
		assert.Equal(t, tc.matches, commitMatches(tc.want, tc.got), "%q vs %q", tc.want, tc.got)
	}
}

// ---------------------------------------------------------------------------
// Limits and failures of the transport
// ---------------------------------------------------------------------------

// Every body is bounded. Past its limit it is refused, naming the limit, and
// a body of exactly the limit is read whole.
func TestBodiesLargerThanTheirLimitAreRefused(t *testing.T) {
	over := func(limit int) []byte { return bytes.Repeat([]byte("a"), limit+1) }

	t.Run("healthz", func(t *testing.T) {
		c := newCluster(t)
		c.setHealth("api", always(reply{body: string(bytes.Repeat([]byte(" "), maxBodyBytes+1))}))
		o := c.verify()
		require.Equal(t, 1, o.code)
		assert.Contains(t, o.finding(t, "api health", "rollout_health_failed").Detail, "larger than 2097152 bytes")
	})
	t.Run("the OS page", func(t *testing.T) {
		c := newCluster(t)
		c.setFile("/", &served{contentType: "text/html", body: over(maxBodyBytes)})
		o := c.verify()
		require.Equal(t, 1, o.code)
		assert.Contains(t, o.finding(t, "os bundle", "rollout_os_invalid").Detail, "larger than 2097152 bytes")
	})
	t.Run("the manifest", func(t *testing.T) {
		c := newCluster(t)
		c.setFile("/memql-bundle.json", &served{contentType: "application/json", body: over(maxBodyBytes)})
		o := c.verify()
		require.Equal(t, 1, o.code)
		assert.Contains(t, o.finding(t, "os bundle", "rollout_os_invalid").Detail, "larger than 2097152 bytes")
	})
	t.Run("an asset", func(t *testing.T) {
		c := newCluster(t)
		c.setFile("/assets/index-a1.js", &served{contentType: "text/javascript", body: over(maxAssetBytes)})
		o := c.verify()
		require.Equal(t, 1, o.code)
		assert.Contains(t, o.finding(t, "os bundle", "rollout_asset_invalid").Detail, "larger than 16777216 bytes")
	})
	t.Run("the docs page", func(t *testing.T) {
		c := newCluster(t)
		c.edit(func(c *cluster) { c.docsPage = &served{contentType: "text/html", body: over(maxBodyBytes)} })
		o := c.verify()
		require.Equal(t, 1, o.code)
		assert.Contains(t, o.finding(t, "docs version", "rollout_docs_version_mismatch").Detail, "larger than 2097152 bytes")
	})
}

func TestABodyAtTheLimitIsReadWhole(t *testing.T) {
	t.Run("healthz", func(t *testing.T) {
		c := newCluster(t)
		rep := healthy("bff-1", testRelease, testCommit)
		rep.body += strings.Repeat(" ", maxBodyBytes-len(rep.body))
		require.Len(t, rep.body, maxBodyBytes)
		c.setHealth("api", always(rep))
		o := c.verify()
		assert.Equal(t, 0, o.code, "stdout:\n%s", o.stdout)
	})
	t.Run("an asset", func(t *testing.T) {
		c := newCluster(t)
		body := bytes.Repeat([]byte("a"), maxAssetBytes)
		entries := maps.Clone(c.entries)
		entries["assets/index-a1.js"] = hashOf(body)
		c.setFile("/memql-bundle.json", manifestServed(entries))
		c.setFile("/assets/index-a1.js", &served{contentType: "text/javascript", body: body})
		o := c.verify()
		assert.Equal(t, 0, o.code, "stdout:\n%s", o.stdout)
	})
}

// A host that is not there is unreachable, named by the class of the failure,
// and sampling stops: more samples would only repeat the wait.
func TestAHostThatDoesNotAnswerIsUnreachable(t *testing.T) {
	c := newCluster(t)
	c.api.Close()
	o := c.verify()

	require.Equal(t, 1, o.code)
	f := o.finding(t, "api health", "rollout_unreachable")
	assert.Contains(t, f.Detail, "refused")
	assert.Empty(t, o.check(t, "api health").Samples)
	assert.Equal(t, []string{"api health"}, o.failing())
}

// A host that takes the connection and never answers does not hang the
// command: a request has its own bound. And it is asked ONCE per attempt: the
// three samples exist to meet replicas, and a host that is not answering has
// none to meet, so three timeouts back to back would be the same wait three
// times over. The findings cannot show this (identical ones collapse), so the
// requests are counted.
func TestAHostThatNeverAnswersTimesOutAndIsNotAskedAgain(t *testing.T) {
	c := newCluster(t)
	var asked atomic.Int32
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(hung.Close)
	o := c.verify(func(cfg *config, _ *fakeClock) {
		cfg.apiURL = hung.URL
		// Long enough that a loaded machine still delivers the request to the
		// handler before the client gives up on it, which the count below needs.
		cfg.requestTimeout = 300 * time.Millisecond
	})

	assert.Equal(t, 1, o.code)
	assert.Contains(t, o.finding(t, "api health", "rollout_unreachable").Detail, "timeout")
	assert.EqualValues(t, 1, asked.Load(), "sampling stops at the first request that gets no answer")
}

func TestTransportErrorsAreClassified(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		class string
	}{
		"no such host":                     {&url.Error{Op: "Get", URL: "https://x", Err: &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}}, "dns"},
		"an unknown certificate authority": {&url.Error{Op: "Get", URL: "https://x", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, "tls"},
		"a record that is not TLS":         {&url.Error{Op: "Get", URL: "https://x", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, "tls"},
		"a refused connection":             {&url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}, "refused"},
		"a reset connection":               {&url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}}, "reset"},
		"a deadline":                       {&url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, "timeout"},
		"a network timeout":                {&url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "read", Err: timeoutError{}}}, "timeout"},
		"a cancelled request":              {&url.Error{Op: "Get", URL: "http://x", Err: context.Canceled}, "canceled"},
		"anything else":                    {errors.New("boom"), "network"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.class, transportClass(tc.err))
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// Each sample opens its own connection, so a load balancer can send it to a
// different replica; one kept-alive connection would put all three on one.
func TestEverySampleOpensItsOwnConnection(t *testing.T) {
	c := newCluster(t)
	var connections atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, healthy("bff-1", testRelease, testCommit).body)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	o := c.verify(func(cfg *config, _ *fakeClock) { cfg.apiURL = srv.URL })
	require.Equal(t, 0, o.code, "stdout:\n%s", o.stdout)
	assert.EqualValues(t, 3, connections.Load())
}

// ---------------------------------------------------------------------------
// The OS page
// ---------------------------------------------------------------------------

func TestAnOSPageThatIsNotTheShellIsInvalid(t *testing.T) {
	const styles = `<link rel="stylesheet" href="/assets/index-a1.css">`
	page := func(head string) []byte {
		return []byte("<!doctype html><html><head>" + head + "</head><body></body></html>")
	}
	for name, tc := range map[string]struct {
		page   *served
		detail string
	}{
		"a server error":                  {&served{status: http.StatusInternalServerError, contentType: "text/html", body: page("<title>MemQL OS</title>")}, "HTTP 500"},
		"not html":                        {&served{contentType: "text/plain", body: page("<title>MemQL OS</title>")}, "text/plain"},
		"another site":                    {&served{contentType: "text/html", body: page("<title>Welcome</title>" + styles + `<script src="/assets/index-a1.js"></script>`)}, `"Welcome"`},
		"no title":                        {&served{contentType: "text/html", body: page(styles + `<script src="/assets/index-a1.js"></script>`)}, "title"},
		"no script":                       {&served{contentType: "text/html", body: page("<title>MemQL OS</title>" + styles)}, "script"},
		"only a script on another origin": {&served{contentType: "text/html", body: page(`<title>MemQL OS</title><script src="https://cdn.invalid/x.js"></script>`)}, "script"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.setFile("/", tc.page)
			o := c.verify()
			require.Equal(t, 1, o.code)
			assert.Equal(t, []string{"os bundle"}, o.failing())
			f := o.finding(t, "os bundle", "rollout_os_invalid")
			assert.Contains(t, f.Detail, tc.detail)
			assert.Zero(t, c.hitCount("os", "/assets/index-a1.js"), "a page that is not the shell has no bundle to judge")
		})
	}
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// A path that cannot be written is found before the wait, not after it.
func TestAnEvidencePathThatCannotBeWrittenFailsBeforeAnyProbe(t *testing.T) {
	c := newCluster(t)
	o := c.verify(func(cfg *config, _ *fakeClock) {
		cfg.evidencePath = filepath.Join(t.TempDir(), "no-such-directory", "evidence.json")
	})

	assert.Equal(t, 2, o.code)
	assert.Contains(t, o.stderr, "evidence")
	assert.Zero(t, c.requests(), "nothing is probed when the evidence cannot be kept")
	assert.Empty(t, o.stdout)
}

// The evidence is part of the verdict. If the file that was writable when the
// run began stops being, a rollout that verified is still not one the pipeline
// can show to anybody, and the run says that where it says everything else.
func TestEvidenceThatCannotBeKeptIsNotAVerifiedRollout(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", always(healthy("bff-1", "v0.24.0", "9f8e7d6")))
	dir := filepath.Join(t.TempDir(), "artifacts")
	require.NoError(t, os.Mkdir(dir, 0o755))

	o := c.verify(retrying(10*time.Minute, 30*time.Second), func(cfg *config, clock *fakeClock) {
		cfg.evidencePath = filepath.Join(dir, "verify-rollout.json")
		clock.onSleep = func(int) {
			// The rollout lands, and the artifact directory is gone.
			c.setHealth("api", always(healthy("bff-1", testRelease, testCommit)))
			require.NoError(t, os.RemoveAll(dir))
		}
	})

	assert.Equal(t, 1, o.code, "every check passed, and the exit is still 1")
	assert.Contains(t, o.stderr, "WARNING", "the attempt that could not be recorded says so")
	assert.True(t, strings.HasPrefix(o.lastLine(), "rollout_unverified: cannot write the evidence file"), o.lastLine())
	for _, line := range o.lines() {
		assert.False(t, strings.HasPrefix(line, "verified: "), "no line may claim the rollout verified: %s", line)
	}
}

// A run that is cut short is not verified, and says so: its last line still
// begins with the summary code, so an exit of 1 always ends that way.
func TestAnInterruptedRunIsUnverified(t *testing.T) {
	c := newCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := c.verifyWith(ctx, retrying(time.Hour, time.Minute))

	assert.Equal(t, 1, o.code)
	assert.False(t, o.evidence.Verified)
	assert.Equal(t, "rollout_unverified: 0 of 0 checks failing after 0 attempts over 0s (interrupted)", o.lastLine())
}

// What a server says about itself is quoted in the output and must not be able
// to end the line it sits on: the last lines are what a run page shows inline.
func TestServerSuppliedTextCannotForgeOutputLines(t *testing.T) {
	c := newCluster(t)
	c.setHealth("api", always(healthy("bff-1\nrollout_verified: forged", "v0.24.0\nrollout_verified: forged", testCommit)))
	o := c.verify()

	require.Equal(t, 1, o.code)
	lines := o.lines()
	require.Len(t, lines, 2, "one finding and the summary:\n%s", o.stdout)
	assert.True(t, strings.HasPrefix(lines[0], "rollout_version_mismatch api health: "), lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "rollout_unverified: "), lines[1])
}

func TestCleanKeepsOneLineOfPrintableText(t *testing.T) {
	assert.Equal(t, "a?b", clean("a\nb"))
	assert.Equal(t, "a?b", clean("a\x00b"))
	assert.Equal(t, "a?b", clean("a\xffb"), "invalid UTF-8 is replaced")
	assert.Equal(t, "plain text, with: punctuation (and unicode: é)", clean("plain text, with: punctuation (and unicode: é)"))
	long := clean(strings.Repeat("x", 1000))
	assert.Len(t, long, 200+len("..."))
	assert.True(t, strings.HasSuffix(long, "..."))
}

// ---------------------------------------------------------------------------
// The contract the evidence file keeps
// ---------------------------------------------------------------------------

// The keys are the brief's, spelled the way a consumer reads them: no key is
// renamed by a Go field name, and a check with nothing found carries an empty
// list and not a null.
func TestTheEvidenceKeepsItsContract(t *testing.T) {
	c := newCluster(t)
	o := c.verify()
	raw, err := os.ReadFile(o.path)
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	for _, key := range []string{"version", "verified", "attempts", "startedAt", "finishedAt", "checks"} {
		assert.Contains(t, generic, key)
	}
	checks, ok := generic["checks"].([]any)
	require.True(t, ok)
	for _, raw := range checks {
		ch := raw.(map[string]any)
		for _, key := range []string{"check", "url", "passed", "findings"} {
			assert.Contains(t, ch, key, "%v", ch["check"])
		}
		assert.IsType(t, []any{}, ch["findings"], "an empty list, not null: %v", ch["check"])
	}
	api := checks[0].(map[string]any)
	sample := api["samples"].([]any)[0].(map[string]any)
	for _, key := range []string{"nodeId", "version", "commit", "status"} {
		assert.Contains(t, sample, key)
	}

	// A finding's keys.
	c.setHealth("api", always(healthy("bff-1", "v0.24.0", testCommit)))
	o = c.verify()
	raw, err = os.ReadFile(o.path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), fmt.Sprintf("%q", "code"))
	var failing struct {
		Checks []struct {
			Findings []map[string]any `json:"findings"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(raw, &failing))
	finding := failing.Checks[0].Findings[0]
	for _, key := range []string{"code", "expected", "observed", "detail"} {
		assert.Contains(t, finding, key)
	}
}
