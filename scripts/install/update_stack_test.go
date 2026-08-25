package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// update_stack_test.go -- znasllc-io/memql#4577.
//
// scripts/install/update-stack.sh brings an existing checkout up to date, and
// the property worth a test file is not "it can fast-forward" -- git can do
// that. It is that A REFUSAL CHANGES NOTHING. The person who presses the button
// this backs is a developer holding uncommitted work, and every row of the
// table below that ends in a refusal asserts both halves: the outcome the
// envelope names, and that HEAD and the working tree are exactly where they
// were.
//
// The fixtures are real local repositories, for the reason clone_stack_test.go
// gives: fast-forward-ness, overlap detection and conflict detection are git's
// semantics, and a stub would only assert our own assumptions back at us. The
// shallow case in particular CANNOT be faked -- it exists because at depth 1
// git genuinely cannot prove an ancestry it would otherwise see, and that is
// the whole reason the script deepens.

//=============================================================================
// HARNESS
//=============================================================================

type updateEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e updateEnvelope) outcome() string {
	s, _ := e.Result["outcome"].(string)
	return s
}

func updateScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(cloneRepoRoot(t), "scripts", "install", "update-stack.sh")
}

// runUpdate invokes the script and returns the parsed envelope plus the exit
// code. The environment is pinned so the run cannot read the developer's own
// git configuration -- an identity inherited from ~/.gitconfig would make the
// no-identity case pass on one machine and fail on another.
func runUpdate(t *testing.T, extraEnv []string, args ...string) (updateEnvelope, int) {
	t.Helper()
	cmd := exec.Command(updateScript(t), args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=memql test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=memql test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.Output() // stdout only: the envelope. Logs go to stderr.
	code := cloneExitCode(err)
	line := cloneLastJSONLine(string(out))
	if line == "" {
		t.Fatalf("no result envelope on stdout (exit %d); stdout was:\n%s", code, out)
	}
	var env updateEnvelope
	if jsonErr := json.Unmarshal([]byte(line), &env); jsonErr != nil {
		t.Fatalf("envelope is not JSON: %v\n%s", jsonErr, line)
	}
	if env.Capability != "install.updateStack" {
		t.Fatalf("envelope names capability %q", env.Capability)
	}
	return env, code
}

// updateFixture is an origin plus a checkout of it, both real repositories.
type updateFixture struct {
	origin string // bare repo standing in for GitHub
	work   string // the tree that pushes to it
	dest   string // the checkout under test
}

func newUpdateFixture(t *testing.T, cloneArgs ...string) *updateFixture {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	dest := filepath.Join(base, "src")

	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	cloneGit(t, work, "init", "--initial-branch=main", ".")
	writeFile(t, work, "README.md", "one\n")
	writeFile(t, work, "other.txt", "untouched\n")
	cloneGit(t, work, "add", ".")
	cloneGit(t, work, "commit", "-m", "first")
	cloneGit(t, base, "clone", "--bare", work, bare)
	cloneGit(t, work, "remote", "add", "origin", bare)

	args := append([]string{"clone"}, cloneArgs...)
	// file:// rather than a plain path, and it is load-bearing for one test:
	// git ignores --depth for a LOCAL clone (it hardlinks the object store
	// instead), so a shallow fixture built from a path is silently not
	// shallow -- and the one row that cannot be faked would quietly test
	// nothing.
	args = append(args, "file://"+bare, dest)
	cloneGit(t, base, args...)
	// A local identity so a merge can author a commit. The no-identity case
	// gets its own fixture rather than being the default, because every other
	// test would otherwise be asserting the wrong refusal.
	cloneGit(t, dest, "config", "user.name", "memql test")
	cloneGit(t, dest, "config", "user.email", "test@example.invalid")

	return &updateFixture{origin: bare, work: work, dest: dest}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// pushUpstream adds a commit to the origin, as somebody else's push would.
func (f *updateFixture) pushUpstream(t *testing.T, name, body, message string) string {
	t.Helper()
	writeFile(t, f.work, name, body)
	cloneGit(t, f.work, "add", ".")
	cloneGit(t, f.work, "commit", "-m", message)
	cloneGit(t, f.work, "push", "origin", "main")
	return cloneGit(t, f.work, "rev-parse", "HEAD")
}

func (f *updateFixture) head(t *testing.T) string {
	t.Helper()
	return cloneGit(t, f.dest, "rev-parse", "HEAD")
}

func (f *updateFixture) status(t *testing.T) string {
	t.Helper()
	return cloneGit(t, f.dest, "status", "--porcelain")
}

// unchanged asserts the property the whole file exists for.
func (f *updateFixture) unchanged(t *testing.T, head, status string) {
	t.Helper()
	if got := f.head(t); got != head {
		t.Errorf("HEAD moved to %s; a refusal must change nothing (was %s)", got, head)
	}
	if got := f.status(t); got != status {
		t.Errorf("the working tree changed.\n  was: %q\n  now: %q\nA refusal must change nothing.", status, got)
	}
}

//=============================================================================
// THE TABLE
//=============================================================================

func TestUpdateIsANoOpWhenAlreadyCurrent(t *testing.T) {
	f := newUpdateFixture(t)
	before := f.head(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 0 || !env.OK {
		t.Fatalf("exit %d, envelope %+v", code, env)
	}
	if env.outcome() != "upToDate" {
		t.Errorf("outcome = %q, want upToDate", env.outcome())
	}
	if env.Changed {
		t.Error("changed = true on a checkout that was already current")
	}
	if got := f.head(t); got != before {
		t.Errorf("HEAD moved to %s from %s", got, before)
	}
}

func TestUpdateFastForwardsACleanCheckout(t *testing.T) {
	f := newUpdateFixture(t)
	want := f.pushUpstream(t, "README.md", "two\n", "second")

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 0 || !env.OK {
		t.Fatalf("exit %d, envelope %+v", code, env)
	}
	if env.outcome() != "fastForward" {
		t.Fatalf("outcome = %q, want fastForward", env.outcome())
	}
	if !env.Changed {
		t.Error("changed = false after moving the checkout forward")
	}
	if got := f.head(t); got != want {
		t.Errorf("HEAD = %s, want the fetched tip %s", got, want)
	}
	if got, _ := env.Result["commitAfter"].(string); got != want {
		t.Errorf("commitAfter = %q, want %q", got, want)
	}
}

// THE CASE THE FEATURE EXISTS FOR: a developer holding uncommitted work that
// does not collide with what is arriving. Their edit has to survive.
func TestUpdateCarriesUnrelatedEditsAcross(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "README.md", "two\n", "second")
	writeFile(t, f.dest, "other.txt", "my work in progress\n")

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 0 || env.outcome() != "fastForward" {
		t.Fatalf("exit %d, outcome %q, error %+v", code, env.outcome(), env.Error)
	}
	body, err := os.ReadFile(filepath.Join(f.dest, "other.txt"))
	if err != nil {
		t.Fatalf("read other.txt: %v", err)
	}
	if string(body) != "my work in progress\n" {
		t.Errorf("other.txt = %q -- the update overwrote uncommitted work", body)
	}
}

func TestUpdateRefusesWhenEditsOverlapWhatIsArriving(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "README.md", "two\n", "second")
	writeFile(t, f.dest, "README.md", "my own edit\n")
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 3 || env.OK {
		t.Fatalf("exit %d, ok %v; want a refusal", code, env.OK)
	}
	if env.outcome() != "blockedByLocalEdits" {
		t.Errorf("outcome = %q, want blockedByLocalEdits", env.outcome())
	}
	if files, _ := env.Result["files"].(string); !strings.Contains(files, "README.md") {
		t.Errorf("files = %q, want it to name README.md", files)
	}
	// The refusal has to be actionable, and "or build what you have" is the
	// button the editor offers beside it.
	if msg := env.Error.Message; !strings.Contains(msg, "README.md") {
		t.Errorf("the message does not name the file: %s", msg)
	}
	f.unchanged(t, head, status)
}

// An incoming commit that ADDS a file the operator already has sitting there
// untracked blocks for a different reason and is just as silent, so it is
// detected the same way.
func TestUpdateRefusesWhenAnUntrackedFileIsInTheWay(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "NEW.md", "theirs\n", "add NEW.md")
	writeFile(t, f.dest, "NEW.md", "mine\n")
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 3 || env.outcome() != "blockedByLocalEdits" {
		t.Fatalf("exit %d, outcome %q; want a refusal naming local edits", code, env.outcome())
	}
	if files, _ := env.Result["files"].(string); !strings.Contains(files, "NEW.md") {
		t.Errorf("files = %q, want it to name NEW.md", files)
	}
	f.unchanged(t, head, status)
}

func TestUpdateRefusesADivergedCheckoutByDefault(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "README.md", "theirs\n", "their commit")
	writeFile(t, f.dest, "mine.txt", "mine\n")
	cloneGit(t, f.dest, "add", ".")
	cloneGit(t, f.dest, "commit", "-m", "my commit")
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 3 || env.outcome() != "blockedByDivergence" {
		t.Fatalf("exit %d, outcome %q; want blockedByDivergence", code, env.outcome())
	}
	if ahead, _ := env.Result["ahead"].(float64); ahead != 1 {
		t.Errorf("ahead = %v, want 1", env.Result["ahead"])
	}
	if behind, _ := env.Result["behind"].(float64); behind != 1 {
		t.Errorf("behind = %v, want 1", env.Result["behind"])
	}
	f.unchanged(t, head, status)
}

func TestUpdateMergesADivergedCheckoutWhenAsked(t *testing.T) {
	f := newUpdateFixture(t)
	theirs := f.pushUpstream(t, "theirs.txt", "theirs\n", "their commit")
	writeFile(t, f.dest, "mine.txt", "mine\n")
	cloneGit(t, f.dest, "add", ".")
	cloneGit(t, f.dest, "commit", "-m", "my commit")
	mine := f.head(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest, "--strategy=merge")
	if code != 0 || env.outcome() != "merged" {
		t.Fatalf("exit %d, outcome %q, error %+v", code, env.outcome(), env.Error)
	}
	parents := strings.Fields(cloneGit(t, f.dest, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("HEAD has %d parents, want a merge commit with two: %v", len(parents)-1, parents)
	}
	if parents[1] != mine || parents[2] != theirs {
		t.Errorf("merge parents = %v, want %s and %s", parents[1:], mine, theirs)
	}
}

func TestUpdateRefusesToMergeOverUncommittedWork(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "theirs.txt", "theirs\n", "their commit")
	writeFile(t, f.dest, "mine.txt", "mine\n")
	cloneGit(t, f.dest, "add", ".")
	cloneGit(t, f.dest, "commit", "-m", "my commit")
	// ...and then keep working.
	writeFile(t, f.dest, "other.txt", "still editing\n")
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest, "--strategy=merge")
	if code != 3 || env.outcome() != "blockedByLocalEdits" {
		t.Fatalf("exit %d, outcome %q; want blockedByLocalEdits", code, env.outcome())
	}
	f.unchanged(t, head, status)
}

// The one row that DOES change the tree, and the one the operator is warned
// about. Leaving the conflict in place is the design: the editor has a merge
// editor, and aborting from under a developer discards work they can see.
func TestUpdateLeavesAConflictInPlaceAndNamesIt(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "README.md", "theirs\n", "their commit")
	writeFile(t, f.dest, "README.md", "mine\n")
	cloneGit(t, f.dest, "add", ".")
	cloneGit(t, f.dest, "commit", "-m", "my commit")

	env, code := runUpdate(t, nil, "--dest="+f.dest, "--strategy=merge")
	if code != 3 || env.outcome() != "blockedByConflict" {
		t.Fatalf("exit %d, outcome %q, error %+v", code, env.outcome(), env.Error)
	}
	if got, _ := env.Result["conflicts"].(string); !strings.Contains(got, "README.md") {
		t.Errorf("conflicts = %q, want it to name README.md", got)
	}
	if !env.Changed {
		t.Error("changed = false, but the conflict was deliberately left in the tree")
	}
	gitDir := cloneGit(t, f.dest, "rev-parse", "--absolute-git-dir")
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err != nil {
		t.Errorf("MERGE_HEAD is absent, so the merge was aborted: %v", err)
	}
	// And the way out is in the message, because nothing else will tell them.
	if msg := env.Error.Message; !strings.Contains(msg, "merge --abort") {
		t.Errorf("the message does not say how to undo it: %s", msg)
	}
}

// Every read in this script is a lie during an unfinished merge, so the check
// comes first -- before the fetch, which is what makes it observable: the run
// must refuse without having touched the network or the tree.
func TestUpdateRefusesWhileAMergeIsUnderWay(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "README.md", "theirs\n", "their commit")
	writeFile(t, f.dest, "README.md", "mine\n")
	cloneGit(t, f.dest, "add", ".")
	cloneGit(t, f.dest, "commit", "-m", "my commit")
	if _, code := runUpdate(t, nil, "--dest="+f.dest, "--strategy=merge"); code != 3 {
		t.Fatalf("setup: expected the conflicting merge to be refused, got exit %d", code)
	}
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 3 || env.outcome() != "blockedByInProgress" {
		t.Fatalf("exit %d, outcome %q; want blockedByInProgress", code, env.outcome())
	}
	if msg := env.Error.Message; !strings.Contains(msg, "merge") {
		t.Errorf("the message does not name what is running: %s", msg)
	}
	f.unchanged(t, head, status)
}

// A DETACHED CHECKOUT IS ORDINARY, not a fault: a release install detaches at a
// tag, and a repair of a branch install detaches at an exact commit. So it gets
// a real answer -- a refusal that names the missing parameter, and a working
// fast-forward once it is given.
func TestUpdateAsksWhichBranchWhenTheCheckoutIsOnNone(t *testing.T) {
	f := newUpdateFixture(t)
	cloneGit(t, f.dest, "checkout", "--detach", "HEAD")
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (a missing parameter)", code)
	}
	if msg := env.Error.Message; !strings.Contains(msg, "--branch") {
		t.Errorf("the message does not name the way through: %s", msg)
	}
	f.unchanged(t, head, status)
}

func TestUpdateFastForwardsADetachedCheckout(t *testing.T) {
	f := newUpdateFixture(t)
	cloneGit(t, f.dest, "checkout", "--detach", "HEAD")
	want := f.pushUpstream(t, "README.md", "two\n", "second")

	env, code := runUpdate(t, nil, "--dest="+f.dest, "--branch=main")
	if code != 0 || env.outcome() != "fastForward" {
		t.Fatalf("exit %d, outcome %q, error %+v", code, env.outcome(), env.Error)
	}
	if got := f.head(t); got != want {
		t.Errorf("HEAD = %s, want %s", got, want)
	}
	if detached, _ := env.Result["detached"].(bool); !detached {
		t.Error("the envelope does not report that the checkout is not on a branch")
	}
}

// THE ROW THAT CANNOT BE FAKED (design D5). The wizard clones `--depth 1
// --single-branch`. At depth 1 the local commit and the fetched tip share no
// ancestry in the object store, so git cannot see the fast-forward and every
// update would report a divergence that does not exist. This asserts the
// deepening AND the correct outcome, because either alone would pass over a
// script that deepened and then still got the answer wrong.
func TestUpdateDeepensAShallowCheckoutSoTheAnswerIsRight(t *testing.T) {
	f := newUpdateFixture(t, "--depth=1", "--single-branch", "--branch=main")
	if got := cloneGit(t, f.dest, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("fixture is not shallow (%q); this test would prove nothing", got)
	}
	want := f.pushUpstream(t, "README.md", "two\n", "second")

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 0 || env.outcome() != "fastForward" {
		t.Fatalf("exit %d, outcome %q, error %+v -- a shallow checkout must not read as diverged",
			code, env.outcome(), env.Error)
	}
	if unshallowed, _ := env.Result["unshallowed"].(bool); !unshallowed {
		t.Error("the envelope does not report that the history was fetched")
	}
	if got := f.head(t); got != want {
		t.Errorf("HEAD = %s, want %s", got, want)
	}
	if got := cloneGit(t, f.dest, "rev-parse", "--is-shallow-repository"); got != "false" {
		t.Errorf("the checkout is still shallow (%q), so the next update would be wrong too", got)
	}
}

// The negative control for the row above: WITHOUT the deepening, the same
// fixture genuinely does read as diverged. This is what makes the assertion
// above evidence rather than a coincidence -- git's own answer at depth 1.
func TestAShallowCheckoutGenuinelyLooksDivergedToGit(t *testing.T) {
	f := newUpdateFixture(t, "--depth=1", "--single-branch", "--branch=main")
	f.pushUpstream(t, "README.md", "two\n", "second")
	cloneGit(t, f.dest, "fetch", "--depth=1", "origin", "main")

	cmd := exec.Command("git", "-C", f.dest, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if err := cmd.Run(); err == nil {
		t.Skip("this git can prove the ancestry at depth 1, so the deepening is not load-bearing here")
	}
	// It cannot -- which is the whole reason update-stack.sh deepens first.
}

func TestUpdateRefusesToWriteAMergeCommitWithNoIdentity(t *testing.T) {
	f := newUpdateFixture(t)
	cloneGit(t, f.dest, "config", "--unset", "user.name")
	cloneGit(t, f.dest, "config", "--unset", "user.email")
	f.pushUpstream(t, "theirs.txt", "theirs\n", "their commit")
	writeFile(t, f.dest, "mine.txt", "mine\n")
	cloneGit(t, f.dest, "add", ".")
	cloneGit(t, f.dest, "-c", "user.name=x", "-c", "user.email=x@y.invalid", "commit", "-m", "my commit")
	head, status := f.head(t), f.status(t)

	env, code := runUpdate(t, nil, "--dest="+f.dest, "--strategy=merge")
	if code != 4 {
		t.Fatalf("exit %d, want 4 (a missing prerequisite); envelope %+v", code, env)
	}
	if msg := env.Error.Message; !strings.Contains(msg, "user.name") {
		t.Errorf("the message does not name what to set: %s", msg)
	}
	f.unchanged(t, head, status)
}

//=============================================================================
// PARAMETERS
//=============================================================================

func TestUpdateRejectsAnUnknownStrategy(t *testing.T) {
	f := newUpdateFixture(t)
	env, code := runUpdate(t, nil, "--dest="+f.dest, "--strategy=rebase")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if msg := env.Error.Message; !strings.Contains(msg, "fastForward") {
		t.Errorf("the message does not name the accepted values: %s", msg)
	}
}

func TestUpdateRejectsAnUnknownRemote(t *testing.T) {
	f := newUpdateFixture(t)
	env, code := runUpdate(t, nil, "--dest="+f.dest, "--remote=upstream")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if msg := env.Error.Message; !strings.Contains(msg, "upstream") {
		t.Errorf("the message does not name the remote: %s", msg)
	}
}

func TestUpdateRefusesADirectoryThatIsNotACheckout(t *testing.T) {
	dir := t.TempDir()
	env, code := runUpdate(t, nil, "--dest="+dir)
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if msg := env.Error.Message; !strings.Contains(msg, dir) {
		t.Errorf("the message does not name the directory: %s", msg)
	}
}

// The envelope names WHERE it ran even when it refuses. A failure carrying only
// a reason leaves the operator guessing which checkout it was about -- and the
// editor renders these fields on the failure screen.
func TestARefusalStillNamesTheCheckout(t *testing.T) {
	f := newUpdateFixture(t)
	f.pushUpstream(t, "README.md", "theirs\n", "their commit")
	writeFile(t, f.dest, "README.md", "mine\n")

	env, code := runUpdate(t, nil, "--dest="+f.dest)
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	for _, key := range []string{"dest", "remote", "branch", "strategy", "commitBefore"} {
		if v, _ := env.Result[key].(string); v == "" {
			t.Errorf("the refusal envelope carries no %s", key)
		}
	}
}
