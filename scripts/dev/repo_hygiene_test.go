// Static guards over two tracking-policy decisions this repo had already made
// but had never actually enforced (znasllc-io/memql#3344).
//
// Both failures were of the same shape: .gitignore stated an intent, the tree
// disagreed, and nothing compared the two.
//
//  1. `.claude/settings.local.json` is a PERSONAL permission allowlist. It was
//     kept out of commits only by an operator's GLOBAL gitignore
//     (~/.config/git/ignore). Nothing in the repo protected it -- and this
//     repository is PUBLIC, so on a different machine, or from any other
//     contributor, it would have been offered up by `git status` and committed
//     for the world to read. A guard that consulted the global config would
//     have reported this healthy, so the check below explicitly disables it.
//
//  2. `.gitignore` exempts `sdk/ts/package-lock.json` from the blanket
//     package-lock.json ignore -- i.e. the repo declared "this file is
//     tracked" -- but the file had never been committed. The declared
//     intent and reality disagreed for months. Downstream, `sdk/ts` could not
//     use `npm ci`, so three sites fell back to `npm install` (including the
//     PUBLISH workflow), and a Makefile comment recorded the workaround as
//     though it were a decision.
//
// Lives in scripts/dev, which `go test ./...` reaches, alongside this repo's
// other CI/layout guards.
package dev

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// personalClaudeState is state that must never become committable. These are
// per-operator or per-machine, and the repo is public.
var personalClaudeState = []string{
	".claude/settings.local.json",
	".claude/worktrees/some-checkout/file.txt",
	".claude/scheduled_tasks.lock",
}

// sharedClaudeState should travel WITH the project -- the whole point of the
// opt-in negations. Asserting both directions keeps a future "just ignore all
// of .claude" from quietly dropping project skills on the floor.
var sharedClaudeState = []string{
	".claude/skills/some-skill/SKILL.md",
	".claude/commands/some-command.md",
	".claude/agents/some-agent.md",
	".claude/settings.json",
}

// ignoredByRepo reports whether the repo's OWN ignore rules exclude path.
//
// core.excludesFile=/dev/null is the load-bearing part: without it this
// consults the operator's global gitignore, which is exactly the thing that
// masked the defect. A guard that can be satisfied by a file outside the
// repository is not a guard on the repository.
func ignoredByRepo(t *testing.T, root, path string) bool {
	t.Helper()
	cmd := exec.Command("git", "-c", "core.excludesFile=/dev/null",
		"check-ignore", "-q", "--no-index", path)
	cmd.Dir = root
	err := cmd.Run()
	if err == nil {
		return true // exit 0: ignored
	}
	var exit *exec.ExitError
	if ok := asExitError(err, &exit); ok && exit.ExitCode() == 1 {
		return false // exit 1: not ignored
	}
	t.Fatalf("git check-ignore %s: %v", path, err)
	return false
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func TestPersonalClaudeStateIsIgnoredByTheRepoItself(t *testing.T) {
	root := repoRoot(t)
	for _, p := range personalClaudeState {
		if !ignoredByRepo(t, root, p) {
			t.Errorf("%s is NOT ignored by the repository's own .gitignore.\n"+
				"This repo is PUBLIC and that path is per-operator or per-machine "+
				"state. Relying on a contributor's global gitignore to keep it out "+
				"is not protection -- it protects exactly one machine (#3344).", p)
		}
	}
}

func TestProjectClaudeStateStaysCommittable(t *testing.T) {
	root := repoRoot(t)
	for _, p := range sharedClaudeState {
		if ignoredByRepo(t, root, p) {
			t.Errorf("%s IS ignored, but project skills / commands / agents / "+
				"shared settings are supposed to travel with the repository.\n"+
				"The `.claude/*` ignore is opt-in by design: it must keep negating "+
				"these back in (#3344).", p)
		}
	}
}

// lockNegation matches a .gitignore line that re-includes a lockfile.
var lockNegation = regexp.MustCompile(`^!(\S*package-lock\.json)$`)

// A negation that re-includes a lockfile is a claim that the file is tracked.
// If it is not, the claim is silently false -- which is precisely how
// sdk/ts/package-lock.json went missing while .gitignore said otherwise, and
// how `npm ci` quietly became impossible for that package.
func TestLockfileNegationsHaveATrackedFileBehindThem(t *testing.T) {
	root := repoRoot(t)
	raw := readRepoFile(t, filepath.Join(root, ".gitignore"))

	var claims []string
	for _, line := range strings.Split(raw, "\n") {
		if m := lockNegation.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			claims = append(claims, m[1])
		}
	}
	if len(claims) == 0 {
		t.Fatal("found no `!...package-lock.json` negations in .gitignore; this " +
			"guard cannot pass vacuously. If lockfiles genuinely stopped being " +
			"tracked, delete this test rather than letting it report green having " +
			"checked nothing (#3344)")
	}

	for _, path := range claims {
		cmd := exec.Command("git", "ls-files", "--error-unmatch", path)
		cmd.Dir = root
		if err := cmd.Run(); err != nil {
			t.Errorf(".gitignore re-includes %q, which asserts the file is tracked "+
				"-- but git does not track it.\n"+
				"An un-ignored-but-absent lockfile is worse than no rule: it reads "+
				"as a reproducible, integrity-pinned install while `npm ci` cannot "+
				"run at all, so every consumer silently falls back to `npm install` "+
				"(#3344).", path)
		}
	}
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("cat", path).Output()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(out)
}
