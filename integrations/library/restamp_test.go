package library

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// restamp_test.go -- the capability the upload route calls after a supersede
// (epic memql#4806).
//
// The claim is narrow and the hazard is the #4288 one, reached from a third
// direction. A new version changes facts the INDEX carries -- the title when
// the bytes arrived under a different name, the format, the watermark the
// Library sorts and pulses on -- and createArtifact's body is a bare
// `insert{}`, so a re-stamp that forgot to carry `labels`, `archived` or
// `folderId` would wipe them as a side effect of uploading a new version.
//
// The tests below drive the REAL capability handler, so they fail against a
// re-stamp that writes its own createArtifact call instead of going through
// the one the analysis pass already uses.

func restamp(t *testing.T, i *Integration, ctx context.Context, fileId string) (bool, error) {
	t.Helper()
	nodes, err := i.handleRestampFileArtifact(ctx, map[string]any{"fileId": fileId}, 0)
	if err != nil {
		return false, err
	}
	if len(nodes) != 1 {
		t.Fatalf("the re-stamp returned %d nodes, want 1", len(nodes))
	}
	var out restampResult
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decode restamp result: %v", err)
	}
	return out.Restamped, nil
}

// A NEW VERSION'S NAME REACHES THE LIST, and the index-only fields survive.
func TestRestampFileArtifact_PushesTheNewNameAndKeepsTheCarryForward(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "q3.pdf", "application/pdf", "pdf")
	artifactId := s.seedPromotedArtifact("file-1", "user-a", []string{"reports", "q3"})
	s.artifacts[artifactId]["folderId"] = "fold-1"
	i := NewIntegration(s)

	// The supersede has already moved the head: a new name, a new format.
	s.files["file-1"]["name"] = "q3-final.pdf"
	s.files["file-1"]["format"] = "document"
	s.files["file-1"]["mimeType"] = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	ctx := auth.ContextWithUserActor(context.Background(), "user-a")
	restamped, err := restamp(t, i, ctx, "file-1")
	if err != nil {
		t.Fatalf("restamp: %v", err)
	}
	if !restamped {
		t.Fatal("the re-stamp reported it did nothing, against a promoted artifact")
	}

	row := s.artifacts[artifactId]
	if got := asString(row["title"]); got != "q3-final.pdf" {
		t.Fatalf("the index still says %q after a new version arrived under a different name -- "+
			"without the re-stamp the list shows the old name until analysis happens to finish", got)
	}
	if got := asString(row["format"]); got != "document" {
		t.Errorf("format = %q, want the new version's own", got)
	}
	// THE CARRY-FORWARD, all three members. A re-stamp that dropped any of
	// them would wipe labels, un-archive a deleted row, or silently re-file
	// the artifact at root -- each as a side effect of an upload.
	labels := stringSliceField(row, "labels")
	if len(labels) != 2 || labels[0] != "reports" || labels[1] != "q3" {
		t.Errorf("labels = %v after a version re-stamp, want [reports q3]", labels)
	}
	if got := asString(row["folderId"]); got != "fold-1" {
		t.Errorf("folderId = %q after a version re-stamp, want fold-1 -- the filing lives on the "+
			"INDEX only, so an omitted field re-files the artifact at root", got)
	}
	if boolField(row, "archived") {
		t.Error("the re-stamp archived an unarchived artifact")
	}
}

// AN ARCHIVED ARTIFACT STAYS ARCHIVED. Uploading a new version of a file the
// owner threw away must not bring it back into the Library.
func TestRestampFileArtifact_KeepsAnArchivedRowArchived(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "old.pdf", "application/pdf", "pdf")
	artifactId := s.seedPromotedArtifact("file-1", "user-a", []string{"reports"})
	s.artifacts[artifactId]["archived"] = true
	i := NewIntegration(s)

	ctx := auth.ContextWithUserActor(context.Background(), "user-a")
	if _, err := restamp(t, i, ctx, "file-1"); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	if !boolField(s.artifacts[artifactId], "archived") {
		t.Fatalf("archived = false after re-stamping an archived artifact -- a new version "+
			"resurrected a row the owner deleted.\n  row: %v", s.artifacts[artifactId])
	}
}

// SOMEBODY ELSE'S FILE IS NOT THERE. The read runs under the caller's own
// actor, so "not yours" and "not there" are one answer -- and the refusal
// must not say which.
func TestRestampFileArtifact_RefusesAFileTheCallerCannotRead(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "mine.pdf", "application/pdf", "pdf")
	artifactId := s.seedPromotedArtifact("file-1", "user-a", []string{"reports"})
	i := NewIntegration(s)

	// The reachable positive: the owner's re-stamp works, so the stranger's
	// refusal is about admission rather than about a broken fixture.
	if _, err := restamp(t, i, auth.ContextWithUserActor(context.Background(), "user-a"), "file-1"); err != nil {
		t.Fatalf("the owner's re-stamp failed (%v); the negative below would prove nothing", err)
	}
	before := asString(s.artifacts[artifactId]["title"])

	_, err := restamp(t, i, auth.ContextWithUserActor(context.Background(), "user-b"), "file-1")
	if err == nil {
		t.Fatal("a stranger re-stamped somebody else's artifact")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("the refusal is %q; it must not distinguish 'not yours' from 'not there'", err)
	}
	if got := asString(s.artifacts[artifactId]["title"]); got != before {
		t.Errorf("the refused re-stamp still wrote: title %q -> %q", before, got)
	}
}

// AN UNPROMOTED FILE IS SKIPPED, not created. createArtifact would happily
// CREATE the index row from here, racing the promotion automation that is
// about to write it -- so the re-stamp reports restamped=false and writes
// nothing.
func TestRestampFileArtifact_SkipsAFileWithNoIndexRowYet(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "fresh.pdf", "application/pdf", "pdf")
	i := NewIntegration(s)

	restamped, err := restamp(t, i, auth.ContextWithUserActor(context.Background(), "user-a"), "file-1")
	if err != nil {
		t.Fatalf("restamp: %v", err)
	}
	if restamped {
		t.Error("the re-stamp claimed it wrote against a file with no index row")
	}
	if len(s.artifacts) != 0 {
		t.Fatalf("the re-stamp CREATED an index row (%d), racing indexFileOnCreate", len(s.artifacts))
	}
}
