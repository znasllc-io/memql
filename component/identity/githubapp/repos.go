package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// repos.go -- the repository reads a pipeline makes (epic memql#5477), under
// whatever bearer it is handed: in practice the installation token a source
// owner's grant reaches, so background work never depends on a person being
// signed in. A 401 here is that token being refused and stays an ordinary
// StatusError; it is NOT lifted to ErrReauthorize, which is a fact about a
// person's authorization and not about an installation's.

// repoEndpoint is the API path of owner/repo.
func repoEndpoint(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// RepositoryInfo is what a pipeline needs to know about a repository.
type RepositoryInfo struct {
	FullName, DefaultBranch string
	Private                 bool
}

// Repository reads owner/repo (GET /repos/{owner}/{repo}).
func (c *Client) Repository(ctx context.Context, bearer, owner, repo string) (RepositoryInfo, error) {
	var payload struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo), bearer, &payload); err != nil {
		return RepositoryInfo{}, err
	}
	return RepositoryInfo{FullName: payload.FullName, DefaultBranch: payload.DefaultBranch, Private: payload.Private}, nil
}

// BranchHead answers the commit at the tip of branch, and its message
// (GET /repos/{owner}/{repo}/branches/{branch}). A branch name keeps its
// slashes; each segment is escaped on its own.
func (c *Client) BranchHead(ctx context.Context, bearer, owner, repo, branch string) (sha, message string, err error) {
	if strings.TrimSpace(branch) == "" {
		return "", "", errors.New("a branch head needs a branch name")
	}
	var payload struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/branches/"+escapePath(branch), bearer, &payload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.Commit.SHA) == "" {
		return "", "", errors.New("GitHub answered for that branch and named no commit")
	}
	return payload.Commit.SHA, payload.Commit.Commit.Message, nil
}

// PullRequestHead is one pull request's head, and where it came from.
type PullRequestHead struct {
	Number         int
	Title          string
	HeadSHA        string
	HeadRef        string
	HeadRepository string
	BaseSHA        string
}

// pullRequestPayload is one pull request as GitHub sends it, cut to what a
// PullRequestHead keeps. The list and the single read decode through it, so
// the two can never read one pull request differently.
type pullRequestPayload struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}

// head is the payload as a PullRequestHead. HeadRepository is the full name
// of the repository the head lives in, and EMPTY when GitHub reports that
// repository deleted -- the caller cannot tell such a head from a fork's, and
// must not read it as this repository's.
func (pr pullRequestPayload) head() PullRequestHead {
	head := PullRequestHead{Number: pr.Number, Title: pr.Title, HeadSHA: pr.Head.SHA, HeadRef: pr.Head.Ref, BaseSHA: pr.Base.SHA}
	if pr.Head.Repo != nil {
		head.HeadRepository = pr.Head.Repo.FullName
	}
	return head
}

// OpenPullRequests lists a repository's open pull requests: ONE page of a
// hundred, newest first, which is GitHub's maximum page. An older pull request
// in a busy repository is not on it; PullRequest reads any one by its number.
func (c *Client) OpenPullRequests(ctx context.Context, bearer, owner, repo string) ([]PullRequestHead, error) {
	var payload []pullRequestPayload
	endpoint := repoEndpoint(owner, repo) + "/pulls?state=open&per_page=" + strconv.Itoa(RepositoriesPerPage)
	if _, err := c.do(ctx, http.MethodGet, endpoint, bearer, nil, &payload, largeBody); err != nil {
		return nil, err
	}
	out := make([]PullRequestHead, 0, len(payload))
	for _, pr := range payload {
		out = append(out, pr.head())
	}
	return out, nil
}

// PullRequest reads one pull request's head as GitHub reports it NOW, whatever
// its age or state (GET /repos/{owner}/{repo}/pulls/{number}), decoded exactly
// as OpenPullRequests decodes each entry. A number GitHub does not know is its
// 404; a number of 0 or less is refused before anything is asked.
func (c *Client) PullRequest(ctx context.Context, bearer, owner, repo string, number int) (PullRequestHead, error) {
	if number <= 0 {
		return PullRequestHead{}, fmt.Errorf("a pull request needs a positive number, not %d", number)
	}
	var payload pullRequestPayload
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number), bearer, &payload); err != nil {
		return PullRequestHead{}, err
	}
	return payload.head(), nil
}

// compareFileLimit is where GitHub stops listing a compare's files.
const compareFileLimit = 300

// Compare lists the paths that differ between base and head
// (GET /repos/{owner}/{repo}/compare/{base}...{head}).
//
// A RENAMED file contributes both paths: the change left one package and
// joined another. COMPLETE IS FALSE at 300 files, where GitHub stops listing:
// the list is then a floor and not the set, and the caller selects
// everything.
func (c *Client) Compare(ctx context.Context, bearer, owner, repo, base, head string) (files []string, complete bool, err error) {
	if strings.TrimSpace(base) == "" || strings.TrimSpace(head) == "" {
		return nil, false, errors.New("a compare needs a base and a head")
	}
	var payload struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	endpoint := repoEndpoint(owner, repo) + "/compare/" + escapePath(base) + "..." + escapePath(head)
	if _, err := c.do(ctx, http.MethodGet, endpoint, bearer, nil, &payload, largeBody); err != nil {
		return nil, false, err
	}
	seen := make(map[string]bool, len(payload.Files))
	files = make([]string, 0, len(payload.Files))
	for _, f := range payload.Files {
		for _, name := range []string{f.Filename, f.PreviousFilename} {
			if name != "" && !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
		}
	}
	return files, len(payload.Files) < compareFileLimit, nil
}

// CommitForRef answers the commit a ref names, and its message
// (GET /repos/{owner}/{repo}/commits/{ref}). The ref is a commit SHA, a branch
// or tag name, or heads/<branch> and tags/<tag> -- what GitHub's commits
// endpoint accepts. A tag resolves to the commit it points at.
func (c *Client) CommitForRef(ctx context.Context, bearer, owner, repo, ref string) (sha, message string, err error) {
	if strings.TrimSpace(ref) == "" {
		return "", "", errors.New("a commit lookup needs a ref")
	}
	var payload struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if _, err := c.do(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/commits/"+escapePath(ref), bearer, nil, &payload, largeBody); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.SHA) == "" {
		return "", "", errors.New("GitHub answered for that ref and named no commit")
	}
	return payload.SHA, payload.Commit.Message, nil
}

// maxArchiveRedirects bounds the hops an archive request follows. GitHub
// answers one (the API to its download host), two for a renamed repository.
const maxArchiveRedirects = 5

// Tarball opens the gzipped archive of owner/repo at sha
// (GET /repos/{owner}/{repo}/tarball/{sha}). The caller reads and closes it,
// and bounds what it reads.
//
// GitHub answers with a redirect to its download host, and the redirect is
// followed HERE, by hand, for two reasons that are the same reason:
//
//   - THE BEARER STAYS ON THE API HOST. An installation token is a bearer for
//     every repository its installation covers; it is sent to the host it was
//     minted for and never to another, and once a hop has left that host it is
//     not sent again. The download URL carries its own short-lived token.
//   - NO ERROR NAMES THE DOWNLOAD URL. That token is in its query string, and
//     the standard client's errors render the URL they failed on. Every error
//     here names the API endpoint instead.
//
// A redirect from https to plain http is refused: the archive is the
// repository's source and the URL carries a token.
//
// NO CLIENT TIMEOUT applies to the download: it would bound reading the whole
// archive, and a large repository takes longer than an API call. The caller's
// context bounds it.
func (c *Client) Tarball(ctx context.Context, bearer, owner, repo, sha string) (io.ReadCloser, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(sha) == "" {
		return nil, errors.New("an archive needs a commit SHA")
	}
	endpoint := repoEndpoint(owner, repo) + "/tarball/" + escapePath(sha)
	origin, err := url.Parse(c.apiBase + endpoint)
	if err != nil {
		return nil, err
	}
	client := *c.http
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	target, authorized := origin, strings.TrimSpace(bearer) != ""
	for hop := 0; ; hop++ {
		if target.Scheme != origin.Scheme || !strings.EqualFold(target.Host, origin.Host) {
			authorized = false
		}
		withBearer := ""
		if authorized {
			withBearer = bearer
		}
		req, rerr := c.newRequest(ctx, http.MethodGet, target.String(), withBearer, nil)
		if rerr != nil {
			return nil, fmt.Errorf("GitHub's archive for %s could not be requested", pathOnly(endpoint))
		}
		resp, derr := client.Do(req)
		if derr != nil {
			var ue *url.Error
			if errors.As(derr, &ue) {
				derr = ue.Err
			}
			return nil, fmt.Errorf("GitHub's archive for %s could not be fetched: %w", pathOnly(endpoint), derr)
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := resp.Header.Get("Location")
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
			_ = resp.Body.Close()
			if location == "" || hop >= maxArchiveRedirects {
				return nil, &StatusError{Status: resp.StatusCode, Endpoint: pathOnly(endpoint)}
			}
			next, perr := target.Parse(location)
			if perr != nil || (next.Scheme != "https" && next.Scheme != "http") ||
				(origin.Scheme == "https" && next.Scheme != "https") {
				return nil, fmt.Errorf("GitHub redirected the archive for %s somewhere this cluster does not follow", pathOnly(endpoint))
			}
			target = next
			continue
		}
		if resp.StatusCode >= 300 {
			se := &StatusError{Status: resp.StatusCode, Endpoint: pathOnly(endpoint), RateLimited: rateLimited(resp)}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
			_ = resp.Body.Close()
			return nil, se
		}
		return resp.Body, nil
	}
}
