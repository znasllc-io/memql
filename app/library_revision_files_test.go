package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

type revisionFileFixture struct {
	head                     server.LibraryFileRow
	history                  map[int]*server.LibraryFileVersionRow
	uploads, saves, restamps int
	loseAcknowledgment, race bool
}

func (f *revisionFileFixture) File(context.Context, string) (*server.LibraryFileRow, error) {
	h := f.head
	return &h, nil
}
func (f *revisionFileFixture) FileVersion(_ context.Context, _ string, n int) (*server.LibraryFileVersionRow, error) {
	return f.history[n], nil
}
func (*revisionFileFixture) StorageFootprint(context.Context) (int64, int64, int64, error) {
	return 100, 200, 0, nil
}
func (f *revisionFileFixture) RestampFileArtifact(context.Context, string) error {
	f.restamps++
	return nil
}
func (f *revisionFileFixture) Upload(_ context.Context, _, name string, data []byte, _ string) (string, error) {
	f.uploads++
	if string(data) != "Approved bytes" {
		return "", errors.New("wrong bytes")
	}
	if f.race {
		f.head.VersionNumber++
		f.head.BlobUrl = "manual-edit"
	}
	return "https://storage/" + name, nil
}
func (f *revisionFileFixture) SupersedeFile(_ context.Context, snap server.LibraryVersionSnapshot, head server.LibraryHeadMove) error {
	if f.head.VersionNumber != snap.VersionNumber || f.head.BlobUrl != snap.BlobUrl {
		return server.ErrFileVersionConflict
	}
	f.history[snap.VersionNumber] = &server.LibraryFileVersionRow{VersionNumber: snap.VersionNumber, Sha256: snap.Sha256, BlobUrl: snap.BlobUrl}
	f.head.VersionNumber = head.VersionNumber
	f.head.BlobUrl = head.BlobUrl
	f.head.Sha256 = head.Sha256
	f.saves++
	if f.loseAcknowledgment {
		return errors.New("response lost after commit")
	}
	return nil
}
func TestLibraryRevisionFileSaveRecoversReceiptWithoutAnotherVersion(t *testing.T) {
	f := &revisionFileFixture{head: server.LibraryFileRow{Name: "A name with spaces.md", VersionNumber: 3, BlobUrl: "original", Sha256: "old", MimeType: "text/markdown"}, history: map[int]*server.LibraryFileVersionRow{}, loseAcknowledgment: true}
	s := &libraryRevisionFiles{store: f, uploader: f, bucket: "library"}
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: "writer"}), &auth.AccessContext{UserId: "writer", Role: auth.RoleWriter})
	write := library.RevisionFileWrite{FileID: "doc", Version: 3, BlobURL: "original", OperationID: "one-run", Content: []byte("Approved bytes")}
	for n := 0; n < 2; n++ {
		if err := s.SaveRevision(ctx, write); err != nil {
			t.Fatal(err)
		}
	}
	if f.uploads != 1 || f.saves != 1 || f.restamps != 2 || f.history[3].BlobUrl != "original" {
		t.Fatalf("duplicate or lost save: %+v", f)
	}
	// A later manual version preserves the receipt in version history.
	f.history[4] = &server.LibraryFileVersionRow{VersionNumber: 4, Sha256: f.head.Sha256, BlobUrl: f.head.BlobUrl}
	f.head.VersionNumber = 5
	f.head.BlobUrl = "later-manual"
	f.head.Name = "Renamed.md"
	if err := s.SaveRevision(ctx, write); err != nil {
		t.Fatal(err)
	}
	if f.head.BlobUrl != "later-manual" || f.uploads != 1 || f.saves != 1 {
		t.Fatal("retry overwrote a later edit")
	}
	write.OperationID = "different-run"
	if err := s.SaveRevision(ctx, write); err == nil {
		t.Fatal("different operation reused the receipt")
	}
}
func TestLibraryRevisionFileSaveRejectsRaceAndReader(t *testing.T) {
	f := &revisionFileFixture{head: server.LibraryFileRow{VersionNumber: 3, BlobUrl: "original"}, history: map[int]*server.LibraryFileVersionRow{}, race: true}
	s := &libraryRevisionFiles{store: f, uploader: f, bucket: "library"}
	write := library.RevisionFileWrite{FileID: "doc", Version: 3, BlobURL: "original", OperationID: "run", Content: []byte("Approved bytes")}
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: "writer"}), &auth.AccessContext{UserId: "writer", Role: auth.RoleReader})
	if err := s.SaveRevision(ctx, write); err == nil || f.uploads != 0 {
		t.Fatal("reader wrote revision")
	}
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "writer", Role: auth.RoleWriter})
	if err := s.SaveRevision(ctx, write); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("concurrent save not refused: %v", err)
	}
	if f.head.BlobUrl != "manual-edit" || f.saves != 0 {
		t.Fatal("concurrent edit overwritten")
	}
}
