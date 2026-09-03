package packages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// artifact_bytes_test.go -- the zip source's bytes, resolved the way
// sitePublishFromArtifact resolves them.
//
// store.artifactBytes used to call `libraryArtifactBytes`, a builtin nothing
// in the tree declares, so a zip source could never be read on a real cluster
// -- every fake answered it and the render-parse test never drove it. Now it
// is the Library's own two owner-scoped reads plus one object-storage read,
// and the cases below are what those reads must refuse before a byte moves.

func artifactRows(sourceRef string) []map[string]any {
	return []map[string]any{{
		"id":               "v1:library:artifact:zip",
		"kind":             "file",
		"archived":         false,
		"sourceConceptRef": sourceRef,
	}}
}

func fileRows(mime, blobUrl string) []map[string]any {
	return []map[string]any{{
		"id":       "v1:library:file:f1",
		"mimeType": mime,
		"blobUrl":  blobUrl,
		"archived": false,
	}}
}

func TestArtifactBytesResolvesThroughTheLibraryUnderTheCaller(t *testing.T) {
	engine := &actorEngine{}
	engine.rows = map[string][]map[string]any{
		"query libraryArtifactById": artifactRows("v1:library:file:f1"),
		"query libraryFileById":     fileRows("application/zip; charset=binary", "blob://library/u/f1/site.zip"),
	}
	s := &store{engine: engine, logger: discardLogger()}

	var readKey string
	raw, mime, err := s.artifactBytes(callerCtx("v1:identity:user:alice"), "v1:library:artifact:zip",
		func(_ context.Context, key string) ([]byte, error) {
			readKey = key
			return []byte("PK\x03\x04zip"), nil
		})
	if err != nil {
		t.Fatalf("artifactBytes: %v", err)
	}
	if string(raw) != "PK\x03\x04zip" || mime != "application/zip" {
		t.Fatalf("got %q %q", raw, mime)
	}
	// The blob key is the file row's own storage path, both prefixes
	// stripped -- the same normalization sitePublishFromArtifact applies.
	if readKey != "library/u/f1/site.zip" {
		t.Fatalf("read key %q", readKey)
	}

	// Both reads run under the CALLER's actor and origin: the two Library
	// queries are owner-scoped, and that scope IS the authorization -- a
	// caller who does not own the artifact resolves zero rows.
	stmts := engine.statements()
	if len(stmts) != 2 || !strings.HasPrefix(stmts[0], "query libraryArtifactById(") || !strings.HasPrefix(stmts[1], "query libraryFileById(fileId: \"f1\")") {
		t.Fatalf("want the artifact read then the file read by its bare id, got %v", stmts)
	}
	for _, name := range []string{"libraryArtifactById", "libraryFileById"} {
		if got := engine.actors[name]; got != "v1:identity:user:alice" {
			t.Errorf("%s must run under the caller's actor, got %q", name, got)
		}
		if got := engine.origins[name]; got != auth.OriginClient {
			t.Errorf("%s is a caller-scoped read and must NOT be stamped, got %v", name, got)
		}
	}
}

// The reference may arrive canonical or bare; anything naming another
// concept is refused rather than read.
func TestArtifactBytesAcceptsABareFileReference(t *testing.T) {
	engine := &actorEngine{}
	engine.rows = map[string][]map[string]any{
		"query libraryArtifactById": artifactRows("f1"),
		"query libraryFileById":     fileRows("application/zip", "library/u/f1/site.zip"),
	}
	s := &store{engine: engine}
	if _, _, err := s.artifactBytes(context.Background(), "v1:library:artifact:zip",
		func(context.Context, string) ([]byte, error) { return []byte("zip"), nil }); err != nil {
		t.Fatalf("a bare file id is the same reference: %v", err)
	}
}

func TestArtifactBytesRefusesBeforeAByteMoves(t *testing.T) {
	cases := []struct {
		name     string
		rows     map[string][]map[string]any
		readErr  error
		wantText string
	}{
		{
			name:     "no artifact the caller can read",
			rows:     map[string][]map[string]any{},
			wantText: "readable by this caller",
		},
		{
			name: "an archived artifact",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": {{"id": "a", "kind": "file", "archived": true, "sourceConceptRef": "v1:library:file:f1"}},
			},
			wantText: "archived",
		},
		{
			name: "an artifact that is not a file",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": {{"id": "a", "kind": "note", "sourceConceptRef": "v1:library:note:n1"}},
			},
			wantText: "only a file artifact",
		},
		{
			name: "a file reference naming another concept",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": artifactRows("v1:library:note:n1"),
			},
			wantText: "not a v1:library:file",
		},
		{
			name: "a backing file the caller cannot read",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": artifactRows("v1:library:file:f1"),
			},
			wantText: "not visible to this caller",
		},
		{
			name: "a file that is not a zip",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": artifactRows("v1:library:file:f1"),
				"query libraryFileById":     fileRows("text/plain", "library/u/f1/notes.txt"),
			},
			wantText: "must be a zip",
		},
		{
			name: "a file with no storage location",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": artifactRows("v1:library:file:f1"),
				"query libraryFileById":     fileRows("application/zip", ""),
			},
			wantText: "no storage location",
		},
		{
			name: "object storage that cannot be read",
			rows: map[string][]map[string]any{
				"query libraryArtifactById": artifactRows("v1:library:file:f1"),
				"query libraryFileById":     fileRows("application/zip", "library/u/f1/site.zip"),
			},
			readErr:  errors.New("blob not found"),
			wantText: "blob not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &recordingEngine{rows: tc.rows}
			s := &store{engine: engine}
			reads := 0
			_, _, err := s.artifactBytes(context.Background(), "v1:library:artifact:zip",
				func(context.Context, string) ([]byte, error) {
					reads++
					return nil, tc.readErr
				})
			if got := RefusalCode(err); got != CodeSourceUnreadable {
				t.Fatalf("want %s, got %s (%v)", CodeSourceUnreadable, got, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("want the refusal to say %q, got: %v", tc.wantText, err)
			}
			if tc.readErr == nil && reads != 0 {
				t.Fatalf("a refusal on the rows must not read object storage, read %d time(s)", reads)
			}
		})
	}

	// A node with no object-storage reader refuses by name, after the rows
	// resolved, and never pretends the artifact is empty.
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query libraryArtifactById": artifactRows("v1:library:file:f1"),
		"query libraryFileById":     fileRows("application/zip", "library/u/f1/site.zip"),
	}}
	s := &store{engine: engine}
	if _, _, err := s.artifactBytes(context.Background(), "v1:library:artifact:zip", nil); RefusalCode(err) != CodeSourceUnreadable || !strings.Contains(err.Error(), "object storage") {
		t.Fatalf("want a refusal naming object storage, got %v", err)
	}
}
