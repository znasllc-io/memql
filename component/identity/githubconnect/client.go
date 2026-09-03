package githubconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// THE THREE CALLS THE CALLBACK MAKES, and nothing else.
//
// Exchange the code for a user token, read who the person is, read which
// installations the grant can reach. Everything about a repository -- listing
// them, fetching one, minting an installation token -- is the ENGINE's half of
// this epic and deliberately absent here: the identity node's job ends the
// moment one sealed grant row exists.
//
// THE HOSTS ARE FIELDS, not constants, for exactly one reason: the acceptance
// tests drive the whole callback against an httptest server. They default to
// github.com and api.github.com, and nothing in the shipped path sets them.
// GitHub Enterprise Server is out of scope (design section G) -- a field that
// happens to accept another host is not support for one.

const (
	defaultOAuthBaseURL = "https://github.com"
	defaultAPIBaseURL   = "https://api.github.com"

	// installationPageSize is GitHub's maximum. Asking for the maximum keeps
	// the ordinary case -- a person with one or two installations -- to a
	// single request.
	installationPageSize = 100

	// installationPageCap bounds the walk. A grant reaching more than a
	// thousand installations is not a case this cluster has, and an unbounded
	// loop against a paginating third party is a hang in a browser redirect,
	// which presents as "Connect does nothing".
	installationPageCap = 10
)

// Client talks to GitHub. The zero value is usable and uses the real hosts
// with a bounded timeout.
type Client struct {
	// HTTP is the transport. Nil means a client with a 15-second timeout,
	// matching oidc.Exchange -- a browser is parked on this redirect, so an
	// unbounded wait is a page that never loads.
	HTTP *http.Client
	// OAuthBaseURL defaults to https://github.com (the authorize + token
	// endpoints live on the web host, not the API host).
	OAuthBaseURL string
	// APIBaseURL defaults to https://api.github.com.
	APIBaseURL string
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) oauthBase() string {
	if c != nil && strings.TrimSpace(c.OAuthBaseURL) != "" {
		return strings.TrimRight(c.OAuthBaseURL, "/")
	}
	return defaultOAuthBaseURL
}

func (c *Client) apiBase() string {
	if c != nil && strings.TrimSpace(c.APIBaseURL) != "" {
		return strings.TrimRight(c.APIBaseURL, "/")
	}
	return defaultAPIBaseURL
}

// TokenResponse is GitHub's answer to the code exchange.
//
// Note that Error is carried IN THE BODY on an HTTP 200: GitHub answers a bad
// verification code with a 200 and an `error` key, so a status-only check
// reads a refusal as a success and seals an empty token. ExchangeCode reads
// the key.
type TokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ExpiresAt turns the relative expiry into an absolute instant, or the zero
// time when GitHub reported none.
//
// An app with user-token expiry DISABLED sends no expires_in at all, and the
// row must then carry no expiresAt rather than a timestamp computed from a
// zero -- which would be `now`, i.e. a token that reads as expired the instant
// it is stored.
func (t TokenResponse) ExpiresAt(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC()
}

// ExchangeCode trades the callback's `code` for a user token.
//
// Form body plus `Accept: application/json`, which is what GitHub's token
// endpoint takes; without the Accept header it answers
// application/x-www-form-urlencoded and every field reads as empty.
func (c *Client) ExchangeCode(ctx context.Context, cfg Config, redirectURI, code string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("code", code)
	if strings.TrimSpace(redirectURI) != "" {
		form.Set("redirect_uri", redirectURI)
	}

	endpoint := c.oauthBase() + "/login/oauth/access_token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, fmt.Errorf("githubconnect: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("githubconnect: token exchange at %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read. The body is a small JSON object, and an unbounded
	// io.ReadAll against a third party is a memory decision made by them.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, fmt.Errorf("githubconnect: read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body is NOT included: it is a refusal from GitHub, this string
		// reaches a log, and a token endpoint's error body is exactly the
		// place a credential could be echoed back.
		return TokenResponse{}, fmt.Errorf("githubconnect: token exchange at %s answered %d", endpoint, resp.StatusCode)
	}

	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return TokenResponse{}, fmt.Errorf("githubconnect: parse token response from %s: %w", endpoint, err)
	}
	if strings.TrimSpace(out.Error) != "" {
		// A 200 carrying `error`. Named, because "bad_verification_code" and
		// "incorrect_client_credentials" are different repairs -- one is a
		// replayed callback, the other is a cluster misconfiguration.
		return TokenResponse{}, fmt.Errorf("githubconnect: GitHub refused the code exchange: %s", out.Error)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return TokenResponse{}, fmt.Errorf("githubconnect: the code exchange returned no access token")
	}
	return out, nil
}

// User is the little of GET /user this flow needs.
type User struct {
	Login string `json:"login"`
	// ID is GitHub's numeric user id -- the STABLE key a reconnect updates in
	// place. A login is a display string that moves when somebody renames
	// their account; keying on it would mint a second grant for one person.
	ID int64 `json:"id"`
}

// CurrentUser reads who the token belongs to.
func (c *Client) CurrentUser(ctx context.Context, token string) (User, error) {
	var out User
	if err := c.getJSON(ctx, token, c.apiBase()+"/user", &out); err != nil {
		return User{}, err
	}
	if strings.TrimSpace(out.Login) == "" || out.ID == 0 {
		return User{}, fmt.Errorf("githubconnect: GET /user answered without a login and id")
	}
	return out, nil
}

type installationsPage struct {
	TotalCount    int `json:"total_count"`
	Installations []struct {
		ID int64 `json:"id"`
	} `json:"installations"`
}

// Installations lists the installation ids this grant can reach, as strings.
//
// STRINGS because that is what the row's installationIds field is: the ids are
// only ever compared and put in a URL path, never added up, and a []string
// keeps the DSL list type honest without a conversion at every read.
//
// An installation still awaiting an organisation owner's approval is NOT in
// this list -- GitHub does not return it -- and that is the correct answer: a
// pending installation is not a reachable one, and the concept's own
// documentation says so.
func (c *Client) Installations(ctx context.Context, token string) ([]string, error) {
	var ids []string
	for page := 1; page <= installationPageCap; page++ {
		endpoint := fmt.Sprintf("%s/user/installations?per_page=%d&page=%d", c.apiBase(), installationPageSize, page)
		var out installationsPage
		if err := c.getJSON(ctx, token, endpoint, &out); err != nil {
			return nil, err
		}
		for _, inst := range out.Installations {
			if inst.ID != 0 {
				ids = append(ids, strconv.FormatInt(inst.ID, 10))
			}
		}
		if len(out.Installations) < installationPageSize || len(ids) >= out.TotalCount {
			break
		}
	}
	return ids, nil
}

// getJSON issues one authenticated GET and decodes the body.
//
// The token rides the Authorization header and appears in no URL: a URL is
// logged by every proxy on the path and by GitHub itself.
func (c *Client) getJSON(ctx context.Context, token, endpoint string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("githubconnect: build request for %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("githubconnect: GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("githubconnect: read %s: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("githubconnect: GET %s answered %d", endpoint, resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("githubconnect: parse %s: %w", endpoint, err)
	}
	return nil
}
