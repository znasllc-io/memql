// Package errorpages serves the pages a front door falls back to when it has
// nothing behind it.
//
// # Why it exists
//
// On 2026-10-11 the whole mesh was down and every host the cluster serves, the
// platform's own and every customer's, answered ingress-nginx's bare
// "503 Service Temporarily Unavailable". Nothing on the page said whose site
// it was, whether the fault was theirs or ours, or whether to wait. This
// package is the replacement: a handler with no database, no secrets and no
// dependency on the mesh, so it is the one thing that still answers when
// everything else does not. `memql error-pages` runs it, and every front-door
// Ingress names it as its default backend
// (nginx.ingress.kubernetes.io/default-backend).
//
// # Who sees what
//
// The decision is made on the request's Host and nothing else.
//
//   - The platform's own hosts (the apex, and api, identity, mcp, os and vscode
//     under it) get the MemQL page: the mark, the name, and a plain statement
//     that the platform is being updated.
//   - Every other host gets a neutral page with no MemQL mark, name, link or
//     colour. A customer's visitor has never heard of the platform underneath
//     their shopfront, and "MemQL is being updated" on someone else's site
//     would put our name on their outage. The hosts a client's reserved
//     account name serves (app, api and id under memql.<their domain>) are
//     sites in this sense too: they appear under the client's brand and are
//     read by people who have never heard of ours.
//
// The platform's host set is not listed here. It is component/frontdoor's,
// the one place those hosts are composed, so a platform site added there is
// branded here without anybody remembering a second list. An unset or
// malformed domain makes every host a site: the unconfigured case fails toward
// not branding somebody's page, and the subcommand says so on stderr at start.
//
// An API client gets JSON, not a page it cannot parse: when Accept names
// application/json, and always on the api host.
//
// # /healthz answers the kubelet and nobody else
//
// The Deployment probes this server's readiness and liveness on /healthz, so
// it answers 200 "ok". But this server is also the default backend of every
// front door, so `https://api.<domain>/healthz` reaches it whenever the real
// node cannot answer, and a 200 there would turn the operator's own checks
// into false greens: the status-code loop in
// docs/public/operate/instance-scale-and-upgrade.md, which files any non-200
// during a roll as a defect, the post-roll /healthz gate on identity, api and
// os, and any uptime monitor would all read the middle of an outage as
// healthy. A request that came through ingress-nginx carries the headers it
// adds to everything it forwards (X-Forwarded-For, X-Real-IP and X-Request-ID
// among them); a kubelet probe is sent straight to the pod and carries none of
// them. So a forwarded /healthz is treated like any other path: a 503.
//
// # What it deliberately does not do
//
// It logs no request. An idle instance was writing about a million log lines a
// day on 2026-10-11, and a default backend in the middle of an outage is
// exactly the process that would add one per visitor.
//
// It is self-contained. Each page is one inline stylesheet, the mark in the
// markup, no font file, no script, no image, and the response's
// Content-Security-Policy allows that one stylesheet by hash and nothing
// else. Nothing the page needs is anywhere but in the response. Measured with
// the mark inline, the MemQL page is 4,214 bytes and the neutral one 1,782,
// against the 20 KB the tests allow.
package errorpages

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// assets holds the two pages and the brand mark. //go:embed cannot reach
// ../../brand, so mark.svg is a byte copy of brand/mark.svg and
// TestMarkMatchesBrand fails the build when the copy drifts, as the identity
// favicon copy does (brand_shared_source_test.go).
//
//go:embed mark.svg maintenance.html.tmpl unavailable.html.tmpl
var assets embed.FS

// Kind says whose page a host gets.
type Kind int

const (
	// KindPlatform is one of the platform's own hosts: it gets the MemQL page.
	KindPlatform Kind = iota
	// KindSite is any other host: a customer's site, a custom domain, an
	// address, a request with no host at all. It gets the neutral page.
	KindSite
)

func (k Kind) String() string {
	switch k {
	case KindPlatform:
		return "platform"
	case KindSite:
		return "site"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// ClassifyHost answers KindPlatform for domain itself and for every host the
// front door serves under it (api, identity, mcp, os, vscode), and KindSite for
// everything else: other subdomains, any other domain, a host nested one label
// deeper, an address, and every host at all when domain is empty.
//
// Case, a port and a trailing dot are not part of the name. ingress-nginx
// forwards the client's Host header as it was sent, so `OS.Memql.Example:443`
// is a real value here and must be the platform's own host.
func ClassifyHost(host, domain string) Kind {
	domain = normalizeHost(domain)
	if domain == "" {
		return KindSite
	}
	host = normalizeHost(host)
	for _, h := range frontdoor.Hosts(domain) {
		// The wildcard is the Ingress rule that reaches every site, not a host
		// a request carries.
		if !h.Wildcard && h.Name == host {
			return KindPlatform
		}
	}
	return KindSite
}

// normalizeHost is a Host header, or a configured domain, reduced to the name:
// trimmed, lower-cased, without a port and without the trailing dot of a fully
// qualified name. An IPv6 literal keeps its brackets and so matches nothing.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}

// Every response is a 503 that says to come back in two minutes: long enough
// that a client honouring it does not lean on a mesh that is still coming up,
// short enough that "a few minutes" on the page is true.
const (
	retryAfterSeconds = "120"
	errorCode         = "unavailable"
)

// copyText is the words of one page. The HTML shows Heading and Detail as a
// heading and a line under it; the JSON carries them as one message, so a
// person and a program are told the same thing.
type copyText struct {
	Title   string
	Heading string
	Detail  string
}

// Message is the whole text as one sentence pair: the JSON body, and the
// description a link preview shows when someone shares the page's address.
func (c copyText) Message() string { return c.Heading + " " + c.Detail }

var (
	platformCopy = copyText{
		Title:   "MemQL is being updated",
		Heading: "MemQL is being updated.",
		Detail:  "It will be back in a few minutes.",
	}
	siteCopy = copyText{
		Title:   "Temporarily unavailable",
		Heading: "This site is temporarily unavailable.",
		Detail:  "It will be back soon.",
	}
)

// pageData is what the templates read: the words, and the mark. The neutral
// page is given no mark, and its template has nowhere to put one.
type pageData struct {
	copyText
	Mark template.HTML
}

// errorBody is the JSON an API client gets.
type errorBody struct {
	Error   string `json:"error"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// rendered is one audience's answer, built once: the pages have no per-request
// input, so a request costs a map lookup and a write.
type rendered struct {
	html []byte
	csp  string
	json []byte
}

var (
	// xmlComment matches the explanatory comment mark.svg carries. The brand
	// file keeps it for people reading the repository; it is dead weight on
	// the wire.
	xmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// blankLines matches the whitespace-only lines the removed comment leaves.
	blankLines = regexp.MustCompile(`\n[ \t]*(?:\n[ \t]*)+`)
)

func loadMark() (template.HTML, error) {
	raw, err := assets.ReadFile("mark.svg")
	if err != nil {
		return "", err
	}
	svg := xmlComment.ReplaceAllString(string(raw), "")
	svg = strings.TrimSpace(blankLines.ReplaceAllString(svg, "\n"))
	if !strings.HasPrefix(svg, "<svg") {
		return "", fmt.Errorf("mark.svg does not start with <svg")
	}
	// The file is the repository's own, embedded at build time, so it is
	// trusted markup; html/template would otherwise escape it as text.
	return template.HTML(svg), nil
}

func render(name string, c copyText, mark template.HTML) (rendered, error) {
	tmpl, err := template.ParseFS(assets, name)
	if err != nil {
		return rendered{}, fmt.Errorf("parse %s: %w", name, err)
	}
	var page bytes.Buffer
	if err := tmpl.Execute(&page, pageData{copyText: c, Mark: mark}); err != nil {
		return rendered{}, fmt.Errorf("execute %s: %w", name, err)
	}
	csp, err := contentSecurityPolicy(page.Bytes())
	if err != nil {
		return rendered{}, fmt.Errorf("%s: %w", name, err)
	}
	body, err := json.Marshal(errorBody{Error: errorCode, Status: http.StatusServiceUnavailable, Message: c.Message()})
	if err != nil {
		return rendered{}, err
	}
	return rendered{html: page.Bytes(), csp: csp, json: body}, nil
}

// contentSecurityPolicy allows the page's one inline stylesheet, by hash, and
// nothing else. "Self-contained" is then a rule the browser enforces on every
// visit, not only a property the tests checked once: a page that grew a script
// or a remote font would be blocked instead of quietly working.
//
// The hash is taken from the rendered bytes, so editing the stylesheet cannot
// leave the policy describing the old one.
func contentSecurityPolicy(page []byte) (string, error) {
	const open, shut = "<style>", "</style>"
	start := bytes.Index(page, []byte(open))
	end := bytes.Index(page, []byte(shut))
	if start < 0 || end < start {
		return "", fmt.Errorf("no <style> element to pin")
	}
	sum := sha256.Sum256(page[start+len(open) : end])
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'", nil
}

// Handler serves every path, for every method, with status 503 and
// Retry-After: 120. The platform's hosts (see ClassifyHost) get the MemQL page,
// every other host the neutral one, and JSON in place of either for an API
// client. GET /healthz answers 200 "ok" to a probe that did not come through
// the ingress.
//
// It panics if its own embedded pages cannot be rendered. That is a defect in
// this package and not a condition of the world: the same bytes fail on every
// start, the tests render both pages, and failing before the listener opens
// keeps a rollout from replacing working pods with broken ones.
func Handler(domain string) http.Handler {
	mark, err := loadMark()
	if err != nil {
		panic("errorpages: " + err.Error())
	}
	platform, err := render("maintenance.html.tmpl", platformCopy, mark)
	if err != nil {
		panic("errorpages: " + err.Error())
	}
	site, err := render("unavailable.html.tmpl", siteCopy, "")
	if err != nil {
		panic("errorpages: " + err.Error())
	}
	return &handler{
		domain: normalizeHost(domain),
		pages:  map[Kind]rendered{KindPlatform: platform, KindSite: site},
	}
}

type handler struct {
	domain string
	pages  map[Kind]rendered
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isProbe(r) {
		hdr := w.Header()
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Content-Length", "2")
		hdr.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("ok"))
		}
		return
	}

	p := h.pages[ClassifyHost(r.Host, h.domain)]
	hdr := w.Header()
	body := p.html
	if h.wantsJSON(r) {
		hdr.Set("Content-Type", "application/json")
		body = p.json
	} else {
		hdr.Set("Content-Type", "text/html; charset=utf-8")
		hdr.Set("Content-Security-Policy", p.csp)
	}
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Retry-After", retryAfterSeconds)
	hdr.Set("Vary", "Accept")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// wantsJSON is true when the client asked for JSON, and always on api.<domain>,
// whose callers are programs whatever they sent.
func (h *handler) wantsJSON(r *http.Request) bool {
	if acceptsJSON(strings.Join(r.Header.Values("Accept"), ",")) {
		return true
	}
	return h.domain != "" && normalizeHost(r.Host) == frontdoor.RoleHost(frontdoor.RoleAPI, h.domain)
}

// acceptsJSON reports whether an Accept header lists application/json with a
// non-zero weight. A wildcard does not count, so a bare */* from curl or a
// browser's fetch gets the page; a client that wants JSON says so.
func acceptsJSON(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || mediaType != "application/json" {
			continue
		}
		if q, ok := params["q"]; ok {
			if weight, err := strconv.ParseFloat(q, 64); err == nil && weight <= 0 {
				continue
			}
		}
		return true
	}
	return false
}

// proxyHeaders are what ingress-nginx adds to every request it forwards. A
// kubelet probe is sent straight to the pod and carries none of them.
var proxyHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-IP", "X-Request-ID", "Forwarded"}

// isProbe is a GET or HEAD for exactly /healthz that no proxy forwarded.
func isProbe(r *http.Request) bool {
	if r.URL.Path != "/healthz" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	for _, name := range proxyHeaders {
		if len(r.Header.Values(name)) > 0 {
			return false
		}
	}
	return true
}
