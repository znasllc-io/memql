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
	if !c.Configured() {
		return 0, ErrNotConfigured
	}
	assertion, err := c.appJWT(c.now())
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
// organisation shares an installation, and a token is a bearer for that whole
// installation, so keeping it anywhere a second process could read it would
// widen its blast radius without shortening its life.
func (c *Client) InstallationToken(ctx context.Context, installationId int64) (string, error) {
	if !c.Configured() {
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

	assertion, err := c.appJWT(now)
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
