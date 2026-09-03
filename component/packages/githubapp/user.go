package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// user.go -- the app speaking as the PERSON.
//
// Every call here runs under the grant's user token, which means every one of
// them is bounded by what that person can see at GitHub. That is the property
// the picker needs: one person can never list another's repositories, because
// the authority presented is the first person's own and nothing in the
// argument list can change it.

// RepositoriesPerPage is the page size for an installation's repositories.
// GitHub's maximum, because the picker's job is to show somebody every
// repository they can reach and a smaller page only adds round trips.
const RepositoriesPerPage = 100

// User is who a grant belongs to.
//
// Id is the STABLE key and Login is the display string. They are separate
// fields because a rename moves the second and not the first, and a grant keyed
// on the login would become a second row the day somebody renamed.
type User struct {
	Login string `json:"login"`
	Id    int64  `json:"id"`
}

// Installation is one place the app is installed that this person can reach.
type Installation struct {
	Id      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	// RepositorySelection is "all" or "selected". The picker shows it because
	// "I do not see my repository" has two different answers depending on it.
	RepositorySelection string `json:"repository_selection"`
	// SuspendedAt is non-empty for an installation an owner suspended. It is
	// carried rather than filtered: a suspended installation is a fact about
	// why nothing under it works, and hiding it makes the repository simply
	// absent.
	SuspendedAt string `json:"suspended_at"`
}

// Repository is one repository, in the shape the picker groups and prefills
// from. InstallationId is stamped by the caller rather than by GitHub: the
// listing is per installation, so the id is known from the request.
type Repository struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Visibility    string `json:"visibility"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
	HTMLURL       string `json:"html_url"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	InstallationId int64 `json:"-"`
}

// InstallationRequest is an installation waiting for an organisation owner's
// approval. GitHub surfaces these to the APP rather than to the person, which
// is why reading them needs the app JWT and why they are a separate call.
type InstallationRequest struct {
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	Requester struct {
		Login string `json:"login"`
	} `json:"requester"`
}

// TokenSet is what an OAuth exchange or refresh returns.
//
// Both tokens are here because a refresh ROTATES the refresh token: storing
// the new access token and keeping the old refresh token is a grant that works
// for eight hours and then cannot be renewed, which presents to a person as a
// connection that breaks overnight for no reason.
type TokenSet struct {
	AccessToken           string
	RefreshToken          string
	ExpiresAt             time.Time
	RefreshTokenExpiresAt time.Time
}

// User reads the login and id behind a user token.
func (c *Client) User(ctx context.Context, userToken string) (User, error) {
	var out User
	if _, err := c.call(ctx, http.MethodGet, "/user", userToken, &out); err != nil {
		return User{}, reauthorizeOn401(err)
	}
	return out, nil
}

// UserInstallations lists the installations this person can reach.
//
// Read LIVE on every call rather than from the grant's stored ids, and that is
// the design: somebody adding the app to another organisation changes this
// answer with nothing to tell the cluster, so a picker driven by a stored list
// would keep offering yesterday's -- the one failure a picker cannot survive.
// The stored ids are a display cache the caller refreshes FROM this.
func (c *Client) UserInstallations(ctx context.Context, userToken string) ([]Installation, error) {
	var payload struct {
		TotalCount    int            `json:"total_count"`
		Installations []Installation `json:"installations"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/user/installations?per_page="+strconv.Itoa(RepositoriesPerPage), userToken, &payload); err != nil {
		return nil, reauthorizeOn401(err)
	}
	return payload.Installations, nil
}

// InstallationRepositories lists one installation's repositories for this
// person, one page at a time, and reports the total so the caller can decide
// whether another page exists.
//
// Page-number driven rather than Link-header driven because the capability
// takes a page argument: the surface's paging and this one's must be the same
// number, or "next page" means two different things on the two sides.
func (c *Client) InstallationRepositories(ctx context.Context, userToken string, installationId int64, page int) ([]Repository, int, error) {
	if page < 1 {
		page = 1
	}
	endpoint := "/user/installations/" + strconv.FormatInt(installationId, 10) +
		"/repositories?per_page=" + strconv.Itoa(RepositoriesPerPage) + "&page=" + strconv.Itoa(page)
	var payload struct {
		TotalCount   int          `json:"total_count"`
		Repositories []Repository `json:"repositories"`
	}
	if _, err := c.call(ctx, http.MethodGet, endpoint, userToken, &payload); err != nil {
		return nil, 0, reauthorizeOn401(err)
	}
	for i := range payload.Repositories {
		payload.Repositories[i].InstallationId = installationId
	}
	return payload.Repositories, payload.TotalCount, nil
}

// PendingInstallationRequests lists installations awaiting an organisation
// owner's approval, as the APP sees them.
//
// It is asked under the app JWT because that is the only credential GitHub
// answers it for -- a person's own token cannot see a request they made that
// somebody else must approve. The caller matches on the REQUESTER's login to
// find the ones that belong to the person asking.
func (c *Client) PendingInstallationRequests(ctx context.Context) ([]InstallationRequest, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	assertion, err := c.appJWT(c.now())
	if err != nil {
		return nil, err
	}
	var out []InstallationRequest
	if _, cerr := c.call(ctx, http.MethodGet, "/app/installation-requests?per_page="+strconv.Itoa(RepositoriesPerPage), assertion, &out); cerr != nil {
		return nil, cerr
	}
	return out, nil
}

// Branches lists a repository's branch names under whatever bearer is handed
// in -- a user token for a probe, an installation token for anything the app
// does on its own account.
func (c *Client) Branches(ctx context.Context, bearer, owner, repo string) ([]string, error) {
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/branches?per_page=" + strconv.Itoa(RepositoriesPerPage)
	var payload []struct {
		Name string `json:"name"`
	}
	if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &payload); err != nil {
		return nil, reauthorizeOn401(err)
	}
	out := make([]string, 0, len(payload))
	for _, b := range payload {
		if name := strings.TrimSpace(b.Name); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// FileContents reads one file through the contents API.
//
// The base64 `content` field rather than Accept: application/vnd.github.raw,
// because the raw media type answers a REDIRECT for anything large and a
// redirect carries the Authorization header onward to a storage host. The
// base64 body is bounded by the same maxBody every other reply is.
func (c *Client) FileContents(ctx context.Context, bearer, owner, repo, ref, filePath string) ([]byte, error) {
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + escapePath(filePath)
	if r := strings.TrimSpace(ref); r != "" {
		endpoint += "?ref=" + url.QueryEscape(r)
	}
	var payload struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &payload); err != nil {
		return nil, reauthorizeOn401(err)
	}
	if payload.Type != "file" {
		return nil, errors.New("that path is not a file")
	}
	return decodeContent(payload.Encoding, payload.Content)
}

// DirectoryNames lists the sub-DIRECTORY names directly under one path. It is
// how the probe reads a package's DSL domains without fetching the tree: a
// dsl/<domain>/ directory is the whole declaration (analyze.go's DslRoot), so
// the names ARE the answer.
func (c *Client) DirectoryNames(ctx context.Context, bearer, owner, repo, ref, dirPath string) ([]string, error) {
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + escapePath(dirPath)
	if r := strings.TrimSpace(ref); r != "" {
		endpoint += "?ref=" + url.QueryEscape(r)
	}
	var payload []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if _, err := c.call(ctx, http.MethodGet, endpoint, bearer, &payload); err != nil {
		return nil, reauthorizeOn401(err)
	}
	out := make([]string, 0, len(payload))
	for _, e := range payload {
		if e.Type == "dir" && strings.TrimSpace(e.Name) != "" {
			out = append(out, e.Name)
		}
	}
	return out, nil
}

// RefreshUserToken exchanges a refresh token for a new pair.
//
// This is the one call whose FAILURE is a different fact from every other
// failure in this package: GitHub answering `error: bad_refresh_token` means
// the authorization is over, and the repair is a person reconnecting rather
// than anything an operator or a retry can do. So it is mapped to
// ErrReauthorize by name, and it is the reason that sentinel exists.
//
// GitHub answers this endpoint with HTTP 200 AND an error object, which is why
// the body is read on success rather than only on a status.
func (c *Client) RefreshUserToken(ctx context.Context, refreshToken string) (TokenSet, error) {
	if !c.Configured() {
		return TokenSet{}, ErrNotConfigured
	}
	if strings.TrimSpace(refreshToken) == "" {
		// No refresh token at all is the same position as a spent one: the
		// user token cannot be renewed and the person reconnects. Naming it
		// ErrReauthorize rather than an internal error is what puts the
		// repair in front of them instead of in a log.
		return TokenSet{}, ErrReauthorize
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.cfg.ClientId},
		"client_secret": {c.cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.oauthBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, derr := c.http.Do(req)
	if derr != nil {
		return TokenSet{}, derr
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest {
			return TokenSet{}, ErrReauthorize
		}
		return TokenSet{}, &StatusError{Status: resp.StatusCode, Endpoint: "/login/oauth/access_token"}
	}
	var payload struct {
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
	}
	if jerr := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&payload); jerr != nil {
		return TokenSet{}, jerr
	}
	if strings.TrimSpace(payload.Error) != "" || strings.TrimSpace(payload.AccessToken) == "" {
		return TokenSet{}, ErrReauthorize
	}
	now := c.now()
	set := TokenSet{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
	}
	if payload.ExpiresIn > 0 {
		set.ExpiresAt = now.Add(time.Duration(payload.ExpiresIn) * time.Second).UTC()
	}
	if payload.RefreshTokenExpiresIn > 0 {
		set.RefreshTokenExpiresAt = now.Add(time.Duration(payload.RefreshTokenExpiresIn) * time.Second).UTC()
	}
	return set, nil
}

// RevokeGrant tells GitHub the person disconnected.
//
// Basic auth with the OAuth pair, because this endpoint authenticates the APP
// rather than the person -- it is the app saying "I no longer hold this
// authorization". A 404 is SUCCESS: it means no such grant exists, which is
// the state the call was trying to reach.
func (c *Client) RevokeGrant(ctx context.Context, userToken string) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	body, merr := json.Marshal(map[string]string{"access_token": userToken})
	if merr != nil {
		return merr
	}
	endpoint := "/applications/" + url.PathEscape(c.cfg.ClientId) + "/grant"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiBase+endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.SetBasicAuth(c.cfg.ClientId, c.cfg.ClientSecret)

	resp, derr := c.http.Do(req)
	if derr != nil {
		return derr
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode < 300:
		return nil
	}
	return &StatusError{Status: resp.StatusCode, Endpoint: pathOnly(endpoint)}
}

// reauthorizeOn401 lifts a 401 into ErrReauthorize.
//
// A 401 under a USER TOKEN is GitHub refusing the authorization itself -- the
// token is dead and the refresh already happened or already failed -- and it
// must never reach a caller as an ordinary status, because the caller's only
// honest reading of a bare 401 is "this credential cannot see it", which sends
// somebody looking for a permission problem that does not exist. Every other
// status passes through untouched.
func reauthorizeOn401(err error) error {
	if StatusOf(err) == http.StatusUnauthorized {
		return ErrReauthorize
	}
	return err
}

// decodeContent turns the contents API's base64 body into bytes. GitHub
// wraps it at 60 columns, which base64.StdEncoding will not accept, so the
// newlines come out first.
func decodeContent(encoding, content string) ([]byte, error) {
	if encoding != "" && encoding != "base64" {
		return nil, errors.New("GitHub returned file content in an encoding this cluster does not read")
	}
	cleaned := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(content)
	return base64.StdEncoding.DecodeString(cleaned)
}

// escapePath escapes each segment of a repository path, leaving the separators
// alone: url.PathEscape would turn "dsl/acme" into one escaped segment and ask
// GitHub for a file with a slash in its name.
func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(p), "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
