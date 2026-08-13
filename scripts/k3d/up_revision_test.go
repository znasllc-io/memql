package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// resolveRevision runs up.sh's resolve_target_revision against a throwaway repo.
//
// The function is sourced out of the real script (truncated at `function main`)
// so the test cannot drift from what the installer actually runs.
func resolveRevision(t *testing.T, repo string) string {
	t.Helper()
	body, err := os.ReadFile(upDomainScript(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	cut := strings.Index(src, "\nfunction main()")
	if cut < 0 {
		t.Fatal("up.sh has no `function main()` -- this harness cannot truncate it")
	}

	root := t.TempDir()
	dir := filepath.Join(root, "k3d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	realLib, err := filepath.Abs(filepath.Join("..", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realLib, filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "harness.sh")
	if err := os.WriteFile(script, []byte(src[:cut]+"\nresolve_target_revision '"+repo+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script).Output()
	if err != nil {
		t.Fatalf("harness failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// gitRepo builds a repo with one commit, and optionally a tag checked out
// detached -- the exact shape clone-stack.sh leaves behind.
func gitRepo(t *testing.T, tag string, detach bool) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f")
	run("commit", "-m", "one")
	if tag != "" {
		run("tag", tag)
	}
	if detach {
		run("checkout", "--detach", tag)
	}
	return dir
}

// THE BUG (memql#3602). An install is always a DETACHED checkout at a release
// tag, and `rev-parse --abbrev-ref HEAD` answers the literal "HEAD" for one.
// ArgoCD resolves "HEAD" to the repository's default branch, so an install that
// pinned its images to a release reconciled its manifests from main -- and the
// pin, the only thing the operator chose, was silently dropped.
func TestResolveTargetRevisionKeepsTheTag(t *testing.T) {
	got := resolveRevision(t, gitRepo(t, "v0.16.1", true))
	if got != "v0.16.1" {
		t.Errorf("resolve_target_revision = %q, want the tag v0.16.1 -- "+
			"a release install must reconcile its manifests from the release it pinned", got)
	}
}

// A developer's checkout is on a branch, and `make up` has always tracked it.
func TestResolveTargetRevisionKeepsTheBranch(t *testing.T) {
	got := resolveRevision(t, gitRepo(t, "", false))
	if got != "main" {
		t.Errorf("resolve_target_revision = %q, want main", got)
	}
}

// Detached but not on a tag: the commit is an honest immutable answer, and
// unlike "HEAD" it cannot come to mean a different tree tomorrow.
func TestResolveTargetRevisionFallsBackToTheCommit(t *testing.T) {
	repo := gitRepo(t, "", false)
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	want, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	detach := exec.Command("git", "checkout", "--detach", strings.TrimSpace(string(want)))
	detach.Dir = repo
	if out, err := detach.CombinedOutput(); err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}

	got := resolveRevision(t, repo)
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("resolve_target_revision = %q, want the commit sha", got)
	}
	if got == "HEAD" {
		t.Error("resolved to the literal HEAD, which ArgoCD reads as the default branch")
	}
}
