package docs

// build_docs_bundle_test.go -- the docs bundle carries the release's version
// and refuses any other (memql#5714).
//
// Bundle 0.21.25 went out with a manifest saying engineVersion 0.15.0, because
// the manifest carried two version fields and the second was read from a
// VERSION file that had lagged the tag for months. The repair is one field and
// a check: the manifest carries `version` only, and the build refuses a
// --version that is not X.Y.Z or that differs from VERSION.
//
// The REAL script runs in a throwaway copy of the tree with a fake `go` on
// PATH. The fake is what proves "before anything is generated": a refused run
// must not have invoked it at all. The manifest is asserted from the OUTPUT a
// full run writes rather than from _bundle.py's source, so the check is about
// what ships.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// fakeGo stands in for `go run ./cmd/docs-gen`, logging that it was called.
const fakeGo = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_GO_LOG"
exit 0
`

func thisDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// bundleTree builds a minimal repository: the two bundle scripts, a VERSION
// file (omitted when version is empty), one public page and one internal page.
func bundleTree(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"build-docs-bundle.sh", "_bundle.py"} {
		body, err := os.ReadFile(filepath.Join(thisDir(t), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		writeFile(t, filepath.Join(root, "scripts", "docs", name), string(body), 0o755)
	}
	if version != "" {
		writeFile(t, filepath.Join(root, "VERSION"), version, 0o644)
	}
	writeFile(t, filepath.Join(root, "docs", "public", "overview", "index.md"),
		"---\ntitle: Overview\naudience: public\narea: overview\n---\n\n# Overview\n", 0o644)
	writeFile(t, filepath.Join(root, "docs", "public", "operate", "private.md"),
		"---\ntitle: Private\naudience: internal\narea: operate\n---\n\n# Private\n", 0o644)
	return root
}

type bundleRun struct {
	code  int
	out   string
	goRan bool
}

func runBundle(t *testing.T, root string, args ...string) bundleRun {
	t.Helper()
	for _, tool := range []string{"bash", "python3", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "go"), fakeGo, 0o755)
	goLog := filepath.Join(bin, "go.log")

	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "docs", "build-docs-bundle.sh")}, args...)...)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + bin,
		"FAKE_GO_LOG=" + goLog,
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running build-docs-bundle.sh: %v", err)
	}
	_, statErr := os.Stat(goLog)
	r := bundleRun{code: code, out: string(out), goRan: statErr == nil}
	t.Logf("exit=%d goRan=%v\n%s", r.code, r.goRan, r.out)
	return r
}

// assertRefusedBeforeGenerating is the refusal contract: non-zero, the reason
// named, and the generator never invoked.
func assertRefusedBeforeGenerating(t *testing.T, r bundleRun, mention string) {
	t.Helper()
	if r.code == 0 {
		t.Fatal("the bundle build accepted a version it must refuse")
	}
	if !strings.Contains(r.out, mention) {
		t.Errorf("the refusal does not say %q", mention)
	}
	if r.goRan {
		t.Error("the generator ran before the version was checked -- the refusal must come first")
	}
}

// TestBundleRefusesAVersionThatDiffersFromTheFile is the incident: a bundle
// asked to be one release from a tree whose VERSION says another.
func TestBundleRefusesAVersionThatDiffersFromTheFile(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	r := runBundle(t, root, "--version=1.2.4", "--out="+filepath.Join(root, "out"))
	assertRefusedBeforeGenerating(t, r, "differs from VERSION")
	if _, err := os.Stat(filepath.Join(root, "docs-1.2.4.tgz")); err == nil {
		t.Error("a refused build still packaged a tarball")
	}
}

// TestBundleRefusesAVersionThatIsNotBareSemver: the version names the asset
// the site fetches by bare X.Y.Z, so anything else would be an asset nothing
// reads -- the v0.15.0 skip, the other way round.
func TestBundleRefusesAVersionThatIsNotBareSemver(t *testing.T) {
	for _, v := range []string{"v1.2.3", "1.2", "1.2.3-rc1", "latest"} {
		t.Run(v, func(t *testing.T) {
			root := bundleTree(t, v+"\n")
			r := runBundle(t, root, "--version="+v, "--dry-run")
			assertRefusedBeforeGenerating(t, r, "is not X.Y.Z")
		})
	}
}

// TestBundleRefusesWithNoVersionFile: with nothing to compare against there is
// no check, and a build without its check is the state this replaces.
func TestBundleRefusesWithNoVersionFile(t *testing.T) {
	root := bundleTree(t, "")
	r := runBundle(t, root, "--version=1.2.3", "--dry-run")
	assertRefusedBeforeGenerating(t, r, "no VERSION file")
}

// TestBundleAcceptsTheMatchingVersion is the positive control: without it,
// every refusal above is satisfied by a script that refuses everything.
func TestBundleAcceptsTheMatchingVersion(t *testing.T) {
	for _, args := range [][]string{
		{"--version=1.2.3", "--dry-run"},
		{"--dry-run"}, // defaulted from VERSION
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := bundleTree(t, "1.2.3\n")
			r := runBundle(t, root, args...)
			if r.code != 0 {
				t.Fatalf("a version matching VERSION was refused: exit %d", r.code)
			}
			if !strings.Contains(r.out, "docs-1.2.3.tgz") {
				t.Error("the plan does not name docs-1.2.3.tgz")
			}
		})
	}
}

// TestManifestCarriesOneVersionField runs a full build and reads the manifest
// it wrote. Exactly four keys: `version` is the only version field, and
// engineVersion -- the field that said 0.15.0 on bundle 0.21.25 -- is gone.
func TestManifestCarriesOneVersionField(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	out := filepath.Join(root, "out")
	r := runBundle(t, root, "--version=1.2.3", "--out="+out)
	if r.code != 0 {
		t.Fatalf("the full build failed: exit %d", r.code)
	}
	if !r.goRan {
		t.Error("the full build never ran the generator, so this run proves less than it claims")
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatalf("no manifest.json: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest.json is not JSON: %v\n%s", err, raw)
	}
	var keys []string
	for k := range manifest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "areas,nav,pageCount,version"; got != want {
		t.Fatalf("manifest keys = %s, want %s", got, want)
	}
	if manifest["version"] != "1.2.3" {
		t.Errorf("manifest version = %v, want 1.2.3", manifest["version"])
	}
	if manifest["pageCount"] != float64(1) {
		t.Errorf("pageCount = %v, want 1 -- only the audience:public page belongs in the bundle", manifest["pageCount"])
	}
	if _, err := os.Stat(filepath.Join(root, "docs-1.2.3.tgz")); err != nil {
		t.Errorf("the tarball was not packaged: %v", err)
	}
}
