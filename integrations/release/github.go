package release

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// github.go -- the GitHub REST client, shaped like integrations/shopify/admin.go:
// one struct holding a base URL and an *http.Client, methods returning decoded
// values or typed refusals, and no global state.
//
// THE BASE URL IS A FIELD SO THE TESTS CAN POINT IT AT A FAKE, and that is the
// whole test strategy for this package: CI never touches real GitHub, never
// creates a tag, and never publishes anything. A live-GitHub test would create
// real releases of a real product every time it ran, which is not a test, and
// mocking the client interface instead would test the mock -- the parsing,
// status-code mapping and half-done sequencing all live between the call and
// the decode, and a mock replaces exactly that.
//
// STATUS-CODE MAPPING IS THE INTERESTING PART, so it lives in one place
// (classify). 401/403 both mean the token cannot do this, which from an
// operator's chair is one action -- mint and seed a better token -- so they
// share one code. 5xx and transport failures share github_unreachable, whose
// contract is "nothing was created"; that guarantee is what lets a caller
// retry a cut without checking first.

// defaultAPIBase is api.github.com. A field on the client rather than a
// constant used inline, so a GitHub Enterprise host is a value change.
const defaultAPIBase = "https://api.github.com"

// requestTimeout bounds one call. Generous by API standards and deliberately
// so: creating a Release with generate_release_notes asks GitHub to walk the
// commits since the previous tag, which on a long gap is slow, and a timeout
// that fired there would produce the half-done state for a Release that then
// published anyway.
const requestTimeout = 60 * time.Second

// Client talks to GitHub's REST API.
type Client struct {
	base string
	http *http.Client
}

// NewClient builds a client against api.github.com.
func NewClient() *Client {
	return &Client{base: defaultAPIBase, http: &http.Client{Timeout: requestTimeout}}
}

// WithBaseURL points the client at another host. Used by the tests and by any
// future GitHub Enterprise deployment.
func (c *Client) WithBaseURL(base string) *Client {
	c.base = strings.TrimSuffix(base, "/")
	return c
}

// do performs one authenticated request and returns the status and body.
//
// The token is set as a header and NEVER logged, echoed into an error, or
// placed in a URL. Every refusal built from here names the VARIABLE instead.
func (c *Client) do(ctx context.Context, token, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, refuse(CodeGithubUnreachable, "could not build the GitHub request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure created nothing, which is the
		// github_unreachable contract.
		return 0, nil, refuse(CodeGithubUnreachable, "GitHub could not be reached: %v", err)
	}
	defer resp.Body.Close()
	// Bounded read. A body is an error message or a release object; an
	// unbounded read here would let a misconfigured proxy return a stream
	// into memory.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, refuse(CodeGithubUnreachable, "GitHub's reply could not be read: %v", err)
	}
	return resp.StatusCode, data, nil
}

// classify maps a status code to a typed refusal, or nil when the call
// succeeded.
//
// `what` names the operation in the operator's words ("list the repository's
// tags"), because a status code alone tells them nothing about which of the
// four calls a cut makes went wrong.
func classify(status int, body []byte, what string) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return refuse(CodeCredentialUnavailable,
			"GitHub refused the credential when asked to %s (HTTP %d). The token in %s is missing, expired, or lacks Contents: read/write on this repository.",
			what, status, SecretName)
	case status == http.StatusNotFound:
		// 404 on a private repository is what GitHub returns for a token
		// that cannot SEE it, which is indistinguishable from a wrong
		// name -- so the message names both possibilities rather than
		// asserting either.
		return refuse(CodeReleaseRepoUnconfigured,
			"GitHub has no such repository, or the credential cannot see it, when asked to %s. Check %s, and that the token in %s is scoped to that repository.",
			what, RepoVariableName, SecretName)
	case status >= 500:
		return refuse(CodeGithubUnreachable, "GitHub returned HTTP %d when asked to %s; nothing was created.", status, what)
	default:
		return refuse(CodeGithubUnreachable, "GitHub returned HTTP %d when asked to %s: %s", status, what, firstLine(body))
	}
}

// maxRemoteMessage bounds how much of GitHub's reply is carried forward.
//
// This text ends up on a v1:cluster:releaseCut row and on an operator's
// screen, and it is REMOTE: this package does not author it and cannot
// promise anything about its length or its encoding.
const maxRemoteMessage = 200

// firstLine renders a GitHub error body compactly. GitHub replies
// {"message": "...", "errors": [...]}; the message is the actionable half.
//
// BOUNDED AND UTF-8 SANITISED, and both halves were defects rather than
// hypotheticals -- measured, not reasoned:
//
//	The decoded `message` path had NO cap at all: a 5000-character message
//	came back at 5000 characters and went straight onto a graph row. Only the
//	raw-body fallback was capped, which is the path GitHub almost never takes.
//
//	The cap it did have sliced BYTES. `s[:200]` on text whose 200th byte sits
//	mid-rune produces invalid UTF-8, and structpb.NewStruct REFUSES it
//	("proto: invalid UTF-8 in string") -- so the capability's own result would
//	fail to encode and the caller would get an encoding error instead of the
//	GitHub failure that caused it. On the failure path, where the operator is
//	already dealing with something going wrong and would read the second error
//	as part of the first.
//
// So: truncate on a RUNE boundary, and pass whatever survives through
// ToValidUTF8, because a remote body can carry invalid bytes without any
// truncation on our part.
func firstLine(body []byte) string {
	var decoded struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Message != "" {
		return clampMessage(decoded.Message)
	}
	return clampMessage(strings.TrimSpace(string(body)))
}

// clampMessage bounds a remote string to maxRemoteMessage RUNES and guarantees
// the result is valid UTF-8.
func clampMessage(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if utf8.RuneCountInString(s) <= maxRemoteMessage {
		return s
	}
	runes := 0
	for idx := range s {
		if runes == maxRemoteMessage {
			return s[:idx] + "..."
		}
		runes++
	}
	return s
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// MainHeadSha returns the sha at the head of the default branch's `main`.
//
// The tag targets THIS, read at call time, and the row records it. A version
// number does not say what shipped; a sha does, and the tag it names can be
// deleted afterwards while the row keeps the answer.
func (c *Client) MainHeadSha(ctx context.Context, token string, repo repoRef) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits/main",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	status, body, err := c.do(ctx, token, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	if err := classify(status, body, "read the head of main"); err != nil {
		return "", err
	}
	var decoded struct {
		Sha string `json:"sha"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Sha == "" {
		return "", refuse(CodeGithubUnreachable, "GitHub's reply did not carry a commit sha for main.")
	}
	return decoded.Sha, nil
}

// tagsAtSha returns the release tags already pointing at one commit.
//
// This is what already_released_at_head is decided on. It walks the tag list
// rather than asking GitHub per-tag, because the tag list has already been
// fetched by the time this question is asked and a second round trip per tag
// would make a hundred-tag repository a hundred calls.
func tagsAtSha(tags []tagRef, sha string) []string {
	var hits []string
	for _, t := range tags {
		if t.Sha == sha {
			if _, ok := parseReleaseTag(t.Name); ok {
				hits = append(hits, t.Name)
			}
		}
	}
	return hits
}

// tagRef is a tag name and the commit it points at.
type tagRef struct {
	Name string
	Sha  string
}

// tagNames projects the names, which is all the version arithmetic needs.
func tagNames(tags []tagRef) []string {
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	return names
}

// maxTagPages bounds the tag walk at 10 000 tags.
//
// A BOUND RATHER THAN A FULL WALK, and it is stated rather than silent: a
// repository past it would have its oldest tags unread, which cannot change
// the MAXIMUM (the newest release is not in the tail) but would change whether
// tagsAtSha sees an old tag on today's head. The trade is deliberate -- an
// unbounded walk against a repository with a pathological tag count is a
// request that never returns.
const maxTagPages = 100

// ListTagRefs returns every tag on the repository with the commit it points at.
//
// PAGINATED, AND THE PAGINATION IS LOAD-BEARING RATHER THAN TIDY. GitHub
// returns 30 refs per page by default and 100 at most. A repository past its
// hundredth tag would, with a single-page read, silently hide its newest
// releases behind older ones and compute the "next" version from a tag that
// was superseded months ago -- which cuts a version that already exists and
// surfaces as ref_exists with no explanation.
//
// ONE WALK SERVES BOTH QUESTIONS. The version arithmetic wants names and the
// head check wants shas; fetching them separately would double the round trips
// and let the two answers disagree about a tag created between the calls.
func (c *Client) ListTagRefs(ctx context.Context, token string, repo repoRef) ([]tagRef, error) {
	var all []tagRef
	for page := 1; page <= maxTagPages; page++ {
		path := fmt.Sprintf("/repos/%s/%s/tags?per_page=100&page=%d",
			url.PathEscape(repo.Owner), url.PathEscape(repo.Name), page)
		status, body, err := c.do(ctx, token, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if err := classify(status, body, "list the repository's tags"); err != nil {
			return nil, err
		}
		var decoded []struct {
			Name   string `json:"name"`
			Commit struct {
				Sha string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, refuse(CodeGithubUnreachable, "GitHub's tag list could not be read: %v", err)
		}
		for _, t := range decoded {
			all = append(all, tagRef{Name: t.Name, Sha: t.Commit.Sha})
		}
		if len(decoded) < 100 {
			break
		}
	}
	return all, nil
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// CreateTagRef creates refs/tags/<tag> pointing at sha.
//
// THIS IS THE CONCURRENCY GATE FOR THE WHOLE FEATURE. GitHub's ref-create is
// atomic and rejects a ref that exists with 422, so two owners cutting at the
// same moment produce one tag and one ref_exists -- no advisory lock, no
// leader election, no window. That is why the tag is created BEFORE the
// Release rather than after: the Release API has no such guarantee, and
// reversing the order would open exactly the race this closes.
func (c *Client) CreateTagRef(ctx context.Context, token string, repo repoRef, tag, sha string) error {
	path := fmt.Sprintf("/repos/%s/%s/git/refs", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	body := map[string]string{"ref": "refs/tags/" + tag, "sha": sha}
	status, respBody, err := c.do(ctx, token, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status == http.StatusUnprocessableEntity {
		// 422 here is "Reference already exists". GitHub uses 422 for
		// other validation failures too, so the message is included --
		// but the code is ref_exists because that is what it means in
		// practice on a ref-create with a well-formed name and sha, and
		// the operator's next step (someone else cut it; re-read the
		// list) is the same either way.
		return refuse(CodeRefExists,
			"the tag %s already exists on the repository. Another cut may have won a race, or the version was cut by hand. GitHub said: %s",
			tag, firstLine(respBody))
	}
	return classify(status, respBody, "create the release tag")
}

// Release is the published GitHub Release.
type Release struct {
	HTMLURL string `json:"html_url"`
	TagName string `json:"tag_name"`
}

// CreateRelease publishes a Release for an existing tag.
//
// PUBLISHED, NOT A DRAFT, and that is the entire point of the call: only
// `release: published` fires dispatch-engine-images-on-release.yml, so a draft
// would create a Release that builds nothing while looking exactly like
// success.
//
// generate_release_notes asks GitHub to write the body from the commits and
// PRs since the previous tag. `notes` is PREPENDED rather than replacing it,
// so an operator's sentence about why they cut sits above the generated list
// instead of erasing it.
func (c *Client) CreateRelease(ctx context.Context, token string, repo repoRef, tag, notes string) (Release, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	body := map[string]any{
		"tag_name":               tag,
		"name":                   tag,
		"draft":                  false,
		"prerelease":             false,
		"generate_release_notes": true,
	}
	if strings.TrimSpace(notes) != "" {
		body["body"] = strings.TrimSpace(notes)
	}
	status, respBody, err := c.do(ctx, token, http.MethodPost, path, body)
	if err != nil {
		// The tag exists at this point -- the caller turns this into
		// the half-done refusal, which is why this returns the raw
		// error rather than deciding.
		return Release{}, err
	}
	if err := classify(status, respBody, "publish the GitHub Release"); err != nil {
		return Release{}, err
	}
	var decoded Release
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return Release{}, refuse(CodeGithubUnreachable, "GitHub's release reply could not be read: %v", err)
	}
	return decoded, nil
}
