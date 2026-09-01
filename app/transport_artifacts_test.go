//go:build bff

package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

// recordingRunner captures what the adapter handed the analysis pass.
type recordingRunner struct {
	got    library.AnalyzeFileParams
	called int
}

func (r *recordingRunner) AnalyzeFile(_ context.Context, p library.AnalyzeFileParams) error {
	r.got = p
	r.called++
	return nil
}

// The upload route's adapter must carry every field the analysis pass declares
// (memql#4341 -> memql#4342).
//
// WHY A TEST AND NOT A REVIEW. This is a struct-to-struct copy across a module
// seam, which is the exact shape where a field added on one side is forgotten
// on the other. Nothing fails when that happens: library.AnalyzeFileParams is a
// plain struct, so the missing field arrives as its zero value and the pass runs
// with an empty Name or an empty MimeType. The result is a worse summary, or a
// file classified as having no known reader -- both indistinguishable from the
// file simply being like that.
//
// So the assertion is driven off REFLECTION over the destination struct rather
// than a hand-written field list: a new field on AnalyzeFileParams fails this
// test until the adapter is taught to fill it, which is the only version of this
// check that survives the next person to add one.
func TestLibraryAnalyzerAdapterCarriesEveryParam(t *testing.T) {
	rec := &recordingRunner{}
	adapter := libraryAnalyzerAdapter{lib: rec}

	req := server.LibraryAnalysisRequest{
		FileId:      "file-1",
		ArtifactId:  "artifact-1",
		OwnerUserId: "user-a",
		Name:        "notes.txt",
		MimeType:    "text/plain",
		Data:        []byte("hello"),
		// The chunked pair (memql#4782): the committed blob's address, and
		// the hash when a one-shot upload already computed it. The fixture
		// fills every source field the adapter maps from, so the
		// destination-driven assertion below measures the COPY, not the
		// fixture.
		BlobUrl: "library/user-a/file-1/notes.txt",
		Sha256:  "sha-1",
	}
	adapter.AnalyzeFile(context.Background(), req)

	if rec.called != 1 {
		t.Fatalf("the pass was called %d times, want exactly 1", rec.called)
	}

	// Every field of the destination must be non-zero, given a request that
	// populated every source field it maps from.
	v := reflect.ValueOf(rec.got)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("library.AnalyzeFileParams.%s arrived ZERO.\n"+
				"The adapter in transport_artifacts.go does not copy it, so the analysis pass runs "+
				"without it -- silently, because a missing field is its zero value and not an error. "+
				"Add it to libraryAnalyzerAdapter.AnalyzeFile (and to server.LibraryAnalysisRequest "+
				"if the handler does not carry it yet).", f.Name)
		}
	}
}

// The adapter must satisfy the seam the handler declares, or the route silently
// falls back to marking every file `ready` with no analysis at all.
func TestLibraryAnalyzerAdapterSatisfiesTheHandlerSeam(t *testing.T) {
	var _ server.LibraryAnalyzer = libraryAnalyzerAdapter{lib: &recordingRunner{}}
}
