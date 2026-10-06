package osbuild_test

// bundle_manifest_test.go holds scripts/os/bundle-manifest.mjs to the contract
// verify-rollout (epic memql#5480) reads: memql-bundle.json, the sha256 of
// every file of the built OS bundle by its dist-relative path. The edge image's
// layer carries the manifest, and verify-rollout compares it with the bytes the
// public front door serves, so what it says about a path must be exactly the
// hash of that file's bytes -- and say it the same way every time.
//
// The script is a Node program and the Go build does not need Node, so every
// case skips when `node` is not on PATH. The OS lane and the image build both
// have it, and the image build runs the script for real.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

const (
	scriptName   = "bundle-manifest.mjs"
	manifestName = "memql-bundle.json"
)

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH: the manifest script is a Node program and the Go build does not need it")
	}
}

// writeFiles lays files down, by dist-relative path, under a fresh directory.
func writeFiles(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// bundle is shaped like vite's output -- a root file, a directory, and a file
// two levels down. The bytes are deliberately not tidy text: a CRLF and a run
// no UTF-8 string holds, because a script that hashed a decoded or
// newline-normalized copy would describe something the edge never serves.
func bundle() map[string][]byte {
	return map[string][]byte{
		"index.html":       []byte("<!doctype html>\r\n<title>MemQL OS</title>\r\n"),
		"assets/a.js":      []byte("console.log('a');\n"),
		"assets/sub/b.css": {'b', '{', '}', 0xff, 0xfe, 0x00, '\n'},
	}
}

// runManifest runs the script from workDir over the directory dirArg names --
// absolute, or relative to workDir -- and answers the manifest it wrote.
func runManifest(t *testing.T, workDir, dirArg string) []byte {
	t.Helper()
	script, err := filepath.Abs(scriptName)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script, dirArg)
	cmd.Dir = workDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node %s %s: %v\n%s", scriptName, dirArg, err, out)
	}
	dir := dirArg
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(workDir, dir)
	}
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatalf("the script wrote no %s: %v", manifestName, err)
	}
	return raw
}

type manifest struct {
	Schema    int               `json:"schema"`
	Algorithm string            `json:"algorithm"`
	Files     map[string]string `json:"files"`
}

func decode(t *testing.T, raw []byte) manifest {
	t.Helper()
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the manifest is not JSON: %v\n%s", err, raw)
	}
	return m
}

// filePaths is the order the manifest's "files" keys are WRITTEN in, which a
// decoded map cannot say.
func filePaths(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // the opening brace
		t.Fatal(err)
	}
	var paths []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		if key != "files" {
			var skipped json.RawMessage
			if err := dec.Decode(&skipped); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := dec.Token(); err != nil { // the files object's opening brace
			t.Fatal(err)
		}
		for dec.More() {
			path, err := dec.Token()
			if err != nil {
				t.Fatal(err)
			}
			paths = append(paths, path.(string))
			var hash string
			if err := dec.Decode(&hash); err != nil {
				t.Fatal(err)
			}
		}
	}
	return paths
}

func TestTheManifestHashesEveryFileOfTheBundleByItsDistRelativePath(t *testing.T) {
	requireNode(t)
	files := bundle()
	dir := writeFiles(t, files)

	got := decode(t, runManifest(t, dir, dir))

	if got.Schema != 1 || got.Algorithm != "sha256" {
		t.Errorf("schema %d, algorithm %q; want 1 and sha256", got.Schema, got.Algorithm)
	}
	want := map[string]string{}
	for rel, content := range files {
		sum := sha256.Sum256(content)
		want[rel] = hex.EncodeToString(sum[:])
	}
	if !reflect.DeepEqual(got.Files, want) {
		t.Errorf("files =\n%v\nwant exactly the bundle's three, hashed as bytes:\n%v", got.Files, want)
	}
}

// Run twice over a directory that now holds the first run's manifest, and once
// more with the directory named relatively: the answer is the same bytes, and
// the manifest never lists itself. Anything that varied -- a timestamp, a walk
// order, the way the directory was named -- would make a rebuild of the same
// sources look like a different bundle.
func TestTheManifestIsDeterministicAndNeverListsItself(t *testing.T) {
	requireNode(t)
	dir := writeFiles(t, bundle())

	first := runManifest(t, dir, dir)
	second := runManifest(t, dir, dir)
	if !bytes.Equal(first, second) {
		t.Errorf("a second run over the same bundle wrote different bytes:\n%s\n%s", first, second)
	}
	relative := runManifest(t, filepath.Dir(dir), filepath.Base(dir))
	if !bytes.Equal(first, relative) {
		t.Errorf("naming the directory relatively changed the manifest:\n%s\n%s", first, relative)
	}

	if _, listed := decode(t, second).Files[manifestName]; listed {
		t.Errorf("the manifest lists itself:\n%s", second)
	}
}

// A directory walk visits fonts/ before fonts.css, but '.' sorts before '/', so
// the order a walk produces is not the sorted one -- and sorted is what makes
// the manifest the same bytes however the filesystem lists a directory.
func TestTheManifestWritesItsPathsSorted(t *testing.T) {
	requireNode(t)
	dir := writeFiles(t, map[string][]byte{
		"fonts/inter.woff2": []byte("i"),
		"fonts.css":         []byte("f"),
		"a.js":              []byte("a"),
	})

	got := filePaths(t, runManifest(t, dir, dir))

	if want := []string{"a.js", "fonts.css", "fonts/inter.woff2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("paths are written %v, want %v", got, want)
	}
}

func TestTheManifestScriptRefusesWithNoDirectory(t *testing.T) {
	requireNode(t)
	script, err := filepath.Abs(scriptName)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", script).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("with no directory the script ended %v, want exit status 2\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("usage")) {
		t.Errorf("the refusal does not say how to call it:\n%s", out)
	}
}
