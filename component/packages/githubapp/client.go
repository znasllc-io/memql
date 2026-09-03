package githubapp

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Default endpoints. Two of them, because GitHub serves its REST API and its
// OAuth endpoints from different hosts and only the first is api.github.com.
const (
	DefaultAPIBase   = "https://api.github.com"
	DefaultOAuthBase = "https://github.com"
)

// The headers every call carries, matching component/packages' probe and
// fetcher exactly. GitHub answers differently under a different Accept, so a
// picker that asked under other headers would be describing a repository the
// fetch would not see.
const (
	acceptJSON = "application/vnd.github+json"
	apiVersion = "2022-11-28"
	userAgent  = "memql-packages"
)

// maxBody bounds every reply this package decodes. A repository listing is a
// few hundred kilobytes; a megabyte is generous and a body without a bound is
// a memory ceiling set by somebody else.
const maxBody = 4 << 20

// ErrNotConfigured is the answer for every call on a cluster with no GitHub
// App. It is a sentinel rather than a status because nothing was asked: the
// caller maps it to github_app_not_configured and offers the token path.
var ErrNotConfigured = errors.New("this cluster has no GitHub App configured")

// ErrReauthorize is GitHub refusing the AUTHORIZATION rather than the request:
// a refresh token that is spent, revoked or six months old. The only repair is
// the person reconnecting, which is what makes it a sentinel of its own rather
// than a 401 the caller has to interpret.
var ErrReauthorize = errors.New("GitHub refused this authorization grant")

// ErrNotInstalled is "no installation of this app covers that repository". The
// person can see it and the app cannot, so the repair is an installation link
// and not another credential.
var ErrNotInstalled = errors.New("the GitHub App is not installed on this repository")

// StatusError is a GitHub answer this package could not read as success.
//
// It carries the ENDPOINT PATH and never the URL: the OAuth calls put a client
// secret in the body and a refresh token in the form, and an error that
// rendered a full URL would be one careless query parameter away from putting
// a secret in a log.
type StatusError struct {
	Status   int
	Endpoint string
	// RateLimited is GitHub saying "ask again later" rather than anything
	// about the request. It is recorded HERE, at the one place a response is
	// read, because it is spelled two different ways -- 429 for a secondary
	// limit and 403 with the remaining count at zero for the primary one --
	// and a caller that had to check both would eventually check one.
	RateLimited bool
}

func (e *StatusError) Error() string {
	if e.RateLimited {
		return fmt.Sprintf("GitHub rate-limited this cluster (HTTP %d) for %s", e.Status, e.Endpoint)
	}
	return fmt.Sprintf("GitHub answered HTTP %d for %s", e.Status, e.Endpoint)
}

// IsRateLimited reports whether err is GitHub asking to be left alone for a
// while. It says nothing about the repository or the credential, which is why
// every caller renders it as its own answer.
func IsRateLimited(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.RateLimited
}

// StatusOf reports the HTTP status behind err, or 0 when err is not a GitHub
// status at all. The caller classifies on it -- a 401 under a grant is a
// different fact from a 404 -- so it is read through a function rather than a
// type assertion at four call sites.
func StatusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// Client talks to GitHub as the app and as the app's users.
//
// Safe for concurrent use: the private key is parsed once behind a sync.Once
// and the installation-token cache is mutex-guarded. Both matter -- a poll
// sweeping thirty packages on one node hits the same installation repeatedly,
// and each mint is a round trip and a rate-limit unit.
type Client struct {
	cfg       Config
	http      *http.Client
	apiBase   string
	oauthBase string
	now       func() time.Time

	keyOnce sync.Once
	key     *rsa.PrivateKey
	keyErr  error

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

// cachedToken is one installation token and when it stops working.
//
// IN MEMORY AND PER PROCESS, never on a row (design E). A minted installation
// token is a bearer for every repository in an installation; writing one to
// the graph would put it behind the row-authz tier of a concept whose whole
// point is that only the fetch ever holds a secret, and it would outlive the
// hour it is good for in every backup taken since.
type cachedToken struct {
	token   string
	expires time.Time
}

// tokenSkew is how early a cached token is treated as expired. A token minted
// for an hour and used at fifty-nine minutes is a fetch that fails halfway
// through a deploy; a minute of headroom costs one extra mint an hour.
const tokenSkew = 60 * time.Second

// Option tunes a Client. Every one of them exists for a test: the HTTP client
// carries the fake, the two bases point at it, and the clock is what makes
// "the cached token expired" observable without sleeping.
type Option func(*Client)

func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }
func WithAPIBase(base string) Option {
	return func(c *Client) { c.apiBase = strings.TrimSuffix(strings.TrimSpace(base), "/") }
}
func WithOAuthBase(base string) Option {
	return func(c *Client) { c.oauthBase = strings.TrimSuffix(strings.TrimSpace(base), "/") }
}
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// New builds a client over cfg. An UNCONFIGURED cfg builds a usable client
// whose every call answers ErrNotConfigured -- rather than a nil client the
// caller has to remember to check, which is the shape that produces a
// dereference on the one node nobody configured.
func New(cfg Config, opts ...Option) *Client {
	c := &Client{
		cfg:       cfg,
		http:      &http.Client{Timeout: 30 * time.Second},
		apiBase:   DefaultAPIBase,
		oauthBase: DefaultOAuthBase,
		now:       func() time.Time { return time.Now().UTC() },
		tokens:    map[int64]cachedToken{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// FromEnv is New over ConfigFromEnv -- what a production node wires.
func FromEnv(opts ...Option) *Client { return New(ConfigFromEnv(), opts...) }

// Configured reports whether this client can do anything at all.
func (c *Client) Configured() bool { return c != nil && c.cfg.Configured() }

// Missing names the absent configuration values, for the operator sentence
// behind github_app_not_configured. Nil is treated as "everything", because a
// node with no client wired is in the same position as one with no values.
func (c *Client) Missing() []string {
	if c == nil {
		return Config{}.Missing()
	}
	return c.cfg.Missing()
}

// InstallURL is where a person installs the app on another account. Empty when
// the slug is absent, so a caller renders no link rather than a broken one.
func (c *Client) InstallURL() string {
	if c == nil || strings.TrimSpace(c.cfg.Slug) == "" {
		return ""
	}
	return c.oauthBase + "/apps/" + url.PathEscape(c.cfg.Slug) + "/installations/new"
}

// ---------------------------------------------------------------------------
// The one request path
// ---------------------------------------------------------------------------

// call issues one API request and decodes its JSON body into out.
//
// EVERY API call in this package goes through here, which is what makes the
// headers, the body bound and the status classification single facts rather
// than five copies that drift. `bearer` is the whole Authorization value's
// credential half and is written straight onto the header: it is never
// inspected, never compared and never returned.
func (c *Client) call(ctx context.Context, method, endpoint, bearer string, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if strings.TrimSpace(bearer) != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, derr := c.http.Do(req)
	if derr != nil {
		return nil, derr
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// The path WITHOUT its query: an endpoint is enough to say which call
		// failed, and a query string is where a token would be if anybody
		// ever put one there.
		return resp, &StatusError{
			Status:      resp.StatusCode,
			Endpoint:    pathOnly(endpoint),
			RateLimited: rateLimited(resp),
		}
	}
	if out != nil {
		if derr := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); derr != nil {
			return resp, fmt.Errorf("GitHub answered %s with a body this cluster could not read: %v", pathOnly(endpoint), derr)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	}
	return resp, nil
}

// rateLimited reads GitHub's two spellings of "ask again later". The primary
// limit is a 403 with the remaining count at zero; the secondary limits are a
// 429. A 403 WITHOUT that header is an ordinary refusal and must not be filed
// as rate limiting -- it would tell somebody to wait for a permission problem
// that waiting does not fix.
func rateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return resp.StatusCode == http.StatusForbidden &&
		strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0"
}

func pathOnly(endpoint string) string {
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		return endpoint[:i]
	}
	return endpoint
}
