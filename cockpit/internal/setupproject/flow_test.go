package setupproject

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubSuccess emits a well-formed success envelope, echoing back --product and
// --dry-run so the test can assert the flag mapping reached the script.
const stubSuccess = `#!/usr/bin/env bash
set -euo pipefail
product="unknown"
dry="false"
for a in "$@"; do
  case "$a" in
    --product=*) product="${a#*=}";;
    --dry-run) dry="true";;
  esac
done
echo "INFO: stub bootstrap running for $product (dry=$dry)" >&2
printf '{"ok":true,"capability":"project.bootstrap","changed":true,"result":{"product":"%s","productOrg":"acme-io","domain":"acme.local","engineVersion":"v0.0.0","workspaceRoot":"%s","stampedRepos":["%s-carrier","%s-client"],"dryRun":%s},"error":null}\n' "$product" "$PWD" "$product" "$product" "$dry"
`

// stubFailure emits a failure envelope and exits 5 (op failed).
const stubFailure = `#!/usr/bin/env bash
echo "ERROR: stub bootstrap failing on purpose" >&2
printf '{"ok":false,"capability":"project.bootstrap","changed":false,"result":{},"error":{"code":5,"message":"stub failure"}}\n'
exit 5
`

// stubSilent exits 0 but emits nothing on stdout -- a malformed success.
const stubSilent = `#!/usr/bin/env bash
echo "INFO: silent stub" >&2
exit 0
`

func requireGitBash(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git/bash integration test in -short mode")
	}
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
}

// makeTemplateRepo builds a local git repo that looks like the memql-project
// template: scripts/bootstrap.sh (executable) with the given body, committed on
// a fresh `main`. Returns its path (usable as --template-repo).
func makeTemplateRepo(t *testing.T, bootstrapBody string) string {
	t.Helper()
	dir := t.TempDir()
	scripts := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scripts, "bootstrap.sh")
	if err := os.WriteFile(script, []byte(bootstrapBody), 0o755); err != nil {
		t.Fatal(err)
	}
	// A README so the repo isn't just the script (mirrors the real template).
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# stub template\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "--quiet", "-b", "main")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "--quiet", "-m", "stub template")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestFlowSuccess(t *testing.T) {
	requireGitBash(t)
	template := makeTemplateRepo(t, stubSuccess)
	target := filepath.Join(t.TempDir(), "workspace") // absent -> clone creates it

	var out, errbuf bytes.Buffer
	flow := &Flow{
		Cfg: Config{
			Product: "acme", ProductOrg: "acme-io",
			Dir: target, TemplateRepo: template,
		},
		Out: &out,
		Err: &errbuf,
	}
	if code := flow.Run(); code != exitOK {
		t.Fatalf("Run() = %d, want 0\nstderr:\n%s", code, errbuf.String())
	}
	summary := out.String()
	for _, want := range []string{"SUCCESS: workspace stamped", "product:        acme", "acme-carrier", "https://bff.acme.local", "cd acme-carrier && make up"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q\n---\n%s", want, summary)
		}
	}
	// Fresh git history: template .git stripped, workspace re-inited on main.
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		t.Errorf("workspace .git missing (expected fresh init): %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "scripts", "bootstrap.sh")); err != nil {
		t.Errorf("template payload not cloned: %v", err)
	}
}

func TestFlowPropagatesFailureExitCode(t *testing.T) {
	requireGitBash(t)
	template := makeTemplateRepo(t, stubFailure)
	target := filepath.Join(t.TempDir(), "workspace")

	var out, errbuf bytes.Buffer
	flow := &Flow{
		Cfg: Config{Product: "acme", ProductOrg: "acme-io", Dir: target, TemplateRepo: template},
		Out: &out, Err: &errbuf,
	}
	if code := flow.Run(); code != 5 {
		t.Fatalf("Run() = %d, want 5 (bootstrap exit propagated)\nstderr:\n%s", code, errbuf.String())
	}
	if !strings.Contains(out.String(), "stub failure") {
		t.Errorf("summary missing the error message:\n%s", out.String())
	}
}

func TestFlowDryRunLeavesTargetUntouched(t *testing.T) {
	requireGitBash(t)
	template := makeTemplateRepo(t, stubSuccess)
	// A dry run must not require (or touch) a target dir.
	target := filepath.Join(t.TempDir(), "workspace")

	var out, errbuf bytes.Buffer
	flow := &Flow{
		Cfg: Config{Product: "acme", ProductOrg: "acme-io", Dir: target, TemplateRepo: template, DryRun: true},
		Out: &out, Err: &errbuf,
	}
	if code := flow.Run(); code != exitOK {
		t.Fatalf("Run() = %d, want 0\nstderr:\n%s", code, errbuf.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("dry-run summary missing DRY RUN marker:\n%s", out.String())
	}
	// The target was never created for a dry run (temp workspace used instead).
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("dry run created/populated the target dir %q (err=%v)", target, err)
	}
}

func TestFlowNoEnvelopeIsFailure(t *testing.T) {
	requireGitBash(t)
	template := makeTemplateRepo(t, stubSilent)
	target := filepath.Join(t.TempDir(), "workspace")

	var out, errbuf bytes.Buffer
	flow := &Flow{
		Cfg: Config{Product: "acme", ProductOrg: "acme-io", Dir: target, TemplateRepo: template},
		Out: &out, Err: &errbuf,
	}
	// bootstrap exited 0 but produced no envelope -> we refuse to claim success.
	if code := flow.Run(); code != exitFailure {
		t.Fatalf("Run() = %d, want %d (no envelope on a 0 exit)", code, exitFailure)
	}
}

func TestFlowRefusesNonEmptyTarget(t *testing.T) {
	requireGitBash(t)
	template := makeTemplateRepo(t, stubSuccess)
	target := t.TempDir() // exists and is NOT empty:
	if err := os.WriteFile(filepath.Join(target, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errbuf bytes.Buffer
	flow := &Flow{
		Cfg: Config{Product: "acme", ProductOrg: "acme-io", Dir: target, TemplateRepo: template},
		Out: &out, Err: &errbuf,
	}
	if code := flow.Run(); code != exitUsage {
		t.Fatalf("Run() into non-empty target = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errbuf.String(), "not empty") {
		t.Errorf("expected a 'not empty' diagnostic, got:\n%s", errbuf.String())
	}
}

// TestFlowRealTemplateE2E stamps against the REAL memql-project template (a
// network clone). It is heavy and network-dependent, so it is gated behind
// MEMQL_SETUP_PROJECT_E2E=1. It uses --dry-run so it does NOT clone the engine.
// The byte-identical golden-tree comparison vs bootstrap-direct stamping is
// tracked separately in memql#2446 (B2).
func TestFlowRealTemplateE2E(t *testing.T) {
	if os.Getenv("MEMQL_SETUP_PROJECT_E2E") != "1" {
		t.Skip("set MEMQL_SETUP_PROJECT_E2E=1 to run the real-template e2e (network clone)")
	}
	requireGitBash(t)
	var out, errbuf bytes.Buffer
	flow := &Flow{
		Cfg: Config{Product: "demo", ProductOrg: "demo-org", Domain: "demo.local", DryRun: true},
		Out: &out, Err: &errbuf,
	}
	if code := flow.Run(); code != exitOK {
		t.Fatalf("Run() = %d, want 0\nstderr:\n%s", code, errbuf.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("expected a DRY RUN summary:\n%s", out.String())
	}
}
