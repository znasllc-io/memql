// The authoring-surface gate is held to the BASE COMMIT, not to its own tree
// (memql#5389, D21).
//
// # The bypass these tests close
//
// `make memqlbreaking` reads the surface baseline out of the tree it is
// judging. Both sides of the comparison are therefore editable by the change
// under test, so deleting a name from the source AND deleting its entry from
// component/language/surface/2026.json leaves no deletion to see. Measured on
// 0e70bc702, with `daysBetween` removed from the function catalog and from the
// committed baseline and no entry in component/language/reserved.json:
//
//	SUCCESS: no breaks against component/language/surface/2026.json
//	exit=0
//
// The reservation the ledger exists to force into the open was never asked for.
// That is the whole gate walking through itself, and it is silent.
//
// # Why there are three tests and not one
//
// The fix has two halves that fail independently, in the way a CI gate usually
// rots: the mechanism can be right while nothing runs it.
//
//   - TestTheBaseCommitScriptRefusesALockstepRemoval drives the real script
//     against a real base commit and asserts BOTH directions in one run -- the
//     committed-baseline comparison passing (the bypass, reproduced) and the
//     base-commit comparison failing by name.
//   - TestCIHoldsTheAuthoringSurfaceToTheBaseCommit asserts the workflow step
//     runs that script with a BASE_SHA. Reverting the step to
//     `make memqlbreaking` reads like a simplification and reopens the bypass
//     in full, with every test above still green.
//   - TestTheScriptAndTheToolNameTheSameCommittedFiles pins the two paths the
//     script extracts to the tool's own constants. A disagreement there
//     extracts a file nobody committed, which git reports as absent -- and an
//     absent base surface is the first-baseline FALLBACK, so the run would go
//     green against the committed baseline with a warning nobody reads.
package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// theScript, and the two files it extracts from the base commit.
const (
	baseScriptRel   = "scripts/ci/memqlbreaking-base.sh"
	toolConstantsGo = "cmd/memqlbreaking/reserved.go"
)

// TestCIHoldsTheAuthoringSurfaceToTheBaseCommit asserts the go-checks step
// runs the script with a base commit, and not the make target.
//
// The BASE_SHA expression names one field per event this workflow declares --
// pull_request, merge_group and push -- because a missing arm is not an error
// anywhere: the expression evaluates to the empty string, the script announces
// the committed-baseline fallback on stderr, and the step goes green over
// exactly the change it exists to catch.
func TestCIHoldsTheAuthoringSurfaceToTheBaseCommit(t *testing.T) {
	root := repoRootForBreakingBase(t)
	step := memqlbreakingStep(t, root)

	if !strings.Contains(step.Run, baseScriptRel) {
		t.Errorf("the memqlbreaking CI step runs %q; it must run %s, which holds this tree to the BASE\n"+
			"COMMIT's baseline. `make memqlbreaking` reads the baseline out of the tree it is judging, so a\n"+
			"change that deletes a name from the source AND from component/language/surface/2026.json passes it.",
			strings.TrimSpace(step.Run), baseScriptRel)
	}
	if strings.Contains(step.Run, "make memqlbreaking") {
		t.Errorf("the memqlbreaking CI step is back on `make memqlbreaking`: %q. That target compares this\n"+
			"tree against its OWN committed baseline, which the change under test may edit in the same commit.",
			strings.TrimSpace(step.Run))
	}

	baseSHA, ok := step.Env["BASE_SHA"]
	if !ok {
		t.Fatalf("the memqlbreaking CI step sets no BASE_SHA. Without it the script falls back to the committed\n"+
			"baseline on every build -- green, loud on stderr, and exactly the comparison this step replaced.\n"+
			"step env: %v", step.Env)
	}
	for _, field := range []string{
		"github.event.pull_request.base.sha",
		"github.event.merge_group.base_sha",
		"github.event.before",
	} {
		if !strings.Contains(baseSHA, field) {
			t.Errorf("BASE_SHA does not name %s: %q.\nEvery event this workflow runs on needs its own arm; a\n"+
				"missing one is an empty string, which is the silent fallback rather than a failure.", field, baseSHA)
		}
	}
}

// TestTheScriptAndTheToolNameTheSameCommittedFiles pins the script's two path
// literals to the tool's constants.
func TestTheScriptAndTheToolNameTheSameCommittedFiles(t *testing.T) {
	root := repoRootForBreakingBase(t)
	script := readFileForBreakingBase(t, filepath.Join(root, baseScriptRel))
	constants := readFileForBreakingBase(t, filepath.Join(root, toolConstantsGo))

	for _, pair := range []struct{ goConst, shellVar string }{
		{"BaselinePath", "SURFACE_REL"},
		{"ReservedPath", "LEDGER_REL"},
	} {
		want := goStringConst(t, constants, pair.goConst)
		got := shellStringConst(t, script, pair.shellVar)
		if got != want {
			t.Errorf("%s in %s is %q but %s in %s is %q.\nThe script extracts the shell one from the base commit\n"+
				"and the tool defaults to the Go one; a disagreement extracts a path the base commit does not\n"+
				"have, which is read as the first-baseline case and falls back to the committed baseline.",
				pair.shellVar, baseScriptRel, got, pair.goConst, toolConstantsGo, want)
		}
	}
}

// TestTheBaseCommitScriptRefusesALockstepRemoval is the property, end to end.
//
// A change that removes a name from the surface AND from the committed baseline,
// with no reservation, still fails.
//
// The head tree is this checkout, unmodified: a fabricated base commit whose
// surface carries two names this tree does not have IS the lockstep change,
// seen from the other side -- the source no longer defines them and the
// committed baseline no longer lists them. The two names are the two answers
// the gate has to give:
//
//	unreservedRemoval  no ledger entry  -> a parse break, named
//	coalesce           reserved         -> accepted, silently
//
// The base commit is built in a temporary repository and read through
// GIT_ALTERNATE_OBJECT_DIRECTORIES, so the real object store is never written
// to and the script's own `git cat-file -e` / `git show` path is the one under
// test rather than a stand-in for it.
func TestTheBaseCommitScriptRefusesALockstepRemoval(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs cmd/memqlbreaking twice")
	}
	root := repoRootForBreakingBase(t)
	const unreservedRemoval = "unreservedRemovalForTest"

	// The base commit's surface: the committed one plus the two names.
	surface := readFileForBreakingBase(t, filepath.Join(root, "component/language/surface/2026.json"))
	injected := `"functions": {` + "\n" +
		`    "` + unreservedRemoval + `": {` + "\n" +
		`      "name": "` + unreservedRemoval + `",` + "\n" +
		`      "form": "` + unreservedRemoval + `() any [M]"` + "\n" +
		`    },` + "\n" +
		`    "coalesce": {` + "\n" +
		`      "name": "coalesce",` + "\n" +
		`      "form": "coalesce(a any, b any) any [P]"` + "\n" +
		`    },`
	if !strings.Contains(surface, `"functions": {`) {
		t.Fatalf("the committed baseline has no functions section; this test's injection point is gone")
	}
	baseSurface := strings.Replace(surface, `"functions": {`, injected, 1)

	// The base commit's ledger is the committed one, unchanged: this change
	// erases no reservation, so the ledger half must stay silent.
	ledger := readFileForBreakingBase(t, filepath.Join(root, "component/language/reserved.json"))
	if !strings.Contains(ledger, `"coalesce"`) {
		t.Fatalf("component/language/reserved.json no longer reserves coalesce; pick another reserved function")
	}

	sha, objects := fabricateBaseCommit(t, root, map[string]string{
		"component/language/surface/2026.json": baseSurface,
		"component/language/reserved.json":     ledger,
	})

	// Direction one: today's command, against the tree's own committed
	// baseline. It passes -- that IS the bypass, and it is here so a future
	// reader can see the two answers side by side rather than take it on trust.
	committed := runInRepo(t, root, nil, "make", "memqlbreaking")
	if committed.code != 0 {
		t.Fatalf("`make memqlbreaking` is not clean on this tree (exit %d), so this test cannot tell the\n"+
			"bypass from a pre-existing break:\n%s", committed.code, committed.out)
	}

	// Direction two: the CI path, against the base commit.
	got := runInRepo(t, root, []string{
		"BASE_SHA=" + sha,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects,
	}, "bash", baseScriptRel)

	if got.code != 1 {
		t.Fatalf("holding the tree to the base commit exited %d, want 1 (breaks found).\nA removal edited into\n"+
			"the source and the committed baseline together must still fail.\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, unreservedRemoval) {
		t.Errorf("the report does not name %q, the name removed with no reservation:\n%s", unreservedRemoval, got.out)
	}
	if strings.Contains(got.out, "coalesce") {
		t.Errorf("the report names coalesce, which component/language/reserved.json reserves. A reservation is\n"+
			"what makes a deletion landable; reporting one anyway leaves no way to land a deliberate\n"+
			"removal:\n%s", got.out)
	}
	if !strings.Contains(got.out, sha) {
		t.Errorf("the report does not name the base commit %s, so a reader cannot tell which tree they were\n"+
			"held to:\n%s", sha, got.out)
	}
}

// TestTheFirstBaselineFallbackSaysWhatItComparedAgainst covers the one run that
// does NOT hold the tree to the base commit: a base commit predating the
// baseline file, where there is nothing to be held to and refusing would red
// every build until the baseline's own commit is the base.
//
// The fallback itself is fine. What is not fine is a report that still names
// the base commit: a claim that is not true of the comparison it made is the
// same defect as the gate this script replaces, one level up, and it is the
// version a reader has no way to catch.
func TestTheFirstBaselineFallbackSaysWhatItComparedAgainst(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs cmd/memqlbreaking")
	}
	root := repoRootForBreakingBase(t)
	sha, objects := fabricateBaseCommit(t, root, map[string]string{
		"README.md": "a commit from before the surface baseline existed\n",
	})

	got := runInRepo(t, root, []string{
		"BASE_SHA=" + sha,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects,
	}, "bash", baseScriptRel)

	if got.code != 0 {
		t.Fatalf("the first-baseline fallback exited %d, want 0. A base commit with no baseline must not red\n"+
			"the build:\n%s", got.code, got.out)
	}
	if !strings.Contains(got.out, "COMMITTED baseline") {
		t.Errorf("the run does not say it fell back to the committed baseline, so nothing distinguishes it\n"+
			"from a real base-commit comparison:\n%s", got.out)
	}
	if strings.Contains(got.out, "as it stood there") {
		t.Errorf("the report claims the base commit's copy of the baseline, which this run never read:\n%s", got.out)
	}
}

// --- helpers ---------------------------------------------------------------

// workflowStep is one step of the go-checks job, as far as these tests read it.
type workflowStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
}

// memqlbreakingStep finds the one go-checks step whose name says memqlbreaking.
func memqlbreakingStep(t *testing.T, root string) workflowStep {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatalf("reading ci.yml: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parsing ci.yml: %v", err)
	}
	job, ok := wf.Jobs["go-checks"]
	if !ok {
		t.Fatal("ci.yml has no go-checks job; the memqlbreaking step lives there")
	}
	var found []workflowStep
	for _, step := range job.Steps {
		if strings.Contains(strings.ToLower(step.Name), "memqlbreaking") {
			found = append(found, step)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatal("go-checks has no memqlbreaking step. The authoring-surface gate is not running at all, which\n" +
			"is a larger hole than the one this file exists to close.")
	}
	t.Fatalf("go-checks has %d memqlbreaking steps; this test cannot tell which one is the gate", len(found))
	return workflowStep{}
}

// fabricateBaseCommit writes files as one commit in a throwaway repository and
// returns its sha plus that repository's object directory.
//
// The objects stay in the temporary repository and are reached through
// GIT_ALTERNATE_OBJECT_DIRECTORIES. Committing into the repository under test
// would write to an object store shared with every other worktree of it, which
// a test has no business doing.
func fabricateBaseCommit(t *testing.T, root string, files map[string]string) (sha, objects string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=memqlbreaking-base-test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=memqlbreaking-base-test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", ".")
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "--", rel)
	}
	git("commit", "-q", "-m", "fabricated base commit")
	sha = git("rev-parse", "HEAD")
	objects = filepath.Join(dir, ".git", "objects")

	// Prove the alternate is readable from the repository under test before
	// handing it to the script: a silently unreadable base commit would send
	// the script down its fetch path and out to the network.
	probe := exec.Command("git", "-C", root, "cat-file", "-e", sha+"^{commit}")
	probe.Env = append(os.Environ(), "GIT_ALTERNATE_OBJECT_DIRECTORIES="+objects)
	if err := probe.Run(); err != nil {
		t.Fatalf("the fabricated base commit %s is not readable from %s through its alternate object "+
			"directory: %v", sha, root, err)
	}
	return sha, objects
}

// commandResult is one command's combined output and exit code.
type commandResult struct {
	out  string
	code int
}

// runInRepo runs a command at the repository root with extra environment.
func runInRepo(t *testing.T, root string, env []string, name string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running %s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		code = exitErr.ExitCode()
	}
	return commandResult{out: string(out), code: code}
}

var (
	goConstPattern    = `(?m)^const\s+%s\s*=\s*"([^"]*)"`
	shellConstPattern = `(?m)^readonly\s+%s="([^"]*)"`
)

func goStringConst(t *testing.T, source, name string) string {
	t.Helper()
	return oneCapture(t, source, strings.Replace(goConstPattern, "%s", name, 1), name)
}

func shellStringConst(t *testing.T, source, name string) string {
	t.Helper()
	return oneCapture(t, source, strings.Replace(shellConstPattern, "%s", name, 1), name)
}

func oneCapture(t *testing.T, source, pattern, name string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(source)
	if m == nil {
		t.Fatalf("no declaration of %s found; this pin reads it by name and a rename makes it read nothing "+
			"at all, which would pass", name)
	}
	return m[1]
}

func readFileForBreakingBase(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// repoRootForBreakingBase walks up from the package directory to the root,
// found by go.work -- the one file that exists exactly once, at the top.
func repoRootForBreakingBase(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.work (the repo root) from the test working directory")
		}
		dir = parent
	}
}
