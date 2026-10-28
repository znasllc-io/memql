package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// train.go -- the GitHub calls a release train makes as the MemQL app
// (design record docs/superpowers/specs/2026-10-27-release-trains-design.md,
// sections 4.3 and 7): build a train branch from main and the boarded pull
// requests on GitHub's side, open the train pull request, read the owner's
// reviews, land it, and talk to the pull requests it carried.
//
// Every call takes the installation token as bearer and the repository as
// owner and name, like the rest of this package. Nothing here decides: the
// coordinator in component/pipelinerun decides, through component/pipelines.

// trainBranchRe bounds the branch names these calls write. A train only ever
// writes its own train/<date>-<n> branches, but the check is on the name's
// shape, so a ".." or a space can never reach a ref path.
var trainBranchRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

func checkBranchName(branch string) error {
	if !trainBranchRe.MatchString(branch) || strings.Contains(branch, "..") {
		return fmt.Errorf("githubapp: %q is not a branch name a train writes", branch)
	}
	return nil
}

// TrainPullRequest is an open pull request as a train's boarding reads it.
type TrainPullRequest struct {
	Number                                   int
	Title, HeadSHA, HeadRef, BaseRef, Author string
	// HeadRepository is the head's "owner/name", empty when GitHub reports it
	// null (a deleted fork): a head in another repository is a fork's.
	HeadRepository string
	URL            string
	Draft          bool
	Labels         []string
}

// TrainPullRequests lists the open pull requests into base, oldest first, one
// page of a hundred. A repository with more than a hundred open pull requests
// into one branch boards the oldest hundred; the rest board the next train.
func (c *Client) TrainPullRequests(ctx context.Context, bearer, owner, repo, base string) ([]TrainPullRequest, error) {
	q := url.Values{"state": {"open"}, "base": {base}, "per_page": {"100"}, "sort": {"created"}, "direction": {"asc"}}
	var wire []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/pulls?"+q.Encode(), bearer, &wire); err != nil {
		return nil, err
	}
	out := make([]TrainPullRequest, 0, len(wire))
	for _, w := range wire {
		pr := TrainPullRequest{Number: w.Number, Title: w.Title, HeadSHA: w.Head.SHA, HeadRef: w.Head.Ref, BaseRef: w.Base.Ref,
			Author: w.User.Login, URL: w.HTMLURL, Draft: w.Draft}
		if w.Head.Repo != nil {
			pr.HeadRepository = w.Head.Repo.FullName
		}
		for _, l := range w.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		out = append(out, pr)
	}
	return out, nil
}

// CheckRunConclusion reads the newest check run called name on sha, from the
// app appID when appID is positive: a check of the same name from another app
// is somebody else's verdict. No such check run answers two empty strings.
func (c *Client) CheckRunConclusion(ctx context.Context, bearer, owner, repo, sha, name string, appID int64) (status, conclusion string, err error) {
	q := url.Values{"check_name": {name}, "per_page": {"100"}}
	var wire struct {
		CheckRuns []struct {
			Name       string    `json:"name"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			StartedAt  time.Time `json:"started_at"`
			App        struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/commits/"+url.PathEscape(sha)+"/check-runs?"+q.Encode(), bearer, &wire); err != nil {
		return "", "", err
	}
	var newest time.Time
	found := false
	for _, r := range wire.CheckRuns {
		if r.Name != name || (appID > 0 && r.App.ID != appID) {
			continue
		}
		if !found || r.StartedAt.After(newest) {
			newest, status, conclusion, found = r.StartedAt, r.Status, r.Conclusion, true
		}
	}
	return status, conclusion, nil
}

// CreateBranchRef creates branch at sha.
func (c *Client) CreateBranchRef(ctx context.Context, bearer, owner, repo, branch, sha string) error {
	if err := checkBranchName(branch); err != nil {
		return err
	}
	in := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	_, err := c.callJSON(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/git/refs", bearer, in, nil)
	return err
}

// ResetBranchRef forces branch to sha: a train is rebuilt from main, so its
// branch moves backwards by design. Only train branches are ever passed here.
func (c *Client) ResetBranchRef(ctx context.Context, bearer, owner, repo, branch, sha string) error {
	if err := checkBranchName(branch); err != nil {
		return err
	}
	in := map[string]any{"sha": sha, "force": true}
	_, err := c.callJSON(ctx, http.MethodPatch, repoEndpoint(owner, repo)+"/git/refs/heads/"+branch, bearer, in, nil)
	return err
}

// DeleteBranchRef deletes branch.
func (c *Client) DeleteBranchRef(ctx context.Context, bearer, owner, repo, branch string) error {
	if err := checkBranchName(branch); err != nil {
		return err
	}
	_, err := c.call(ctx, http.MethodDelete, repoEndpoint(owner, repo)+"/git/refs/heads/"+branch, bearer, nil)
	return err
}

// MergeOutcome is what GitHub's branch merge did: a merge commit (SHA),
// nothing because the head was already in the branch, or a conflict.
type MergeOutcome struct {
	SHA            string
	Conflict       bool
	NothingToMerge bool
}

// MergeIntoBranch merges headSHA into base on GitHub's side (POST /merges).
// A conflict is an answer, not an error: the train leaves that pull request
// off and says why. 204 means the head was already in base, which is not a
// conflict either.
func (c *Client) MergeIntoBranch(ctx context.Context, bearer, owner, repo, base, headSHA, message string) (MergeOutcome, error) {
	if err := checkBranchName(base); err != nil {
		return MergeOutcome{}, err
	}
	in := map[string]string{"base": base, "head": headSHA, "commit_message": message}
	var out struct {
		SHA string `json:"sha"`
	}
	status, err := c.callAllowingEmpty(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/merges", bearer, in, &out)
	switch {
	case StatusOf(err) == http.StatusConflict:
		return MergeOutcome{Conflict: true}, nil
	case err != nil:
		return MergeOutcome{}, err
	case status == http.StatusNoContent:
		return MergeOutcome{NothingToMerge: true}, nil
	}
	return MergeOutcome{SHA: out.SHA}, nil
}

// PutBranchFile writes content at filePath on branch and returns the new
// commit's SHA. It sends the SHA of the blob it replaces, so a file another
// writer changed in between is refused rather than overwritten.
func (c *Client) PutBranchFile(ctx context.Context, bearer, owner, repo, branch, filePath, message string, content []byte) (string, error) {
	if err := checkBranchName(branch); err != nil {
		return "", err
	}
	endpoint := repoEndpoint(owner, repo) + "/contents/" + escapeFilePath(filePath)
	var current struct {
		SHA string `json:"sha"`
	}
	_, err := c.call(ctx, http.MethodGet, endpoint+"?"+url.Values{"ref": {branch}}.Encode(), bearer, &current)
	if err != nil && StatusOf(err) != http.StatusNotFound {
		return "", err
	}
	in := map[string]string{"message": message, "content": base64.StdEncoding.EncodeToString(content), "branch": branch}
	if current.SHA != "" {
		in["sha"] = current.SHA
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := c.callJSON(ctx, http.MethodPut, endpoint, bearer, in, &out); err != nil {
		return "", err
	}
	return out.Commit.SHA, nil
}

// CreatePullRequest opens a ready pull request (never a draft: a train
// pull request is ready for the owner the moment its run passes) and returns
// its number and page.
func (c *Client) CreatePullRequest(ctx context.Context, bearer, owner, repo, head, base, title, body string) (int, string, error) {
	in := map[string]any{"title": title, "head": head, "base": base, "body": body, "draft": false}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if _, err := c.callJSON(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/pulls", bearer, in, &out); err != nil {
		return 0, "", err
	}
	return out.Number, out.HTMLURL, nil
}

// EditPullRequestBody replaces a pull request's description.
func (c *Client) EditPullRequestBody(ctx context.Context, bearer, owner, repo string, number int, body string) error {
	_, err := c.callJSON(ctx, http.MethodPatch, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number), bearer, map[string]string{"body": body}, nil)
	return err
}

// PullRequestReview is one review as GitHub reports it.
type PullRequestReview struct {
	Login, State, CommitID string
	SubmittedAt            time.Time
}

// PullRequestReviews lists a pull request's reviews, oldest first, one page
// of a hundred: a train pull request lives for hours and collects a handful.
func (c *Client) PullRequestReviews(ctx context.Context, bearer, owner, repo string, number int) ([]PullRequestReview, error) {
	var wire []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State       string    `json:"state"`
		CommitID    string    `json:"commit_id"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number)+"/reviews?per_page=100", bearer, &wire); err != nil {
		return nil, err
	}
	out := make([]PullRequestReview, 0, len(wire))
	for _, w := range wire {
		out = append(out, PullRequestReview{Login: w.User.Login, State: w.State, CommitID: w.CommitID, SubmittedAt: w.SubmittedAt})
	}
	return out, nil
}

// trainFilePages bounds a pull request's file list at 300 paths. A larger
// pull request is reported incomplete, and the triage suspects it of every
// package-scoped failure rather than reading a partial list as innocence.
const trainFilePages = 3

// PullRequestFiles lists the paths a pull request changes, and whether the
// list is whole.
func (c *Client) PullRequestFiles(ctx context.Context, bearer, owner, repo string, number int) (files []string, complete bool, err error) {
	for page := 1; page <= trainFilePages; page++ {
		var wire []struct {
			Filename string `json:"filename"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number)+"/files?"+q.Encode(), bearer, &wire); err != nil {
			return nil, false, err
		}
		for _, w := range wire {
			files = append(files, w.Filename)
		}
		if len(wire) < 100 {
			return files, true, nil
		}
	}
	return files, false, nil
}

// MergePullRequest lands a pull request with a merge commit, only if its head
// is still headSHA: GitHub refuses (409) when the head moved after the owner
// approved, which is exactly the case that must not land.
func (c *Client) MergePullRequest(ctx context.Context, bearer, owner, repo string, number int, headSHA string) (string, error) {
	in := map[string]string{"sha": headSHA, "merge_method": "merge"}
	var out struct {
		SHA string `json:"sha"`
	}
	if _, err := c.callJSON(ctx, http.MethodPut, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number)+"/merge", bearer, in, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// CommentOnPullRequest adds a comment to a pull request's conversation.
func (c *Client) CommentOnPullRequest(ctx context.Context, bearer, owner, repo string, number int, body string) error {
	_, err := c.callJSON(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/issues/"+strconv.Itoa(number)+"/comments", bearer, map[string]string{"body": body}, nil)
	return err
}

// AddPullRequestLabels adds labels to a pull request, creating any the
// repository does not have yet.
func (c *Client) AddPullRequestLabels(ctx context.Context, bearer, owner, repo string, number int, labels []string) error {
	_, err := c.callJSON(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/issues/"+strconv.Itoa(number)+"/labels", bearer, map[string][]string{"labels": labels}, nil)
	return err
}

// PullRequestOpen reports whether a pull request is open, and whether it was
// merged.
func (c *Client) PullRequestOpen(ctx context.Context, bearer, owner, repo string, number int) (open, merged bool, err error) {
	var out struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/pulls/"+strconv.Itoa(number), bearer, &out); err != nil {
		return false, false, err
	}
	return out.State == "open", out.Merged, nil
}

// callAllowingEmpty is callJSON for the one write here whose success may have
// no body: GitHub answers a merge that had nothing to merge with 204, and do
// decodes every 2xx body, so an empty one would read as a failure. It keeps
// do's headers, status classification and reply bound.
func (c *Client) callAllowingEmpty(ctx context.Context, method, endpoint, bearer string, in, out any) (int, error) {
	if c == nil {
		return 0, ErrNotConfigured
	}
	body, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := c.newRequest(ctx, method, c.apiBase+endpoint, bearer, body)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, Endpoint: pathOnly(endpoint), RateLimited: rateLimited(resp)}
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return resp.StatusCode, fmt.Errorf("GitHub answered %s with a body this cluster could not read: %v", pathOnly(endpoint), err)
	}
	return resp.StatusCode, nil
}

// escapeFilePath escapes each segment of a repository path, keeping its
// slashes.
func escapeFilePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
