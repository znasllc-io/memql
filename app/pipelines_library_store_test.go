package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// pipelines_library_store_test.go -- the Library half of a pipeline step
// (epic memql#5478, issue memql#5495), measured at the writer: a step's log or
// artifact reaches the owner's Library as source "pipeline", bound to the work
// run and step, under the owner's own actor -- or, where the Library would not
// take it, nothing is written and the answer says why.
//
// TO CONFIRM THESE ARE LOAD-BEARING: drop the owner check and the first test
// fails; drop the quota read and the quota test fails; send a stamped
// content type instead of the runner's and the content-type test fails.

// pipelinesLibraryCall is one call the store made through the engine, and the
// actor it ran under.
type pipelinesLibraryCall struct {
	query string
	actor string
}

// pipelinesFakeLibraryEngine answers the store's engine calls: the owner's
// three quota reads with one row each of the sizes below, and every write with
// nothing. It records every call with the actor it ran under.
type pipelinesFakeLibraryEngine struct {
	mu    sync.Mutex
	calls []pipelinesLibraryCall
	// The owner's footprint, as libraryFileSizesForOwner,
	// libraryFileVersionSizesForOwner and openUploadSessionsForOwner sum it.
	fileBytes, versionBytes, sessionBytes int64
	// failOn fails every call whose query contains it.
	failOn string
}

var _ server.MemQLExecutor = (*pipelinesFakeLibraryEngine)(nil)

func (f *pipelinesFakeLibraryEngine) Execute(ctx context.Context, q string) (any, error) {
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		actor = ac.UserId
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pipelinesLibraryCall{query: q, actor: actor})
	if f.failOn != "" && strings.Contains(q, f.failOn) {
		return nil, fmt.Errorf("the engine refused %s", f.failOn)
	}
	sized := func(n int64) any {
		return memqlengine.NewResultWithOutput([]any{map[string]any{"size": n}})
	}
	switch {
	case strings.Contains(q, "libraryFileSizesForOwner("):
		return sized(f.fileBytes), nil
	case strings.Contains(q, "libraryFileVersionSizesForOwner("):
		return sized(f.versionBytes), nil
	case strings.Contains(q, "openUploadSessionsForOwner("):
		return sized(f.sessionBytes), nil
	}
	return memqlengine.NewResultWithOutput(nil), nil
}

func (f *pipelinesFakeLibraryEngine) recorded() []pipelinesLibraryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pipelinesLibraryCall(nil), f.calls...)
}

// callsTo is every recorded call to the named construct, in order.
func (f *pipelinesFakeLibraryEngine) callsTo(construct string) []pipelinesLibraryCall {
	var out []pipelinesLibraryCall
	for _, c := range f.recorded() {
		if strings.Contains(c.query, construct+"(") {
			out = append(out, c)
		}
	}
	return out
}

// pipelinesUpload is one object the fake storage was handed.
type pipelinesUpload struct {
	bucket, object, contentType string
	size                        int
}

type pipelinesFakeUploader struct {
	mu      sync.Mutex
	uploads []pipelinesUpload
	err     error
}

var _ server.FileUploader = (*pipelinesFakeUploader)(nil)

func (u *pipelinesFakeUploader) Upload(_ context.Context, bucket, object string, data []byte, contentType string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return "", u.err
	}
	u.uploads = append(u.uploads, pipelinesUpload{bucket: bucket, object: object, contentType: contentType, size: len(data)})
	return "https://blob.example/" + bucket + "/" + object, nil
}

func (u *pipelinesFakeUploader) recorded() []pipelinesUpload {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]pipelinesUpload(nil), u.uploads...)
}

const pipelinesTestOwner = "v1:identity:user:alice"

// newPipelinesStoreForTest is the store over fakes, with limits far above any
// file here so a test that is not about them never meets them.
func newPipelinesStoreForTest(t *testing.T) (*pipelinesLibraryStore, *pipelinesFakeLibraryEngine, *pipelinesFakeUploader) {
	t.Helper()
	eng := &pipelinesFakeLibraryEngine{}
	up := &pipelinesFakeUploader{}
	s := newPipelinesLibraryStore(eng, up, "memql", quietLogger())
	s.maxBytes, s.quotaBytes = 1<<20, 1<<30
	return s, eng, up
}

// pipelineLogFile is a step's archived log, as the runner hands it over.
func pipelineLogFile() pipelinesteps.RunFile {
	return pipelinesteps.RunFile{
		OwnerUserID: pipelinesTestOwner,
		WorkRunID:   "run-7",
		StepKey:     "tests.unit",
		Name:        "tests.unit.log",
		MimeType:    "text/plain; charset=utf-8",
		Bytes:       []byte("ok  \tgithub.com/acme/widget\t0.412s\n"),
	}
}

// pipelinesCallArg reads one argument out of a rendered call: a JSON string,
// number or boolean, or nil when the call does not carry it.
func pipelinesCallArg(t *testing.T, call, name string) any {
	t.Helper()
	re := regexp.MustCompile(`(?:\(|, )` + regexp.QuoteMeta(name) + `: ("(?:[^"\\]|\\.)*"|-?[0-9]+|true|false)`)
	m := re.FindStringSubmatch(call)
	if m == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
		t.Fatalf("argument %s of %s is not a JSON literal: %v", name, call, err)
	}
	return v
}

// TestAPipelineFileLandsInItsOwnersLibrary -- the whole write, in order: the
// owner's footprint is read, the bytes go to the Library's own object path,
// the row is created as source "pipeline" bound to the work run and step, and
// the file is marked ready, because nothing analyzes a pipeline's file. Every
// read and write runs as the OWNER: the row is theirs, and theirs to read.
func TestAPipelineFileLandsInItsOwnersLibrary(t *testing.T) {
	s, eng, up := newPipelinesStoreForTest(t)
	f := pipelineLogFile()

	got, err := s.StoreRunFile(context.Background(), f)
	if err != nil {
		t.Fatalf("StoreRunFile: %v", err)
	}
	if got.FileID == "" || got.Omitted != "" {
		t.Fatalf("StoreRunFile = %+v, want a stored file", got)
	}

	uploads := up.recorded()
	if len(uploads) != 1 {
		t.Fatalf("uploaded %d objects, want 1", len(uploads))
	}
	wantObject := "library/" + pipelinesTestOwner + "/" + got.FileID + "/tests.unit.log"
	if uploads[0].bucket != "memql" || uploads[0].object != wantObject {
		t.Errorf("the bytes went to %s/%s, want memql/%s -- the upload route's layout, so one bucket has one",
			uploads[0].bucket, uploads[0].object, wantObject)
	}

	creates := eng.callsTo("createLibraryFile")
	if len(creates) != 1 {
		t.Fatalf("wrote %d file rows, want 1", len(creates))
	}
	create := creates[0].query
	sum := sha256.Sum256(f.Bytes)
	for name, want := range map[string]any{
		"fileId":            got.FileID,
		"source":            "pipeline",
		"producedByRunId":   "run-7",
		"producedByStepKey": "tests.unit",
		"name":              "tests.unit.log",
		"mimeType":          "text/plain; charset=utf-8",
		"format":            "text",
		"size":              float64(len(f.Bytes)),
		"sha256":            hex.EncodeToString(sum[:]),
		"blobUrl":           "https://blob.example/memql/" + wantObject,
	} {
		if got := pipelinesCallArg(t, create, name); got != want {
			t.Errorf("the file row's %s = %v, want %v\n%s", name, got, want, create)
		}
	}

	readies := eng.callsTo("setLibraryFileStatus")
	if len(readies) != 1 {
		t.Fatalf("made %d status transitions, want 1 (ready)", len(readies))
	}
	if id, status := pipelinesCallArg(t, readies[0].query, "fileId"), pipelinesCallArg(t, readies[0].query, "status"); id != got.FileID || status != "ready" {
		t.Errorf("the transition = %s, want this file marked ready", readies[0].query)
	}

	// The order is the contract: measured, created, then ready.
	var order []string
	for _, c := range eng.recorded() {
		switch {
		case strings.Contains(c.query, "SizesForOwner(") || strings.Contains(c.query, "openUploadSessionsForOwner("):
			if len(order) == 0 || order[len(order)-1] != "footprint" {
				order = append(order, "footprint")
			}
		case strings.Contains(c.query, "createLibraryFile("):
			order = append(order, "create")
		case strings.Contains(c.query, "setLibraryFileStatus("):
			order = append(order, "ready")
		}
		if c.actor != pipelinesTestOwner {
			t.Errorf("%q ran as %q, want the owner %q -- a row written under any other actor is one its owner cannot find",
				c.query, c.actor, pipelinesTestOwner)
		}
	}
	if strings.Join(order, ",") != "footprint,create,ready" {
		t.Errorf("the calls ran in the order %v, want footprint, create, ready", order)
	}
}

// TestAPipelineFileKeepsTheContentTypeTheRunnerGaveIt (ruling R32b): the row
// says what the runner said. A step's report.html stays text/html and its
// badge.svg image/svg+xml: the Library serves every byte as an attachment and
// the OS saves through an anchor, so neither is rendered in the OS's origin,
// and a stamped type would make a person's own artifact lie about what it is.
func TestAPipelineFileKeepsTheContentTypeTheRunnerGaveIt(t *testing.T) {
	for _, tc := range []struct {
		name, mimeType, format string
	}{
		{"dist__report.html", "text/html; charset=utf-8", "text"},
		{"badge.svg", "image/svg+xml", "image"},
		{"coverage.out", "application/octet-stream", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, eng, up := newPipelinesStoreForTest(t)
			f := pipelineLogFile()
			f.Name, f.MimeType = tc.name, tc.mimeType
			if _, err := s.StoreRunFile(context.Background(), f); err != nil {
				t.Fatalf("StoreRunFile: %v", err)
			}
			uploads, creates := up.recorded(), eng.callsTo("createLibraryFile")
			if len(uploads) != 1 || len(creates) != 1 {
				t.Fatalf("%d uploads and %d rows, want one of each", len(uploads), len(creates))
			}
			if uploads[0].contentType != tc.mimeType {
				t.Errorf("the object's content type = %q, want the runner's %q", uploads[0].contentType, tc.mimeType)
			}
			if got := pipelinesCallArg(t, creates[0].query, "mimeType"); got != tc.mimeType {
				t.Errorf("the row's mimeType = %v, want the runner's %q", got, tc.mimeType)
			}
			if got := pipelinesCallArg(t, creates[0].query, "format"); got != tc.format {
				t.Errorf("the row's format = %v, want %q, the Library's own reading of that type", got, tc.format)
			}
		})
	}
}

// TestAPipelineFileWithNoOwnerIsRefused. The row's owner is the row's only
// reader, and with no owner to borrow the call would run as whoever the
// context already names -- on the workbench, the engine's own forward.
func TestAPipelineFileWithNoOwnerIsRefused(t *testing.T) {
	s, eng, up := newPipelinesStoreForTest(t)
	f := pipelineLogFile()
	f.OwnerUserID = "  "
	if got, err := s.StoreRunFile(context.Background(), f); err == nil {
		t.Fatalf("a file with no owner was answered %+v, want an error", got)
	}
	if len(up.recorded()) != 0 || len(eng.recorded()) != 0 {
		t.Errorf("it moved bytes or asked the engine anyway: %d uploads, %d calls", len(up.recorded()), len(eng.recorded()))
	}
}

// TestAPipelineFileOverTheLibraryLimitIsOmitted. The upload route's limit on
// one file holds for a pipeline's too, and a file over it is a note beside the
// step: nothing is read, moved or written.
func TestAPipelineFileOverTheLibraryLimitIsOmitted(t *testing.T) {
	s, eng, up := newPipelinesStoreForTest(t)
	s.maxBytes = 8
	f := pipelineLogFile()
	f.Bytes = []byte("123456789")

	got, err := s.StoreRunFile(context.Background(), f)
	if err != nil {
		t.Fatalf("a file over the limit is an omission, not an error: %v", err)
	}
	if got.FileID != "" || !strings.Contains(got.Omitted, "8 bytes") || !strings.Contains(got.Omitted, server.LibraryMaxUploadBytesEnv) {
		t.Errorf("StoreRunFile = %+v, want an omission naming the limit and its setting", got)
	}
	if len(up.recorded()) != 0 || len(eng.recorded()) != 0 {
		t.Errorf("an oversized file moved bytes or asked the engine: %d uploads, %d calls", len(up.recorded()), len(eng.recorded()))
	}

	// The reachable positive: a file AT the limit is stored.
	f.Bytes = []byte("12345678")
	if got, err := s.StoreRunFile(context.Background(), f); err != nil || got.FileID == "" {
		t.Errorf("a file at the limit = %+v, %v; want it stored", got, err)
	}
}

// TestAPipelineFileOverTheOwnersQuotaIsOmitted. Everything the upload route
// counts counts here -- stored files, every earlier version, open upload
// sessions -- plus this file; at the quota is admitted, past it is not.
func TestAPipelineFileOverTheOwnersQuotaIsOmitted(t *testing.T) {
	s, eng, up := newPipelinesStoreForTest(t)
	s.quotaBytes = 100
	eng.fileBytes, eng.versionBytes, eng.sessionBytes = 40, 30, 20
	f := pipelineLogFile()
	f.Bytes = []byte("12345678901") // 11 bytes: 101 in all

	got, err := s.StoreRunFile(context.Background(), f)
	if err != nil {
		t.Fatalf("a file over the quota is an omission, not an error: %v", err)
	}
	if got.FileID != "" || !strings.Contains(got.Omitted, "101") || !strings.Contains(got.Omitted, "100") ||
		!strings.Contains(got.Omitted, server.LibraryUserQuotaBytesEnv) {
		t.Errorf("StoreRunFile = %+v, want an omission naming both numbers and the quota's setting", got)
	}
	if len(up.recorded()) != 0 || len(eng.callsTo("createLibraryFile")) != 0 {
		t.Error("a file over the quota moved bytes or wrote a row")
	}
	for _, c := range eng.recorded() {
		if c.actor != pipelinesTestOwner {
			t.Errorf("the footprint was read as %q, want the owner: it is THEIR Library being measured", c.actor)
		}
	}

	f.Bytes = []byte("1234567890") // 10 bytes: exactly 100
	if got, err := s.StoreRunFile(context.Background(), f); err != nil || got.FileID == "" {
		t.Errorf("a file that brings the Library exactly to its quota = %+v, %v; want it stored", got, err)
	}
}

// TestAPipelineFileWithNoStorageIsOmitted. A cluster without object storage
// keeps a step's outcome and loses only its Library copy, and says so.
func TestAPipelineFileWithNoStorageIsOmitted(t *testing.T) {
	for _, tc := range []struct {
		what     string
		uploader server.FileUploader
		bucket   string
	}{
		{"no uploader", nil, "memql"},
		{"no container", &pipelinesFakeUploader{}, " "},
	} {
		t.Run(tc.what, func(t *testing.T) {
			eng := &pipelinesFakeLibraryEngine{}
			s := newPipelinesLibraryStore(eng, tc.uploader, tc.bucket, quietLogger())
			got, err := s.StoreRunFile(context.Background(), pipelineLogFile())
			if err != nil {
				t.Fatalf("no storage is an omission, not an error: %v", err)
			}
			if got.FileID != "" || !strings.Contains(got.Omitted, "object storage") {
				t.Errorf("StoreRunFile = %+v, want an omission saying there is no object storage", got)
			}
			if len(eng.recorded()) != 0 {
				t.Errorf("it asked the engine %d times with nowhere to put the bytes", len(eng.recorded()))
			}
		})
	}
}

// TestThePipelineStoreReadsTheLibrarysOwnLimits. Left at zero, the two limits
// are the upload route's own settings -- one Library, one pair of numbers --
// rather than a second pair that could disagree with them.
func TestThePipelineStoreReadsTheLibrarysOwnLimits(t *testing.T) {
	eng := &pipelinesFakeLibraryEngine{fileBytes: 8}
	s := newPipelinesLibraryStore(eng, &pipelinesFakeUploader{}, "memql", quietLogger())
	f := pipelineLogFile()
	f.Bytes = []byte("12345")

	t.Setenv(server.LibraryMaxUploadBytesEnv, "4")
	got, err := s.StoreRunFile(context.Background(), f)
	if err != nil || !strings.Contains(got.Omitted, server.LibraryMaxUploadBytesEnv) {
		t.Errorf("with %s=4 a 5-byte file = %+v, %v; want it omitted for the limit", server.LibraryMaxUploadBytesEnv, got, err)
	}

	t.Setenv(server.LibraryMaxUploadBytesEnv, "1024")
	t.Setenv(server.LibraryUserQuotaBytesEnv, "10")
	got, err = s.StoreRunFile(context.Background(), f)
	if err != nil || !strings.Contains(got.Omitted, server.LibraryUserQuotaBytesEnv) {
		t.Errorf("with %s=10 and 8 bytes stored, a 5-byte file = %+v, %v; want it omitted for the quota",
			server.LibraryUserQuotaBytesEnv, got, err)
	}
}

// TestAPipelineFileNameNeverEscapesItsPrefix. The name is the last segment of
// the object path, so a separator in it would write outside the file's own
// prefix. The runner's names are already flat; the Library's writer does not
// lean on that.
func TestAPipelineFileNameNeverEscapesItsPrefix(t *testing.T) {
	s, _, up := newPipelinesStoreForTest(t)
	f := pipelineLogFile()
	f.Name = "dist/../..\\..\\etc/passwd"
	if _, err := s.StoreRunFile(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	uploads := up.recorded()
	if len(uploads) != 1 {
		t.Fatal("nothing was uploaded")
	}
	// Exactly the three separators library/{owner}/{fileId}/{name} has.
	if object := uploads[0].object; strings.Count(object, "/") != 3 || strings.Contains(object, "\\") {
		t.Errorf("object path %q escaped library/{owner}/{fileId}/{name}", object)
	}
}

// TestAPipelineFileWithNoRunIsFiledUnbound. A blank is not a binding: the row
// carries producedByRunId and producedByStepKey only when the runner named
// them, rather than an empty reference.
func TestAPipelineFileWithNoRunIsFiledUnbound(t *testing.T) {
	s, eng, _ := newPipelinesStoreForTest(t)
	f := pipelineLogFile()
	f.WorkRunID, f.StepKey = " ", ""
	if _, err := s.StoreRunFile(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	creates := eng.callsTo("createLibraryFile")
	if len(creates) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(creates))
	}
	if strings.Contains(creates[0].query, "producedByRunId") || strings.Contains(creates[0].query, "producedByStepKey") {
		t.Errorf("an unbound file was written with a blank binding:\n%s", creates[0].query)
	}
}

// TestAPipelineFileTheLibraryCouldNotRecordIsAnError. A failure of the
// machinery -- the footprint unreadable, the storage or the engine refusing --
// is an error, which the runner logs and turns into a note; an omission is
// kept for the Library's own rules. The ready transition is the exception:
// the file IS stored by then, so its id is the answer.
func TestAPipelineFileTheLibraryCouldNotRecordIsAnError(t *testing.T) {
	t.Run("the footprint cannot be read", func(t *testing.T) {
		s, eng, up := newPipelinesStoreForTest(t)
		eng.failOn = "libraryFileSizesForOwner"
		if got, err := s.StoreRunFile(context.Background(), pipelineLogFile()); err == nil {
			t.Fatalf("an unmeasurable Library was answered %+v, want an error -- a quota read that fails must not admit", got)
		}
		if len(up.recorded()) != 0 {
			t.Error("bytes moved without the quota having been read")
		}
	})
	t.Run("the storage refuses", func(t *testing.T) {
		s, eng, up := newPipelinesStoreForTest(t)
		up.err = errors.New("the blob store is unreachable")
		if _, err := s.StoreRunFile(context.Background(), pipelineLogFile()); err == nil {
			t.Fatal("a refused upload was not an error")
		}
		if len(eng.callsTo("createLibraryFile")) != 0 {
			t.Error("a row was written for bytes that never landed")
		}
	})
	t.Run("the row cannot be written", func(t *testing.T) {
		s, eng, _ := newPipelinesStoreForTest(t)
		eng.failOn = "createLibraryFile"
		if got, err := s.StoreRunFile(context.Background(), pipelineLogFile()); err == nil || got.FileID != "" {
			t.Fatalf("a refused row = %+v, %v; want an error and no file id", got, err)
		}
	})
	t.Run("the file cannot be marked ready", func(t *testing.T) {
		s, eng, _ := newPipelinesStoreForTest(t)
		eng.failOn = "setLibraryFileStatus"
		got, err := s.StoreRunFile(context.Background(), pipelineLogFile())
		if err != nil || got.FileID == "" {
			t.Fatalf("a stored file whose ready transition failed = %+v, %v; want its id -- the bytes and the row exist", got, err)
		}
	})
	t.Run("there is no engine", func(t *testing.T) {
		up := &pipelinesFakeUploader{}
		s := newPipelinesLibraryStore(nil, up, "memql", quietLogger())
		if _, err := s.StoreRunFile(context.Background(), pipelineLogFile()); err == nil {
			t.Fatal("a store with no engine answered without an error")
		}
		if len(up.recorded()) != 0 {
			t.Error("bytes moved with no engine to record them")
		}
	})
}
