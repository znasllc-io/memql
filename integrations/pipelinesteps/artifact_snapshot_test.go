package pipelinesteps

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotTar(t *testing.T, headers []tar.Header) []byte {
	t.Helper()
	var body bytes.Buffer
	w := tar.NewWriter(&body)
	for _, header := range headers {
		if err := w.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := io.CopyN(w, strings.NewReader(strings.Repeat("a", int(header.Size))), header.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestArtifactSnapshotValidatesWholeStreamBeforeReturningFiles(t *testing.T) {
	good := snapshotTar(t, []tar.Header{{Name: "dist/proof.txt", Typeflag: tar.TypeReg, Mode: 0600, Size: 3}})
	for _, tc := range []struct {
		name  string
		data  []byte
		paths []string
		limit int64
	}{
		{"traversal", snapshotTar(t, []tar.Header{{Name: "../bad", Typeflag: tar.TypeReg}}), []string{"dist"}, 4096},
		{"link", snapshotTar(t, []tar.Header{{Name: "dist/link", Typeflag: tar.TypeSymlink, Linkname: "/secret"}}), []string{"dist"}, 4096},
		{"duplicate", snapshotTar(t, []tar.Header{{Name: "dist/a", Typeflag: tar.TypeReg}, {Name: "dist/a", Typeflag: tar.TypeReg}}), []string{"dist"}, 4096},
		{"undeclared", good, []string{"other"}, 4096},
		{"missing", good, []string{"dist", "absent"}, 4096},
		{"truncated-file", good[:514], []string{"dist"}, 4096},
		{"missing-footer", good[:1024], []string{"dist"}, 4096},
		{"partial-footer", good[:1536], []string{"dist"}, 4096},
		{"trailing-data", append(append([]byte{}, good...), []byte("hidden")...), []string{"dist"}, 4096},
		{"wire-limit", good, []string{"dist"}, int64(len(good)) - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			got, err := SnapshotArtifacts(t.Context(), bytes.NewReader(tc.data), tc.paths, tc.limit)
			if err == nil || got != nil {
				if got != nil {
					_ = got.Close()
				}
				t.Fatalf("admitted malformed stream: %v", err)
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed snapshot left scratch files: %v %v", entries, err)
			}
		})
	}
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if cancelled {
			cancel()
		}
		lost := errors.New("remote exit status lost")
		got, err := SnapshotArtifacts(ctx, io.MultiReader(bytes.NewReader(good), snapshotErrorReader{lost}), []string{"dist"}, 4096)
		cancel()
		if got != nil || err == nil || (!cancelled && !errors.Is(err, lost)) {
			t.Fatalf("accepted uncertain/cancelled transfer: %v", err)
		}
	}
}

func TestArtifactSnapshotKeepsMeasuredBytesInPrivateFiles(t *testing.T) {
	body := snapshotTar(t, []tar.Header{{Name: "dist", Typeflag: tar.TypeDir}, {Name: "dist/b", Typeflag: tar.TypeReg, Mode: 0777, Size: 3}, {Name: "dist/a", Typeflag: tar.TypeReg, Size: 0}})
	got, err := SnapshotArtifacts(t.Context(), bytes.NewReader(body), []string{"dist"}, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = got.Close() })
	if len(got.Files) != 2 || got.Files[0].Path != "dist/a" || got.Files[1].SHA256 != "9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0" {
		t.Fatalf("wrong snapshot: %+v", got.Files)
	}
	dir, err := os.Stat(got.directory)
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatal("snapshot directory is not private", err)
	}
	for _, file := range got.Files {
		if filepath.Dir(file.local) != got.directory {
			t.Fatal("untrusted path escaped")
		}
		f, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		info, _ := f.Stat()
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil || int64(len(data)) != file.Size || info.Mode().Perm() != 0600 {
			t.Fatal("wrong file bytes/mode", err)
		}
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got.directory); !os.IsNotExist(err) {
		t.Fatal("snapshot was not removed")
	}
}

type snapshotErrorReader struct{ err error }

func (r snapshotErrorReader) Read([]byte) (int, error) { return 0, r.err }

// Cockpit uses GNU long-name encoding so filenames are preserved without
// admitting PAX metadata to the verified snapshot contract.
func TestArtifactSnapshotAcceptsNativeGNULongUnicodeNames(t *testing.T) {
	name := "dist/" + strings.Repeat("long-", 35) + "résumé %2F #.txt"
	body := snapshotTar(t, []tar.Header{{Format: tar.FormatGNU, Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: 3}})
	snapshot, err := SnapshotArtifacts(t.Context(), bytes.NewReader(body), []string{"dist/*"}, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	if len(snapshot.Files) != 1 || snapshot.Files[0].Path != name || snapshot.Files[0].Size != 3 {
		t.Fatalf("native filename was lost: %+v", snapshot.Files)
	}
	file, err := snapshot.Files[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "aaa" {
		t.Fatalf("wrong snapshot bytes: %q %v", data, err)
	}
}
