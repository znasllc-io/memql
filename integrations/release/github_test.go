package release

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// github_test.go -- the cut, end to end, against a fake GitHub.
//
// CI NEVER TOUCHES REAL GITHUB. Not "should not" -- there is no code path from
// this file to api.github.com, because every client here is constructed with
// WithBaseURL pointed at an httptest server. A live test of this feature would
// create real tags and publish real releases of a real product on every run,
// which is not a test but a release process with an audience of nobody.
//
// AND IT IS A FAKE SERVER RATHER THAN A MOCKED CLIENT, deliberately. The
// interesting behaviour of this package lives BETWEEN the call and the decode:
// status-code classification, the paginated tag walk, the order in which the
// tag and the Release are created, and what happens when the second one fails
// after the first succeeded. A mocked *Client replaces exactly that region and
// leaves a suite that tests the mock.

// fakeGitHub is a scriptable GitHub.
type fakeGitHub struct {
	mu sync.Mutex

	// tags is the repository's tag list, returned paginated.
	tags []tagRef
	// headSha is what GET /commits/main answers.
	headSha string

	// releaseStatus overrides the status POST /releases answers with. Zero
	// means 201 -- the happy path.
	releaseStatus int
	// tagStatus does the same for POST /git/refs.
	tagStatus int

	// created records what the fake was actually asked to create, which is
	// how the half-done test proves a tag exists.
	createdRefs     []string
	createdReleases []string

	// The pin-bump follow-on's endpoints, served by the same fake so a
	// test can break the follow-on without breaking the cut. Routing them
	// through a second server is not possible: creating a TAG and creating
	// a BRANCH are the same POST /git/refs path, distinguished only by the
	// ref prefix in the body.
	pinContent  string
	pinFailAt   string
	pinBranches []string
	pinCommits  []string
	pinPROpened bool

	server *httptest.Server
}

func newFakeGitHub(t *testing.T, tags []tagRef, headSha string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tags: tags, headSha: headSha}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tags"):
		// Paginated exactly as GitHub is, so the walk in ListTagRefs is
		// exercised rather than accidentally satisfied by one page.
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		const per = 100
		start := (page - 1) * per
		end := start + per
		if start > len(f.tags) {
			start = len(f.tags)
		}
		if end > len(f.tags) {
			end = len(f.tags)
		}
		out := make([]map[string]any, 0, end-start)
		for _, tag := range f.tags[start:end] {
			out = append(out, map[string]any{"name": tag.Name, "commit": map[string]any{"sha": tag.Sha}})
		}
		writeJSON(w, http.StatusOK, out)

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits/main"):
		writeJSON(w, http.StatusOK, map[string]any{"sha": f.headSha})

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
		var body struct{ Ref, Sha string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasPrefix(body.Ref, "refs/heads/") {
			// The pin bump's branch. Same endpoint as the tag, and
			// only the prefix tells them apart -- which is why one
			// fake serves both.
			if f.pinFailAt == "branch" {
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"})
				return
			}
			f.pinBranches = append(f.pinBranches, body.Ref)
			writeJSON(w, http.StatusCreated, map[string]any{"ref": body.Ref})
			return
		}
		if f.tagStatus != 0 {
			writeJSON(w, f.tagStatus, map[string]any{"message": "scripted"})
			return
		}
		name := strings.TrimPrefix(body.Ref, "refs/tags/")
		for _, tag := range f.tags {
			if tag.Name == name {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Reference already exists"})
				return
			}
		}
		f.createdRefs = append(f.createdRefs, name)
		f.tags = append(f.tags, tagRef{Name: name, Sha: body.Sha})
		writeJSON(w, http.StatusCreated, map[string]any{"ref": body.Ref})

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/releases"):
		var body struct {
			TagName              string `json:"tag_name"`
			Draft                bool   `json:"draft"`
			GenerateReleaseNotes bool   `json:"generate_release_notes"`
			Body                 string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.releaseStatus != 0 {
			writeJSON(w, f.releaseStatus, map[string]any{"message": "scripted"})
			return
		}
		// The cascade fires on `release: published`. A draft would build
		// nothing while looking like success, so the fake refuses one --
		// it would be a defect the handler could otherwise ship.
		if body.Draft {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "a draft release fires no workflow"})
			return
		}
		f.createdReleases = append(f.createdReleases, body.TagName)
		writeJSON(w, http.StatusCreated, map[string]any{
			"html_url": "https://github.test/acme/widget/releases/tag/" + body.TagName,
			"tag_name": body.TagName,
		})

	case strings.Contains(r.URL.Path, "/contents/") && r.Method == http.MethodGet:
		if f.pinFailAt == "read" {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"})
			return
		}
		// Wrapped at 60 columns exactly as GitHub does, which the strict
		// base64 decoder rejects -- so the unwrap in GetFile is
		// exercised rather than assumed.
		encoded := base64.StdEncoding.EncodeToString([]byte(f.pinContent))
		var wrapped strings.Builder
		for idx := 0; idx < len(encoded); idx += 60 {
			end := min(idx+60, len(encoded))
			wrapped.WriteString(encoded[idx:end] + "\n")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"content": wrapped.String(), "encoding": "base64", "sha": "blobsha",
		})

	case strings.Contains(r.URL.Path, "/contents/") && r.Method == http.MethodPut:
		if f.pinFailAt == "commit" {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"})
			return
		}
		var body struct{ Content, Sha, Branch string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Sha != "blobsha" {
			// GitHub's optimistic-concurrency check: a write that
			// does not name the blob it replaces is refused rather
			// than clobbering a concurrent edit.
			writeJSON(w, http.StatusConflict, map[string]any{"message": "sha mismatch"})
			return
		}
		raw, _ := base64.StdEncoding.DecodeString(body.Content)
		f.pinCommits = append(f.pinCommits, string(raw))
		writeJSON(w, http.StatusOK, map[string]any{"commit": map[string]any{"sha": "newsha"}})

	case strings.Contains(r.URL.Path, "/pulls"):
		if f.pinFailAt == "pr" {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"})
			return
		}
		f.pinPROpened = true
		writeJSON(w, http.StatusCreated, map[string]any{"html_url": "https://github.test/acme/widget/pull/42"})

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "no such endpoint: " + r.URL.Path})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// recordingEngine accepts every call and remembers it, so a test can assert on
// the row a cut wrote AND on the origin it was written under.
type recordingEngine struct {
	memql.IntegrationEngineAccess
	mu      sync.Mutex
	calls   []string
	origins []auth.CallOrigin
	rows    []map[string]any
}

func (e *recordingEngine) Execute(ctx context.Context, call string) (*memql.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	return &memql.ExecuteResult{}, nil
}

func (e *recordingEngine) callsNamed(prefix string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		if strings.Contains(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// ownerIntegration wires a fake GitHub, a recording engine and a seeded
// resolver -- the standard fixture for a happy-path cut.
func ownerIntegration(t *testing.T, f *fakeGitHub) (*Integration, *recordingEngine) {
	t.Helper()
	engine := &recordingEngine{}
	i := NewIntegration(slog.New(slog.DiscardHandler), engine, resolver{
		env: func(name string) string {
			switch name {
			case RepoVariableName:
				return "acme/widget"
			case SecretName:
				return "token"
			}
			return ""
		},
	})
	i.github = NewClient().WithBaseURL(f.server.URL)
	return i, engine
}

func ownerCtx() context.Context { return actorContext(auth.RoleOwner) }

// ---------------------------------------------------------------------------
// Version arithmetic, through the whole call
// ---------------------------------------------------------------------------

func TestCutComputesTheNextVersionFromTheNEWESTTag(t *testing.T) {
	// The list is deliberately not sorted, and v0.9.2 sorts AFTER v0.17.0
	// as a STRING -- so a lexicographic max would name v0.9.2 as newest and
	// cut v0.9.3, a version that already exists. This is the case that
	// makes the arithmetic worth having.
	//
	// It also carries three refs that are not release tags: a pre-release,
	// a bare version, and a backup ref whose name CONTAINS a release tag.
	// Each is a different way a naive parse goes wrong.
	tags := []tagRef{
		{Name: "v0.9.2", Sha: "old"},
		{Name: "v0.17.0", Sha: "older"},
		{Name: "v0.17.1", Sha: "recent"},
		{Name: "v0.18.0-rc1", Sha: "prerelease"},
		{Name: "0.19.0", Sha: "bare"},
		{Name: "backup-v9.9.9-old", Sha: "backup"},
		{Name: "nightly", Sha: "nightly"},
	}
	for _, tc := range []struct{ bump, want string }{
		{"patch", "v0.17.2"},
		{"minor", "v0.18.0"},
		{"major", "v1.0.0"},
	} {
		t.Run(tc.bump, func(t *testing.T) {
			f := newFakeGitHub(t, tags, "headsha1234567")
			i, _ := ownerIntegration(t, f)
			out, err := i.Cut(ownerCtx(), CutRequest{Bump: tc.bump})
			if err != nil {
				t.Fatalf("cut: %v", err)
			}
			if out.Version != tc.want {
				t.Fatalf("next version = %s, want %s (previous was %s)", out.Version, tc.want, out.PreviousTag)
			}
			if out.PreviousTag != "v0.17.1" {
				t.Fatalf("previous = %s, want v0.17.1 -- a non-release ref was read as one", out.PreviousTag)
			}
		})
	}
}

func TestCutWalksEveryPageOfTags(t *testing.T) {
	// 150 tags forces a second page. A single-page read would see only the
	// first hundred and, because the newest are appended last here, would
	// compute the next version from a superseded one -- silently reissuing
	// a version that already exists.
	var tags []tagRef
	for n := 0; n < 150; n++ {
		tags = append(tags, tagRef{Name: fmt.Sprintf("v0.%d.0", n), Sha: fmt.Sprintf("sha%d", n)})
	}
	f := newFakeGitHub(t, tags, "headsha")
	i, _ := ownerIntegration(t, f)
	out, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if out.PreviousTag != "v0.149.0" {
		t.Fatalf("previous = %s, want v0.149.0 -- the tag walk stopped at one page", out.PreviousTag)
	}
}

func TestCutTagsMainsHeadAndRecordsIt(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "oldsha"}}, "abcdef1234567890")
	i, engine := ownerIntegration(t, f)
	out, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if out.BaseSha != "abcdef1234567890" {
		t.Fatalf("baseSha = %q, want main's head", out.BaseSha)
	}
	// The tag has to point at that sha, or the row records a provenance
	// the repository does not have.
	for _, tag := range f.tags {
		if tag.Name == "v1.0.1" && tag.Sha != "abcdef1234567890" {
			t.Fatalf("the tag points at %q, not main's head", tag.Sha)
		}
	}
	rows := engine.callsNamed("createReleaseCut")
	if len(rows) != 1 {
		t.Fatalf("expected one row write, got %d: %v", len(rows), rows)
	}
	if !strings.Contains(rows[0], `baseSha: "abcdef1234567890"`) {
		t.Fatalf("the row does not record the base sha: %s", rows[0])
	}
}

func TestCutOrdersTagBeforeRelease(t *testing.T) {
	// The order is the concurrency gate: GitHub's ref-create is atomic and
	// the Release API is not, so creating the Release first would open the
	// race the tag closes.
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head")
	i, _ := ownerIntegration(t, f)
	if _, err := i.Cut(ownerCtx(), CutRequest{Bump: "minor"}); err != nil {
		t.Fatalf("cut: %v", err)
	}
	if len(f.createdRefs) != 1 || f.createdRefs[0] != "v1.1.0" {
		t.Fatalf("refs created = %v, want [v1.1.0]", f.createdRefs)
	}
	if len(f.createdReleases) != 1 || f.createdReleases[0] != "v1.1.0" {
		t.Fatalf("releases created = %v, want [v1.1.0]", f.createdReleases)
	}
}

// ---------------------------------------------------------------------------
// Every typed refusal
// ---------------------------------------------------------------------------

func TestCutRefusesWhenTheNextTagAlreadyExists(t *testing.T) {
	// The second of two racing owners gets this. Simulated by a repository
	// that already carries the tag the arithmetic is about to compute --
	// which is exactly the state the loser of the race observes.
	f := newFakeGitHub(t, []tagRef{
		{Name: "v1.0.0", Sha: "old"},
		{Name: "v1.0.1", Sha: "someone-elses-cut"},
	}, "head")
	// The arithmetic sees v1.0.1 as newest and computes v1.0.2, so to
	// reach ref_exists the fake must refuse the create instead.
	f.tagStatus = http.StatusUnprocessableEntity
	i, _ := ownerIntegration(t, f)
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeRefExists {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeRefExists, err)
	}
}

func TestCutRefusesWhenMainsHeadIsAlreadyReleased(t *testing.T) {
	// Cutting again from an already-tagged head publishes a second version
	// of identical code.
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "head"}}, "head")
	i, engine := ownerIntegration(t, f)
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeAlreadyReleasedAtHead {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeAlreadyReleasedAtHead, err)
	}
	if !strings.Contains(err.Error(), "v1.0.0") {
		t.Fatalf("the refusal does not name the tag already at head: %v", err)
	}
	// Nothing created, nothing recorded.
	if len(f.createdRefs) != 0 || len(f.createdReleases) != 0 {
		t.Fatalf("a refused cut created %v / %v", f.createdRefs, f.createdReleases)
	}
	if len(engine.callsNamed("createReleaseCut")) != 0 {
		t.Fatal("a refused cut wrote a row")
	}
}

func TestCutRefusesWithNoRepositoryConfigured(t *testing.T) {
	i := NewIntegration(slog.New(slog.DiscardHandler), &recordingEngine{}, resolver{
		env: func(string) string { return "" },
	})
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeReleaseRepoUnconfigured {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeReleaseRepoUnconfigured, err)
	}
	if !strings.Contains(err.Error(), RepoVariableName) {
		t.Fatalf("the refusal must name the variable to seed: %v", err)
	}
}

func TestCutRefusesWithNoCredential(t *testing.T) {
	i := NewIntegration(slog.New(slog.DiscardHandler), &recordingEngine{}, resolver{
		env: func(name string) string {
			if name == RepoVariableName {
				return "acme/widget"
			}
			return ""
		},
	})
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeCredentialUnavailable {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeCredentialUnavailable, err)
	}
	if !strings.Contains(err.Error(), SecretName) {
		t.Fatalf("the refusal must name %s, which is the actionable half: %v", SecretName, err)
	}
}

func TestCutRefusesWhenGitHubIs5xx(t *testing.T) {
	f := newFakeGitHub(t, nil, "head")
	f.server.Close()
	i, _ := ownerIntegration(t, f)
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeGithubUnreachable {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeGithubUnreachable, err)
	}
}

func TestCutRefusesARepositoryWithNoReleaseTags(t *testing.T) {
	// Deliberately NOT defaulted to v0.0.0-and-bump: the first release of a
	// repository is a version somebody chooses.
	f := newFakeGitHub(t, []tagRef{{Name: "nightly", Sha: "x"}, {Name: "v1.0-beta", Sha: "y"}}, "head")
	i, _ := ownerIntegration(t, f)
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeNoReleaseTags {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeNoReleaseTags, err)
	}
	if len(f.createdRefs) != 0 {
		t.Fatalf("a repository with no releases had a tag created: %v", f.createdRefs)
	}
}

func TestCutRefusesAnInvalidBump(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head")
	i, _ := ownerIntegration(t, f)
	// A network call must not happen for an argument error either --
	// validation is before the config resolve for the same reason the
	// owner wall is.
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "PATCH"})
	if got := RefusalCode(err); got != CodeInvalidBump {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeInvalidBump, err)
	}
}

// ---------------------------------------------------------------------------
// The half-done state
// ---------------------------------------------------------------------------

func TestCutReportsTheHalfDoneStateAndRecordsIt(t *testing.T) {
	// The tag lands and the Release does not. Nothing will build, and the
	// repository now carries a tag whose origin only this cluster knows --
	// so the row is what makes it explicable later.
	f := newFakeGitHub(t, []tagRef{{Name: "v2.3.4", Sha: "old"}}, "head")
	f.releaseStatus = http.StatusUnprocessableEntity
	i, engine := ownerIntegration(t, f)

	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeTagCreatedReleaseFailed {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeTagCreatedReleaseFailed, err)
	}
	// The tag name is the one fact needed to finish or undo it, so it must
	// be ON the refusal rather than only in the prose.
	var r *Refusal
	if !asRefusalPtr(err, &r) || r.TagName != "v2.3.5" {
		t.Fatalf("the refusal does not carry the created tag: %+v", r)
	}
	if len(f.createdRefs) != 1 {
		t.Fatalf("expected the tag to exist; created = %v", f.createdRefs)
	}
	rows := engine.callsNamed("createReleaseCut")
	if len(rows) != 1 {
		t.Fatalf("the half-done cut wrote %d rows, want 1 -- an unrecorded tag is one nobody can explain later", len(rows))
	}
	if !strings.Contains(rows[0], `status: "tag_created_release_failed"`) {
		t.Fatalf("the row does not record the half-done status: %s", rows[0])
	}
	if !strings.Contains(rows[0], `tagName: "v2.3.5"`) {
		t.Fatalf("the row does not record the created tag: %s", rows[0])
	}
}

// ---------------------------------------------------------------------------
// Dry run
// ---------------------------------------------------------------------------

func TestDryRunComputesThePlanAndCreatesNothing(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v3.1.4", Sha: "old"}}, "deadbeefcafe")
	i, engine := ownerIntegration(t, f)

	out, err := i.Cut(ownerCtx(), CutRequest{Bump: "minor", DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if out.Version != "v3.2.0" || out.BaseSha != "deadbeefcafe" {
		t.Fatalf("the plan is wrong: %+v", out)
	}
	if !out.DryRun || out.Status != "dry_run" {
		t.Fatalf("a dry run must say so: %+v", out)
	}
	// The three things it must not have done.
	if len(f.createdRefs) != 0 {
		t.Fatalf("a dry run created a tag: %v", f.createdRefs)
	}
	if len(f.createdReleases) != 0 {
		t.Fatalf("a dry run published a release: %v", f.createdReleases)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("a dry run wrote to the graph: %v", engine.calls)
	}
}

// ---------------------------------------------------------------------------
// The graph writes
// ---------------------------------------------------------------------------

// TestCutWritesUnderInternalOrigin is the test the memql#2800 class needs.
//
// Both mutations are @serverOnly, auth.CallOrigin's zero value is OriginClient,
// and the engine refuses a @serverOnly construct on a client origin -- logging
// one WARN and carrying on. So a missing stamp leaves the release history
// permanently empty with every other test in this file still green. Asserting
// the ORIGIN each call arrived under is what makes that unmissable.
func TestCutWritesUnderInternalOrigin(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head")
	i, engine := ownerIntegration(t, f)
	if _, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"}); err != nil {
		t.Fatalf("cut: %v", err)
	}
	if len(engine.origins) == 0 {
		t.Fatal("no graph call was made at all, so this asserts nothing")
	}
	for idx, origin := range engine.origins {
		if origin != auth.OriginInternal {
			t.Errorf("call %d (%s) ran under origin %v, want internal -- a @serverOnly mutation is refused on any other, silently",
				idx, engine.calls[idx], origin)
		}
	}
}

func TestCutWritesTheAuditEventBesideTheRow(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head")
	i, engine := ownerIntegration(t, f)
	if _, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"}); err != nil {
		t.Fatalf("cut: %v", err)
	}
	audits := engine.callsNamed("createAuditEvent")
	if len(audits) != 1 {
		t.Fatalf("expected one audit event, got %d", len(audits))
	}
	for _, want := range []string{
		`action: "release_cut"`,
		`targetType: "releaseCut"`,
		`targetId: "v1.0.1"`,
		`actorUserId: "v1:identity:user:someone"`,
		`category: "admin"`,
	} {
		if !strings.Contains(audits[0], want) {
			t.Errorf("the audit event is missing %s:\n%s", want, audits[0])
		}
	}
}

// TestBookkeepingFailureDoesNotFailAPublishedCut is the rule stated in
// Store.WriteCut, checked.
//
// By the time the row is written the Release is published and the build is
// running. Reporting that as a failed cut invites the operator to cut again --
// which produces a second version of the same code and is the worst outcome
// available here.
func TestBookkeepingFailureDoesNotFailAPublishedCut(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head")
	i, _ := ownerIntegration(t, f)
	i.store = NewStore(&failingEngine{})

	out, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if err != nil {
		t.Fatalf("a published cut was reported as failed because its row did not land: %v", err)
	}
	if out.Status != "dispatched" || out.ReleaseURL == "" {
		t.Fatalf("the outcome does not describe the release that actually published: %+v", out)
	}
}

type failingEngine struct{ memql.IntegrationEngineAccess }

func (e *failingEngine) Execute(context.Context, string) (*memql.ExecuteResult, error) {
	return nil, fmt.Errorf("the database is down")
}
