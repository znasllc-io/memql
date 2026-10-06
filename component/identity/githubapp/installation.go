package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// installation.go -- the app speaking as itself (C6).
//
// Two calls, and between them they are the whole reason background work under
// a grant does not depend on a person being signed in: ask which installation
// covers a repository, then mint that installation's token.

// InstallationForRepo answers the installation covering owner/repo, or
// ErrNotInstalled.
//
// It is asked under the APP JWT, not under anybody's user token, and that is
// what makes its 404 mean something precise: the app itself is asking whether
// it is installed on a repository, so "no" is a fact about the INSTALLATION
// and not about the asker's visibility. That distinction is the whole
// difference between repository_not_installed and a 404 the fetcher would have
// to read as "private, or not there".
func (c *Client) InstallationForRepo(ctx context.Context, owner, repo string) (int64, error) {
	cfg := c.config()
	if !cfg.Configured() {
		return 0, ErrNotConfigured
	}
	assertion, err := c.appJWT(cfg, c.now())
	if err != nil {
		return 0, err
	}
	var payload struct {
		Id int64 `json:"id"`
	}
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if _, cerr := c.call(ctx, http.MethodGet, endpoint, assertion, &payload); cerr != nil {
		if StatusOf(cerr) == http.StatusNotFound {
			return 0, ErrNotInstalled
		}
		return 0, cerr
	}
	if payload.Id == 0 {
		// A 200 naming no installation is the same fact as a 404 and must
		// answer the same way: a zero id used as an installation id would
		// mint against /app/installations/0 and fail with a status nobody
		// can act on.
		return 0, ErrNotInstalled
	}
	return payload.Id, nil
}

// InstallationToken mints (or reuses) the bearer background work runs under.
//
// The cache is keyed on the installation and is PER PROCESS, which is the
// right grain for both reasons it exists: every package fetching from one
// organization shares an installation, and a token is a bearer for that whole
// installation, so keeping it anywhere a second process could read it would
// widen its blast radius without shortening its life.
func (c *Client) InstallationToken(ctx context.Context, installationId int64) (string, error) {
	cfg := c.config()
	if !cfg.Configured() {
		return "", ErrNotConfigured
	}
	if installationId == 0 {
		return "", ErrNotInstalled
	}
	now := c.now()

	c.mu.Lock()
	cached, ok := c.tokens[installationId]
	c.mu.Unlock()
	if ok && now.Before(cached.expires.Add(-tokenSkew)) {
		return cached.token, nil
	}

	assertion, err := c.appJWT(cfg, now)
	if err != nil {
		return "", err
	}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	endpoint := "/app/installations/" + strconv.FormatInt(installationId, 10) + "/access_tokens"
	if _, cerr := c.call(ctx, http.MethodPost, endpoint, assertion, &payload); cerr != nil {
		if StatusOf(cerr) == http.StatusNotFound {
			// The installation is gone -- somebody uninstalled the app while
			// a stored id still named it. That is not a failure of this
			// cluster's configuration, it is the same fact ErrNotInstalled
			// carries, and answering it that way is what turns a stale id
			// into an installation link rather than an unexplained 404.
			return "", ErrNotInstalled
		}
		return "", cerr
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		return "", errors.New("GitHub minted an empty installation token")
	}

	// An UNPARSEABLE expiry is treated as one minute from now rather than as
	// forever: the token still works, and the next call re-mints. Reading it
	// as never-expiring would cache a dead bearer for the life of the process.
	expires := now.Add(time.Minute)
	if parsed, perr := time.Parse(time.RFC3339, strings.TrimSpace(payload.ExpiresAt)); perr == nil {
		expires = parsed.UTC()
	}
	c.mu.Lock()
	c.tokens[installationId] = cachedToken{token: token, expires: expires}
	c.mu.Unlock()
	return token, nil
}

// ScopedInstallationToken mints an UNCACHED installation token narrowed to
// the named repositories and permissions (POST access_tokens with a body).
// The substrate mints the clone token a Job carries this way: contents read
// on one repository, never the installation-wide token the cache holds.
//
// Repositories are NAMES within the installation's account ("widget", not
// "acme/widget"), which is how GitHub's access_tokens body takes them.
//
// A REQUEST THAT WOULD NOT NARROW IS REFUSED before anything is signed. No
// repositories, or no permissions, is GitHub's spelling of "all of them": a
// call meant to mint the narrow token would hand back the wide one, so an empty
// list is an error here rather than a default.
//
// Never cached, in either direction. The cache is keyed on the installation
// and holds the wide token; a narrowed one stored there would be served to a
// caller expecting the installation's reach, and the wide one served here
// would be exactly the token this exists to keep off a Job.
func (c *Client) ScopedInstallationToken(ctx context.Context, installationID int64, repositories []string, permissions map[string]string) (token string, expiresAt time.Time, err error) {
	cfg := c.config()
	if !cfg.Configured() {
		return "", time.Time{}, ErrNotConfigured
	}
	if installationID == 0 {
		return "", time.Time{}, ErrNotInstalled
	}
	if len(repositories) == 0 || len(permissions) == 0 {
		return "", time.Time{}, errors.New("a scoped installation token names the repositories and the permissions it is narrowed to")
	}
	for _, name := range repositories {
		if strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
			return "", time.Time{}, errors.New("a scoped installation token names repositories by name within the installation's account")
		}
	}
	for scope, level := range permissions {
		if strings.TrimSpace(scope) == "" || strings.TrimSpace(level) == "" {
			return "", time.Time{}, errors.New("a scoped installation token names each permission and its level")
		}
	}

	now := c.now()
	assertion, err := c.appJWT(cfg, now)
	if err != nil {
		return "", time.Time{}, err
	}
	body := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{Repositories: repositories, Permissions: permissions}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	endpoint := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	if _, cerr := c.callJSON(ctx, http.MethodPost, endpoint, assertion, body, &payload); cerr != nil {
		if StatusOf(cerr) == http.StatusNotFound {
			return "", time.Time{}, ErrNotInstalled
		}
		return "", time.Time{}, cerr
	}
	token = strings.TrimSpace(payload.Token)
	if token == "" {
		return "", time.Time{}, errors.New("GitHub minted an empty installation token")
	}
	// The same reading of an unparseable expiry InstallationToken makes: a
	// minute from now, never forever.
	expiresAt = now.Add(time.Minute)
	if parsed, perr := time.Parse(time.RFC3339, strings.TrimSpace(payload.ExpiresAt)); perr == nil {
		expiresAt = parsed.UTC()
	}
	return token, expiresAt, nil
}

// AppInstallation is one installation of this cluster's app, as the APP sees
// it (the person-facing listing is Installation, in user.go):
// whose it is, where its settings live on GitHub, and the permissions and
// events it has ACCEPTED -- which lag what the app asks for until the account
// that installed it approves a change GitHub sends it.
type AppInstallation struct {
	ID          int64
	Account     string
	AccountType string
	// HTMLURL is the installation's own settings page on GitHub, where a
	// permissions change waits to be accepted.
	HTMLURL     string
	Permissions map[string]string
	Events      []string
	// SuspendedAt is set while the installation is suspended.
	SuspendedAt string
}

// installationsPerPage is GitHub's largest page for the app's installations.
const installationsPerPage = 100

// Installations lists every installation of this cluster's app, asked under
// the app JWT, page by page until a page comes back short.
//
// It is the one read that can see a PERMISSIONS CHANGE nobody accepted: an app
// whose permissions grew (pipelines' checks write, epic memql#5477) keeps
// every existing installation on the old set until its account approves, and
// until then every check-run write there answers 403. The pipelines readiness
// item compares what each installation accepted with what the app asks for and
// names the ones that lag, with the page where the change waits.
func (c *Client) Installations(ctx context.Context) ([]AppInstallation, error) {
	cfg := c.config()
	if !cfg.Configured() {
		return nil, ErrNotConfigured
	}
	var out []AppInstallation
	for page := 1; ; page++ {
		assertion, err := c.appJWT(cfg, c.now())
		if err != nil {
			return nil, err
		}
		var payload []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
			HTMLURL     string            `json:"html_url"`
			Permissions map[string]string `json:"permissions"`
			Events      []string          `json:"events"`
			SuspendedAt string            `json:"suspended_at"`
		}
		endpoint := "/app/installations?per_page=" + strconv.Itoa(installationsPerPage) + "&page=" + strconv.Itoa(page)
		if _, cerr := c.call(ctx, http.MethodGet, endpoint, assertion, &payload); cerr != nil {
			return nil, cerr
		}
		for _, p := range payload {
			out = append(out, AppInstallation{
				ID: p.ID, Account: p.Account.Login, AccountType: p.Account.Type, HTMLURL: p.HTMLURL,
				Permissions: p.Permissions, Events: p.Events, SuspendedAt: p.SuspendedAt,
			})
		}
		if len(payload) < installationsPerPage {
			return out, nil
		}
	}
}
