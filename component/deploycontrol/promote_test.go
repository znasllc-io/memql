package deploycontrol

// promote_test.go -- engine promotion end to end through the REAL Executor
// (memql#3769, epic memql#3748 §4.1/§4.3).
//
// These drive execExecutor, not a fake: the claim is "promote pins production's
// digests from staging's, in one commit, and rollback re-pins the prior
// digests", and every load-bearing part of that lives in the effects -- the
// script invocation, the envelope parse, the commit, and `git revert` of that
// commit. A fake executor would assert that the Service calls RunPromote, which
// is already covered and is not what this issue changed.
//
// A throwaway git repository is built in t.TempDir() and populated from the
// overlays and scripts in this checkout. Nothing here touches the working tree.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// promoteFixture is a scratch repository shaped like this one: the two
// overlays, the promote script, and the capability library it sources.
type promoteFixture struct {
	root     string
	exec     Executor
	fromFile string
	toFile   string
}

func newPromoteFixture(t *testing.T) *promoteFixture {
	t.Helper()
	for _, bin := range []string{"git", "bash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	repo := repoRootFromTest(t)
	root := t.TempDir()

	copyInto := func(rel string) {
		src := filepath.Join(repo, rel)
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.WriteFile(dst, raw, 0o755); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}
	copyInto(filepath.Join("scripts", "lib", "capability.sh"))
	copyInto(filepath.Join("scripts", "deploy", "promote-overlay.sh"))
	for _, env := range []string{"staging", "prod"} {
		surface, ok := SurfaceFor(env)
		if !ok {
			t.Fatalf("SurfaceFor(%q) missing", env)
		}
		copyInto(filepath.Join(surface.OverlayDir, "kustomization.yaml"))
	}

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "promote-test@example.invalid")
	git("config", "user.name", "promote test")
	git("config", "commit.gpgsign", "false")
	git("add", "--", "scripts", "deploy")
	git("commit", "-q", "-m", "fixture: the two overlays and the promote script")

	from, _ := SurfaceFor("staging")
	to, _ := SurfaceFor("prod")
	return &promoteFixture{
		root:     root,
		exec:     NewExecutor(root),
		fromFile: filepath.Join(root, from.OverlayDir, "kustomization.yaml"),
		toFile:   filepath.Join(root, to.OverlayDir, "kustomization.yaml"),
	}
}

// git runs a read-only git command in the fixture repo and returns its stdout.
func (f *promoteFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

var digestLineRe = regexp.MustCompile(`(?m)^\s*digest:\s*(\S+)\s*$`)

// digests reads the ordered digest values out of a kustomization's images:
// block. Order and count are enough to compare two overlays here, and reading
// them with a different expression than the script uses is deliberate: a shared
// parser would make a parsing bug invisible to both.
func digests(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, m := range digestLineRe.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no digests", path)
	}
	return out
}

// TestPromoteIsOneCommitAndRollbackRepinsThePriorDigests is the whole
// acceptance criterion in one pass:
//
//	promote  -> production carries staging's digests, in ONE commit
//	rollback -> `git revert` of that commit restores exactly the prior digests
//
// The two halves are one test on purpose. "Rollback re-pins the prior digests"
// is only true BECAUSE the promote is one commit touching one file in one tree;
// asserting them separately would let the second pass against a promote that
// had quietly grown a second commit.
func TestPromoteIsOneCommitAndRollbackRepinsThePriorDigests(t *testing.T) {
	f := newPromoteFixture(t)
	ctx := context.Background()

	before := digests(t, f.toFile)
	sourceDigests := digests(t, f.fromFile)
	headBefore := f.git(t, "rev-parse", "HEAD")

	out, err := f.exec.RunPromote(ctx, "9.9.9", "prod")
	if err != nil {
		t.Fatalf("RunPromote: %v\n%s", err, out)
	}

	// ONE commit. Not two, and not zero -- ArgoCD reconciles what is committed,
	// so a promote that edits without committing looks like it worked and
	// deploys nothing.
	newCommits := f.git(t, "rev-list", "--count", headBefore+"..HEAD")
	if newCommits != "1" {
		t.Fatalf("promote produced %s commits, want exactly 1", newCommits)
	}
	promoteSHA := f.git(t, "rev-parse", "HEAD")

	// The working tree is clean afterwards: everything the promote changed is
	// IN the commit. A leftover unstaged edit is what makes a later `git revert`
	// restore something other than the prior state.
	if dirty := f.git(t, "status", "--porcelain"); dirty != "" {
		t.Errorf("promote left uncommitted changes:\n%s", dirty)
	}

	after := digests(t, f.toFile)
	if strings.Join(after, ",") != strings.Join(sourceDigests, ",") {
		t.Errorf("production is not pinned to what staging runs:\n  prod    %v\n  staging %v", after, sourceDigests)
	}
	if strings.Join(after, ",") == strings.Join(before, ",") {
		t.Fatal("promote changed no digest, so this test proves nothing about the rollback below")
	}

	// The commit message carries the promote's provenance -- which is where it
	// lives now that both environments are in one repository (executor.go).
	msg := f.git(t, "log", "-1", "--format=%B", promoteSHA)
	for _, want := range []string{"9.9.9", "staging", "prod", "Trained constructs do NOT travel"} {
		if !strings.Contains(msg, want) {
			t.Errorf("promote commit message is missing %q:\n%s", want, msg)
		}
	}

	// Rollback: `git revert` of the one promote commit.
	if out, err := f.exec.RunRollback(ctx, "prod", promoteSHA); err != nil {
		t.Fatalf("RunRollback: %v\n%s", err, out)
	}
	restored := digests(t, f.toFile)
	if strings.Join(restored, ",") != strings.Join(before, ",") {
		t.Errorf("rollback did not re-pin the prior digests:\n  got  %v\n  want %v", restored, before)
	}
}

// TestPromoteCarriesNoGraphState is the assertion the issue asks for in the
// CODE rather than in prose: a promoted construct in staging is ABSENT from
// production after an engine promote.
//
// It cannot be asserted here by reading two databases -- that is the DB-gated
// TestPromotedConstructsDoNotCrossEnvironments in component/database. What is
// asserted here is the MECHANISM, which is the stronger half: an engine promote
// writes image digests into one git-tracked file and does nothing else. It
// opens no database connection, executes no engine query, and touches no other
// path -- so there is nothing in it that could carry a v1:authoring:construct
// row across even in principle, and the same is true of a DSL bundle digest
// riding the same commit.
//
// This is the single most likely wrong assumption about the combined system
// ("I promoted the bundle, why is my promoted construct missing"), so it fails
// loudly if a promote ever grows a second effect.
func TestPromoteCarriesNoGraphState(t *testing.T) {
	f := newPromoteFixture(t)
	head := f.git(t, "rev-parse", "HEAD")

	if out, err := f.exec.RunPromote(context.Background(), "9.9.9", "prod"); err != nil {
		t.Fatalf("RunPromote: %v\n%s", err, out)
	}

	target, _ := SurfaceFor("prod")
	wantPath := filepath.ToSlash(filepath.Join(target.OverlayDir, "kustomization.yaml"))
	touched := strings.Split(f.git(t, "diff", "--name-only", head, "HEAD"), "\n")
	if len(touched) != 1 || touched[0] != wantPath {
		t.Fatalf("a promote must touch exactly one file (%s); it touched %v", wantPath, touched)
	}

	// Every changed line is a digest value. Anything else -- a namespace, a
	// replica count, an env var, a mounted ConfigMap -- would be the promote
	// moving something that is not an image.
	for _, line := range strings.Split(f.git(t, "diff", "-U0", head, "HEAD"), "\n") {
		if !strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "-") {
			continue
		}
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if digestLineRe.MatchString(line[1:]) {
			continue
		}
		t.Errorf("promote changed a non-digest line, so it is no longer just an image pin:\n  %s", line)
	}
}

// TestPromoteRefusesAnEnvironmentThatIsPromotedFromNothing -- staging's digests
// are cut on the GitHub build server and pinned into its overlay directly, so
// there is no source overlay to copy them from.
//
// The refusal replaces a worse failure rather than introducing one: this path
// shelled out to scripts/release/promote.sh, which left the repository with the
// product deploy estate (992deb41), so it had been failing with `fork/exec ...
// no such file or directory` -- an error naming a path and no reason.
func TestPromoteRefusesAnEnvironmentThatIsPromotedFromNothing(t *testing.T) {
	f := newPromoteFixture(t)
	head := f.git(t, "rev-parse", "HEAD")

	_, err := f.exec.RunPromote(context.Background(), "9.9.9", "staging")
	if err == nil {
		t.Fatal("promoting staging succeeded; there is no overlay to promote it from")
	}
	for _, want := range []string{"staging", "promoted from nothing", "build server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not explain itself (missing %q):\n  %s", want, err.Error())
		}
	}
	if f.git(t, "rev-parse", "HEAD") != head {
		t.Error("a refused promote still committed something")
	}

	if _, err := f.exec.RunPromote(context.Background(), "9.9.9", "development"); err == nil {
		t.Error("promoting `development` succeeded; development is not a deploy target")
	}
}

// TestPromotingTheSameVersionTwiceMakesNoEmptyCommit -- promote is idempotent,
// and an idempotent no-op must not manufacture a commit. An empty commit in the
// overlay history is a promote that appears to have happened, which is exactly
// what an operator would later try to `git revert`.
func TestPromotingTheSameVersionTwiceMakesNoEmptyCommit(t *testing.T) {
	f := newPromoteFixture(t)
	ctx := context.Background()

	if out, err := f.exec.RunPromote(ctx, "9.9.9", "prod"); err != nil {
		t.Fatalf("first RunPromote: %v\n%s", err, out)
	}
	head := f.git(t, "rev-parse", "HEAD")

	if out, err := f.exec.RunPromote(ctx, "9.9.9", "prod"); err != nil {
		t.Fatalf("second RunPromote: %v\n%s", err, out)
	}
	if got := f.git(t, "rev-parse", "HEAD"); got != head {
		t.Errorf("re-promoting the same digests created a commit (%s -> %s)", head, got)
	}
}
