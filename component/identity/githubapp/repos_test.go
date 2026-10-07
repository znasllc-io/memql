package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// repos_test.go -- the repository reads a pipeline makes under an installation
// token (epic memql#5477): the repository and its default branch, a branch's
// head, the open pull requests, what changed between two commits, a ref's
// commit, and the archive at a SHA.

const headSHA = "0123456789abcdef0123456789abcdef01234567"

func TestRepositoryReadsTheDefaultBranchAndVisibility(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget", http.StatusOK,
		`{"full_name":"acme/widget","default_branch":"trunk","private":true,"owner":{"login":"acme"}}`)
	c := testClient(t, hub, &now)

	info, err := c.Repository(context.Background(), installToken, "acme", "widget")
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if info != (RepositoryInfo{FullName: "acme/widget", DefaultBranch: "trunk", Private: true}) {
		t.Fatalf("got %+v", info)
	}
	if got := hub.seen()[0].Header.Get("Authorization"); got != "Bearer "+installToken {
		t.Fatalf("the read must carry the bearer it was handed, got %q", got)
	}

	// Under an INSTALLATION token a 401 is that token being refused, not a
	// person's authorization: it stays an ordinary status for the caller to
	// classify, and is never lifted to ErrReauthorize.
	hub.json("/repos/acme/widget", http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	_, err = c.Repository(context.Background(), installToken, "acme", "widget")
	if StatusOf(err) != http.StatusUnauthorized || errors.Is(err, ErrReauthorize) {
		t.Fatalf("want a plain 401 StatusError, got %v", err)
	}
}

func TestBranchHeadAnswersTheCommitAndItsMessage(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget/branches/release/1.0", http.StatusOK,
		`{"name":"release/1.0","commit":{"sha":"`+headSHA+`","commit":{"message":"Cut 1.0\n\nThe release."}}}`)
	c := testClient(t, hub, &now)

	sha, message, err := c.BranchHead(context.Background(), installToken, "acme", "widget", "release/1.0")
	if err != nil {
		t.Fatalf("branch head: %v", err)
	}
	if sha != headSHA || message != "Cut 1.0\n\nThe release." {
		t.Fatalf("got %q / %q", sha, message)
	}
	// A branch GitHub knows nothing about is its 404, for the caller to read.
	if _, _, err := c.BranchHead(context.Background(), installToken, "acme", "widget", "gone"); StatusOf(err) != http.StatusNotFound {
		t.Fatalf("want a 404, got %v", err)
	}
	// A 200 naming no commit is not a head.
	hub.json("/repos/acme/widget/branches/empty", http.StatusOK, `{"name":"empty"}`)
	if _, _, err := c.BranchHead(context.Background(), installToken, "acme", "widget", "empty"); err == nil {
		t.Fatal("a branch answered with no commit must be an error, not an empty head")
	}
}

func TestOpenPullRequestsReadsEachHeadAndWhereItCameFrom(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget/pulls", http.StatusOK, `[
		{"number":7,"title":"Add widgets","body":"long prose","head":{"sha":"head7","ref":"feature","repo":{"full_name":"acme/widget"}},"base":{"sha":"base7","ref":"main","repo":{"full_name":"acme/widget"}}},
		{"number":8,"title":"From a fork","head":{"sha":"head8","ref":"patch-1","repo":{"full_name":"stranger/widget"}},"base":{"sha":"base8","ref":"main"}},
		{"number":9,"title":"Its fork was deleted","head":{"sha":"head9","ref":"gone","repo":null},"base":{"sha":"base9","ref":"main"}}
	]`)
	c := testClient(t, hub, &now)

	prs, err := c.OpenPullRequests(context.Background(), installToken, "acme", "widget")
	if err != nil {
		t.Fatalf("pulls: %v", err)
	}
	want := []PullRequestHead{
		{Number: 7, Title: "Add widgets", HeadSHA: "head7", HeadRef: "feature", HeadRepository: "acme/widget", BaseSHA: "base7"},
		{Number: 8, Title: "From a fork", HeadSHA: "head8", HeadRef: "patch-1", HeadRepository: "stranger/widget", BaseSHA: "base8"},
		// A DELETED head repository answers empty: the caller cannot tell it
		// from a fork, and must not read it as this repository.
		{Number: 9, Title: "Its fork was deleted", HeadSHA: "head9", HeadRef: "gone", HeadRepository: "", BaseSHA: "base9"},
	}
	if len(prs) != len(want) {
		t.Fatalf("want %d pull requests, got %+v", len(want), prs)
	}
	for n := range want {
		if prs[n] != want[n] {
			t.Errorf("pull request %d: got %+v, want %+v", n, prs[n], want[n])
		}
	}
	q := hub.seen()[0].URL.Query()
	if q.Get("state") != "open" || q.Get("per_page") != "100" {
		t.Fatalf("one page of open pull requests, got query %v", q)
	}
}

// One pull request, by its number, whatever its age or state: the list above
// is ONE page of a hundred, newest first, and would miss an older pull request
// in a busy repository. Decoded exactly as the list decodes each entry.
func TestPullRequestReadsOneHeadByItsNumber(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().
		json("/repos/acme/widget/pulls/7", http.StatusOK,
			`{"number":7,"state":"open","title":"Add widgets","body":"long prose","head":{"sha":"head7","ref":"feature","repo":{"full_name":"acme/widget"}},"base":{"sha":"base7","ref":"main","repo":{"full_name":"acme/widget"}}}`).
		json("/repos/acme/widget/pulls/9", http.StatusOK,
			`{"number":9,"state":"closed","title":"Its fork was deleted","head":{"sha":"head9","ref":"gone","repo":null},"base":{"sha":"base9","ref":"main"}}`)
	c := testClient(t, hub, &now)

	got, err := c.PullRequest(context.Background(), installToken, "acme", "widget", 7)
	if err != nil {
		t.Fatalf("pull request: %v", err)
	}
	if want := (PullRequestHead{Number: 7, Title: "Add widgets", HeadSHA: "head7", HeadRef: "feature", HeadRepository: "acme/widget", BaseSHA: "base7"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if auth := hub.seen()[0].Header.Get("Authorization"); auth != "Bearer "+installToken {
		t.Fatalf("the read must carry the bearer it was handed, got %q", auth)
	}
	if q := hub.seen()[0].URL.RawQuery; q != "" {
		t.Fatalf("one pull request is asked for by its path alone, got query %q", q)
	}

	// A DELETED head repository answers empty, as in the list.
	deleted, err := c.PullRequest(context.Background(), installToken, "acme", "widget", 9)
	if err != nil {
		t.Fatalf("pull request 9: %v", err)
	}
	if deleted.HeadRepository != "" || deleted.HeadSHA != "head9" || deleted.Number != 9 {
		t.Fatalf("a deleted head repository reads empty: %+v", deleted)
	}

	// A number GitHub does not know is its 404, for the caller to read; a
	// number that is no number asks nothing.
	if _, err := c.PullRequest(context.Background(), installToken, "acme", "widget", 8); StatusOf(err) != http.StatusNotFound {
		t.Fatalf("want a 404, got %v", err)
	}
	before := len(hub.seen())
	for _, n := range []int{0, -3} {
		if _, err := c.PullRequest(context.Background(), installToken, "acme", "widget", n); err == nil {
			t.Fatalf("pull request %d must be refused", n)
		}
	}
	if len(hub.seen()) != before {
		t.Fatalf("a pull request numbered 0 or less reached GitHub")
	}
}

// compareBody renders a compare reply with n files, each carrying a patch the
// size GitHub sends for an ordinary change.
func compareBody(n int, patch string) string {
	var b strings.Builder
	b.WriteString(`{"status":"ahead","ahead_by":1,"files":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"filename":"pkg%d/file.go","status":"modified","patch":%q}`, i, patch)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestCompareListsChangedPathsAndSaysWhenItIsComplete(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().json("/repos/acme/widget/compare/base7...head7", http.StatusOK, `{"files":[
		{"filename":"component/a/a.go","status":"modified"},
		{"filename":"component/b/moved.go","previous_filename":"component/c/moved.go","status":"renamed"},
		{"filename":"README.md","status":"added"}
	]}`)
	c := testClient(t, hub, &now)

	files, complete, err := c.Compare(context.Background(), installToken, "acme", "widget", "base7", "head7")
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !complete {
		t.Fatal("three files is a complete answer")
	}
	// A RENAME changed two places: the package it left and the one it joined.
	if got := strings.Join(files, ","); got != "component/a/a.go,component/b/moved.go,component/c/moved.go,README.md" {
		t.Fatalf("got %s", got)
	}

	// AT 300 FILES GitHub stops listing, so the answer is a floor and not the
	// set: incomplete, which the caller reads as "everything". The patches
	// make the reply several megabytes, which is ordinary for a change that
	// size and must still be read.
	hub.json("/repos/acme/widget/compare/base8...head8", http.StatusOK, compareBody(300, strings.Repeat("+line\n", 3500)))
	files, complete, err = c.Compare(context.Background(), installToken, "acme", "widget", "base8", "head8")
	if err != nil {
		t.Fatalf("a 300-file compare must still be read: %v", err)
	}
	if complete || len(files) != 300 {
		t.Fatalf("300 files is a truncated list: complete=%v, %d files", complete, len(files))
	}
	// The control for the threshold: one file fewer is complete.
	hub.json("/repos/acme/widget/compare/base9...head9", http.StatusOK, compareBody(299, "+x\n"))
	if _, complete, err = c.Compare(context.Background(), installToken, "acme", "widget", "base9", "head9"); err != nil || !complete {
		t.Fatalf("299 files is the whole answer: complete=%v err=%v", complete, err)
	}
}

func TestCommitForRefResolvesARefToItsCommit(t *testing.T) {
	now := time.Now().UTC()
	// A release commit can touch a great many files, and this endpoint lists
	// them with their patches: the reply is read whole however large it is
	// within reason, because the two facts asked for sit at its top.
	files := compareBody(300, strings.Repeat("+line\n", 3500))
	hub := newHub().json("/repos/acme/widget/commits/tags/v1.4.0", http.StatusOK,
		`{"sha":"`+headSHA+`","commit":{"message":"Release 1.4.0"},`+strings.TrimPrefix(files, `{`))
	c := testClient(t, hub, &now)

	sha, message, err := c.CommitForRef(context.Background(), installToken, "acme", "widget", "tags/v1.4.0")
	if err != nil {
		t.Fatalf("commit for ref: %v", err)
	}
	if sha != headSHA || message != "Release 1.4.0" {
		t.Fatalf("got %q / %q", sha, message)
	}
	hub.json("/repos/acme/widget/commits/nothing", http.StatusOK, `{}`)
	if _, _, err := c.CommitForRef(context.Background(), installToken, "acme", "widget", "nothing"); err == nil {
		t.Fatal("a reply naming no commit must be an error")
	}
}

// ---------------------------------------------------------------------------
// The archive
// ---------------------------------------------------------------------------

const codeloadToken = "CODELOADTOKEN_abc123"

func archiveHub() *fakeHub {
	return newHub().
		redirect("/repos/acme/widget/tarball/"+headSHA, http.StatusFound,
			"https://codeload.github.com/acme/widget/legacy.tar.gz/"+headSHA+"?token="+codeloadToken).
		on("/acme/widget/legacy.tar.gz/"+headSHA, func(*http.Request) (int, string) { return http.StatusOK, "ARCHIVE-BYTES" })
}

// TestTarballFollowsTheRedirectWithoutTheBearer: GitHub answers an archive
// request with a redirect to its download host, and the installation token is
// a bearer for every repository in the installation. It goes to the API host
// it was minted for and nowhere else.
func TestTarballFollowsTheRedirectWithoutTheBearer(t *testing.T) {
	now := time.Now().UTC()
	hub := archiveHub()
	c := testClient(t, hub, &now)

	body, err := c.Tarball(context.Background(), installToken, "acme", "widget", headSHA)
	if err != nil {
		t.Fatalf("tarball: %v", err)
	}
	raw, _ := io.ReadAll(body)
	_ = body.Close()
	if string(raw) != "ARCHIVE-BYTES" {
		t.Fatalf("got %q", raw)
	}

	seen := hub.seen()
	if len(seen) != 2 {
		t.Fatalf("want the API request and the download, got %d requests", len(seen))
	}
	if seen[0].URL.Host != "api.github.com" || seen[0].Header.Get("Authorization") != "Bearer "+installToken {
		t.Fatalf("the API request must carry the bearer: %s %q", seen[0].URL.Host, seen[0].Header.Get("Authorization"))
	}
	if seen[1].URL.Host != "codeload.github.com" {
		t.Fatalf("the redirect was not followed to the download host: %s", seen[1].URL)
	}
	if got := seen[1].Header.Get("Authorization"); got != "" {
		t.Fatalf("the bearer was sent to another host: %q", got)
	}
	// The download is authorized by the redirect's own URL, followed as given.
	if seen[1].URL.Query().Get("token") != codeloadToken {
		t.Fatalf("the redirect's URL was not followed as given: %s", seen[1].URL)
	}
}

// The reachable positive for "no bearer on the redirect": a renamed repository
// redirects on the API host first, and THAT hop does carry the bearer -- so the
// instrument above can see one on a redirected request, and its silence is
// about the host. Once a hop leaves the API host the bearer is gone for good.
func TestTarballKeepsTheBearerOnlyWhileItStaysOnTheAPIHost(t *testing.T) {
	now := time.Now().UTC()
	hub := archiveHub().
		redirect("/repos/acme/old-name/tarball/"+headSHA, http.StatusMovedPermanently,
			"https://api.github.com/repos/acme/widget/tarball/"+headSHA)
	c := testClient(t, hub, &now)

	body, err := c.Tarball(context.Background(), installToken, "acme", "old-name", headSHA)
	if err != nil {
		t.Fatalf("tarball: %v", err)
	}
	_ = body.Close()
	seen := hub.seen()
	if len(seen) != 3 {
		t.Fatalf("want three hops, got %d", len(seen))
	}
	for n, want := range []string{"Bearer " + installToken, "Bearer " + installToken, ""} {
		if got := seen[n].Header.Get("Authorization"); got != want {
			t.Errorf("hop %d (%s): Authorization %q, want %q", n, seen[n].URL.Host, got, want)
		}
	}
}

func TestATarballFailureNeverNamesTheDownloadURL(t *testing.T) {
	now := time.Now().UTC()

	t.Run("the download answers 404", func(t *testing.T) {
		hub := archiveHub().json("/acme/widget/legacy.tar.gz/"+headSHA, http.StatusNotFound, `{}`)
		c := testClient(t, hub, &now)
		_, err := c.Tarball(context.Background(), installToken, "acme", "widget", headSHA)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Endpoint != "/repos/acme/widget/tarball/"+headSHA {
			t.Fatalf("want a 404 naming the API endpoint, got %v", err)
		}
		if strings.Contains(err.Error(), codeloadToken) || strings.Contains(err.Error(), "codeload") {
			t.Fatalf("the download URL reached the error: %v", err)
		}
	})

	t.Run("the download host cannot be reached", func(t *testing.T) {
		hub := archiveHub()
		c := New(testConfig(t), WithHTTPClient(&http.Client{Transport: unreachable{hub: hub, host: "codeload.github.com"}}),
			WithClock(func() time.Time { return now }))
		_, err := c.Tarball(context.Background(), installToken, "acme", "widget", headSHA)
		if err == nil {
			t.Fatal("an unreachable download host must be an error")
		}
		if strings.Contains(err.Error(), codeloadToken) || strings.Contains(err.Error(), "token=") {
			t.Fatalf("the download URL's token reached the error: %v", err)
		}
		// The control: the request really went to the host that failed, with
		// the token in its URL -- so the error had a URL to leak.
		if seen := hub.seen(); len(seen) != 2 || !strings.Contains(seen[1].URL.String(), codeloadToken) {
			t.Fatalf("control failed: the failing request was not the download: %v", seen)
		}
	})
}

// unreachable fails every request to one host the way a reset connection does,
// after recording it.
type unreachable struct {
	hub  *fakeHub
	host string
}

func (u unreachable) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := u.hub.RoundTrip(req)
	if req.URL.Host == u.host {
		return nil, errors.New("read: connection reset by peer")
	}
	return resp, err
}

// An archive is never fetched over a downgraded connection: the download URL
// carries its own token, and the archive is the repository's source.
func TestTarballRefusesARedirectToPlainHTTP(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub().redirect("/repos/acme/widget/tarball/"+headSHA, http.StatusFound,
		"http://codeload.github.com/acme/widget/legacy.tar.gz/"+headSHA+"?token="+codeloadToken)
	c := testClient(t, hub, &now)
	if _, err := c.Tarball(context.Background(), installToken, "acme", "widget", headSHA); err == nil {
		t.Fatal("a redirect from https to http must be refused")
	} else if strings.Contains(err.Error(), codeloadToken) {
		t.Fatalf("the refused URL's token reached the error: %v", err)
	}
	if n := len(hub.seen()); n != 1 {
		t.Fatalf("the downgraded URL was requested: %d requests", n)
	}
}

// Every bearer call on a nil client answers ErrNotConfigured rather than
// dereferencing nothing: a node with no client wired is a node with no app.
func TestTheBearerCallsAnswerNotConfiguredOnANilClient(t *testing.T) {
	var c *Client
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["CreateCheckRun"] = c.CreateCheckRun(ctx, installToken, "acme", "widget", CheckRun{Name: "n", HeadSHA: "s"})
	checks["UpdateCheckRun"] = c.UpdateCheckRun(ctx, installToken, "acme", "widget", 1, CheckRun{})
	_, checks["Repository"] = c.Repository(ctx, installToken, "acme", "widget")
	_, _, checks["BranchHead"] = c.BranchHead(ctx, installToken, "acme", "widget", "main")
	_, checks["OpenPullRequests"] = c.OpenPullRequests(ctx, installToken, "acme", "widget")
	_, checks["PullRequest"] = c.PullRequest(ctx, installToken, "acme", "widget", 7)
	_, _, checks["Compare"] = c.Compare(ctx, installToken, "acme", "widget", "a", "b")
	_, _, checks["CommitForRef"] = c.CommitForRef(ctx, installToken, "acme", "widget", "main")
	_, checks["Tarball"] = c.Tarball(ctx, installToken, "acme", "widget", headSHA)
	_, _, checks["ScopedInstallationToken"] = c.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"})
	_, checks["UploadSARIF"] = c.UploadSARIF(ctx, installToken, "acme", "widget", SARIFAnalysis{})
	_, checks["SARIFUploadStatus"] = c.SARIFUploadStatus(ctx, installToken, "acme", "widget", "id")
	for name, err := range checks {
		if !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s on a nil client: want ErrNotConfigured, got %v", name, err)
		}
	}
}
