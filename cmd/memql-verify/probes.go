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
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/net/html"
)

// probes.go -- one function per probe, each answering with a checkResult.
//
// Every probe asks a public URL the way any client would, and decides from
// what comes back. None of them reads a pod, a database or a file, which is the
// point of the command: the in-cluster observer this replaces could say a
// cluster was healthy while its front door served something else.
//
// What a probe never does is echo a response body into a finding. A finding
// quotes the few fields it judged -- a status, a version, a hash, a header --
// each cleaned to one line of printable text, because the last lines of this
// command's output are shown inline on a run page and what a server says about
// itself must not be able to shape them.

// The size limits are the observer's. A response is read through
// io.LimitReader(limit+1), so a body over its limit is SEEN to be over it and
// not silently cut to it.
const (
	// maxBodyBytes bounds a page, a /healthz answer and the bundle manifest.
	maxBodyBytes = 2 << 20
	// maxAssetBytes bounds one same-origin script or stylesheet.
	maxAssetBytes = 16 << 20
)

// The codes a finding carries: printed, and in the evidence. They are the
// command's whole vocabulary, and a caller may branch on any of them.
const (
	// codeUnreachable: the request produced no response -- DNS, TLS, a refused
	// or reset connection, a timeout. Its detail names which.
	codeUnreachable = "rollout_unreachable"
	// codeRedirectRefused: a 3xx. A rollout is judged where it is served, and
	// a host that sends the client elsewhere has not shown it serves the release.
	codeRedirectRefused = "rollout_redirect_refused"
	// codeHealthFailed: /healthz did not answer 200 with status ok.
	codeHealthFailed = "rollout_health_failed"
	// codeVersionUnreported: a healthy node that does not say what it runs.
	codeVersionUnreported = "rollout_version_unreported"
	// codeVersionMismatch: a healthy node that runs something else.
	codeVersionMismatch = "rollout_version_mismatch"
	// codeOSInvalid: what the OS host serves is not the MemQL OS shell, or its
	// manifest cannot be read.
	codeOSInvalid = "rollout_os_invalid"
	// codeBundleUnreported: the OS host serves no bundle manifest.
	codeBundleUnreported = "rollout_bundle_unreported"
	// codeBundleMismatch: a served file is not the file the image was built with.
	codeBundleMismatch = "rollout_bundle_mismatch"
	// codeAssetInvalid: a script or stylesheet the page names is not one.
	codeAssetInvalid = "rollout_asset_invalid"
	// codeDocsMismatch: the docs site is not the release's.
	codeDocsMismatch = "rollout_docs_version_mismatch"
	// codeUnverified is the summary code, not a finding's.
	codeUnverified = "rollout_unverified"
)

// The checks, in the order they are probed and reported.
const (
	checkAPI      = "api health"
	checkIdentity = "identity health"
	checkOSHealth = "os health"
	checkOSBundle = "os bundle"
	checkDocs     = "docs version"
)

const (
	// osTitle is what the MemQL OS shell calls itself.
	osTitle = "MemQL OS"
	// manifestName is the edge image's bundle manifest, and manifestPath where
	// it is served.
	manifestName = "memql-bundle.json"
	manifestPath = "/" + manifestName
	// edgePrefix is the edge's own namespace: the scripts it injects into every
	// document it serves are not part of the bundle and are in no manifest.
	edgePrefix = "/_memql/"
	// docsMetaName is the meta tag the docs site carries its release in.
	docsMetaName = "memql-docs-version"
	// healthyAnswer is what a node that is serving says.
	healthyAnswer = "HTTP 200, status ok"
	// userAgent names the probe to the hosts it asks.
	userAgent = "memql-verify"
	// maxQuoted bounds what is quoted from a server in one finding.
	maxQuoted = 200
)

// ---------------------------------------------------------------------------
// What the rollout must be running
// ---------------------------------------------------------------------------

var (
	// releasePattern is a release tag: the leading v is optional, and a
	// pre-release or build suffix is part of the release.
	releasePattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+].*)?$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// minCommitLen is the fewest hex characters of a commit that name one.
const minCommitLen = 7

// expectation is what --version asks for: a release, compared with its leading
// v stripped on both sides, or a commit, compared as a prefix from either side.
type expectation struct {
	raw     string // as given, trimmed: what a finding quotes
	release bool
	bare    string // a release without its leading v
	commit  string // a commit, lower case
}

// parseExpectation reads --version. A value that is neither a release nor a
// commit is refused here, as a bad flag: a gate that retried for two hours on
// a typo it could have refused in a millisecond has failed its operator.
func parseExpectation(v string) (expectation, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return expectation{}, errors.New("--version is required: the release (v1.2.3) or the commit (at least 7 hex characters) the cluster must be serving")
	case releasePattern.MatchString(v):
		return expectation{raw: v, release: true, bare: strings.TrimPrefix(v, "v")}, nil
	case commitPattern.MatchString(v):
		return expectation{raw: v, commit: strings.ToLower(v)}, nil
	}
	return expectation{}, fmt.Errorf("--version %q is neither a release (v1.2.3) nor a commit of at least %d hex characters", v, minCommitLen)
}

// commitMatches reports whether a node built from got is a build of want. The
// node reports a SHORT commit and the flag may be longer or shorter, so either
// may be the prefix of the other -- but only at seven characters or more, or
// "1" would match everything. A commit that is not plain hex never matches,
// which is how a "-dirty" build is refused: it was built from a modified tree,
// and that is not the commit.
func commitMatches(want, got string) bool {
	got = strings.ToLower(got)
	if len(got) < minCommitLen || !isHex(got) {
		return false
	}
	return strings.HasPrefix(want, got) || strings.HasPrefix(got, want)
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return s != ""
}

// healthBody is the part of a /healthz answer the verdict rests on.
type healthBody struct {
	Status  string `json:"status"`
	NodeID  string `json:"nodeId"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// clean makes what a node said about itself safe to quote.
func (h *healthBody) clean() {
	h.Status, h.NodeID, h.Version, h.Commit = clean(h.Status), clean(h.NodeID), clean(h.Version), clean(h.Commit)
}

// judge says whether one node is running what the rollout must be: nil when it
// is, and the finding saying why when it is not.
func (e expectation) judge(h healthBody) *finding {
	node := nodeLabel(h.NodeID)
	if e.release {
		switch {
		case h.Version == "":
			return &finding{Code: codeVersionUnreported, Expected: e.raw,
				Detail: node + " answered /healthz without a version, so which release it runs cannot be told"}
		case strings.TrimPrefix(h.Version, "v") != e.bare:
			return &finding{Code: codeVersionMismatch, Expected: e.raw, Observed: h.Version,
				Detail: fmt.Sprintf("%s runs %s, not %s", node, h.Version, e.raw)}
		}
		return nil
	}
	switch {
	case h.Commit == "":
		return &finding{Code: codeVersionUnreported, Expected: e.raw,
			Detail: node + " answered /healthz without a commit, so which build it runs cannot be told"}
	case !commitMatches(e.commit, h.Commit):
		detail := fmt.Sprintf("%s was built from %s, not %s", node, h.Commit, e.raw)
		if strings.HasSuffix(h.Commit, "-dirty") {
			detail += " (a build from a modified tree is not that commit)"
		}
		return &finding{Code: codeVersionMismatch, Expected: e.raw, Observed: h.Commit, Detail: detail}
	}
	return nil
}

// failure is a finding of a code whose detail is formatted.
func failure(code, format string, args ...any) *finding {
	return &finding{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func nodeLabel(id string) string {
	if id == "" {
		return "a node with no id"
	}
	return "node " + id
}

// ---------------------------------------------------------------------------
// Asking
// ---------------------------------------------------------------------------

// prober carries what every probe needs.
type prober struct {
	client  *http.Client
	want    expectation
	samples int
}

func newProber(cfg config, want expectation) *prober {
	return &prober{
		client: &http.Client{
			Timeout: cfg.requestTimeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				// One connection per request. A kept-alive connection pins the
				// samples of a /healthz probe to whichever replica the load
				// balancer chose first, and the samples exist to meet others.
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		want:    want,
		samples: cfg.samples,
	}
}

// response is one answer, read to its limit.
type response struct {
	status   int
	header   http.Header
	body     []byte
	oversize bool // the body was longer than the limit; body holds its first limit bytes
}

// get asks for a URL. A non-nil finding means the request produced no answer
// to judge: it failed on the way, or it was redirected.
func (p *prober) get(ctx context.Context, target string, limit int64) (*response, *finding) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, unreachable(target, err)
	}
	req.Header.Set("User-Agent", userAgent)
	// A probe wants the answer of the host, not of a cache in front of it.
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, unreachable(target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := clean(resp.Header.Get("Location"))
		return nil, &finding{
			Code:     codeRedirectRefused,
			Observed: location,
			Detail:   fmt.Sprintf("GET %s answered HTTP %d redirecting to %q: a redirect is not followed", clean(target), resp.StatusCode, location),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, unreachable(target, err)
	}
	answer := &response{status: resp.StatusCode, header: resp.Header, body: body}
	if int64(len(body)) > limit {
		answer.body, answer.oversize = body[:limit], true
	}
	return answer, nil
}

// unreachable is the finding for a request that got no answer. It names the
// class of the failure, which is what an operator acts on, and quotes the
// error without the URL it carries (the finding says the URL once).
func unreachable(target string, err error) *finding {
	inner := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		inner = urlErr.Err
	}
	return &finding{
		Code:   codeUnreachable,
		Detail: fmt.Sprintf("GET %s: %s: %s", clean(target), transportClass(err), clean(inner.Error())),
	}
}

// transportClass names why a request failed: dns, tls, refused, reset, timeout,
// canceled, or network for everything else.
func transportClass(err error) string {
	var (
		dnsErr     *net.DNSError
		verifyErr  *tls.CertificateVerificationError
		authority  x509.UnknownAuthorityError
		hostname   x509.HostnameError
		invalid    x509.CertificateInvalidError
		record     tls.RecordHeaderError
		alert      tls.AlertError
		netTimeout net.Error
	)
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &verifyErr), errors.As(err, &authority), errors.As(err, &hostname),
		errors.As(err, &invalid), errors.As(err, &record), errors.As(err, &alert),
		strings.Contains(err.Error(), "tls: "), strings.Contains(err.Error(), "x509: "):
		return "tls"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netTimeout) && netTimeout.Timeout():
		return "timeout"
	}
	return "network"
}

// ---------------------------------------------------------------------------
// Health: api, identity and the OS host
// ---------------------------------------------------------------------------

// probeHealth asks a host's /healthz as many times as --samples says. One
// request reaches one replica, so a rollout that is half done answers
// correctly to half of them; the samples are how the other half is met, and a
// replica that is behind is a finding even when the next sample is fine.
func (p *prober) probeHealth(ctx context.Context, name, origin string) checkResult {
	target := origin + "/healthz"
	res := newCheck(name, target)
	var current []string // the nodes that answered as the rollout must
	for i := 0; i < p.samples; i++ {
		resp, miss := p.get(ctx, target, maxBodyBytes)
		if miss != nil {
			// Not answering is not a thing to ask again twice for.
			res.add(*miss)
			break
		}
		s, bad := p.judgeHealth(resp)
		res.Samples = append(res.Samples, s)
		if bad == nil {
			current = appendMissing(current, nodeLabel(s.NodeID))
			continue
		}
		res.add(*bad)
	}
	// A node that is behind is a PARTIAL rollout if another node is not, and
	// that is worth saying in the finding: it names both.
	if len(current) > 0 {
		for i, f := range res.Findings {
			if f.Code == codeVersionMismatch || f.Code == codeVersionUnreported {
				res.Findings[i].Detail += fmt.Sprintf("; %s answered as expected, so the rollout is partial", strings.Join(current, ", "))
			}
		}
	}
	return res.finish()
}

// judgeHealth reads one /healthz answer: the sample to record and, when the
// answer is not what a node of the release says, the finding.
//
// The finding is ONE per sample, the first thing wrong with it. A node that is
// draining is not asked what it runs: it is leaving, and its version is not
// what the verdict rests on.
func (p *prober) judgeHealth(resp *response) (sample, *finding) {
	status := fmt.Sprintf("http %d", resp.status)
	if resp.oversize {
		return sample{Status: status}, &finding{Code: codeHealthFailed,
			Detail: fmt.Sprintf("the /healthz answer is larger than %d bytes", maxBodyBytes)}
	}
	var h healthBody
	if err := json.Unmarshal(resp.body, &h); err != nil {
		return sample{Status: status}, &finding{Code: codeHealthFailed, Expected: healthyAnswer, Observed: status,
			Detail: fmt.Sprintf("HTTP %d with a body that is not a /healthz answer (content type %q)", resp.status, clean(mediaType(resp.header)))}
	}
	h.clean()
	s := sample{NodeID: h.NodeID, Version: h.Version, Commit: h.Commit, Status: h.Status}
	if s.Status == "" {
		s.Status = status
	}
	if resp.status != http.StatusOK || h.Status != "ok" {
		observed := fmt.Sprintf("HTTP %d, status %q", resp.status, h.Status)
		return s, &finding{Code: codeHealthFailed, Expected: healthyAnswer, Observed: observed,
			Detail: fmt.Sprintf("%s answered %s", nodeLabel(h.NodeID), observed)}
	}
	return s, p.want.judge(h)
}

func appendMissing(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}

// ---------------------------------------------------------------------------
// The OS bundle
// ---------------------------------------------------------------------------

// bundleManifest is what the edge image says its own OS bundle is: the sha256
// of every file, by path relative to the bundle's root (no leading slash).
type bundleManifest struct {
	Schema    int               `json:"schema"`
	Algorithm string            `json:"algorithm"`
	Files     map[string]string `json:"files"`
}

// probeOS asks the OS host for its page, then for the bundle manifest, then
// for every script and stylesheet the page names, and holds each to the
// manifest. That is the check the health probes cannot make: a replica can be
// healthy and on the new release while the front door still serves last
// week's shell, or a page whose hashed scripts are gone and answered with the
// shell in their place.
//
// The page itself is never compared with the manifest's copy: the edge
// injects its refresh script into every document it serves, so its bytes are
// not the bundle's.
func (p *prober) probeOS(ctx context.Context, origin string) checkResult {
	page := origin + "/"
	res := newCheck(checkOSBundle, page)
	base, err := url.Parse(page)
	if err != nil {
		res.add(*failure(codeOSInvalid, "the OS origin is not a URL: %s", clean(err.Error())))
		return res.finish()
	}

	resp, miss := p.get(ctx, page, maxBodyBytes)
	if miss != nil {
		res.add(*miss)
		return res.finish()
	}
	assets, bad := readOSPage(resp, base)
	if bad != nil {
		// A page that is not the shell has references that prove nothing.
		res.add(*bad)
		return res.finish()
	}

	manifest, bad := p.fetchManifest(ctx, origin)
	if bad != nil {
		res.add(*bad)
	}
	for _, ref := range assets {
		if f := p.checkAsset(ctx, ref, manifest); f != nil {
			res.add(*f)
		}
	}
	return res.finish()
}

// readOSPage judges the page and returns the bundle files it names. The
// references are the page's own, resolved against it: an absolute path, a
// relative one and a dot-relative one are the same file to a browser.
func readOSPage(resp *response, base *url.URL) ([]*url.URL, *finding) {
	switch {
	case resp.oversize:
		return nil, failure(codeOSInvalid, "the page is larger than %d bytes", maxBodyBytes)
	case resp.status != http.StatusOK:
		return nil, failure(codeOSInvalid, "the page answered HTTP %d, not 200", resp.status)
	case mediaType(resp.header) != "text/html":
		return nil, failure(codeOSInvalid, "the page is served as %q, not text/html", clean(mediaType(resp.header)))
	}
	doc, err := html.Parse(bytes.NewReader(resp.body))
	if err != nil {
		return nil, failure(codeOSInvalid, "the page does not parse as HTML: %s", clean(err.Error()))
	}
	title, found := pageTitle(doc)
	switch {
	case !found:
		f := failure(codeOSInvalid, "the page has no <title>, and the MemQL OS shell is titled %q", osTitle)
		f.Expected = osTitle
		return nil, f
	case title != osTitle:
		f := failure(codeOSInvalid, "the page is titled %q, not %q: this is not the MemQL OS shell", clean(title), osTitle)
		f.Expected, f.Observed = osTitle, clean(title)
		return nil, f
	}
	assets, scripts := bundleReferences(doc, base)
	if scripts == 0 {
		// A shell with no script of its own is a page that cannot run, and a
		// check with nothing to compare would pass without having compared.
		return nil, failure(codeOSInvalid, "the page names no same-origin script, so there is no bundle to compare with the manifest")
	}
	return assets, nil
}

// fetchManifest reads the bundle manifest. A manifest that is not there is
// "unreported": a 404, or -- because the edge answers a path it does not have
// with the shell, 200 -- an HTML page where a JSON file should be.
func (p *prober) fetchManifest(ctx context.Context, origin string) (map[string]string, *finding) {
	target := origin + manifestPath
	resp, miss := p.get(ctx, target, maxBodyBytes)
	if miss != nil {
		return nil, miss
	}
	switch {
	case resp.status == http.StatusNotFound:
		return nil, failure(codeBundleUnreported, "%s is not served (HTTP 404): the edge image carries no bundle manifest", manifestName)
	case resp.oversize:
		return nil, failure(codeOSInvalid, "%s is larger than %d bytes", manifestName, maxBodyBytes)
	case resp.status != http.StatusOK:
		return nil, failure(codeOSInvalid, "%s answered HTTP %d, not 200", manifestName, resp.status)
	case mediaType(resp.header) == "text/html" || looksLikeHTML(resp.body):
		return nil, failure(codeBundleUnreported, "%s is answered with an HTML page: the edge serves its fallback for a file it does not have", manifestName)
	}
	var m bundleManifest
	if err := json.Unmarshal(resp.body, &m); err != nil {
		return nil, failure(codeOSInvalid, "%s is not a JSON manifest: %s", manifestName, clean(err.Error()))
	}
	if m.Schema != 1 || m.Algorithm != "sha256" {
		return nil, failure(codeOSInvalid, "%s is schema %d with algorithm %q, not a schema 1 sha256 manifest", manifestName, m.Schema, clean(m.Algorithm))
	}
	if len(m.Files) == 0 {
		return nil, failure(codeOSInvalid, "%s lists no files", manifestName)
	}
	return m.Files, nil
}

// checkAsset fetches one script or stylesheet the page names and holds it to
// two questions: is it a real file, and is it THE file. The first catches the
// shell served in its place, the second a build that is not the image's. With
// no manifest to compare to, the second is skipped -- the manifest's own
// finding already says so.
func (p *prober) checkAsset(ctx context.Context, ref *url.URL, manifest map[string]string) *finding {
	key := strings.TrimPrefix(ref.Path, "/")
	resp, miss := p.get(ctx, ref.String(), maxAssetBytes)
	if miss != nil {
		return miss
	}
	invalid := func(format string, args ...any) *finding {
		return failure(codeAssetInvalid, "%s: %s", clean(key), fmt.Sprintf(format, args...))
	}
	switch {
	case resp.status != http.StatusOK:
		return invalid("HTTP %d, not 200", resp.status)
	case resp.oversize:
		return invalid("larger than %d bytes", maxAssetBytes)
	case len(resp.body) == 0:
		return invalid("empty")
	case mediaType(resp.header) == "text/html":
		return invalid("served as text/html: the single-page fallback and not the file")
	case looksLikeHTML(resp.body):
		return invalid("the body is an HTML document: the single-page fallback and not the file")
	}
	if manifest == nil {
		return nil
	}
	sum := sha256.Sum256(resp.body)
	got := hex.EncodeToString(sum[:])
	want, listed := manifest[key]
	switch {
	case !listed:
		return &finding{Code: codeBundleMismatch, Observed: got,
			Detail: fmt.Sprintf("%s is served but %s has no entry for it (served hash %s)", clean(key), manifestName, got)}
	case !strings.EqualFold(want, got):
		return &finding{Code: codeBundleMismatch, Expected: clean(want), Observed: got,
			Detail: fmt.Sprintf("%s hashes to %s but %s lists %s", clean(key), got, manifestName, clean(want))}
	}
	return nil
}

// looksLikeHTML says whether a body is a document and not a script or a
// stylesheet: it opens with a doctype or an html tag, after any byte order
// mark and whitespace, in any case.
func looksLikeHTML(body []byte) bool {
	head := body
	if len(head) > 256 {
		head = head[:256]
	}
	s := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(string(head), "\xef\xbb\xbf")))
	return strings.HasPrefix(s, "<!doctype") || strings.HasPrefix(s, "<html")
}

// ---------------------------------------------------------------------------
// Docs
// ---------------------------------------------------------------------------

// probeDocs reads the release off the docs site: a meta tag carrying the bare
// version. The site is versioned by release, so for a commit there is nothing
// to compare, and the check says it was skipped rather than passing.
func (p *prober) probeDocs(ctx context.Context, docsURL string) checkResult {
	res := newCheck(checkDocs, docsURL)
	switch {
	case docsURL == "":
		return res.skip("no --docs URL was given")
	case !p.want.release:
		return res.skip("--version is a commit, and the docs site is versioned by release")
	}

	resp, miss := p.get(ctx, docsURL, maxBodyBytes)
	if miss != nil {
		res.add(*miss)
		return res.finish()
	}
	mismatch := func(format string, args ...any) *finding {
		f := failure(codeDocsMismatch, format, args...)
		f.Expected = p.want.bare
		return f
	}
	switch {
	case resp.oversize:
		res.add(*mismatch("the docs page is larger than %d bytes", maxBodyBytes))
		return res.finish()
	case resp.status != http.StatusOK:
		res.add(*mismatch("the docs page answered HTTP %d, not 200", resp.status))
		return res.finish()
	}
	doc, err := html.Parse(bytes.NewReader(resp.body))
	if err != nil {
		res.add(*mismatch("the docs page does not parse as HTML: %s", clean(err.Error())))
		return res.finish()
	}
	content, found := docsVersion(doc)
	switch {
	case !found:
		res.add(*mismatch("the docs page carries no <meta name=%q>, so the release it documents cannot be told", docsMetaName))
	case content != p.want.bare:
		f := mismatch("the docs say %q, not %q", clean(content), p.want.bare)
		f.Observed = clean(content)
		res.add(*f)
	}
	return res.finish()
}

// ---------------------------------------------------------------------------
// Reading HTML and headers
// ---------------------------------------------------------------------------

// walk visits every node under root, depth first, in document order. It follows
// the parent links instead of recursing: the page is whatever the host serves,
// up to the response limit, and how deeply it nests is not the command's to
// trust.
func walk(root *html.Node, visit func(*html.Node)) {
	n := root
	for n != nil {
		visit(n)
		if n.FirstChild != nil {
			n = n.FirstChild
			continue
		}
		for n != nil && n != root && n.NextSibling == nil {
			n = n.Parent
		}
		if n == nil || n == root {
			return
		}
		n = n.NextSibling
	}
}

func isElement(n *html.Node, name string) bool {
	return n.Type == html.ElementNode && n.Namespace == "" && n.Data == name
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// pageTitle is the text of the first <title>.
func pageTitle(doc *html.Node) (string, bool) {
	var (
		title string
		found bool
	)
	walk(doc, func(n *html.Node) {
		if found || !isElement(n, "title") {
			return
		}
		found = true
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
		}
		title = strings.TrimSpace(b.String())
	})
	return title, found
}

// bundleReferences is the files of the bundle a page names: every <script src>
// and every <link rel="stylesheet|modulepreload" href>, resolved against the
// page, in document order, each once. It returns how many of them are scripts.
//
// A reference to another origin is not the bundle's to account for and is
// ignored, as the observer ignored it; so is the edge's own namespace, whose
// scripts are injected at serve time and are in no manifest.
func bundleReferences(doc *html.Node, base *url.URL) (assets []*url.URL, scripts int) {
	seen := map[string]bool{}
	add := func(raw string, script bool) {
		ref, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || strings.TrimSpace(raw) == "" {
			return
		}
		u := base.ResolveReference(ref)
		if u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
			return
		}
		if strings.HasPrefix(u.Path, edgePrefix) {
			return
		}
		u.Fragment, u.RawFragment = "", ""
		key := u.String()
		if seen[key] {
			return
		}
		seen[key] = true
		if script {
			scripts++
		}
		assets = append(assets, u)
	}
	walk(doc, func(n *html.Node) {
		switch {
		case isElement(n, "script"):
			if src, ok := attr(n, "src"); ok {
				add(src, true)
			}
		case isElement(n, "link"):
			href, ok := attr(n, "href")
			if !ok {
				return
			}
			rel, _ := attr(n, "rel")
			for _, token := range strings.Fields(strings.ToLower(rel)) {
				if token == "stylesheet" || token == "modulepreload" {
					add(href, false)
					return
				}
			}
		}
	})
	return assets, scripts
}

// docsVersion is the content of the docs meta tag.
func docsVersion(doc *html.Node) (string, bool) {
	var (
		content string
		found   bool
	)
	walk(doc, func(n *html.Node) {
		if found || !isElement(n, "meta") {
			return
		}
		if name, _ := attr(n, "name"); !strings.EqualFold(name, docsMetaName) {
			return
		}
		content, _ = attr(n, "content")
		found = true
	})
	return strings.TrimSpace(content), found
}

// mediaType is a response's content type without its parameters, in lower
// case, or "" when it has none.
func mediaType(h http.Header) string {
	ct := h.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	// A malformed parameter is not a reason to lose the type in front of it.
	mt, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// clean makes text a server supplied safe to put on one line of output: valid
// UTF-8, printable characters only (a newline is not one), at most maxQuoted of
// them.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, s)
	if runes := []rune(s); len(runes) > maxQuoted {
		s = string(runes[:maxQuoted]) + "..."
	}
	return s
}
