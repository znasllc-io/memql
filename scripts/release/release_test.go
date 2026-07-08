// Package release holds tests for the memQL release-image shell
// script (scripts/release/release.sh) plus a standalone-build
// resolution guard for the Azure deployment foundation
// (znasllc-io/memql#493, epic #491).
//
// The script itself is function-based bash per the Skills+Scripts
// architecture; these tests exercise its dry-run / validation
// contract from `go test ./...` so CI catches regressions without
// needing Docker or a registry. They mirror the existing
// scripts/<area>/*_test.go pattern (e.g. scripts/deploy,
// scripts/dedupe-peruser-seeds).
//
// There is no Go production code here -- this directory is purely a
// test harness for the release tooling.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the repo root relative to this test file. This
// file lives at scripts/release/, so the root is two directories up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// scriptPath resolves scripts/release/release.sh.
func scriptPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), "scripts", "release", "release.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("release.sh not found at %s: %v", p, err)
	}
	return p
}

// run executes the release script with the given args and returns
// combined output + the error (nil on exit 0).
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell release test")
	}
	cmd := exec.Command("bash", append([]string{scriptPath(t)}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSyntax fails fast on a bash syntax error (`bash -n`).
func TestSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", scriptPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestHelp checks that --help exits 0 and prints usage.
func TestHelp(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("--help missing 'Usage:' banner:\n%s", out)
	}
	for _, want := range []string{"--version=", "--push", "--dry-run", "release.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q", want)
		}
	}
}

// TestDryRunPlanImmutableTag asserts the dry-run plans an immutable
// memql:X.Y.Z image against the shared ACR, mutates nothing (no bare
// `docker build`/`docker push` -- every line is a [plan] marker), and
// stamps the source revision label.
func TestDryRunPlanImmutableTag(t *testing.T) {
	out, err := run(t, "--version=2.4.0", "--acr=acrmemql", "--push", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run exited non-zero: %v\n%s", err, out)
	}

	// Side-effect-free guarantee: in dry-run every docker invocation
	// must be prefixed with the [plan] marker.
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "docker ") {
			t.Errorf("dry-run appears to execute docker (no [plan] marker): %q", trimmed)
		}
	}

	for _, want := range []string{
		"acrmemql.azurecr.io/memql:2.4.0",        // pinnable immutable tag
		"org.opencontainers.image.revision=",     // traceable to a commit
		"[plan] docker build",
		"[plan] docker push",
		"DRY RUN complete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n---\n%s", want, out)
		}
	}
}

// TestVersionFromFileIsCleanSemver proves that with no --version the
// script derives a clean three-part semver from the VERSION file and
// never tags an image with a pre-release/epoch suffix. The VERSION
// file is a plain X.Y.Z post the v0.9.0 reset (epic #501); the script
// still strips any legacy `-<epoch>` dev stamp as a safety net, which
// this guard (no `-` in the tag) continues to enforce.
func TestVersionFromFileIsCleanSemver(t *testing.T) {
	out, err := run(t, "--dry-run")
	if err != nil {
		t.Fatalf("default dry-run exited non-zero: %v\n%s", err, out)
	}
	// The tag must be a clean three-part semver with no epoch suffix.
	if strings.Contains(out, "Image:      memql:") == false {
		t.Errorf("expected a local memql:X.Y.Z image line:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Image:") {
			if strings.Contains(line, "-") {
				t.Errorf("release image tag must not carry an epoch suffix: %q", line)
			}
		}
	}
}

// TestInvalidVersionRejected asserts a non-semver --version fails.
func TestInvalidVersionRejected(t *testing.T) {
	out, err := run(t, "--version=2.4", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for bad version, got success:\n%s", out)
	}
	if !strings.Contains(out, "clean semver") {
		t.Errorf("expected 'clean semver' error, got:\n%s", out)
	}
}

// TestPushRequiresRegistry asserts --push without a registry fails.
func TestPushRequiresRegistry(t *testing.T) {
	out, err := run(t, "--version=2.4.0", "--push", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for --push with no registry:\n%s", out)
	}
	if !strings.Contains(out, "requires a registry") {
		t.Errorf("expected 'requires a registry' error, got:\n%s", out)
	}
}

// TestStandaloneBuildResolves is the CI-style guard for issue #493's
// core requirement: a release build with GOWORK=off (no workspace)
// must resolve every dependency from go.mod/go.sum alone. The engine
// is product-agnostic -- it compiles in no product code, and product
// DSL is mounted at RUNTIME via MEMQL_DSL_PATH (a data-only bundle),
// not compile-linked from a carrier. So there is nothing product-shaped
// to resolve here; the test simply proves the standalone graph is
// self-contained so a containerized release build (which runs without
// go.work) cannot regress into needing the workspace.
func TestStandaloneBuildResolves(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping standalone build in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := repoRoot(t)

	// `go mod verify` is fast and fully offline: it confirms every
	// module in the build list is present and unmodified in the
	// module cache, which is exactly the standalone-resolution
	// property a GOWORK=off container build depends on.
	cmd := exec.Command("go", "mod", "verify")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("GOWORK=off go mod verify failed (standalone graph not self-contained): %v\n%s", err, out)
	}
}
