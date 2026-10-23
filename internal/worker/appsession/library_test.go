package appsession

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectProduced_RepositoryCheckoutIsNotEvidence(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	repo := filepath.Join(ws, "checkout")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", "-q")
	for n := range 40 {
		if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("source-%02d.go", n)), []byte("package example\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "baseline")
	if err := os.WriteFile(filepath.Join(repo, "source-00.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "verification.md"), []byte("Observed behavior and test results"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, skipped := collectProduced(ws, before)
	names := map[string]bool{}
	for _, f := range files {
		names[filepath.ToSlash(f.rel)] = true
	}
	if len(names) != 2 || !names["checkout/source-00.go"] || !names["verification.md"] {
		t.Fatalf("retained %v; want changed source and verification, never the checkout", names)
	}
	if len(skipped) != 0 {
		t.Fatalf("checkout consumed artifact budget: %v", skipped)
	}
	// Committing a file must not erase the evidence that the app authored it.
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "change")
	files, _ = collectProduced(ws, before, map[string]bool{filepath.Join(repo, "source-00.go"): true})
	if len(files) != 2 {
		t.Fatalf("committed work was lost: %+v", files)
	}
}

func TestCollectProduced_TotalByteBudget(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	for n := range 3 {
		f, err := os.Create(filepath.Join(ws, fmt.Sprintf("large-%d.bin", n)))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxPushFileBytes); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	files, omitted := collectProduced(ws, before)
	if len(files) != 2 || len(omitted) != 1 || !strings.Contains(omitted[0], "total cap") {
		t.Fatalf("files=%d omitted=%v; want aggregate budget with explicit omission", len(files), omitted)
	}
}

func TestCollectProduced_WorktreeGitFileNeverLeaves(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	git("init", "-q")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "baseline")
	git("worktree", "add", "--detach", filepath.Join(ws, "tree"), "HEAD")
	files, omitted := collectProduced(ws, before)
	if len(files) != 0 || len(omitted) != 0 {
		t.Fatalf("worktree scaffolding leaked: %+v %v", files, omitted)
	}
}

func TestLibraryBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":     "https://api.example.com",
		"https://api.example.com:443": "https://api.example.com",
		"grpcs://api.example.com":     "https://api.example.com",
		"api.example.com:443":         "https://api.example.com",
		"api.example.com":             "https://api.example.com",
		"http://localhost:8080":       "http://localhost:8080",
		"grpc://localhost:50051":      "http://localhost:50051",
	}
	for in, want := range cases {
		got, err := LibraryBaseURL(in)
		if err != nil {
			t.Errorf("LibraryBaseURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("LibraryBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := LibraryBaseURL("   "); err == nil {
		t.Error("an empty cluster_url must be an error")
	}
}

// TestPull_WritesUnderTheWorkspaceOnly. A Content-Disposition filename is
// server-supplied and lands inside somebody's workspace; a traversal in
// it would put a file wherever the header said.
func TestPull_WritesUnderTheWorkspaceOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../escaped.txt"`)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	l := NewLibrary(srv.URL, testBearer, srv.Client())
	path, err := l.Pull(context.Background(), "artifact-1", dir)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("pulled to %q, which is outside the workspace %q", path, dir)
	}
	if strings.Contains(path, "..") {
		t.Errorf("path %q still carries a traversal", path)
	}
}

// TestPull_SendsTheSessionBearer, in both directions.
func TestPull_SendsTheSessionBearer(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	if _, err := l.Pull(context.Background(), "a", t.TempDir()); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if seen != "Bearer "+testBearer {
		t.Errorf("Authorization = %q", seen)
	}
}

// TestPull_404SaysBothThingsItCouldMean. The engine's download route
// answers 404 on deny by design, so a link cannot be used to probe which
// ids exist -- which means a denied read and a missing artifact are
// genuinely indistinguishable here. Asserting one of them would send
// somebody to the wrong place half the time.
func TestPull_404SaysBothThingsItCouldMean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	_, err := l.Pull(context.Background(), "ghost", t.TempDir())
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error must name the id: %q", msg)
	}
	if !strings.Contains(msg, "does not exist") || !strings.Contains(msg, "cannot reach it") {
		t.Errorf("error must state both readings: %q", msg)
	}
}

// TestPush_ReturnsTheArtifactId.
func TestPush_ReturnsTheArtifactId(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, "no file part", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"artifactId":"art-9","fileId":"file-9"}`)
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	id, err := l.PushBytes(context.Background(), []byte("hello"), "out.txt")
	if err != nil {
		t.Fatalf("PushBytes: %v", err)
	}
	if id != "art-9" {
		t.Errorf("id = %q", id)
	}
}

// TestPush_ForbiddenNamesTheUser: the credential acts as the owning user,
// so a 403 is a statement about that user's access.
func TestPush_ForbiddenNamesTheUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	_, err := l.PushBytes(context.Background(), []byte("x"), "out.txt")
	if err == nil || !strings.Contains(err.Error(), "owning user") {
		t.Errorf("error = %v, want it to name the user's access", err)
	}
}

// TestCollectProduced_OnlyWhatTheRunChanged.
func TestCollectProduced_OnlyWhatTheRunChanged(t *testing.T) {
	ws := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("untouched.txt", "before")
	write("node_modules/huge.js", "dependency")
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	before := snapshotWorkspace(ws)

	write("new.txt", "produced")
	write("sub/nested.txt", "produced too")
	write("untouched.txt", "before")            // same content, rewritten
	write("node_modules/another.js", "ignored") // excluded tree

	produced, skipped := collectProduced(ws, before)
	names := map[string]bool{}
	for _, f := range produced {
		names[filepath.ToSlash(f.rel)] = true
	}
	if !names["new.txt"] || !names["sub/nested.txt"] {
		t.Errorf("produced = %v, want the two new files", names)
	}
	for _, unwanted := range []string{"node_modules/huge.js", "node_modules/another.js", ".git/config"} {
		if names[unwanted] {
			t.Errorf("%s should never be pushed: it is reproducible and enormous", unwanted)
		}
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none at this size", skipped)
	}

	// A rewritten file with a new mtime IS produced -- the run touched
	// it, and the reader wants to see what it wrote.
	touched := filepath.Join(ws, "untouched.txt")
	if err := os.WriteFile(touched, []byte("after"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	produced, _ = collectProduced(ws, before)
	found := false
	for _, f := range produced {
		if f.rel == "untouched.txt" {
			found = true
		}
	}
	if !found {
		t.Error("a file the run rewrote was not reported as produced")
	}
}

// TestCollectProduced_BoundedArchiveEntries. A bounded list that reads
// as complete is worse than a shorter one that says what it dropped:
// downstream, produced_artifact_ids is taken as the whole answer.
func TestCollectProduced_BoundedArchiveEntries(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	for i := range maxOutputEntries + 5 {
		path := filepath.Join(ws, fmt.Sprintf("out-%03d.txt", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	produced, skipped := collectProduced(ws, before)
	if len(produced) != maxOutputEntries {
		t.Errorf("produced = %d, want the cap of %d", len(produced), maxOutputEntries)
	}
	if len(skipped) != 5 {
		t.Fatalf("skipped = %d entries, want the 5 that were dropped named", len(skipped))
	}
	for _, note := range skipped {
		if !strings.Contains(note, "cap") {
			t.Errorf("skip note does not say why: %q", note)
		}
	}
}

// TestCollectProduced_IgnoresSessionScaffolding: the MCP config, the
// launcher and the transcript are not the run's output.
func TestCollectProduced_IgnoresSessionScaffolding(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"+backupSuffix), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".memql-session"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".memql-session", "transcript.log"), []byte("log"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	produced, _ := collectProduced(ws, before)
	if len(produced) != 0 {
		t.Errorf("produced = %+v, want none: none of that is the run's output", produced)
	}
}

// TestArtifactName_KeepsTheLocation. The Library stores under the last
// path segment, so two files with the same basename in different
// directories would arrive indistinguishable.
func TestArtifactName_KeepsTheLocation(t *testing.T) {
	if got := artifactName("api/schema.json"); got != "api__schema.json" {
		t.Errorf("artifactName = %q", got)
	}
	if got := artifactName("output.txt"); got != "output.txt" {
		t.Errorf("a root file was altered: %q", got)
	}
	if artifactName("api/schema.json") == artifactName("web/schema.json") {
		t.Error("two files with the same basename collapsed to one artifact name")
	}
}

// TestLibraryStatusError_401SaysWhatIsActuallyWrong pins the sentence a 401
// produces, because that sentence is the whole deliverable of
// memql-cockpit#371 and its previous version sent people to the wrong place.
//
// The assertions are on WORDS rather than on a byte-exact string. What must
// survive a rewrite is that the operator is pointed at renewal and away from
// the class reading that memql#4863 retired; the exact phrasing is allowed to
// improve without a test failure that teaches nothing.
func TestLibraryStatusError_401SaysWhatIsActuallyWrong(t *testing.T) {
	err := libraryStatusError("input", "v1:library:artifact:abc", &http.Response{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
	})
	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	got := err.Error()

	for _, want := range []string{
		"401",
		"v1:library:artifact:abc",
		"expired",
		"renew_credential",
		"app_session",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the 401 sentence must mention %q; got:\n%s", want, got)
		}
	}

	// The retired reading. Before memql#4863 the app-session back-channel
	// was minted as class="service_account" and the Library's byte routes
	// pinned that class off, so a 401 meant "wrong class" and the sentence
	// said "expired or malformed" -- wrong on both counts. Naming
	// "malformed" again would resurrect a diagnosis nobody can act on: the
	// engine's verify path is JWKS-only, so a malformed bearer and an
	// expired one are indistinguishable from here anyway.
	if strings.Contains(got, "malformed") {
		t.Errorf("the 401 sentence must not claim the bearer is malformed; got:\n%s", got)
	}
}

// TestLibraryStatusError_AuthAndAuthorizationStayApart. A 403 is the owning
// user's access, not a login failure, and folding the two together is how an
// operator ends up re-running `memql login` against a working session.
func TestLibraryStatusError_AuthAndAuthorizationStayApart(t *testing.T) {
	forbidden := libraryStatusError("input", "id", &http.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 Forbidden",
	}).Error()
	if strings.Contains(forbidden, "renew_credential") {
		t.Errorf("a 403 must not be reported as a credential problem; got:\n%s", forbidden)
	}
	if !strings.Contains(forbidden, "403") {
		t.Errorf("a 403 must name its status; got:\n%s", forbidden)
	}

	notFound := libraryStatusError("input", "id", &http.Response{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
	}).Error()
	if strings.Contains(notFound, "renew_credential") {
		t.Errorf("a 404 must not be reported as a credential problem; got:\n%s", notFound)
	}
}

// TestPushReportsA401TheSameWayAPullDoes. A run pulls its inputs at the start
// and pushes its output at the end, which can be an hour apart -- long enough
// for one credential to expire between them. Reporting the second half of that
// single event as a bare "401 Unauthorized" made it look like a different
// problem from the first.
func TestPushReportsA401TheSameWayAPullDoes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	lib := NewLibrary(srv.URL, "token", srv.Client())
	_, err := lib.PushBytes(context.Background(), []byte("hello"), "out.txt")
	if err == nil {
		t.Fatal("a 401 on push must be an error")
	}
	if !strings.Contains(err.Error(), "renew_credential") {
		t.Errorf("push must point at renewal the way pull does; got:\n%s", err)
	}
}

// A CLOSE ON A WRITABLE HANDLE IS WHERE A SHORT WRITE SURFACES, so both
// the failure path and the success path check it.
//
// The success path is the one that decides whether the pulled file is
// WHOLE: a buffered write that could not be flushed reports at Close and
// nowhere else, so returning the path without checking would hand an
// agent a truncated input to work from. The failure path joins the close
// error onto the copy error, because a copy that failed AND a close that
// failed together say "the disk is full", where either alone sends
// somebody looking at the network.
func TestPull_ChecksTheCloseOnBothPaths(t *testing.T) {
	// A body that ends early makes io.Copy fail, which is the path that
	// used to drop the close error on the floor.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		_, _ = w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack and drop the connection so the body is truncated.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	l := NewLibrary(srv.URL, testBearer, srv.Client())
	path, err := l.Pull(context.Background(), "artifact-1", dir)
	if err == nil {
		t.Fatalf("a truncated body was accepted, writing %q", path)
	}
	// AND THE PARTIAL FILE IS REMOVED. A half-written input left on
	// disk is one an agent would read as complete.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("a partial download was left behind: %v", entries)
	}
}

// The ordinary success path is unchanged by the joined error: with a
// close that succeeds, errors.Join returns just the first error, so the
// message an operator reads is the one it always was.
func TestPull_SucceedsAndWritesTheWholeBody(t *testing.T) {
	body := strings.Repeat("payload-", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	l := NewLibrary(srv.URL, testBearer, srv.Client())
	path, err := l.Pull(context.Background(), "artifact-1", dir)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("pulled %d bytes, want %d", len(got), len(body))
	}
}
