package inference

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// The stage is the one part of a native install that touches the network
// and the filesystem, so it is driven here against a fake release served
// over HTTP into a temporary home: the archive, its checksum file, the
// unit, and every way the archive can lie.

type fakeEntry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

// tarZst builds a release archive in memory the way the vendor's is laid
// out: bin/ollama and lib/ollama/... with a version symlink.
func tarZst(t *testing.T, entries []fakeEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func vendorLayout() []fakeEntry {
	return []fakeEntry{
		{name: "./bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "./bin/ollama", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho fake ollama\n"},
		{name: "./lib/ollama/", typ: tar.TypeDir, mode: 0o755},
		{name: "./lib/ollama/libggml-base.so.0.0.1", typ: tar.TypeReg, mode: 0o644, body: "so"},
		{name: "./lib/ollama/libggml-base.so", typ: tar.TypeSymlink, mode: 0o777, link: "libggml-base.so.0.0.1"},
	}
}

// release serves a fake GitHub release: the archive at the pinned tag's
// download path and a sha256sum.txt in the vendor's `<hex>  ./<name>`
// spelling. The Archive under test is pointed at it by overriding the
// release base, which is why ollamaReleases is a variable the test may
// swap.
func release(t *testing.T, tag string, files map[string][]byte) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, body := range files {
		sum := sha256.Sum256(body)
		sums.WriteString(hex.EncodeToString(sum[:]) + "  ./" + name + "\n")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		base := "/download/" + tag + "/"
		if !strings.HasPrefix(r.URL.Path, base) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, base)
		if name == ollamaChecksumFile {
			_, _ = w.Write([]byte(sums.String()))
			return
		}
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	return httptest.NewServer(mux)
}

func stageIn(home string) Stage {
	n := DefaultNativeFacts(home, "")
	return Stage{
		Archives:   []Archive{{Name: "ollama-linux-amd64.tar.zst", Release: "v9.9.9"}},
		RuntimeDir: n.RuntimeDir,
		ModelsDir:  n.ModelsDir,
		LogPath:    n.LogPath,
		UnitPath:   n.UnitPath,
		Listen:     nativeListen,
	}
}

func withReleases(t *testing.T, base string) {
	t.Helper()
	old := ollamaReleases
	ollamaReleases = base
	t.Cleanup(func() { ollamaReleases = old })
}

func TestStageRuntimeFetchesVerifiesUnpacksAndWritesTheUnit(t *testing.T) {
	archive := tarZst(t, vendorLayout())
	srv := release(t, "v9.9.9", map[string][]byte{"ollama-linux-amd64.tar.zst": archive})
	defer srv.Close()
	withReleases(t, srv.URL)

	home := t.TempDir()
	s := stageIn(home)
	var seen []Progress
	if err := StageRuntime(context.Background(), s, func(p Progress) { seen = append(seen, p) }); err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}

	bin := filepath.Join(s.RuntimeDir, "bin", "ollama")
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("bin/ollama was not unpacked: %v", err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Errorf("bin/ollama must keep its executable bit, mode %v", fi.Mode())
	}
	if target, err := os.Readlink(filepath.Join(s.RuntimeDir, "lib", "ollama", "libggml-base.so")); err != nil || target != "libggml-base.so.0.0.1" {
		t.Errorf("the version symlink must be kept inside the runtime, got %q, %v", target, err)
	}
	unit, err := os.ReadFile(s.UnitPath)
	if err != nil {
		t.Fatalf("the unit was not written: %v", err)
	}
	for _, want := range []string{
		"ExecStart=/usr/bin/env " + bin + " serve",
		"Environment=OLLAMA_HOST=127.0.0.1:11434",
		"Environment=OLLAMA_MODELS=" + s.ModelsDir,
		"Environment=OLLAMA_KV_CACHE_TYPE=q8_0",
		"StandardOutput=append:" + s.LogPath,
		"WantedBy=default.target",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("the unit must carry %q:\n%s", want, unit)
		}
	}
	if fi, err := os.Stat(s.ModelsDir); err != nil || !fi.IsDir() {
		t.Errorf("the models directory must exist before the unit starts: %v", err)
	}
	// The verified archive is not kept: it costs a gigabyte and a half
	// and is worth nothing once unpacked.
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(s.RuntimeDir), ".download-*"))
	if len(leftovers) != 0 {
		t.Errorf("the download must be removed after unpacking, found %v", leftovers)
	}
	if _, err := os.Stat(s.RuntimeDir + ".new"); err == nil {
		t.Error("the staging directory must be renamed away, not left beside the runtime")
	}
	// Progress named the archive and reached its full size.
	if len(seen) == 0 || seen[len(seen)-1].Model != "ollama-linux-amd64.tar.zst" || seen[len(seen)-1].Completed != uint64(len(archive)) {
		t.Errorf("progress must name the archive and end at its size, got %+v", seen)
	}
}

func TestStageRuntimeReplacesAnEarlierRuntimeWholesale(t *testing.T) {
	archive := tarZst(t, vendorLayout())
	srv := release(t, "v9.9.9", map[string][]byte{"ollama-linux-amd64.tar.zst": archive})
	defer srv.Close()
	withReleases(t, srv.URL)

	home := t.TempDir()
	s := stageIn(home)
	stale := filepath.Join(s.RuntimeDir, "lib", "ollama", "stale.so")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageRuntime(context.Background(), s, nil); err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("an earlier runtime's files must not survive under the new one")
	}
}

func TestStageRuntimeRefusesAnArchiveThatDoesNotMatchTheChecksum(t *testing.T) {
	good := tarZst(t, vendorLayout())
	srv := release(t, "v9.9.9", map[string][]byte{"ollama-linux-amd64.tar.zst": good})
	defer srv.Close()
	// The checksum file describes `good`; the server hands out something
	// else for the archive itself.
	tampered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ollamaChecksumFile) {
			resp, err := http.Get(srv.URL + r.URL.Path)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer resp.Body.Close()
			var b bytes.Buffer
			_, _ = b.ReadFrom(resp.Body)
			_, _ = w.Write(b.Bytes())
			return
		}
		_, _ = w.Write(append(good, 'x'))
	}))
	defer tampered.Close()
	withReleases(t, tampered.URL)

	home := t.TempDir()
	s := stageIn(home)
	err := StageRuntime(context.Background(), s, nil)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if _, err := os.Stat(filepath.Join(s.RuntimeDir, "bin", "ollama")); err == nil {
		t.Error("a mismatched archive must not be unpacked")
	}
	if _, err := os.Stat(s.UnitPath); err == nil {
		t.Error("no unit may be written for a runtime that was not installed")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(s.RuntimeDir), ".download-*"))
	if len(leftovers) != 0 {
		t.Errorf("a refused download must be removed, found %v", leftovers)
	}
}

func TestStageRuntimeRefusesAnArchiveTheChecksumFileDoesNotList(t *testing.T) {
	archive := tarZst(t, vendorLayout())
	srv := release(t, "v9.9.9", map[string][]byte{"something-else.tar.zst": archive})
	defer srv.Close()
	withReleases(t, srv.URL)

	s := stageIn(t.TempDir())
	err := StageRuntime(context.Background(), s, nil)
	if err == nil || !strings.Contains(err.Error(), "does not list ollama-linux-amd64.tar.zst") {
		t.Fatalf("err = %v, want a refusal naming the unlisted archive", err)
	}
}

func TestUnpackRefusesEntriesThatEscapeTheRuntimeDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []fakeEntry
	}{
		{"a parent path", []fakeEntry{{name: "../evil", typ: tar.TypeReg, mode: 0o644, body: "x"}}},
		{"an absolute path", []fakeEntry{{name: "/etc/evil", typ: tar.TypeReg, mode: 0o644, body: "x"}}},
		{"a symlink out", []fakeEntry{{name: "bin/ollama", typ: tar.TypeSymlink, mode: 0o777, link: "../../../../usr/bin/env"}}},
		{"an absolute symlink", []fakeEntry{{name: "bin/ollama", typ: tar.TypeSymlink, mode: 0o777, link: "/usr/bin/env"}}},
		{"a hard link out", []fakeEntry{{name: "bin/ollama", typ: tar.TypeLink, mode: 0o644, link: "../../outside"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := tarZst(t, tc.entries)
			path := filepath.Join(t.TempDir(), "a.tar.zst")
			if err := os.WriteFile(path, archive, 0o644); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "runtime")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := unpackTarZst(path, dest); !errors.Is(err, ErrArchiveEscapes) {
				t.Fatalf("err = %v, want ErrArchiveEscapes", err)
			}
		})
	}
}

func TestStageRuntimeRefusesAnArchiveWithoutTheBinary(t *testing.T) {
	archive := tarZst(t, []fakeEntry{{name: "./README", typ: tar.TypeReg, mode: 0o644, body: "hi"}})
	srv := release(t, "v9.9.9", map[string][]byte{"ollama-linux-amd64.tar.zst": archive})
	defer srv.Close()
	withReleases(t, srv.URL)

	s := stageIn(t.TempDir())
	err := StageRuntime(context.Background(), s, nil)
	if err == nil || !strings.Contains(err.Error(), "bin/ollama") {
		t.Fatalf("err = %v, want a refusal naming the missing binary", err)
	}
}

func TestStageReuseWritesOnlyTheUnit(t *testing.T) {
	// No server at all: a reuse must not touch the network.
	withReleases(t, "http://127.0.0.1:1")
	s := stageIn(t.TempDir())
	s.Archives = nil
	s.Reuse = true
	if err := StageRuntime(context.Background(), s, nil); err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	if _, err := os.Stat(s.UnitPath); err != nil {
		t.Errorf("the unit must be written on a reuse: %v", err)
	}
}

func TestParseChecksumAcceptsBothVendorSpellings(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, text := range []string{
		sum + "  ./ollama-linux-amd64.tar.zst\n",
		sum + "  ollama-linux-amd64.tar.zst\n",
		"deadbeef  ./other\n" + sum + "  ./ollama-linux-amd64.tar.zst\n",
	} {
		got, err := parseChecksum(strings.NewReader(text), "ollama-linux-amd64.tar.zst")
		if err != nil || got != sum {
			t.Errorf("%q: got %q, %v", text, got, err)
		}
	}
	if _, err := parseChecksum(strings.NewReader(sum+"  ./other\n"), "ollama-linux-amd64.tar.zst"); err == nil {
		t.Error("an archive the file does not list must not verify")
	}
}

// TestStageLinesSayEverythingBeforeTheQuestion. The consent covers the
// stage as well as the commands, so the stage has to be on the screen in
// the same detail: what is fetched, from where, into where, and what is
// written.
func TestStageLinesSayEverythingBeforeTheQuestion(t *testing.T) {
	s := stageIn("/home/op")
	lines := s.Lines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"download ollama-linux-amd64.tar.zst from https://github.com/ollama/ollama/releases/download/v9.9.9",
		"unpack it into /home/op/.memql/ollama/runtime",
		"checked against the release's sha256sum.txt",
		"write /home/op/.config/systemd/user/memql-ollama.service",
		"listening on 127.0.0.1:11434",
		"models in /home/op/.memql/ollama/models",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the stage lines must carry %q:\n%s", want, joined)
		}
	}
	s.Archives = nil
	s.Reuse = true
	if got := strings.Join(s.Lines(), "\n"); !strings.Contains(got, "reuse the Ollama already unpacked at /home/op/.memql/ollama/runtime") {
		t.Errorf("a reuse must say so:\n%s", got)
	}
}

func TestArchiveURLsPinTheRelease(t *testing.T) {
	a := Archive{Name: "ollama-linux-amd64.tar.zst", Release: "v0.33.3"}
	if got := a.URL(); got != "https://github.com/ollama/ollama/releases/download/v0.33.3/ollama-linux-amd64.tar.zst" {
		t.Errorf("URL = %q", got)
	}
	if got := a.ChecksumURL(); got != "https://github.com/ollama/ollama/releases/download/v0.33.3/sha256sum.txt" {
		t.Errorf("ChecksumURL = %q", got)
	}
	// Unpinned falls back to the vendor's latest alias for both, so a
	// checksum and an archive still come from the same place.
	a.Release = ""
	if got := a.URL(); got != "https://github.com/ollama/ollama/releases/latest/download/ollama-linux-amd64.tar.zst" {
		t.Errorf("unpinned URL = %q", got)
	}
}

func TestDefaultNativeFactsKeepEverythingInsideTheFence(t *testing.T) {
	n := DefaultNativeFacts("/home/op", "")
	for name, path := range map[string]string{"runtime": n.RuntimeDir, "models": n.ModelsDir, "log": n.LogPath} {
		if !strings.HasPrefix(path, "/home/op/.memql/") {
			t.Errorf("%s at %q is outside ~/.memql, which the uninstaller will not touch", name, path)
		}
	}
	if n.UnitPath != "/home/op/.config/systemd/user/memql-ollama.service" {
		t.Errorf("unit at %q", n.UnitPath)
	}
	if got := DefaultNativeFacts("/home/op", "/mnt/big/models").ModelsDir; got != "/mnt/big/models" {
		t.Errorf("OLLAMA_MODELS must win for the models, got %q", got)
	}
	if NativeArchiveName("amd64") != "ollama-linux-amd64.tar.zst" || NativeArchiveName("arm64") != "ollama-linux-arm64.tar.zst" || NativeArchiveName("riscv64") != "" {
		t.Error("the archive names must follow the vendor's per-arch layout")
	}
	if NativeROCmArchiveName("amd64") != "ollama-linux-amd64-rocm.tar.zst" || NativeROCmArchiveName("arm64") != "" {
		t.Error("the ROCm add-on ships for amd64 only")
	}
}

// TestStageRuntimeRefusesAFullDiskBeforeDownloading. A 1.4 GB download
// that could not be unpacked is bandwidth spent to reach the same
// refusal later, so the volume is checked against the archive's size
// before the first byte, with both numbers in the sentence.
func TestStageRuntimeRefusesAFullDiskBeforeDownloading(t *testing.T) {
	archive := tarZst(t, vendorLayout())
	served := 0
	srv := release(t, "v9.9.9", map[string][]byte{"ollama-linux-amd64.tar.zst": archive})
	defer srv.Close()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ollamaChecksumFile) {
			served++
		}
		resp, err := http.Get(srv.URL + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
		_, _ = io.Copy(w, resp.Body)
	}))
	defer counting.Close()
	withReleases(t, counting.URL)

	old := stageFreeBytes
	stageFreeBytes = func(string) uint64 { return uint64(len(archive)) }
	t.Cleanup(func() { stageFreeBytes = old })

	s := stageIn(t.TempDir())
	err := StageRuntime(context.Background(), s, nil)
	if !errors.Is(err, ErrNotEnoughDisk) {
		t.Fatalf("err = %v, want ErrNotEnoughDisk", err)
	}
	if !strings.Contains(err.Error(), "needs about") || !strings.Contains(err.Error(), "it has") {
		t.Errorf("the refusal must carry both numbers: %v", err)
	}
	if _, err := os.Stat(s.UnitPath); err == nil {
		t.Error("no unit may be written when nothing was unpacked")
	}
}

func TestUnpackRejectsSymlinkChainEscape(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(home, "runtime")
	outside := filepath.Join(home, "outside")
	for _, d := range []string{dest, outside} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	entries := []fakeEntry{
		{name: "a/b", typ: tar.TypeDir, mode: 0755},
		{name: "a/b/up", typ: tar.TypeSymlink, link: ".."},
		{name: "a/b/up/escape", typ: tar.TypeSymlink, link: "../../outside"},
		{name: "a/escape/owned", typ: tar.TypeReg, mode: 0644, body: "escaped"},
	}
	archive := filepath.Join(home, "archive.zst")
	if err := os.WriteFile(archive, tarZst(t, entries), 0644); err != nil {
		t.Fatal(err)
	}
	if err := unpackTarZst(archive, dest); err == nil {
		t.Error("symlink chain escaping extraction root was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "owned")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archive wrote outside runtime: %v", err)
	}
}

func TestWaitReadyRequiresAnOllamaVersion(t *testing.T) {
	for _, body := range []string{"<html>wrong service</html>", "{}", `{"version":""}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer srv.Close()
			if err := WaitReady(context.Background(), srv.URL, time.Millisecond); err == nil {
				t.Fatal("unrelated HTTP 200 accepted as Ollama readiness")
			}
		})
	}
}

// A library symlink can escape without the archive ever writing through it.
// Extraction must reject it before a later runtime load follows that link.
func TestUnpackRejectsUnusedEscapingSymlinkChain(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(home, "runtime")
	for _, dir := range []string{dest, filepath.Join(home, "outside")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	entries := []fakeEntry{
		{name: "a/b", typ: tar.TypeDir, mode: 0755},
		{name: "a/b/up", typ: tar.TypeSymlink, link: ".."},
		{name: "a/b/up/escape", typ: tar.TypeSymlink, link: "../../outside"},
	}
	archive := filepath.Join(home, "archive.zst")
	if err := os.WriteFile(archive, tarZst(t, entries), 0644); err != nil {
		t.Fatal(err)
	}
	if err := unpackTarZst(archive, dest); err == nil {
		t.Fatal("accepted an escaping symlink that the runtime could follow later")
	}
}

func TestNativeUnitEscapesOperatorPaths(t *testing.T) {
	s := stageIn("/home/A Person%u")
	s.ModelsDir = "/mnt/a \"quoted\" path%u/line\nnext"
	unit := s.Unit()
	for _, want := range []string{
		`ExecStart=/usr/bin/env "/home/A Person%%u/.memql/ollama/runtime/bin/ollama" serve`,
		`Environment="OLLAMA_MODELS=/mnt/a \"quoted\" path%%u/line\nnext"`,
		`StandardOutput=append:/home/A Person%%u/.memql/state/ollama.log`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing escaped directive %q:\n%s", want, unit)
		}
	}
}

func TestUnpackRejectsRelocatedSymlinkHardlink(t *testing.T) {
	dest := t.TempDir()
	archive := filepath.Join(t.TempDir(), "a.zst")
	entries := []fakeEntry{
		{name: "x", typ: tar.TypeDir, mode: 0755},
		{name: "x/link", typ: tar.TypeSymlink, link: "../x"},
		{name: "z", typ: tar.TypeLink, link: "x/link"},
	}
	if err := os.WriteFile(archive, tarZst(t, entries), 0644); err != nil {
		t.Fatal(err)
	}
	if err := unpackTarZst(archive, dest); !errors.Is(err, ErrArchiveEscapes) {
		t.Fatalf("relocated relative symlink escaped extraction root: %v", err)
	}
}

func TestNativeUnitQuotesApostrophePaths(t *testing.T) {
	s := stageIn("/home/O'Brien")
	s.ModelsDir = "/mnt/O'Brien/models"
	unit := s.Unit()
	for _, want := range []string{`ExecStart=/usr/bin/env "/home/O'Brien/.memql/ollama/runtime/bin/ollama" serve`, `Environment="OLLAMA_MODELS=/mnt/O'Brien/models"`} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd parses apostrophe as quote; missing %s", want)
		}
	}
}

// tarGz is tarZst's gzip twin: Ollama's macOS archive is a .tgz.
func tarGz(t *testing.T, entries []fakeEntry) []byte {
	t.Helper()
	zst := tarZst(t, entries)
	zr, err := zstd.NewReader(bytes.NewReader(zst))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// darwinLayout is the macOS archive: ollama and its libraries at the root.
func darwinLayout() []fakeEntry {
	return []fakeEntry{
		{name: "ollama", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho fake ollama\n"},
		{name: "libggml-base.0.27.0.dylib", typ: tar.TypeReg, mode: 0o755, body: "dylib"},
		{name: "libggml-base.0.dylib", typ: tar.TypeSymlink, mode: 0o777, link: "libggml-base.0.27.0.dylib"},
	}
}

// A Mac without Homebrew (memql-cockpit#524): the release's .tgz,
// verified, unpacked with ollama at the runtime's root, and a LaunchAgent
// written and handed to launchd -- the loopback bind, the models directory
// inside the fence and the log path all in the plist.
func TestStageRuntimeOnMacWritesAndLoadsTheLaunchAgent(t *testing.T) {
	archive := tarGz(t, darwinLayout())
	srv := release(t, "v9.9.9", map[string][]byte{DarwinArchiveName: archive})
	defer srv.Close()
	withReleases(t, srv.URL)

	var loaded []string
	old := launchdLoad
	launchdLoad = func(_ context.Context, uid int, plist string) error {
		loaded = append(loaded, fmt.Sprintf("gui/%d %s", uid, plist))
		return nil
	}
	t.Cleanup(func() { launchdLoad = old })

	home := t.TempDir()
	n := DarwinNativeFacts(home, "")
	s := Stage{
		Archives:   []Archive{{Name: DarwinArchiveName, Release: "v9.9.9"}},
		RuntimeDir: n.RuntimeDir, ModelsDir: n.ModelsDir, LogPath: n.LogPath, UnitPath: n.UnitPath,
		Listen: nativeListen, Launchd: true, UID: 501,
	}
	if err := StageRuntime(context.Background(), s, nil); err != nil {
		t.Fatalf("StageRuntime: %v", err)
	}
	bin := filepath.Join(s.RuntimeDir, "ollama")
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("ollama must be unpacked executable at the runtime root: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(s.RuntimeDir, "libggml-base.0.dylib")); err != nil || target != "libggml-base.0.27.0.dylib" {
		t.Errorf("the dylib version symlink must be kept, got %q, %v", target, err)
	}
	plist, err := os.ReadFile(s.UnitPath)
	if err != nil {
		t.Fatalf("the LaunchAgent was not written: %v", err)
	}
	for _, want := range []string{
		"<string>com.znasllc.memql-ollama</string>",
		"<string>" + bin + "</string>",
		"<string>serve</string>",
		"<key>OLLAMA_HOST</key>\n        <string>127.0.0.1:11434</string>",
		"<key>OLLAMA_MODELS</key>\n        <string>" + s.ModelsDir + "</string>",
		"<key>StandardOutPath</key>\n    <string>" + s.LogPath + "</string>",
		"<key>KeepAlive</key>\n    <true/>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("the LaunchAgent must carry %q:\n%s", want, plist)
		}
	}
	if len(loaded) != 1 || loaded[0] != "gui/501 "+s.UnitPath {
		t.Errorf("the agent must be loaded into gui/501 once, got %q", loaded)
	}
	if !strings.HasPrefix(s.UnitPath, filepath.Join(home, "Library", "LaunchAgents")) {
		t.Errorf("the agent belongs under ~/Library/LaunchAgents, got %s", s.UnitPath)
	}
}

// A path with XML in it is data in the plist, never markup.
func TestLaunchAgentEscapesOperatorPaths(t *testing.T) {
	s := Stage{RuntimeDir: "/Users/a&b/rt", ModelsDir: "/Volumes/<x>/models", LogPath: "/tmp/o.log", Listen: nativeListen, Launchd: true}
	plist := s.Plist()
	if strings.Contains(plist, "a&b") || strings.Contains(plist, "<x>") {
		t.Fatalf("operator paths must be escaped:\n%s", plist)
	}
	if !strings.Contains(plist, "/Users/a&amp;b/rt/ollama") || !strings.Contains(plist, "/Volumes/&lt;x&gt;/models") {
		t.Fatalf("escaped paths must still name the same directories:\n%s", plist)
	}
}

func TestStageLinesOnMacSayLaunchd(t *testing.T) {
	n := DarwinNativeFacts("/Users/op", "")
	s := Stage{Archives: []Archive{{Name: DarwinArchiveName}}, RuntimeDir: n.RuntimeDir, ModelsDir: n.ModelsDir, LogPath: n.LogPath, UnitPath: n.UnitPath, Listen: nativeListen, Launchd: true}
	lines := strings.Join(s.Lines(), "\n")
	for _, want := range []string{
		"download ollama-darwin.tgz from https://github.com/ollama/ollama/releases/latest/download and unpack it into /Users/op/.memql/ollama/runtime, checked against the release's sha256sum.txt",
		"write /Users/op/Library/LaunchAgents/com.znasllc.memql-ollama.plist and load it into launchd (Ollama as your user, listening on 127.0.0.1:11434, models in /Users/op/.memql/ollama/models)",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("Lines must say %q, got:\n%s", want, lines)
		}
	}
}
