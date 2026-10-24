//go:build linux || darwin

package appsession

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Numerous small outputs must survive as exact retrievable bytes, including
// legitimate archive outputs. Credential filtering happens before bundling.
func TestSession_ManyOutputsRetainExactArchiveMembers(t *testing.T) {
	fakeApp(t, "claude", "mkdir -p evidence node_modules evidence/runtime-17/Default/Cache\ni=0; while [ $i -lt 40 ]; do printf 'evidence %s\\n' $i > evidence/proof-$i.txt; i=$((i+1)); done\nprintf '\\000\\001\\377' > evidence/source.tar\ncp \"$memql_test_config\" evidence/leaked.json\necho dependency > node_modules/cache.txt\necho state > 'evidence/runtime-17/Local State'\necho preferences > evidence/runtime-17/Default/Preferences\necho cache > evidence/runtime-17/Default/Cache/data\n"+quietClaude)
	h := newRig(t)
	end := h.start(t, nil)
	if end.GetExitCode() != 0 || end.GetError() != "" {
		t.Fatalf("bounded evidence set was not retained: %s", end.GetError())
	}
	uploads := h.library.uploaded()
	var manifest struct {
		Files []struct {
			Path, SHA256, ArtifactID, ArchiveEntry string
			Bytes                                  int64
		}
		Omitted, Failures []string
	}
	for name, body := range uploads {
		if strings.HasSuffix(name, "-outputs.json") {
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(manifest.Files) != 41 || len(manifest.Omitted) != 1 || len(manifest.Failures) != 0 {
		t.Fatalf("incomplete manifest: %+v", manifest)
	}
	expected := map[string][]byte{"evidence/source.tar": {0, 1, 255}}
	for i := range 40 {
		expected[fmt.Sprintf("evidence/proof-%d.txt", i)] = []byte(fmt.Sprintf("evidence %d\n", i))
	}
	retained := map[string][]byte{}
	archives := map[string]bool{}
	for id, body := range uploads {
		if !strings.HasSuffix(id, ".zip") {
			continue
		}
		if int64(len(body)) > maxPushFileBytes {
			t.Fatal("archive exceeded per-file cap")
		}
		archives[id] = true
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range zr.File {
			r, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(testBearer)) {
				t.Fatal("credential bundled in archive")
			}
			if _, duplicate := retained[entry.Name]; duplicate {
				t.Fatal("duplicate archive entry")
			}
			retained[entry.Name] = data
		}
	}
	if len(archives) != 1 || len(retained) != len(expected) {
		t.Fatalf("archives=%d members=%d", len(archives), len(retained))
	}
	for _, f := range manifest.Files {
		data, ok := retained[f.ArchiveEntry]
		want, wanted := expected[f.Path]
		if !ok || !wanted || f.ArchiveEntry != f.Path || !archives[f.ArtifactID] || !bytes.Equal(data, want) || f.Bytes != int64(len(data)) || f.SHA256 != fmt.Sprintf("sha256:%x", sha256.Sum256(data)) {
			t.Fatalf("manifest did not map exact retained bytes: %+v", f)
		}
		delete(expected, f.Path)
	}
	if len(expected) != 0 {
		t.Fatal("missing manifest entries")
	}
}

func TestSession_ArchiveUploadFailureNamesEveryMissingMember(t *testing.T) {
	fakeApp(t, "claude", "i=0; while [ $i -lt 35 ]; do echo evidence > proof-$i.txt; i=$((i+1)); done\n"+quietClaude)
	h := newRig(t)
	original := h.library.server.Config.Handler
	h.library.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/artifacts" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			file.Close()
			if strings.HasSuffix(header.Filename, ".zip") {
				http.Error(w, "storage unavailable", 503)
				return
			}
		}
		original.ServeHTTP(w, r)
	})
	end := h.start(t, nil)
	if end.GetError() == "" {
		t.Fatal("upload failure reported success")
	}
	var manifest outputManifest
	for name, body := range h.library.uploaded() {
		if strings.HasSuffix(name, "-outputs.json") {
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(manifest.Files) != 0 || len(manifest.Archives) != 0 || len(manifest.Omitted) != 35 || len(manifest.Failures) == 0 {
		t.Fatalf("missing members misreported: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "proof-34.txt")); err != nil {
		t.Fatal("failed export removed source evidence", err)
	}
}

func TestReadOutputRejectsChangedMissingAndEscapedFiles(t *testing.T) {
	for _, kind := range []string{"changed", "missing", "symlink", "parent-symlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			ws := t.TempDir()
			outside := t.TempDir()
			before := snapshotWorkspace(ws)
			if err := os.Mkdir(filepath.Join(ws, "evidence"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(ws, "evidence", "result.txt")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			files, skipped := collectProduced(ws, before)
			if len(files) != 1 || len(skipped) != 0 {
				t.Fatal("invalid fixture")
			}
			switch kind {
			case "changed":
				if err := os.WriteFile(path, []byte("different"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.WriteFile(filepath.Join(outside, "result.txt"), []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "result.txt"), path); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				if err := os.Rename(filepath.Join(ws, "evidence"), filepath.Join(outside, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "moved"), filepath.Join(ws, "evidence")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.Truncate(path, maxPushFileBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			body, digest, held, err := newContentPolicy(ws, newRedactor(testBearer)).readOutput(files[0])
			if err == nil || body != nil || digest != "" || held {
				t.Fatalf("unsafe candidate retained: bytes=%d digest=%q held=%t err=%v", len(body), digest, held, err)
			}
		})
	}
}

// Real byte ceilings, including ZIP overhead, are applied before HTTP upload.
func TestSession_ArchivePartsRespectByteBudgets(t *testing.T) {
	for _, nearTotal := range []bool{false, true} {
		t.Run(fmt.Sprint(nearTotal), func(t *testing.T) {
			size := int64(36 << 20)
			if nearTotal {
				size = maxPushFileBytes - 500
			}
			script := fmt.Sprintf("dd if=/dev/zero of=a.bin bs=1 count=1 seek=%d 2>/dev/null\ncp a.bin b.bin\ni=0; while [ $i -lt 33 ]; do echo proof > z-$i.txt; i=$((i+1)); done\n", size-1)
			fakeApp(t, "claude", script+quietClaude)
			h := newRig(t)
			end := h.start(t, nil)
			var manifest outputManifest
			for name, body := range h.library.uploaded() {
				if strings.HasSuffix(name, "-outputs.json") {
					if err := json.Unmarshal(body, &manifest); err != nil {
						t.Fatal(err)
					}
				}
			}
			var uploadedBytes int64
			for _, archive := range manifest.Archives {
				body, ok := h.library.uploaded()[archive.ArtifactID]
				if !ok || archive.Bytes != int64(len(body)) || archive.Bytes > maxPushFileBytes || archive.SHA256 != fmt.Sprintf("sha256:%x", sha256.Sum256(body)) {
					t.Fatal("archive provenance or cap mismatch")
				}
				uploadedBytes += archive.Bytes
			}
			if uploadedBytes > maxPushTotalBytes {
				t.Fatal("archive overhead escaped total budget")
			}
			if nearTotal {
				if end.GetError() == "" || len(manifest.Omitted) == 0 || len(manifest.Failures) == 0 {
					t.Fatal("overhead omission reported complete")
				}
			} else if end.GetError() != "" || len(manifest.Files) != 35 || len(manifest.Archives) != 2 {
				t.Fatalf("bounded parts lost files: %s files=%d parts=%d", end.GetError(), len(manifest.Files), len(manifest.Archives))
			}
		})
	}
}
