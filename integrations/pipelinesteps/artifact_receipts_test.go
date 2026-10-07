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
