//go:build agent

package pipelinesteps

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

func fleetArtifactAnswer(encoded string) func(context.Context, worker.Request) (worker.Result, error) {
	return func(context.Context, worker.Request) (worker.Result, error) {
		return worker.Result{OK: true, WorkerId: "reg-1", NodeId: "agent-b", OutputJSON: `{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"` + encoded + `"}`}, nil
	}
}

func TestFleetValidatesTheEntireArchiveBeforeStoringAnyArtifact(t *testing.T) {
	valid := extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "proof"}, {name: "dist/report.json", body: "{}"}})
	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-8] ^= 1
	gunzip, err := gzip.NewReader(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, err := io.ReadAll(gunzip)
	if err != nil {
		t.Fatal(err)
	}
	if err := gunzip.Close(); err != nil {
		t.Fatal(err)
	}
	var noEnd bytes.Buffer
	zip := gzip.NewWriter(&noEnd)
	if _, err := zip.Write(tarBytes[:len(tarBytes)-1024]); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	encode := base64.StdEncoding.EncodeToString
	for name, encoded := range map[string]string{
		"bad base64 after valid gzip": encode(valid) + "!",
		"bad gzip checksum":           encode(badCRC),
		"truncated gzip footer":       encode(valid[:len(valid)-4]),
		"missing tar end":             encode(noEnd.Bytes()),
		"missing declared file":       encode(extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "proof"}})),
		"duplicate declared file":     encode(extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "proof"}, {name: "coverage.out", body: "replacement"}})),
		"undeclared trailing file":    encode(extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "proof"}, {name: "other.txt", body: "untrusted"}})),
	} {
		t.Run(name, func(t *testing.T) {
			f, lib, _, _ := newTestFleet(t, &fakeDispatcher{answer: fleetArtifactAnswer(encoded)})
			res := runFleet(t, f, fleetArtifactReq())
			if res.Status != pl.OutcomeFailed || res.Failure == nil || res.Failure.Code != pl.CodeArtifactUnavailable || len(res.ArtifactIntentIDs) != 0 || len(res.ArtifactFileIDs) != 0 {
				t.Fatalf("invalid archive accepted: %+v", res)
			}
			if len(lib.files) != 1 {
				t.Fatalf("partial artifact side effect: %+v", lib.files)
			}
		})
	}
}

func TestFleetRequiresVerifiedStorageReceipts(t *testing.T) {
	archive := base64.StdEncoding.EncodeToString(extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "proof"}, {name: "dist/report.json", body: "{}"}}))
	for name, alter := range map[string]func(*StoredFile){
		"missing":         func(f *StoredFile) { f.Receipt = nil },
		"owner":           func(f *StoredFile) { f.Receipt.OwnerUserID = "different" },
		"run":             func(f *StoredFile) { f.Receipt.WorkRunID = "different" },
		"attempt":         func(f *StoredFile) { f.Receipt.Attempt++ },
		"path":            func(f *StoredFile) { f.Receipt.Path = "different" },
		"digest":          func(f *StoredFile) { f.Receipt.SHA256 = strings.Repeat("0", 64) },
		"version":         func(f *StoredFile) { f.Receipt.ETag = "" },
		"repeated intent": func(f *StoredFile) { f.Receipt.IntentID = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _, _ := newTestFleet(t, &fakeDispatcher{answer: fleetArtifactAnswer(archive)})
			f.library = alteredArtifactLibrary{&rtLibrary{}, alter}
			res := runFleet(t, f, fleetArtifactReq())
			if res.Status != pl.OutcomeFailed || res.Failure == nil || res.Failure.Code != pl.CodeArtifactUnavailable {
				t.Fatalf("unverified receipt accepted: %+v", res)
			}
		})
	}
	t.Run("byte-only port cannot store artifacts", func(t *testing.T) {
		f, lib, _, _ := newTestFleet(t, &fakeDispatcher{answer: fleetArtifactAnswer(archive)})
		f.library = struct{ LibraryStore }{lib}
		res := runFleet(t, f, fleetArtifactReq())
		if res.Status != pl.OutcomeFailed || len(res.ArtifactIntentIDs) != 0 || len(lib.files) != 1 {
			t.Fatalf("artifact fell back to editable byte storage: %+v, files=%d", res, len(lib.files))
		}
	})
}
