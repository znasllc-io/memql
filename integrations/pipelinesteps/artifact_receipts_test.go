package pipelinesteps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

type alteredArtifactLibrary struct {
	*rtLibrary
	alter func(*StoredFile)
}

func (l alteredArtifactLibrary) StoreRunFileStream(ctx context.Context, f StreamRunFile) (StoredFile, error) {
	got, err := l.rtLibrary.StoreRunFileStream(ctx, f)
	if err == nil {
		l.alter(&got)
	}
	return got, err
}

func TestRunnerRequiresMatchingVerifiedArtifactReceipt(t *testing.T) {
	for name, alter := range map[string]func(*StoredFile){
		"missing": func(f *StoredFile) { f.Receipt = nil },
		"file":    func(f *StoredFile) { f.Receipt.FileID = "different" },
		"owner":   func(f *StoredFile) { f.Receipt.OwnerUserID = "another-owner" },
		"run":     func(f *StoredFile) { f.Receipt.WorkRunID = "another-run" },
		"step":    func(f *StoredFile) { f.Receipt.StepKey = "another-step" },
		"attempt": func(f *StoredFile) { f.Receipt.Attempt++ },
		"path":    func(f *StoredFile) { f.Receipt.Path = "another-path" },
		"size":    func(f *StoredFile) { f.Receipt.Size++ },
		"digest":  func(f *StoredFile) { f.Receipt.SHA256 = strings.Repeat("0", 64) },
		"version": func(f *StoredFile) { f.Receipt.ETag = "" },
		"intent":  func(f *StoredFile) { f.Receipt.IntentID = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.r.library = alteredArtifactLibrary{h.lib, alter}
			run := rtRun()
			run.Artifacts = []string{"proof.txt"}
			rtArtifactStream(t, h, extractTestTgz(t, []extractTestEntry{{name: "proof.txt", body: "verified"}}))
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))
			res := h.run(t, run)
			if res.Status != pl.OutcomeFailed || res.ExitCode != 0 || res.Failure == nil || res.Failure.Code != pl.CodeArtifactUnavailable || len(res.ArtifactIntentIDs) != 0 {
				t.Fatalf("unverified artifact accepted: %+v", res)
			}
			h.leftNoArchive(t)
		})
	}
}

// The Library keys a receipt by the BARE owner and work run (the app's
// streamIdentity), while the driver hands the runner the owner as the
// pipeline row stores it: canonical. Run b304c28a's npm report was stored,
// verified and ready, and its step still failed artifact_unavailable on the
// spelling alone. The receipt must match the request's identity, not its
// spelling -- and still refuse a different owner however it is spelled.
func TestRunnerMatchesAReceiptKeyedByBareIds(t *testing.T) {
	bare := func(f *StoredFile) {
		f.Receipt.OwnerUserID = strings.TrimPrefix(f.Receipt.OwnerUserID, "v1:identity:user:")
		f.Receipt.WorkRunID = strings.TrimPrefix(f.Receipt.WorkRunID, "v1:work:run:")
	}
	for name, c := range map[string]struct {
		alter func(*StoredFile)
		ok    bool
	}{
		"the same ids, bare":       {bare, true},
		"another owner, bare":      {func(f *StoredFile) { bare(f); f.Receipt.OwnerUserID = "another-owner" }, false},
		"another owner, canonical": {func(f *StoredFile) { f.Receipt.OwnerUserID = "v1:identity:user:another-owner" }, false},
		"another work run, bare":   {func(f *StoredFile) { bare(f); f.Receipt.WorkRunID = "another-run" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.r.library = alteredArtifactLibrary{h.lib, c.alter}
			run := rtRun()
			run.OwnerUserID = "v1:identity:user:" + run.OwnerUserID
			run.WorkRunID = "v1:work:run:" + run.WorkRunID
			run.Artifacts = []string{"proof.txt"}
			rtArtifactStream(t, h, extractTestTgz(t, []extractTestEntry{{name: "proof.txt", body: "verified"}}))
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))
			res := h.run(t, run)
			if accepted := res.Status != pl.OutcomeFailed && len(res.ArtifactIntentIDs) == 1; accepted != c.ok {
				t.Fatalf("accepted = %v, want %v: %+v", accepted, c.ok, res)
			}
		})
	}
}

func TestRunnerDoesNotBufferArtifactsForAByteOnlyLibrary(t *testing.T) {
	h := newRunnerHarness(t)
	h.r.library = struct{ LibraryStore }{h.lib}
	run := rtRun()
	run.Artifacts = []string{"proof.txt"}
	rtArtifactStream(t, h, extractTestTgz(t, []extractTestEntry{{name: "proof.txt", body: "verified"}}))
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))
	res := h.run(t, run)
	if res.Status != pl.OutcomeFailed || len(res.ArtifactFileIDs) != 0 || len(h.lib.stored()) != 1 {
		t.Fatalf("artifact fell back to a byte-slice upload: %+v", res)
	}
}

func (l *rtLibrary) StoreRunFileStream(ctx context.Context, f StreamRunFile) (StoredFile, error) {
	body, err := io.ReadAll(io.LimitReader(f.Body, f.Size+1))
	if err != nil {
		return StoredFile{}, err
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
		return StoredFile{}, errors.New("fixture received unverified bytes")
	}
	got, err := l.StoreRunFile(ctx, RunFile{OwnerUserID: f.OwnerUserID, WorkRunID: f.WorkRunID, StepKey: f.StepKey, Name: f.Name, MimeType: f.MimeType, Bytes: body})
	if err != nil || got.Omitted != "" {
		return got, err
	}
	l.mu.Lock()
	l.nextIntent++
	intent := fmt.Sprintf("%064x", l.nextIntent)
	l.mu.Unlock()
	got.Receipt = &StoredFileReceipt{IntentID: intent, FileID: got.FileID,
		OwnerUserID: f.OwnerUserID, WorkRunID: f.WorkRunID, StepKey: f.StepKey, Attempt: f.Attempt, Path: f.Path,
		Container: "fixture", Object: f.Path, URL: "https://fixture.invalid/" + f.Path, ETag: "fixture-version",
		Size: f.Size, SHA256: f.SHA256}
	return got, nil
}
