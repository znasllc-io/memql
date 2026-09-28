package docs

// build_docs_bundle_test.go -- the docs bundle wrapper (memql#5714, #5717).
//
// scripts/docs/build-docs-bundle.sh is a capability script over
// `cmd/docs-gen bundle`. These tests hold the WRAPPER's half of the contract:
// the version is checked before anything is compiled, docs-gen is handed the
// build the caller asked for, its exit codes come back unchanged, and stdout
// carries exactly one JSON envelope whatever happens.
//
// Bundle 0.21.25 went out with a manifest saying engineVersion 0.15.0, because
// the manifest carried two version fields and the second was read from a
// VERSION file that had lagged the tag for months. The wrapper refuses a
// --version that is not X.Y.Z or that differs from VERSION; the manifest's own
// contract (schema 2, one version field) is pinned by cmd/docs-gen/bundle's
// TestManifestV2, next to the code that writes it.
//
// The REAL script runs in a throwaway copy of the tree with a fake `go` on
// PATH. `go build` there writes a fake docs-gen that logs its arguments and
// exits as told, which is what proves "before anything is compiled": a refused
// run must not have invoked `go` at all.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGo stands in for `go build -o <path> ./cmd/docs-gen`: it logs the call
// and writes a fake docs-gen at <path>, unless FAKE_GO_FAIL asks it to fail.
const fakeGo = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_GO_LOG"
if [ -n "${FAKE_GO_FAIL:-}" ]; then echo "fake compile error" >&2; exit 1; fi
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
cat > "$out" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_DOCSGEN_LOG"
echo "fake docs-gen log line" >&2
summary="${FAKE_DOCSGEN_SUMMARY:-}"
[ -n "$summary" ] || summary='{"pages":1,"violations":[]}'
printf '%s\n' "$summary"
exit "${FAKE_DOCSGEN_EXIT:-0}"
EOF
chmod +x "$out"
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

// bundleTree builds a minimal repository: the wrapper, the capability library
// it sources, and a VERSION file (omitted when version is empty).
func bundleTree(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	for src, dst := range map[string]string{
		filepath.Join(thisDir(t), "build-docs-bundle.sh"):       filepath.Join(root, "scripts", "docs", "build-docs-bundle.sh"),
		filepath.Join(thisDir(t), "..", "lib", "capability.sh"): filepath.Join(root, "scripts", "lib", "capability.sh"),
	} {
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		writeFile(t, dst, string(body), 0o755)
	}
	if version != "" {
		writeFile(t, filepath.Join(root, "VERSION"), version, 0o644)
	}
	return root
}

type envelope struct {
	OK      bool                `json:"ok"`
	Changed bool                `json:"changed"`
	Result  map[string]any      `json:"result"`
	Error   *struct{ Code int } `json:"error"`
}

type bundleRun struct {
	code       int
	stdout     string
	stderr     string
	env        envelope
	goRan      bool
	docsGenRan bool
	docsGenArg string
}

func runBundle(t *testing.T, root string, extraEnv []string, args ...string) bundleRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "go"), fakeGo, 0o755)
	goLog := filepath.Join(bin, "go.log")
	docsGenLog := filepath.Join(bin, "docs-gen.log")

	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "docs", "build-docs-bundle.sh")}, args...)...)
	cmd.Dir = root
	cmd.Env = append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + bin,
		"TMPDIR=" + t.TempDir(),
		"FAKE_GO_LOG=" + goLog,
		"FAKE_DOCSGEN_LOG=" + docsGenLog,
	}, extraEnv...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running build-docs-bundle.sh: %v", err)
	}
	r := bundleRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	_, statErr := os.Stat(goLog)
	r.goRan = statErr == nil
	if b, err := os.ReadFile(docsGenLog); err == nil {
		r.docsGenRan = true
		r.docsGenArg = strings.TrimSpace(string(b))
	}
	t.Logf("exit=%d goRan=%v docs-gen %q\nstdout: %s\nstderr: %s", r.code, r.goRan, r.docsGenArg, r.stdout, r.stderr)

	// Contract rule 5: exactly one JSON envelope on stdout, whatever happened.
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout carries %d lines, want exactly one JSON envelope", len(lines))
	}
	if err := json.Unmarshal([]byte(lines[0]), &r.env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v", err)
	}
	if r.env.OK != (r.code == 0) {
		t.Errorf("envelope ok=%v disagrees with exit %d", r.env.OK, r.code)
	}
	if r.code != 0 && (r.env.Error == nil || r.env.Error.Code != r.code) {
		t.Errorf("envelope error %+v disagrees with exit %d", r.env.Error, r.code)
	}
	return r
}

// assertRefusedBeforeCompiling is the refusal contract: the expected exit, the
// reason named, and go never invoked.
func assertRefusedBeforeCompiling(t *testing.T, r bundleRun, code int, mention string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d", r.code, code)
	}
	if !strings.Contains(r.stdout, mention) {
		t.Errorf("the refusal does not say %q", mention)
	}
	if r.goRan {
		t.Error("go ran before the version was checked -- the refusal must come first")
	}
}

// TestBundleRefusesAVersionThatDiffersFromTheFile is the incident: a bundle
// asked to be one release from a tree whose VERSION says another.
func TestBundleRefusesAVersionThatDiffersFromTheFile(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	r := runBundle(t, root, nil, "--version=1.2.4", "--out="+filepath.Join(root, "out"))
	assertRefusedBeforeCompiling(t, r, 3, "differs from VERSION")
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
			r := runBundle(t, root, nil, "--version="+v, "--dryRun")
			assertRefusedBeforeCompiling(t, r, 2, "is not X.Y.Z")
		})
	}
}

// TestBundleRefusesWithNoVersionFile: with nothing to compare against there is
// no check, and a build without its check is the state this replaces.
func TestBundleRefusesWithNoVersionFile(t *testing.T) {
	root := bundleTree(t, "")
	r := runBundle(t, root, nil, "--version=1.2.3", "--dryRun")
	assertRefusedBeforeCompiling(t, r, 4, "no VERSION file")
}

// TestBundleRequiresAVersion: the version is a required parameter, never read
// back from the VERSION file it is checked against.
func TestBundleRequiresAVersion(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	r := runBundle(t, root, nil, "--dryRun")
	assertRefusedBeforeCompiling(t, r, 2, "missing required parameter: version")
}

// TestBundleDryRunRunsNothing is the positive control for the refusals above:
// without it they are satisfied by a script that refuses everything.
func TestBundleDryRunRunsNothing(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	r := runBundle(t, root, nil, "--version=1.2.3", "--dryRun")
	if r.code != 0 || r.goRan {
		t.Fatalf("a dry run with a matching version: exit %d, go ran %v", r.code, r.goRan)
	}
	if got, _ := r.env.Result["tarball"].(string); !strings.HasSuffix(got, "/docs-1.2.3.tgz") {
		t.Errorf("result.tarball = %q, want <repo>/docs-1.2.3.tgz", got)
	}
	if r.env.Changed {
		t.Error("a dry run reported a change")
	}
}

// TestBundleHandsTheBuildToDocsGen: a build compiles docs-gen and runs
// `docs-gen bundle` with the version, the tree's root, the output directory
// and the tarball the site's fetcher expects, and returns its summary.
func TestBundleHandsTheBuildToDocsGen(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	out := filepath.Join(root, "staging")
	r := runBundle(t, root, []string{`FAKE_DOCSGEN_SUMMARY={"pages":7,"violations":[]}`}, "--version=1.2.3", "--out="+out)
	if r.code != 0 || !r.goRan {
		t.Fatalf("exit %d, go ran %v", r.code, r.goRan)
	}
	want := "bundle -version 1.2.3 -root " + root + " -site-url https://memql.io -out " + out + " -tarball " + root + "/docs-1.2.3.tgz"
	if r.docsGenArg != want {
		t.Errorf("docs-gen ran as\n  %s\nwant\n  %s", r.docsGenArg, want)
	}
	if !r.env.Changed {
		t.Error("a build reported no change")
	}
	summary, _ := r.env.Result["bundle"].(map[string]any)
	if summary["pages"] != float64(7) {
		t.Errorf("result.bundle = %v, want docs-gen's summary", r.env.Result["bundle"])
	}
	if !strings.Contains(r.stderr, "fake docs-gen log line") {
		t.Error("docs-gen's log lines did not reach stderr")
	}
}

// TestBundleCheckWritesNothing: --check asks docs-gen for the check and names
// no output.
func TestBundleCheckWritesNothing(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	r := runBundle(t, root, nil, "--version=1.2.3", "--check", "--siteUrl=https://docs.acme.test")
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	if want := "bundle -version 1.2.3 -root " + root + " -site-url https://docs.acme.test -check"; r.docsGenArg != want {
		t.Errorf("docs-gen ran as %q, want %q", r.docsGenArg, want)
	}
	if r.env.Changed {
		t.Error("a check reported a change")
	}
}

// TestBundlePassesDocsGenExitCodesThrough: the wrapper's stable codes are
// docs-gen's, so a boundary violation is 6 at every layer, and anything
// docs-gen does not name is an operation failure.
func TestBundlePassesDocsGenExitCodesThrough(t *testing.T) {
	for docsGen, want := range map[string]int{"2": 2, "4": 4, "5": 5, "6": 6, "1": 5} {
		t.Run(docsGen, func(t *testing.T) {
			root := bundleTree(t, "1.2.3\n")
			r := runBundle(t, root, []string{"FAKE_DOCSGEN_EXIT=" + docsGen,
				`FAKE_DOCSGEN_SUMMARY={"violations":[{"file":"docs/public/a.md","rule":"leaves-public"}]}`},
				"--version=1.2.3", "--check")
			if r.code != want {
				t.Fatalf("docs-gen exit %s: wrapper exit %d, want %d", docsGen, r.code, want)
			}
			if _, ok := r.env.Result["bundle"]; !ok {
				t.Error("the failure envelope dropped docs-gen's summary, and with it the violations")
			}
		})
	}
}

// TestBundleCompileFailureIsAnOperationFailure: docs-gen that does not compile
// is exit 5, reported in the envelope, not an abort.
func TestBundleCompileFailureIsAnOperationFailure(t *testing.T) {
	root := bundleTree(t, "1.2.3\n")
	r := runBundle(t, root, []string{"FAKE_GO_FAIL=1"}, "--version=1.2.3")
	if r.code != 5 || r.docsGenRan {
		t.Fatalf("exit %d, docs-gen ran %v; want 5 and never run", r.code, r.docsGenRan)
	}
	if !strings.Contains(r.stdout, "did not compile") {
		t.Error("the envelope does not say docs-gen failed to compile")
	}
}
