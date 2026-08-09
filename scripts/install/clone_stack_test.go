package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// clone_stack_test.go -- znasllc-io/memql#3363.
//
// scripts/install/clone-stack.sh fetches the memQL stack at a release tag into
// ~/.memql/src, which is what the rest of the install substrate then runs.
//
// The assertion that matters is that a BRANCH ref is rejected -- exit 2, no
// checkout. scripts/k3d/up.sh defaults its ArgoCD targetRevision to whatever
// branch the operator is sitting on, and that is exactly right for repo
// development. It is exactly wrong for an install: a branch moves, so two
// installs of "the same version" days apart are not the same install, and
// nobody can say afterwards what a machine is actually running. A tag is the
// only ref that makes the answer to "what is installed here" durable.
//
// The tests are hermetic without stubbing git: the fixture is a real local
// bare repository under t.TempDir() carrying a branch and two tags, so real
// git plumbing is exercised (tag-vs-branch resolution is git's semantics, and
// a stub asserting our own assumptions back at us would prove nothing) with no
// network and nothing written outside the temp tree.

//=============================================================================
// HARNESS
//=============================================================================

type cloneEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func cloneRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func cloneScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(cloneRepoRoot(t), "scripts", "install", "clone-stack.sh")
}

func cloneExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func cloneLastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

// git runs a git command for the fixtures, failing the test on error.
func cloneGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=memql test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=memql test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cloneOrigin is a local bare repository standing in for the GitHub remote:
// a `main` branch and the tags v1.0.0 and v1.1.0 on successive commits.
type cloneOrigin struct {
	path   string
	v100   string // commit sha at tag v1.0.0
	v110   string // commit sha at tag v1.1.0
	branch string
}

func newCloneOrigin(t *testing.T) *cloneOrigin {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	cloneGit(t, work, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(work, "VERSION"), []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	cloneGit(t, work, "add", "VERSION")
	cloneGit(t, work, "commit", "-m", "v1.0.0")
	cloneGit(t, work, "tag", "v1.0.0")
	v100 := cloneGit(t, work, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(work, "VERSION"), []byte("1.1.0\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	cloneGit(t, work, "add", "VERSION")
	cloneGit(t, work, "commit", "-m", "v1.1.0")
	// An annotated tag, so the peeling path is exercised too.
	cloneGit(t, work, "tag", "-a", "v1.1.0", "-m", "release 1.1.0")
	v110 := cloneGit(t, work, "rev-parse", "HEAD")

	cloneGit(t, base, "clone", "--bare", work, bare)

	return &cloneOrigin{path: bare, v100: v100, v110: v110, branch: "main"}
}

// run invokes the script with stdin closed.
func cloneRun(t *testing.T, args ...string) (cloneEnvelope, int, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{cloneScript(t)}, args...)...)
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		// A hermetic run must never be able to prompt for credentials.
		"GIT_TERMINAL_PROMPT=0",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := cloneExitCode(err)
	combined := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

	var env cloneEnvelope
	line := cloneLastJSONLine(stdout.String())
	if line == "" {
		return env, code, combined
	}
	if jerr := json.Unmarshal([]byte(line), &env); jerr != nil {
		t.Fatalf("stdout is not a valid JSON envelope: %v\nline: %s\n%s", jerr, line, combined)
	}
	return env, code, combined
}

// cloneDest returns a not-yet-existing destination path under t.TempDir().
func cloneDest(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "src")
}

//=============================================================================
// THE ASSERTION THAT MATTERS: a branch is not a version
//=============================================================================

func TestCloneStackRejectsABranchRef(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag="+origin.branch, "--dest="+dest)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (bad param) for a branch ref: %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 2 {
		t.Errorf("envelope should carry ok=false error.code=2: %s", out)
	}
	if !strings.Contains(strings.ToLower(env.Error.Message), "branch") {
		t.Errorf("the error should say the ref is a branch so the operator knows what to "+
			"pass instead; got: %q", env.Error.Message)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("a checkout was created for a rejected ref at %s", dest)
	}
}

func TestCloneStackRejectsAnUnknownRef(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v9.9.9-nope", "--dest="+dest)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (bad param) for a ref that is not a tag: %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 2 {
		t.Errorf("envelope should carry ok=false error.code=2: %s", out)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("a checkout was created for an unknown ref at %s", dest)
	}
}

func TestCloneStackRequiresATag(t *testing.T) {
	origin := newCloneOrigin(t)
	env, code, out := cloneRun(t, "--repo="+origin.path, "--dest="+cloneDest(t))
	if code != 2 {
		t.Errorf("exit %d, want 2 (bad param) with no --tag: %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 2 {
		t.Errorf("envelope should carry ok=false error.code=2: %s", out)
	}
}

//=============================================================================
// THE HAPPY PATH: a pinned checkout
//=============================================================================

func TestCloneStackChecksOutTheTag(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0", "--dest="+dest)
	if code != 0 || !env.OK {
		t.Fatalf("clone failed (exit %d): %s", code, out)
	}
	if !env.Changed {
		t.Errorf("changed = false on a fresh clone: %s", out)
	}
	if env.Capability != "install.cloneStack" {
		t.Errorf("capability %q, want install.cloneStack", env.Capability)
	}
	if got := cloneGit(t, dest, "rev-parse", "HEAD"); got != origin.v100 {
		t.Errorf("HEAD is %s, want the v1.0.0 commit %s", got, origin.v100)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "VERSION")); err != nil || string(got) != "1.0.0\n" {
		t.Errorf("working tree is not at v1.0.0: %q (%v)", got, err)
	}
	if env.Result["commit"] != origin.v100 {
		t.Errorf("result.commit = %v, want %s", env.Result["commit"], origin.v100)
	}
	if env.Result["tag"] != "v1.0.0" {
		t.Errorf("result.tag = %v, want v1.0.0", env.Result["tag"])
	}
	if env.Result["dest"] != dest {
		t.Errorf("result.dest = %v, want %s", env.Result["dest"], dest)
	}
}

// An annotated tag resolves through a tag object, not straight to a commit.
// Getting the peeling wrong is the classic way a "same tag" check reports a
// spurious difference forever.
func TestCloneStackHandlesAnAnnotatedTag(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)

	if _, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.1.0", "--dest="+dest); code != 0 {
		t.Fatalf("clone failed (exit %d): %s", code, out)
	}
	if got := cloneGit(t, dest, "rev-parse", "HEAD"); got != origin.v110 {
		t.Errorf("HEAD is %s, want the v1.1.0 commit %s", got, origin.v110)
	}

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.1.0", "--dest="+dest)
	if code != 0 || !env.OK {
		t.Fatalf("re-run failed (exit %d): %s", code, out)
	}
	if env.Changed {
		t.Errorf("re-running at the same annotated tag reported changed=true: %s", out)
	}
}

//=============================================================================
// IDEMPOTENCY + UPGRADE
//=============================================================================

func TestCloneStackSameTagIsUnchanged(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)

	if env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0", "--dest="+dest); code != 0 || !env.Changed {
		t.Fatalf("first clone: exit %d changed %v: %s", code, env.Changed, out)
	}

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0", "--dest="+dest)
	if code != 0 || !env.OK {
		t.Fatalf("second run failed (exit %d): %s", code, out)
	}
	if env.Changed {
		t.Errorf("second run at the same tag reported changed=true: %s", out)
	}
	if got := cloneGit(t, dest, "rev-parse", "HEAD"); got != origin.v100 {
		t.Errorf("HEAD moved on a no-op run: %s", got)
	}
}

func TestCloneStackMovesToANewTag(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)

	if _, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0", "--dest="+dest); code != 0 {
		t.Fatalf("first clone failed (exit %d): %s", code, out)
	}

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.1.0", "--dest="+dest)
	if code != 0 || !env.OK {
		t.Fatalf("upgrade failed (exit %d): %s", code, out)
	}
	if !env.Changed {
		t.Errorf("moving to a different tag reported changed=false: %s", out)
	}
	if got := cloneGit(t, dest, "rev-parse", "HEAD"); got != origin.v110 {
		t.Errorf("HEAD is %s, want the v1.1.0 commit %s", got, origin.v110)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "VERSION")); err != nil || string(got) != "1.1.0\n" {
		t.Errorf("working tree did not move to v1.1.0: %q (%v)", got, err)
	}
}

//=============================================================================
// REFUSALS + PREREQUISITES
//=============================================================================

// The destination is the operator's directory. If something that is not our
// checkout is sitting there, refuse rather than clobber it.
func TestCloneStackRefusesANonGitDestination(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	keep := filepath.Join(dest, "operator-file")
	if err := os.WriteFile(keep, []byte("do not delete me\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0", "--dest="+dest)
	if code != 3 {
		t.Errorf("exit %d, want 3 (refused): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 3 {
		t.Errorf("envelope should carry ok=false error.code=3: %s", out)
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "do not delete me\n" {
		t.Errorf("the operator's file was disturbed: %q (%v)", got, err)
	}
}

// An EMPTY directory is not somebody's data -- cloning into it is fine, and
// requiring the operator to remove it would be pointless friction.
func TestCloneStackAcceptsAnEmptyDestination(t *testing.T) {
	origin := newCloneOrigin(t)
	dest := cloneDest(t)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}

	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0", "--dest="+dest)
	if code != 0 || !env.OK {
		t.Fatalf("clone into an empty dir failed (exit %d): %s", code, out)
	}
	if got := cloneGit(t, dest, "rev-parse", "HEAD"); got != origin.v100 {
		t.Errorf("HEAD is %s, want %s", got, origin.v100)
	}
}

func TestCloneStackUnreachableRepoIsExitFive(t *testing.T) {
	dest := cloneDest(t)
	missing := filepath.Join(t.TempDir(), "no-such-repo.git")

	env, code, out := cloneRun(t, "--repo="+missing, "--tag=v1.0.0", "--dest="+dest)
	if code != 5 {
		t.Errorf("exit %d, want 5 (operation failed): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 5 {
		t.Errorf("envelope should carry ok=false error.code=5: %s", out)
	}
}

func TestCloneStackMissingGitIsExitFour(t *testing.T) {
	origin := newCloneOrigin(t)
	env, code, out := cloneRun(t, "--repo="+origin.path, "--tag=v1.0.0",
		"--dest="+cloneDest(t), "--git="+filepath.Join(t.TempDir(), "definitely-not-git"))
	if code != 4 {
		t.Errorf("exit %d, want 4 (prerequisite missing): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Errorf("envelope should carry ok=false error.code=4: %s", out)
	}
}

//=============================================================================
// DEFAULTS
//=============================================================================

// The default destination is ~/.memql/src and the default repo is the engine.
// Asserted statically: a test that actually ran the defaults would clone over
// the network into the runner's home directory.
func TestCloneStackDefaults(t *testing.T) {
	b, err := os.ReadFile(cloneScript(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		`DEFAULT_DEST="${HOME}/.memql/src"`,
		"znasllc-io/memql",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("script should declare %q", want)
		}
	}
}
