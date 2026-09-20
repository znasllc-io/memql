//go:build agent

package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// appsession_content_store_test.go -- issue memql#5400's two acceptances,
// measured at the writer rather than through a session.
//
// TO CONFIRM THESE ARE LOAD-BEARING: remove the sha256 lookup and the first
// fails; return an error instead of an Omitted result above the cap and the
// second does.

type fakeContentEngine struct {
	mu      sync.Mutex
	queries []string
	// byDigest answers libraryFileBySha256 with an existing file id.
	byDigest map[string]string
	err      error
}

func (f *fakeContentEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	if strings.HasPrefix(q, "query libraryFileBySha256") {
		for digest, fileId := range f.byDigest {
			if strings.Contains(q, digest) {
				return memqlengine.NewResultWithOutput([]any{map[string]any{"id": fileId}}), nil
			}
		}
		return memqlengine.NewResultWithOutput(nil), nil
	}
	// A create: remember the digest so the next identical content dedups,
	// the way the real query would.
	if strings.HasPrefix(q, "mutation createLibraryFile") {
		if f.byDigest == nil {
			f.byDigest = map[string]string{}
		}
		digest := argValue(q, "sha256")
		f.byDigest[digest] = argValue(q, "fileId")
	}
	return memqlengine.NewResultWithOutput(nil), nil
}

func (f *fakeContentEngine) creates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, q := range f.queries {
		if strings.HasPrefix(q, "mutation createLibraryFile") {
			out = append(out, q)
		}
	}
	return out
}

// argValue pulls a quoted string argument out of a rendered call.
func argValue(q, name string) string {
	i := strings.Index(q, name+": \"")
	if i < 0 {
		return ""
	}
	rest := q[i+len(name)+3:]
	j := strings.Index(rest, "\"")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

type fakeUploader struct {
	mu      sync.Mutex
	objects []string
	err     error
}

func (u *fakeUploader) Upload(_ context.Context, bucket, object string, _ []byte, _ string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return "", u.err
	}
	u.objects = append(u.objects, object)
	return "https://blob.example/" + bucket + "/" + object, nil
}

func (u *fakeUploader) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.objects)
}

var _ server.FileUploader = (*fakeUploader)(nil)

func newContentStore(t *testing.T, maxBytes int) (*appSessionContentStore, *fakeContentEngine, *fakeUploader) {
	t.Helper()
	eng := &fakeContentEngine{}
	up := &fakeUploader{}
	return &appSessionContentStore{
		engine: eng, uploader: up, bucket: "memql", maxBytes: maxBytes,
		// A nil EngineLibraryStore is fine: nothing here goes through
		// CreateFile, because that struct may not carry producedBy and this
		// writer composes the call itself.
		store: server.NewEngineLibraryStore(nil),
	}, eng, up
}

func contentRequest(name string, body []byte) workerservice.ContentRequest {
	return workerservice.ContentRequest{
		OwnerUserId: "v1:identity:user:alice",
		Name:        name,
		MimeType:    "text/plain",
		Bytes:       body,
		Provenance: workerservice.ArtifactProvenance{
			App: "claude-code", Model: "claude-sonnet-4-6", SessionId: "v1:worker:appSession:s1",
		},
	}
}

// TestTwoIdenticalContentsProduceOneFile -- issue #5400's first acceptance.
//
// `v1:library:file.sha256` has carried the words "a DEDUP HINT and an
// integrity check" since the concept existed and nothing read it that way. An
// agent reads one file in three steps and writes it back twice; without this
// that is five copies in somebody's Library.
func TestTwoIdenticalContentsProduceOneFile(t *testing.T) {
	s, eng, up := newContentStore(t, 0)
	body := []byte("package main\n")

	first, err := s.StoreContent(context.Background(), contentRequest("main.go", body))
	if err != nil {
		t.Fatalf("first StoreContent: %v", err)
	}
	second, err := s.StoreContent(context.Background(), contentRequest("main.go", body))
	if err != nil {
		t.Fatalf("second StoreContent: %v", err)
	}

	if first.FileId == "" || first.Deduplicated {
		t.Fatalf("the first store = %+v, want a fresh file", first)
	}
	if !second.Deduplicated {
		t.Error("the second identical content was not recognised as a duplicate")
	}
	if second.FileId != first.FileId {
		t.Errorf("the two contents landed at %q and %q, want ONE file referenced twice -- "+
			"a branch from a recorded step must cost no copy", first.FileId, second.FileId)
	}
	if n := len(eng.creates()); n != 1 {
		t.Errorf("wrote %d file rows, want 1", n)
	}
	if n := up.count(); n != 1 {
		t.Errorf("uploaded %d blobs, want 1 -- the dedup must happen BEFORE the bytes move", n)
	}
	if first.Sha256 == "" || second.Sha256 != first.Sha256 {
		t.Errorf("the digest must be reported either way: %q / %q", first.Sha256, second.Sha256)
	}
}

// TestDifferentContentsAreDifferentFiles -- the control for the test above.
// A dedup that matched everything would pass it while measuring nothing.
func TestDifferentContentsAreDifferentFiles(t *testing.T) {
	s, eng, _ := newContentStore(t, 0)
	a, err := s.StoreContent(context.Background(), contentRequest("a.txt", []byte("one")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.StoreContent(context.Background(), contentRequest("b.txt", []byte("two")))
	if err != nil {
		t.Fatal(err)
	}
	if a.FileId == b.FileId || b.Deduplicated {
		t.Fatalf("two different contents collapsed into one file: %+v / %+v", a, b)
	}
	if n := len(eng.creates()); n != 2 {
		t.Errorf("wrote %d file rows, want 2", n)
	}
}

// TestAContentAboveTheCapIsOmittedWithItsDigest -- issue #5400's second
// acceptance, plus design D5's rule that a content above the cap is
// referenced BY DIGEST. Without the digest, two runs that read the same large
// file could not be told from two that read different ones.
func TestAContentAboveTheCapIsOmittedWithItsDigest(t *testing.T) {
	s, eng, up := newContentStore(t, 8)
	res, err := s.StoreContent(context.Background(), contentRequest("huge.bin", []byte("far too many bytes")))
	if err != nil {
		t.Fatalf("StoreContent returned an error; an oversized content must never fail the session: %v", err)
	}
	if res.FileId != "" {
		t.Errorf("an omitted content returned a file id %q", res.FileId)
	}
	if res.Sha256 == "" {
		t.Error("an omitted content must still report its digest -- it is referenced by digest only")
	}
	if !strings.Contains(res.Omitted, "cap") {
		t.Errorf("Omitted = %q, want it to say why", res.Omitted)
	}
	if up.count() != 0 || len(eng.creates()) != 0 {
		t.Error("an oversized content moved bytes or wrote a row")
	}
}

// TestAStoreFailureIsOmittedNotFatal. The session already happened on
// somebody's machine; losing one stored copy costs a reader a journey, and
// failing the run would cost them the work.
func TestAStoreFailureIsOmittedNotFatal(t *testing.T) {
	s, _, up := newContentStore(t, 0)
	up.err = fmt.Errorf("the blob store is unreachable")

	res, err := s.StoreContent(context.Background(), contentRequest("a.txt", []byte("hello")))
	if err != nil {
		t.Fatalf("a failed upload must not be an error: %v", err)
	}
	if res.FileId != "" || res.Omitted == "" {
		t.Fatalf("result = %+v, want an omission carrying the reason", res)
	}
	if res.Sha256 == "" {
		t.Error("the digest survives a failed store")
	}
}

// TestTheFileIsStampedWithWhatProducedIt (epic memql#5391, design D9). The
// stamp is composed here rather than through server.LibraryFileCreateParams,
// which TestTheUploadRouteCannotStampProvenance forbids from carrying it --
// because "absence is the answer" for a human upload and the opposite is true
// for these bytes.
func TestTheFileIsStampedWithWhatProducedIt(t *testing.T) {
	s, eng, _ := newContentStore(t, 0)
	if _, err := s.StoreContent(context.Background(), contentRequest("a.txt", []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	creates := eng.creates()
	if len(creates) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(creates))
	}
	q := creates[0]
	for _, want := range []string{`"app_session"`, "producedBy", "claude-code", "v1:worker:appSession:s1"} {
		if !strings.Contains(q, want) {
			t.Errorf("the create call does not carry %s:\n%s", want, q)
		}
	}
	// NOT agent_generated: that means an agent produced a standalone
	// deliverable, and a person filtering their Library for what an agent
	// MADE them should not be shown an app's working files.
	if strings.Contains(q, "agent_generated") {
		t.Errorf("a recorded content was filed as agent_generated:\n%s", q)
	}
}

// TestAContentWithNoOwnerIsRefused. The row's owner is the row's only reader.
func TestAContentWithNoOwnerIsRefused(t *testing.T) {
	s, eng, up := newContentStore(t, 0)
	req := contentRequest("a.txt", []byte("hello"))
	req.OwnerUserId = "  "
	if _, err := s.StoreContent(context.Background(), req); err == nil {
		t.Fatal("a content with no owner was filed")
	}
	if up.count() != 0 || len(eng.creates()) != 0 {
		t.Error("it wrote anyway")
	}
}

// TestAContentNameNeverEscapesItsPrefix. The name is the last segment of the
// blob path, so one carrying a SEPARATOR would write outside the file's own
// prefix -- and the harness reports a PATH, not a base name.
//
// The assertion is about separators and not about dots, deliberately. A blob
// key is a flat string with no traversal semantics, so `..` inside one
// segment is inert; stripping it would also have to decide what to do with
// `.gitignore`, and a sanitizer that mangles legitimate names to defend
// against a mechanism that does not exist is worse than one that does not.
func TestAContentNameNeverEscapesItsPrefix(t *testing.T) {
	s, _, up := newContentStore(t, 0)
	req := contentRequest("../../etc/passwd", []byte("hello"))
	if _, err := s.StoreContent(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if up.count() != 1 {
		t.Fatal("nothing was uploaded")
	}
	object := up.objects[0]
	// Exactly the three separators library/{owner}/{fileId}/{name} has.
	if strings.Count(object, "/") != 3 || strings.Contains(object, "\\") {
		t.Errorf("object path %q escaped library/{owner}/{fileId}/{name}", object)
	}
	if !strings.HasSuffix(object, "etc-passwd") {
		t.Errorf("object path %q did not flatten the reported path into one segment", object)
	}
}
