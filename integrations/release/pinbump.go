package release

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// pinbump.go -- the one follow-on a cut can open, as a PULL REQUEST and never
// a push.
//
// WHY A PR AND NOT A COMMIT. `main` refuses direct pushes -- a repository
// ruleset, not a convention -- so a commit is not available even to a token
// that holds Contents: write. That is a good constraint rather than an
// obstacle: the pin is the tag every fresh install checks out, and a value
// that important passing through review and the merge queue like any other
// change is the correct shape. The runbook says so, so nobody later "fixes"
// this into a direct commit.
//
// WHY IT CANNOT FAIL THE CUT. By the time this runs the Release is published
// and the image build is running. The cut succeeded. A token scoped for
// cutting alone (Contents: read/write, which is all the cut needs) legitimately
// cannot open a PR, and reporting a shipped release as a failure because a
// follow-on could not open would be actively harmful -- it invites the operator
// to cut again. So every failure below returns a NOTE, which lands on the row
// and renders on the card, and the outcome stays a success.
//
// ===========================================================================
// THE REWRITE IS ANCHORED ON THE DECLARATION, NEVER ON PROSE AROUND IT
// ===========================================================================
// stackPin.ts is one of the most heavily commented files in the extension --
// the constant sits under a doc block and a long postmortem, and both get
// rewritten as the pin's role changes (memql#4429 narrowed it to offline
// fallback while this was being written). A rewriter that matched surrounding
// comment text would break on a prose edit that changed nothing about the
// constant. So the pattern matches the DECLARATION and nothing else, and it is
// anchored to the start of a line so a mention inside a comment cannot be hit.
//
// It also refuses to write when it does not find EXACTLY ONE match. Zero means
// the file moved or the constant was renamed; two means something is ambiguous.
// Either way the honest answer is a note saying so, not a guess at which
// occurrence was meant.

// pinFile is the file the PR edits.
const pinFile = "editors/vscode/src/install/stackPin.ts"

// pinDeclRe matches the DEFAULT_STACK_TAG declaration line only.
//
// `(?m)` plus `^` anchors it to a line start, so `DEFAULT_STACK_TAG` appearing
// inside a comment or another expression cannot match. The capture groups let
// the replacement keep whatever quoting and spacing the file uses.
var pinDeclRe = regexp.MustCompile(`(?m)^(export const DEFAULT_STACK_TAG\s*=\s*)"[^"]*"(;?)$`)

// openPinBumpPR opens the follow-on PR, returning (url, note). Exactly one of
// the two is non-empty.
func (i *Integration) openPinBumpPR(ctx context.Context, cfg settings, v version) (string, string) {
	branch := "release/pin-" + v.tag()

	head, err := i.github.MainHeadSha(ctx, cfg.token, cfg.repo)
	if err != nil {
		return "", pinNote("could not read main's head to branch from", err)
	}
	content, sha, err := i.github.GetFile(ctx, cfg.token, cfg.repo, pinFile, head)
	if err != nil {
		return "", pinNote("could not read "+pinFile, err)
	}

	updated, ok := rewritePin(content, v.tag())
	if !ok {
		return "", fmt.Sprintf(
			"the pin was left alone: %s does not contain exactly one `export const DEFAULT_STACK_TAG = \"...\"` line. Bump it by hand, or fix this rewriter if the constant moved.",
			pinFile)
	}
	if updated == content {
		// Already at this tag. Not a failure and not worth a PR --
		// which happens when a cut is retried after a PR already
		// landed.
		return "", fmt.Sprintf("the pin in %s was already %s, so no pull request was needed.", pinFile, v.tag())
	}

	if err := i.github.CreateBranch(ctx, cfg.token, cfg.repo, branch, head); err != nil {
		return "", pinNote("could not create the branch "+branch, err)
	}
	if err := i.github.PutFile(ctx, cfg.token, cfg.repo, pinFile, branch, sha, updated,
		"chore(vscode): pin DEFAULT_STACK_TAG to "+v.tag()); err != nil {
		return "", pinNote("could not commit the pin bump", err)
	}
	prURL, err := i.github.CreatePullRequest(ctx, cfg.token, cfg.repo,
		"chore(vscode): pin DEFAULT_STACK_TAG to "+v.tag(),
		branch, "main", pinBody(v, cfg.repo))
	if err != nil {
		return "", pinNote("the branch and commit landed but the pull request could not be opened", err)
	}
	return prURL, ""
}

// rewritePin replaces the pinned tag, reporting false when the file does not
// carry exactly one declaration.
func rewritePin(content, tag string) (string, bool) {
	if len(pinDeclRe.FindAllStringIndex(content, -1)) != 1 {
		return "", false
	}
	return pinDeclRe.ReplaceAllString(content, `${1}"`+tag+`"${2}`), true
}

// pinNote renders a failure as a row note. It never carries a credential --
// the underlying refusals name the variable rather than the value, and this
// only forwards their message.
func pinNote(what string, err error) string {
	return fmt.Sprintf("the release is published; the pin-bump pull request was not opened because it %s: %s. Bump %s by hand when convenient.",
		what, describeRefusal(err), pinFile)
}

// pinBody is the PR description. It LINKS THE RELEASE rather than restating
// it, so a reviewer lands on the thing that motivated the change.
func pinBody(v version, repo repoRef) string {
	return strings.Join([]string{
		"Bumps the VS Code extension's `DEFAULT_STACK_TAG` to `" + v.tag() + "`.",
		"",
		"Opened automatically by the release cut of `" + v.tag() + "`:",
		"https://github.com/" + repo.String() + "/releases/tag/" + v.tag(),
		"",
		"`DEFAULT_STACK_TAG` is the release an install checks out when it is not told otherwise.",
		"Keeping it current is a release step; this pull request is that step, as a reviewed diff",
		"rather than a push, because `main` takes neither.",
	}, "\n")
}

// ---------------------------------------------------------------------------
// The GitHub calls only the pin bump needs
// ---------------------------------------------------------------------------

// GetFile reads one file at a ref, returning its decoded content and its blob
// sha.
//
// THE BLOB SHA IS THE POINT of returning two values. GitHub's contents API
// requires the sha of the blob being replaced on an update, and that is an
// optimistic-concurrency check: if somebody else edits stackPin.ts between the
// read and the write, the write is refused rather than silently clobbering
// them.
func (c *Client) GetFile(ctx context.Context, token string, repo repoRef, path, ref string) (string, string, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), path, url.QueryEscape(ref))
	status, body, err := c.do(ctx, token, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	if err := classify(status, body, "read "+path); err != nil {
		return "", "", err
	}
	var decoded struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Sha      string `json:"sha"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", refuse(CodeGithubUnreachable, "GitHub's file reply could not be read: %v", err)
	}
	if decoded.Encoding != "base64" {
		return "", "", refuse(CodeGithubUnreachable,
			"GitHub returned %s in %q encoding; only base64 is handled.", path, decoded.Encoding)
	}
	// GitHub wraps base64 at 60 columns, which the strict decoder rejects.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(decoded.Content, "\n", ""))
	if err != nil {
		return "", "", refuse(CodeGithubUnreachable, "GitHub's file content could not be decoded: %v", err)
	}
	return string(raw), decoded.Sha, nil
}

// CreateBranch creates refs/heads/<branch> at sha.
func (c *Client) CreateBranch(ctx context.Context, token string, repo repoRef, branch, sha string) error {
	endpoint := fmt.Sprintf("/repos/%s/%s/git/refs", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	status, body, err := c.do(ctx, token, http.MethodPost, endpoint,
		map[string]string{"ref": "refs/heads/" + branch, "sha": sha})
	if err != nil {
		return err
	}
	if status == http.StatusUnprocessableEntity {
		return refuse(CodeRefExists, "the branch %s already exists: %s", branch, firstLine(body))
	}
	return classify(status, body, "create the branch "+branch)
}

// PutFile commits one file onto a branch.
func (c *Client) PutFile(ctx context.Context, token string, repo repoRef, path, branch, sha, content, message string) error {
	endpoint := fmt.Sprintf("/repos/%s/%s/contents/%s",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), path)
	status, body, err := c.do(ctx, token, http.MethodPut, endpoint, map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"sha":     sha,
		"branch":  branch,
	})
	if err != nil {
		return err
	}
	return classify(status, body, "commit "+path)
}

// CreatePullRequest opens the PR and returns its html_url.
func (c *Client) CreatePullRequest(ctx context.Context, token string, repo repoRef, title, head, base, body string) (string, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	status, respBody, err := c.do(ctx, token, http.MethodPost, endpoint, map[string]any{
		"title": title, "head": head, "base": base, "body": body,
	})
	if err != nil {
		return "", err
	}
	if err := classify(status, respBody, "open the pull request"); err != nil {
		return "", err
	}
	var decoded struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", refuse(CodeGithubUnreachable, "GitHub's pull-request reply could not be read: %v", err)
	}
	return decoded.HTMLURL, nil
}
