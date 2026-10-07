package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
)

// pipeline_step_test.go -- workerHost.pipeline_step (memql#5494): one CI step
// on this machine, with the clone-and-command contract the cluster's Job has.
//
// Every clone here comes from a bare repository on disk, so nothing reaches a
// network: production refuses anything but an https clone URL, and these tests
// open the file:// door through allowLocalClones.

// pipelineFixture is a bare repository with three commits, at a path that
// ends in o/r.git -- a local clone URL names its repository by ending in it,
// and stepArgs names o/r.
type pipelineFixture struct {
	url  string   // file:// URL of the bare repository
	shas []string // oldest first
}

func newPipelineFixture(t *testing.T) pipelineFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed; pipeline_step fetches with it")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "o", "r.git")
	src := filepath.Join(dir, "src")
	run := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "-q", "--bare", bare)
	run(dir, "init", "-q", src)
	var shas []string
	for _, content := range []string{"first", "second", "third"} {
		if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(src, "add", "file.txt")
		run(src, "commit", "-q", "-m", content)
		shas = append(shas, run(src, "rev-parse", "HEAD"))
	}
	run(src, "push", "-q", bare, "HEAD:refs/heads/main")
	return pipelineFixture{url: "file://" + bare, shas: shas}
}

func pipelineTestWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "step-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// allowLocalClones opens the file:// door for one test.
func allowLocalClones(t *testing.T) {
	t.Helper()
	prev := localCloneSources
	localCloneSources = true
	t.Cleanup(func() { localCloneSources = prev })
}

// pipelineTestPolicy allows pipelines with a workspace root of the test's own.
// extra is more of the pipelines block, indented two spaces.
func pipelineTestPolicy(t *testing.T, extra string) (*Policy, string) {
	t.Helper()
	isolatePipelineCapacity(t)
	root := filepath.Join(t.TempDir(), "pipelines")
	return policyWith(t, "pipelines:\n  allow: true\n  workspace_root: "+root+"\n"+extra), root
}

// stepArgs is a pipeline_step request for the fixture's middle commit -- not
// the tip, so a checkout of the branch instead of the sha would show.
//
// The map is round-tripped through JSON so it holds exactly the types the
// dispatcher's decodeArgs hands over: float64 numbers, []any lists, nested
// map[string]any.
func stepArgs(t *testing.T, fx pipelineFixture, command string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{
		"cloneUrl":   fx.url,
		"sha":        fx.shas[1],
		"token":      "",
		"repository": "o/r",
		"command":    command,
		"execution":  "native",
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		"env":        map[string]any{"MEMQL_RUN_ID": "run-1"},
		"timeoutSec": 60,
	}
	for k, v := range extra {
		args[k] = v
	}
	return roundTrip(t, args)
}

func roundTrip(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// recordedOutput is the stream a step's output goes to.
type recordedOutput struct {
	mu      sync.Mutex
	chunks  []recordedChunk
	onChunk func(recordedChunk)
}

type recordedChunk struct {
	stderr bool
	data   string
}

func (r *recordedOutput) emit(stderr bool, data []byte) {
	c := recordedChunk{stderr: stderr, data: string(data)}
	r.mu.Lock()
	r.chunks = append(r.chunks, c)
	hook := r.onChunk
	r.mu.Unlock()
	if hook != nil {
		hook(c)
	}
}

func (r *recordedOutput) text(stderr bool) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, c := range r.chunks {
		if c.stderr == stderr {
			b.WriteString(c.data)
		}
	}
	return b.String()
}

func (r *recordedOutput) all() string { return r.text(false) + r.text(true) }

func (r *recordedOutput) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.chunks)
}

// stepResult is the result contract, field for field.
type stepResult struct {
	ExitCode           int      `json:"exitCode"`
	DurationMs         int64    `json:"durationMs"`
	ArtifactsTgzBase64 string   `json:"artifactsTgzBase64"`
	ArtifactsMissing   []string `json:"artifactsMissing"`
	ArtifactsTooLarge  bool     `json:"artifactsTooLarge"`
}

// decoded returns decodeStep bound to t, so a step's two results can be
// passed to it whole: decoded(t)(runPipelineStep(...)).
func decoded(t *testing.T) func(*memqlv1.Success, *memqlv1.Failure) stepResult {
	return func(s *memqlv1.Success, f *memqlv1.Failure) stepResult {
		t.Helper()
		return decodeStep(t, s, f)
	}
}

func decodeStep(t *testing.T, s *memqlv1.Success, f *memqlv1.Failure) stepResult {
	t.Helper()
	if f != nil {
		t.Fatalf("the step failed: %s: %s", f.GetErrorCode(), f.GetErrorMessage())
	}
	if s == nil {
		t.Fatal("the step returned neither a success nor a failure")
	}
	var res stepResult
	if err := json.Unmarshal(s.GetResultJson(), &res); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, s.GetResultJson())
	}
	if int(s.GetExitCode()) != res.ExitCode {
		t.Errorf("Success.exit_code = %d but the result says %d", s.GetExitCode(), res.ExitCode)
	}
	return res
}

func untarGz(t *testing.T, b64 string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if b64 == "" {
		return files // no archive: nothing was packed
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("artifactsTgzBase64 is not base64: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("artifacts are not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("artifacts are not a tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			t.Errorf("entry %q is not a regular file (type %c)", hdr.Name, hdr.Typeflag)
		}
		if strings.HasPrefix(hdr.Name, "/") || strings.Contains(hdr.Name, "..") {
			t.Errorf("entry %q leaves the step directory", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(data)
	}
	return files
}

func assertNoStepDirs(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("a step left %s behind in %s", e.Name(), root)
	}
}

// --- admission -----------------------------------------------------------

func TestPipelineStepRefusedUnlessThisMachineAllowsPipelines(t *testing.T) {
	allowLocalClones(t)
	t.Setenv("HOME", t.TempDir())
	fx := newPipelineFixture(t)
	root := filepath.Join(t.TempDir(), "pipelines")
	for name, body := range map[string]string{
		"no pipelines block": "shell:\n  allow: [ls]\n",
		"allow: false":       "pipelines:\n  allow: false\n  workspace_root: " + root + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			out := &recordedOutput{}
			success, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "echo ran", nil), policyWith(t, body), out.emit)
			if success != nil || fail == nil {
				t.Fatalf("a machine that allows no pipelines ran a step: %+v", success)
			}
			if fail.GetErrorCode() != "denied_by_policy" {
				t.Errorf("error code %q, want denied_by_policy", fail.GetErrorCode())
			}
			if !strings.Contains(fail.GetErrorMessage(), "pipelines.allow") {
				t.Errorf("the refusal does not name the setting: %s", fail.GetErrorMessage())
			}
			if out.count() != 0 {
				t.Errorf("a refused step produced output: %q", out.all())
			}
		})
	}
	assertNoStepDirs(t, root)

	// The reachable positive: the same request on a machine that says yes
	// runs, so the refusals above are about the policy.
	p, _ := pipelineTestPolicy(t, "")
	res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", nil), p))
	if res.ExitCode != 0 {
		t.Fatalf("an allowed step exited %d", res.ExitCode)
	}
}

func runPipelineStepQuietly(t *testing.T, args map[string]any, p *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	t.Helper()
	out := &recordedOutput{}
	return runPipelineStep(context.Background(), "", args, p, out.emit)
}

func TestPipelineStepRefusesARepositoryThePolicyDoesNotList(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "  repos: [o/other]\n")
	_, fail := runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", nil), p)
	if fail == nil || fail.GetErrorCode() != "denied_by_policy" {
		t.Fatalf("an unlisted repository was not refused by policy: %+v", fail)
	}
	if !strings.Contains(fail.GetErrorMessage(), "pipelines.repos") || !strings.Contains(fail.GetErrorMessage(), "o/r") {
		t.Errorf("the refusal must name the repository and the setting: %s", fail.GetErrorMessage())
	}
	assertNoStepDirs(t, root)

	listed, _ := pipelineTestPolicy(t, "  repos: [O/R]\n")
	if res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", nil), listed)); res.ExitCode != 0 {
		t.Fatalf("a listed repository's step exited %d", res.ExitCode)
	}
}

func TestPipelineStepRefusesMalformedRequests(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "")
	for name, extra := range map[string]map[string]any{
		"no clone URL":               {"cloneUrl": ""},
		"no sha":                     {"sha": ""},
		"a short sha":                {"sha": "abc123"},
		"a sha that is not hex":      {"sha": strings.Repeat("z", 40)},
		"no command":                 {"command": "  "},
		"an option for a clone URL":  {"cloneUrl": "--upload-pack=touch /tmp/pwned"},
		"an ssh clone URL":           {"cloneUrl": "ssh://git@github.com/o/r.git"},
		"an scp-like clone URL":      {"cloneUrl": "git@github.com:o/r.git"},
		"a plain-http clone URL":     {"cloneUrl": "http://github.com/o/r.git"},
		"credentials in the URL":     {"cloneUrl": "https://user:pass@github.com/o/r.git"},
		"an env name with a dash":    {"env": map[string]any{"BAD-NAME": "x"}},
		"an env name with =":         {"env": map[string]any{"A=B": "x"}},
		"an env value that is a map": {"env": map[string]any{"A": map[string]any{}}},
		"a secret name starting 1":   {"secrets": map[string]any{"1X": "value"}},
		"a secret shadowing env":     {"secrets": map[string]any{"MEMQL_RUN_ID": "value"}},
		"a NUL in a secret":          {"secrets": map[string]any{"S": "a\x00b"}},
		"an absolute artifact":       {"artifacts": []any{"/etc/passwd"}},
		"a parent artifact":          {"artifacts": []any{"../outside"}},
		"a parent inside a path":     {"artifacts": []any{"dist/../../outside"}},
		"an empty artifact":          {"artifacts": []any{""}},
		"a malformed artifact glob":  {"artifacts": []any{"dist/["}},
		"an artifact list of maps":   {"artifacts": []any{map[string]any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			out := &recordedOutput{}
			_, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "echo ran", extra), p, out.emit)
			if fail == nil || fail.GetErrorCode() != "bad_request" {
				t.Fatalf("want bad_request, got %+v", fail)
			}
			if out.count() != 0 {
				t.Errorf("a refused request produced output: %q", out.all())
			}
		})
	}
	assertNoStepDirs(t, root)
}

func TestPipelineStepClonesOnlyOverHTTPSInProduction(t *testing.T) {
	fx := newPipelineFixture(t) // localCloneSources left false
	p, root := pipelineTestPolicy(t, "")
	_, fail := runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", nil), p)
	if fail == nil || fail.GetErrorCode() != "bad_request" || !strings.Contains(fail.GetErrorMessage(), "https") {
		t.Fatalf("a file:// clone URL must be refused, naming https: %+v", fail)
	}
	assertNoStepDirs(t, root)
}

// A pipeline step comes from the cluster's pipeline runner, which dispatches
// with no agent. One an agent dispatched is refused whatever the policy says:
// pipelines.allow admits a command with no consent window, and an agent's
// tool loop -- which always names its agent -- must not reach that door.
func TestPipelineStepDispatchedByAnAgentIsRefused(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "")
	out := &recordedOutput{}
	_, fail := runPipelineStep(context.Background(), "v1:agents:agent:abc123", stepArgs(t, fx, "echo ran", nil), p, out.emit)
	if fail == nil || fail.GetErrorCode() != "denied_by_policy" {
		t.Fatalf("a step an agent dispatched must be refused by policy, got %+v", fail)
	}
	if !strings.Contains(fail.GetErrorMessage(), "pipeline runner") {
		t.Errorf("the refusal must say where pipeline steps come from: %s", fail.GetErrorMessage())
	}
	if out.count() != 0 {
		t.Errorf("a refused step produced output: %q", out.all())
	}
	assertNoStepDirs(t, root)

	// The reachable positive: the same request from the runner runs.
	if res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, "echo ran", nil), p, out.emit)); res.ExitCode != 0 {
		t.Fatalf("the runner's step exited %d", res.ExitCode)
	}
}

// pipelines.repos filters what is CLONED, not a name the request states: the
// clone URL must name the step's repository.
func TestPipelineStepClonesOnlyTheRepositoryItNames(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t) // its URL names o/r
	listed, root := pipelineTestPolicy(t, "  repos: [o/other]\n")
	_, fail := runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", map[string]any{"repository": "o/other"}), listed)
	if fail == nil || fail.GetErrorCode() != "bad_request" {
		t.Fatalf("a listed repository over another repository's clone URL must be refused, got %+v", fail)
	}
	if msg := fail.GetErrorMessage(); !strings.Contains(msg, "o/other") || !strings.Contains(msg, "o/r") {
		t.Errorf("the refusal must name both repositories: %s", msg)
	}
	assertNoStepDirs(t, root)

	open, _ := pipelineTestPolicy(t, "")
	if _, fail := runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", map[string]any{"repository": ""}), open); fail == nil || fail.GetErrorCode() != "bad_request" {
		t.Errorf("a step naming no repository must be refused, got %+v", fail)
	}
	if res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "echo ran", map[string]any{"repository": "O/R"}), open)); res.ExitCode != 0 {
		t.Fatalf("the repository its URL names, in another case, exited %d", res.ExitCode)
	}
}

func TestCloneURLNamesTheRepository(t *testing.T) {
	for _, tc := range []struct {
		url, repository string
		want            bool
	}{
		{"https://github.com/o/r.git", "o/r", true},
		{"https://github.com/o/r", "o/r", true},
		{"https://github.com/O/R.git/", "o/r", true},
		{"https://ghe.example.com/acme/widgets.git", "Acme/Widgets.git", true},
		{"https://github.com/o/r.git", "o/other", false},
		{"https://github.com/o/r.git", "o", false},
		{"https://github.com/scm/o/r.git", "o/r", false}, // a longer path is another repository
		{"https://github.com/o/r.git", "", false},
		{"https://github.com/", "o/r", false},
		// A local path -- the tests' door -- names a repository by ending in it.
		{"file:///tmp/fixture/o/r.git", "o/r", true},
		{"file:///tmp/fixture/o/r.git", "fixture/o", false},
	} {
		if got := cloneNamesRepository(tc.url, tc.repository); got != tc.want {
			t.Errorf("cloneNamesRepository(%q, %q) = %v, want %v", tc.url, tc.repository, got, tc.want)
		}
	}
}

// --- the step ------------------------------------------------------------

func TestPipelineStepFetchesTheShaAndRunsTheCommandThere(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "")
	out := &recordedOutput{}
	success, fail := runPipelineStep(context.Background(), "",
		stepArgs(t, fx, `cat file.txt; git rev-parse HEAD; git log --oneline | wc -l | tr -d ' '`, nil), p, out.emit)
	res := decodeStep(t, success, fail)
	if res.ExitCode != 0 {
		t.Fatalf("exit %d; output:\n%s", res.ExitCode, out.all())
	}
	stdout := out.text(false)
	// The middle commit, not the branch tip: fetched by sha.
	if !strings.Contains(stdout, "second\n") || strings.Contains(stdout, "third") {
		t.Errorf("the checkout is not the requested sha:\n%s", stdout)
	}
	if !strings.Contains(stdout, fx.shas[1]+"\n") {
		t.Errorf("HEAD is not %s:\n%s", fx.shas[1], stdout)
	}
	// Shallow, as the Job's clone is: one commit of history.
	if !strings.Contains(stdout, "\n1\n") {
		t.Errorf("the fetch was not --depth=1:\n%s", stdout)
	}
	// The Job's clone script says what it checked out; so does this.
	if !strings.Contains(stdout, "memql: checked out "+fx.shas[1]) {
		t.Errorf("no checkout line:\n%s", stdout)
	}
	if res.DurationMs < 0 {
		t.Errorf("durationMs = %d", res.DurationMs)
	}
	assertNoStepDirs(t, root)
}

func TestPipelineStepResultIsTheContractShape(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	success, fail := runPipelineStepQuietly(t, stepArgs(t, fx, "true", nil), p)
	if fail != nil {
		t.Fatalf("%s: %s", fail.GetErrorCode(), fail.GetErrorMessage())
	}
	var raw map[string]any
	if err := json.Unmarshal(success.GetResultJson(), &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"artifactsMissing", "artifactsTgzBase64", "durationMs", "exitCode"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("result keys %v, want exactly %v", keys, want)
	}
	if _, isList := raw["artifactsMissing"].([]any); !isList {
		t.Errorf("artifactsMissing is %T, want a list (never null)", raw["artifactsMissing"])
	}
}

func TestPipelineStepReportsTheCommandsOwnExitStatus(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")

	out := &recordedOutput{}
	res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, "echo out; echo err >&2; exit 7", nil), p, out.emit))
	// A failing command is the step's own answer, not a failed call.
	if res.ExitCode != 7 {
		t.Errorf("exitCode %d, want 7", res.ExitCode)
	}
	if !strings.Contains(out.text(false), "out\n") || !strings.Contains(out.text(true), "err\n") {
		t.Errorf("stdout and stderr were not kept apart: stdout=%q stderr=%q", out.text(false), out.text(true))
	}

	// Killed by a signal: what a shell -- and the cluster's Job -- reports.
	res = decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "kill -9 $$", nil), p))
	if res.ExitCode != 128+9 {
		t.Errorf("a step killed by SIGKILL reported %d, want 137", res.ExitCode)
	}
}

func TestPipelineStepInheritsTheMachineEnvironment(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	t.Setenv("MEMQL_PIPELINE_TEST_MACHINE", "from-the-machine")
	t.Setenv("MEMQL_WORKER_TOKEN", "mql_wkr_must_not_reach_a_step")
	out := &recordedOutput{}
	res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx,
		`echo "machine=$MEMQL_PIPELINE_TEST_MACHINE"; echo "run=$MEMQL_RUN_ID"; `+
			`[ -n "$DEPLOY_KEY" ] && echo "secret=present"; echo "worker=${MEMQL_WORKER_TOKEN:-unset}"; `+
			`echo "path=${PATH:+set}"`,
		map[string]any{
			"env":     map[string]any{"MEMQL_RUN_ID": "run-7"},
			"secrets": map[string]any{"DEPLOY_KEY": "hunter2-long-value"},
		}), p, out.emit))
	if res.ExitCode != 0 {
		t.Fatalf("exit %d:\n%s", res.ExitCode, out.all())
	}
	stdout := out.text(false)
	for _, want := range []string{
		"machine=from-the-machine\n", // INHERITED, unlike exec: a CI step needs the machine's toolchains
		"run=run-7\n",                // the request's env
		"secret=present\n",           // and its secrets
		"path=set\n",
		// The worker's own credential is the one thing held back: a test
		// that prints its environment must not print it into a log.
		"worker=unset\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestPipelineStepTimeoutIsClampedToThePolicyMaximum(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "  max_timeout_sec: 1\n")
	out := &recordedOutput{}
	started := time.Now()
	success, fail := runPipelineStep(context.Background(), "",
		stepArgs(t, fx, "echo started; sleep 30; echo never", map[string]any{"timeoutSec": 600}), p, out.emit)
	elapsed := time.Since(started)
	if success != nil || fail == nil || fail.GetErrorCode() != "timeout" {
		t.Fatalf("a step past its clamped timeout must fail with timeout; got success=%v failure=%+v", success, fail)
	}
	if elapsed > 15*time.Second {
		t.Errorf("the step was stopped after %s: the request's 600s was not clamped to max_timeout_sec 1", elapsed)
	}
	if !strings.Contains(out.text(false), "started") || strings.Contains(out.text(false), "never") {
		t.Errorf("output does not show a step stopped mid-command:\n%s", out.all())
	}
	if !strings.Contains(out.text(true), "timeout") {
		t.Errorf("the log does not say the step was stopped for its timeout:\n%s", out.text(true))
	}
	assertNoStepDirs(t, root)
}

func TestPipelineStepTimeoutDefaultsAndClamps(t *testing.T) {
	for _, tc := range []struct {
		requested, max int
		want           time.Duration
	}{
		{0, 3600, 1200 * time.Second},  // absent: the engine's own per-step default
		{-5, 3600, 1200 * time.Second}, // nonsense: the same
		{600, 3600, 600 * time.Second},
		{7200, 3600, 3600 * time.Second}, // clamped to the policy
		{1200, 60, 60 * time.Second},
		{0, 60, 60 * time.Second}, // the default is clamped too
		{30, 0, 30 * time.Second}, // no policy maximum: the default maximum
		{9000, 0, 3600 * time.Second},
	} {
		if got := pipelineStepTimeout(tc.requested, tc.max); got != tc.want {
			t.Errorf("pipelineStepTimeout(%d, %d) = %s, want %s", tc.requested, tc.max, got, tc.want)
		}
	}
}

// A step stopped for its timeout is asked first -- SIGTERM, the way a pod is
// -- so a test runner can say what it was doing and clean up after itself.
func TestPipelineStepAsksAStepToStopBeforeKillingIt(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "  max_timeout_sec: 1\n")
	prev := pipelineStopGrace
	pipelineStopGrace = 5 * time.Second
	t.Cleanup(func() { pipelineStopGrace = prev })

	out := &recordedOutput{}
	_, fail := runPipelineStep(context.Background(), "",
		stepArgs(t, fx, "trap 'echo got-term; exit 0' TERM; echo started; sleep 30 & wait", nil), p, out.emit)
	if fail == nil || fail.GetErrorCode() != "timeout" {
		t.Fatalf("want timeout, got %+v", fail)
	}
	if !strings.Contains(out.text(false), "got-term") {
		t.Errorf("the step was never sent SIGTERM before it was killed:\n%s", out.all())
	}
}

func TestPipelineStepEscalatesToKillWhenTheStepIgnoresTERM(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "  max_timeout_sec: 1\n")
	prev := pipelineStopGrace
	pipelineStopGrace = 300 * time.Millisecond
	t.Cleanup(func() { pipelineStopGrace = prev })

	started := time.Now()
	_, fail := runPipelineStepQuietly(t, stepArgs(t, fx, "trap '' TERM; echo started; sleep 30", nil), p)
	if fail == nil || fail.GetErrorCode() != "timeout" {
		t.Fatalf("want timeout, got %+v", fail)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("a step ignoring SIGTERM ran %s: it was never killed", elapsed)
	}
	assertNoStepDirs(t, root)
}

func TestPipelineStepStopsWhatTheCommandLeftRunning(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	beat := filepath.Join(t.TempDir(), "beat")
	started := time.Now()
	// The command returns only once the loop it left behind has beaten, so
	// the check below is about a process that was certainly running.
	res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx,
		`(i=0; while :; do i=$((i+1)); echo $i > "$BEAT"; sleep 0.1; done) & while [ ! -s "$BEAT" ]; do sleep 0.05; done; echo started`,
		map[string]any{"env": map[string]any{"BEAT": beat}}), p))
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	// The background loop holds the step's stdout open; the step still ends
	// promptly, because what it left running is stopped -- the way a pod's
	// teardown stops it.
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("the step took %s: a background process kept it open", elapsed)
	}
	before, _ := os.ReadFile(beat)
	time.Sleep(600 * time.Millisecond)
	after, _ := os.ReadFile(beat)
	if string(before) != string(after) {
		t.Errorf("a process the step started is still running after it ended (beat %q -> %q)", before, after)
	}
}

func TestPipelineStepStreamsOutputWhileTheCommandRuns(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	gate := filepath.Join(t.TempDir(), "go")
	var once sync.Once
	out := &recordedOutput{onChunk: func(c recordedChunk) {
		if strings.Contains(c.data, "first-line") {
			once.Do(func() { _ = os.WriteFile(gate, nil, 0o600) })
		}
	}}
	// The command waits for a file the TEST writes only once it has seen the
	// first line -- so a step that held its output until exit would never
	// see the file, and would say so.
	res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx,
		`echo first-line; i=0; while [ ! -f "$GATE" ]; do i=$((i+1)); [ $i -gt 200 ] && { echo held-back; exit 9; }; sleep 0.05; done; echo second-line`,
		map[string]any{"env": map[string]any{"GATE": gate}}), p, out.emit))
	if res.ExitCode != 0 || !strings.Contains(out.text(false), "second-line") {
		t.Fatalf("output was not streamed while the command ran (exit %d):\n%s", res.ExitCode, out.all())
	}
}

func TestPipelineStepCloneFailureIsTyped(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "")
	out := &recordedOutput{}
	_, fail := runPipelineStep(context.Background(), "",
		stepArgs(t, fx, "echo ran", map[string]any{"sha": strings.Repeat("0", 40)}), p, out.emit)
	if fail == nil || fail.GetErrorCode() != "pipeline_clone_failed" {
		t.Fatalf("a sha the repository does not have must fail as pipeline_clone_failed, got %+v", fail)
	}
	if strings.Contains(out.text(false), "ran") {
		t.Error("the command ran after the clone failed")
	}
	// git's own reason travels: in the failure, and in the streamed log.
	if !strings.Contains(fail.GetErrorMessage(), "not our ref") || !strings.Contains(out.text(true), "not our ref") {
		t.Errorf("git's reason was lost: message=%q stderr=%q", fail.GetErrorMessage(), out.text(true))
	}
	assertNoStepDirs(t, root)
}

func TestPipelineStepRemovesItsWorkspaceWhateverTheOutcome(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	for name, tc := range map[string]struct {
		command string
		extra   map[string]any
		policy  string
	}{
		"success":                 {command: "echo ok > made.txt"},
		"a failing command":       {command: "echo made > made.txt; exit 3"},
		"a clone failure":         {command: "true", extra: map[string]any{"sha": strings.Repeat("0", 40)}},
		"a timeout":               {command: "sleep 30", policy: "  max_timeout_sec: 1\n"},
		"a read-only tree inside": {command: "mkdir -p ro/inner && touch ro/inner/f && chmod a-w ro/inner ro"},
	} {
		t.Run(name, func(t *testing.T) {
			p, root := pipelineTestPolicy(t, tc.policy)
			runPipelineStepQuietly(t, stepArgs(t, fx, tc.command, tc.extra), p)
			assertNoStepDirs(t, root)
		})
	}
}

// --- the token -----------------------------------------------------------

func TestPipelineStepNeverPutsTheTokenOnDiskOrInArgv(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	const token = "ghs_pipelineTestToken0123456789abcdef"
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))

	for _, tc := range []struct {
		name, token string
	}{{"with a token", token}, {"anonymous", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			// A git that writes down how it was called, then is the real one.
			dir := t.TempDir()
			logPath := filepath.Join(dir, "git.log")
			wrapper := filepath.Join(dir, "git")
			script := "#!/bin/sh\n{\n  printf 'argv:'\n  for a in \"$@\"; do printf ' [%s]' \"$a\"; done\n  printf '\\n'\n  env | grep '^GIT_' | sort\n} >> '" + logPath + "'\nexec '" + realGit + "' \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			prev := gitBinary
			gitBinary = wrapper
			t.Cleanup(func() { gitBinary = prev })
			// A GIT_ variable on the machine must not steer the fetch.
			t.Setenv("GIT_DIR", filepath.Join(dir, "not-the-step"))

			p, _ := pipelineTestPolicy(t, "")
			out := &recordedOutput{}
			res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx,
				`if grep -rqF -e "$NEEDLE_TOKEN" -e "$NEEDLE_HEADER" .git; then echo on-disk=yes; else echo on-disk=no; fi; `+
					`echo "step-git-config=$(env | grep -c '^GIT_CONFIG_' || true)"`,
				map[string]any{
					"token": tc.token,
					"env":   map[string]any{"NEEDLE_TOKEN": token, "NEEDLE_HEADER": header},
				}), p, out.emit))
			if res.ExitCode != 0 {
				t.Fatalf("exit %d:\n%s", res.ExitCode, out.all())
			}
			stdout := out.text(false)
			if !strings.Contains(stdout, "on-disk=no") {
				t.Errorf("the token or its header is on disk in the checkout:\n%s", stdout)
			}
			// The step itself never sees the fetch's credential.
			if !strings.Contains(stdout, "step-git-config=0") {
				t.Errorf("the step's environment carries the fetch's git configuration:\n%s", stdout)
			}

			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("the wrapper never ran: %v", err)
			}
			log := string(raw)
			sawFetch := false
			for _, line := range strings.Split(log, "\n") {
				if !strings.HasPrefix(line, "argv:") {
					continue
				}
				if strings.Contains(line, token) || strings.Contains(line, header) {
					t.Errorf("the token is on git's argv: %s", line)
				}
				if strings.Contains(line, "[fetch]") && strings.Contains(line, fx.shas[1]) {
					sawFetch = true
				}
			}
			if !sawFetch {
				t.Errorf("no fetch of the sha in the git calls:\n%s", log)
			}
			for _, want := range []string{
				"GIT_CONFIG_GLOBAL=" + os.DevNull, // not the owner's gitconfig: no insteadOf to their SSH key
				"GIT_CONFIG_NOSYSTEM=1",
				"GIT_TERMINAL_PROMPT=0",
				"credential.helper", // reset, so no credential helper of the owner's answers
			} {
				if !strings.Contains(log, want) {
					t.Errorf("git ran without %q:\n%s", want, log)
				}
			}
			if strings.Contains(log, "GIT_DIR=") {
				t.Errorf("the machine's GIT_DIR reached git:\n%s", log)
			}
			if tc.token != "" {
				if !strings.Contains(log, "GIT_CONFIG_KEY_0=http.extraheader") ||
					!strings.Contains(log, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+header) {
					t.Errorf("the header did not travel in git's environment:\n%s", log)
				}
			} else if strings.Contains(log, "http.extraheader") {
				t.Errorf("an anonymous fetch carried an auth header:\n%s", log)
			}
		})
	}
}

// --- artifacts -----------------------------------------------------------

func TestPipelineStepPacksTheDeclaredArtifacts(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx,
		`mkdir -p dist/sub && echo report > dist/report.xml && echo a > dist/sub/a.txt && echo b > dist/sub/b.log && echo top > top.txt`,
		map[string]any{"artifacts": []any{"dist/report.xml", "dist/sub", "*.txt", "missing/*.xml", "nope.txt"}}), p))
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	files := untarGz(t, res.ArtifactsTgzBase64)
	want := map[string]string{
		"dist/report.xml": "report\n",
		"dist/sub/a.txt":  "a\n",
		"dist/sub/b.log":  "b\n",
		"top.txt":         "top\n",
		"file.txt":        "second\n", // from the checkout: a glob sees the whole step directory
	}
	for name, content := range want {
		if files[name] != content {
			t.Errorf("artifact %q = %q, want %q", name, files[name], content)
		}
	}
	if len(files) != len(want) {
		t.Errorf("archive holds %d files, want %d: %v", len(files), len(want), files)
	}
	if got := strings.Join(res.ArtifactsMissing, ","); got != "missing/*.xml,nope.txt" {
		t.Errorf("artifactsMissing = %v, want [missing/*.xml nope.txt] in the declared order", res.ArtifactsMissing)
	}
	if res.ArtifactsTooLarge {
		t.Error("a small archive was reported too large")
	}
}

// The declared paths are globs over the CHECKOUT, never over the path the
// checkout happens to live at: a workspace root an owner named "ci [main]"
// must not turn into a character class. And a directory declared with a
// trailing slash is the directory.
func TestPipelineStepArtifactGlobsAreRelativeToTheCheckoutOnly(t *testing.T) {
	isolatePipelineCapacity(t)
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	root := filepath.Join(t.TempDir(), "ci [main] *")
	p := policyWith(t, "pipelines:\n  allow: true\n  workspace_root: \""+root+"\"\n")
	res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx,
		`mkdir -p dist/sub && echo report > dist/report.xml && echo a > dist/sub/a.txt`,
		map[string]any{"artifacts": []any{"dist/*.xml", "dist/sub/", "./dist/sub/a.txt"}}), p))
	files := untarGz(t, res.ArtifactsTgzBase64)
	if files["dist/report.xml"] != "report\n" || files["dist/sub/a.txt"] != "a\n" || len(files) != 2 {
		t.Errorf("artifacts under a workspace path with glob characters: %v (missing %v)", files, res.ArtifactsMissing)
	}
	if len(res.ArtifactsMissing) != 0 {
		t.Errorf("artifactsMissing = %v, want none", res.ArtifactsMissing)
	}
	assertNoStepDirs(t, root)
}

func TestPipelineStepArtifactsNeverFollowALinkOutOfTheStep(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside the step\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx,
		`ln -s "$OUTSIDE" out && ln -s "$OUTSIDE/secret.txt" link.txt && mkdir d && ln -s "$OUTSIDE/secret.txt" d/inner.txt && echo ok > d/ok.txt`,
		map[string]any{
			"env":       map[string]any{"OUTSIDE": outside},
			"artifacts": []any{"out/secret.txt", "link.txt", "d"},
		}), p))
	files := untarGz(t, res.ArtifactsTgzBase64)
	if len(files) != 1 || files["d/ok.txt"] != "ok\n" {
		t.Errorf("the archive followed a link out of the step: %v", files)
	}
	for _, content := range files {
		if strings.Contains(content, "outside the step") {
			t.Error("a file outside the step directory was archived")
		}
	}
	if got := strings.Join(res.ArtifactsMissing, ","); got != "out/secret.txt,link.txt" {
		t.Errorf("artifactsMissing = %v, want the two links", res.ArtifactsMissing)
	}
}

func TestPipelineStepArtifactsOverALimitAreReportedNotFailed(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	big := make([]byte, 8<<10)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		limit *int64
	}{
		{"the 64 MiB archive limit", &pipelineArtifactMaxBytes},
		{"what one stream message can carry", &pipelineArtifactWireBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := *tc.limit
			*tc.limit = 1024
			t.Cleanup(func() { *tc.limit = prev })
			src := filepath.Join(t.TempDir(), "big.bin")
			if err := os.WriteFile(src, big, 0o600); err != nil {
				t.Fatal(err)
			}
			p, _ := pipelineTestPolicy(t, "")
			out := &recordedOutput{}
			res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx,
				`cp "$SRC" big.bin; exit 4`,
				map[string]any{
					"env":       map[string]any{"SRC": src},
					"artifacts": []any{"big.bin"},
				}), p, out.emit))
			// The step keeps its own exit status: an artifact problem is a
			// note on the step, never the step failing.
			if res.ExitCode != 4 {
				t.Errorf("exitCode %d, want the command's own 4", res.ExitCode)
			}
			if !res.ArtifactsTooLarge || res.ArtifactsTgzBase64 != "" {
				t.Errorf("an over-limit archive was returned (tooLarge=%v, %d base64 bytes)", res.ArtifactsTooLarge, len(res.ArtifactsTgzBase64))
			}
			if len(res.ArtifactsMissing) != 0 {
				t.Errorf("a file that exists is not missing: %v", res.ArtifactsMissing)
			}
			if !strings.Contains(out.text(true), "artifacts") {
				t.Errorf("the log does not say why no artifacts came back:\n%s", out.text(true))
			}
		})
	}
}

// --- secrets -------------------------------------------------------------

func TestPipelineStepMasksSecretsInEveryChunkAndTheResult(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	const key = "s3cr3t-value-123"
	pem := "-----BEGIN KEY-----\nline-two-of-the-key\n-----END KEY-----"
	out := &recordedOutput{}
	res := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx,
		// The key once on stdout, once on stderr, and once split across two
		// writes a fifth of a second apart -- two reads, one value.
		`echo "key=$API_KEY"; echo "$API_KEY" >&2; printf '%s' "${API_KEY%????????}"; sleep 0.2; printf '%s\n' "${API_KEY#????????}"; `+
			`echo "$PEM"; echo "short=$SHORT"`,
		map[string]any{
			"secrets":   map[string]any{"API_KEY": key, "PEM": pem, "SHORT": "abc"},
			"artifacts": []any{key},
		}), p, out.emit))
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	all := out.all()
	for _, leak := range []string{key, key[:8], "line-two-of-the-key", "BEGIN KEY", "END KEY"} {
		if strings.Contains(all, leak) {
			t.Errorf("%q reached the stream:\n%s", leak, all)
		}
	}
	if strings.Count(all, "***") < 4 {
		t.Errorf("expected the mask in place of each value:\n%s", all)
	}
	// Values shorter than four characters are not masked: masking "abc" would
	// shred ordinary output, which is why the cluster draws the same line.
	if !strings.Contains(all, "short=abc") {
		t.Errorf("a three-character value was masked:\n%s", all)
	}
	// The result is masked too: a declared artifact named like a secret.
	for _, m := range res.ArtifactsMissing {
		if strings.Contains(m, key) {
			t.Errorf("the result carries a secret value: %v", res.ArtifactsMissing)
		}
	}
}

func TestSecretMaskerMasksLongestFirstAndEveryLineOfAMultiLineValue(t *testing.T) {
	m := newSecretMasker([]string{"abcd", "abcdefgh", "two\nlines-here", "", "xyz"})
	if got := m.mask("[abcdefgh] [abcd] [xyz]"); got != "[***] [***] [xyz]" {
		t.Errorf("mask = %q", got)
	}
	// Each line of a multi-line value is a value of its own, so a log cut at
	// line ends still masks every part.
	if got := m.mask("lines-here\n"); got != "***\n" {
		t.Errorf("the second line of a multi-line value was not masked: %q", got)
	}
	if got := newSecretMasker(nil).mask("nothing to hide"); got != "nothing to hide" {
		t.Errorf("an empty masker changed text: %q", got)
	}
}

func TestOutputChunkerHoldsAPartialLineUntilItEnds(t *testing.T) {
	var chunks []string
	c := newLineChunker(newSecretMasker([]string{"0123456789"}), func(b []byte) { chunks = append(chunks, string(b)) })
	c.write([]byte("token=01234"))
	if len(chunks) != 0 {
		t.Fatalf("a partial line was sent before it ended: %q", chunks)
	}
	c.write([]byte("56789 rest\nnext"))
	c.flush()
	if got := strings.Join(chunks, ""); got != "token=*** rest\nnext" {
		t.Errorf("chunks %q", chunks)
	}
}

// chunkLine writes line through a fresh chunker in reads of read bytes and
// returns everything it sent.
func chunkLine(m *secretMasker, line string, read int) string {
	var got strings.Builder
	c := newLineChunker(m, func(b []byte) { got.WriteString(string(b)) })
	for i := 0; i < len(line); i += read {
		end := i + read
		if end > len(line) {
			end = len(line)
		}
		c.write([]byte(line[i:end]))
	}
	c.flush()
	return got.String()
}

// distinctValue is n bytes no window of which repeats elsewhere in it, so a
// leak of any part is a leak that can be seen.
func distinctValue(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%08d", i)
	}
	return b.String()[:n]
}

func TestOutputChunkerNeverCutsALongLineInsideASecret(t *testing.T) {
	const secret = "0123456789-SECRET-VALUE"
	m := newSecretMasker([]string{secret})
	// A line far longer than the chunker will hold, with the secret placed so
	// it straddles the cut the chunker is forced to make, at every offset
	// around it, written in reads of several sizes.
	for offset := -2 * len(secret); offset <= len(secret); offset++ {
		for _, read := range []int{1, 7, 4096} {
			line := strings.Repeat("x", maxPendingOutput+offset) + secret + strings.Repeat("y", 300)
			got := chunkLine(m, line, read)
			if strings.Contains(got, secret[:10]) {
				t.Fatalf("offset %d, reads of %d: part of the secret was sent unmasked", offset, read)
			}
			if want := m.mask(line); got != want {
				t.Fatalf("offset %d, reads of %d: the chunks do not reassemble to the masked line", offset, read)
			}
		}
	}

	// A secret LONGER than the chunker holds -- a base64 keystore a `set -x`
	// prints whole -- has no cut point until all of it has arrived: a value
	// still arriving may begin anywhere in what is held, the first byte
	// included. The chunker holds on until it can see the whole value.
	for _, size := range []int{maxPendingOutput + 1, 2 * maxPendingOutput} {
		long := distinctValue(size)
		lm := newSecretMasker([]string{long})
		for _, prefix := range []int{0, 7, maxPendingOutput - 3} {
			for _, read := range []int{7, 4096, 32 << 10} {
				line := strings.Repeat("x", prefix) + long + strings.Repeat("y", 300)
				got := chunkLine(lm, line, read)
				for _, window := range []string{long[:32], long[size/2 : size/2+32], long[size-32:]} {
					if strings.Contains(got, window) {
						t.Fatalf("a %d-byte secret after %d bytes, reads of %d: part of it was sent unmasked", size, prefix, read)
					}
				}
				if want := lm.mask(line); got != want {
					t.Fatalf("a %d-byte secret after %d bytes, reads of %d: the chunks do not reassemble to the masked line", size, prefix, read)
				}
			}
		}
	}

	// Values that overlap one another, and a line that is one unbroken run of
	// them -- one span, however long: the parts still join into the line
	// masked whole.
	om := newSecretMasker([]string{"abababab", "SECRET-ONE-xyz", "xyz-SECRET-TWO"})
	for _, line := range []string{
		strings.Repeat("ab", maxPendingOutput) + "tail",
		strings.Repeat("x", maxPendingOutput-12) + "SECRET-ONE-xyz-SECRET-TWO" + strings.Repeat("y", 50),
	} {
		for _, read := range []int{1, 7, 4096} {
			if got, want := chunkLine(om, line, read), om.mask(line); got != want {
				t.Fatalf("overlapping values, reads of %d: the chunks do not reassemble to the masked line", read)
			}
		}
	}
}

// TestSecretMaskerMasksAsTheClusterDoes (memql#5478's final review, its three
// probes): a value is masked in every form the cluster masks it in, and values
// that overlap where printed are one span -- in the stream and in the result
// alike. What the cluster cannot find again -- a value printed trimmed, the
// remnant of two that overlap -- must not leave this machine, and an
// indent-only line of a key is no value at all.
func TestSecretMaskerMasksAsTheClusterDoes(t *testing.T) {
	for _, c := range []struct {
		name    string
		secrets []string
		output  string // what the step prints
		want    string
		leaks   []string
	}{
		{
			// `echo $S` prints a value stored with whitespace around it
			// without that whitespace.
			name:    "a value stored padded and printed trimmed",
			secrets: []string{"hunter2-token "},
			output:  "token=hunter2-token\nhunter2-token\n",
			want:    "token=***\n***\n",
			leaks:   []string{"hunter2-token"},
		},
		{
			name:    "a multi-line value with an indent-only line",
			secrets: []string{"-----BEGIN KEY-----\n    \nMIIBOgIBAAJBAKj34GkxFhD90vcN\n-----END KEY-----\n"},
			output:  "func main() {\n    return nil\n        MIIBOgIBAAJBAKj34GkxFhD90vcN\n",
			want:    "func main() {\n    return nil\n        ***\n",
			leaks:   []string{"MIIBOgIBAAJBAKj34GkxFhD90vcN"},
		},
		{
			// Masking them one after the other leaves "***ijkl".
			name:    "overlapping values",
			secrets: []string{"abcdefgh", "efghijkl"},
			output:  "abcdefghijkl\nx efghijklmnop abcdefgh y\n",
			want:    "***\nx ***mnop *** y\n",
			leaks:   []string{"ijkl", "efgh"},
		},
		{
			// A carriage return breaks a value into parts as a newline does.
			name:    "a value whose parts a bare carriage return separates, each printed alone",
			secrets: []string{"user-name-abcd\rpass-word-efgh"},
			output:  "pass-word-efgh\nuser-name-abcd\n",
			want:    "***\n***\n",
			leaks:   []string{"pass-word-efgh", "user-name-abcd"},
		},
		{
			name:    "a value stored with CRLF line ends, its lines printed alone",
			secrets: []string{"line-one-abcd\r\nline-two-efgh\r\n"},
			output:  "x line-two-efgh\r\nline-one-abcd y\r\n",
			want:    "x ***\r\n*** y\r\n",
			leaks:   []string{"line-one-abcd", "line-two-efgh"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newSecretMasker(c.secrets)
			for _, read := range []int{1, 7, 4096} {
				got := chunkLine(m, c.output, read)
				if got != c.want {
					t.Errorf("reads of %d: the stream is %q, want %q", read, got, c.want)
				}
				for _, leak := range c.leaks {
					if strings.Contains(got, leak) {
						t.Errorf("reads of %d: the stream holds %q", read, leak)
					}
				}
			}
			if got := m.mask(c.output); got != c.want {
				t.Errorf("mask = %q, want %q", got, c.want)
			}
		})
	}
}

// TestOutputChunkerMasksAPaddedValueAcrossItsCut: a value stored with
// whitespace around it, printed trimmed -- or printed whole, where its two
// forms are one span -- across the point where the chunker must cut a line
// longer than it holds, is masked whole: the parts are the line masked whole,
// and none of the value is in either.
func TestOutputChunkerMasksAPaddedValueAcrossItsCut(t *testing.T) {
	m := newSecretMasker([]string{"  hunter2-token-0123456789 \n", "pad-value-9876  "})
	for _, printed := range []string{"hunter2-token-0123456789", "pad-value-9876  ", "pad-value-9876"} {
		for offset := -len(printed) - 2; offset <= 2; offset++ {
			for _, read := range []int{1, 7, 4096} {
				line := strings.Repeat("x", maxPendingOutput+offset) + printed + strings.Repeat("y", 300)
				got := chunkLine(m, line, read)
				if want := strings.Repeat("x", maxPendingOutput+offset) + "***" + strings.Repeat("y", 300); got != want {
					head := strings.TrimLeft(got, "x")
					t.Fatalf("%q at offset %d, reads of %d: the stream ends %.60q..., want the value masked whole", printed, offset, read, head)
				}
			}
		}
	}
}

// TestOutputChunkerMasksRandomLongLinesAsTheClusterDoes: values drawn from a
// small alphabet -- so they overlap, nest and repeat -- stored padded, over
// lines with an indent-only one among them, or in parts a carriage return
// separates, bare or before a newline; lines longer than the chunker holds,
// their forms whole and cut short placed around the cut it must make, written
// in reads of every size. The stream is each line as the cluster masks it,
// and -- apart from that reference -- holds no part of a value between its
// line breaks, trimmed and four bytes or more.
func TestOutputChunkerMasksRandomLongLinesAsTheClusterDoes(t *testing.T) {
	rng := mrand.New(mrand.NewSource(20261004))
	alphabet := []string{"a", "b", "c", "a", "b", "é", " ", "\t"}
	word := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	for n := 0; n < 200; n++ {
		var values []string
		for k := 0; k < 1+rng.Intn(3); k++ {
			v := word(4 + rng.Intn(8))
			switch rng.Intn(6) {
			case 0: // stored padded
				v = strings.Repeat(" ", rng.Intn(3)) + v + strings.Repeat(" ", rng.Intn(3)) + []string{"", "\n"}[rng.Intn(2)]
			case 1: // over lines, an indent-only one among them
				v += "\n" + strings.Repeat(" ", 4+rng.Intn(3)) + "\n" + word(4+rng.Intn(6)) + "\n"
			case 2: // the end of another, carried on
				if len(values) > 0 {
					prev := strings.TrimSpace(values[rng.Intn(len(values))])
					v = prev[len(prev)/2:] + word(3)
				}
			case 3: // in parts a bare carriage return separates
				v += "\r" + word(4+rng.Intn(6))
			case 4: // over lines CRLF ends
				v += "\r\n" + word(4+rng.Intn(6)) + "\r\n"
			}
			values = append(values, v)
		}
		var parts, printable []string
		for _, v := range values {
			for _, part := range strings.FieldsFunc(v, isLineBreakForTest) {
				if part = strings.TrimSpace(part); len(part) >= minSecretLength {
					parts = append(parts, part)
				}
			}
			if whole := strings.TrimSpace(v); len(whole) >= minSecretLength && !strings.Contains(whole, "\n") {
				printable = append(printable, whole)
			}
		}
		printable = append(printable, parts...)
		var line strings.Builder
		line.WriteString(strings.Repeat("x", maxPendingOutput-rng.Intn(80)))
		for size := line.Len() + 40 + rng.Intn(200); line.Len() < size; {
			if len(printable) == 0 || rng.Intn(3) == 0 {
				line.WriteString(word(1 + rng.Intn(6)))
				continue
			}
			f := printable[rng.Intn(len(printable))]
			if rng.Intn(3) == 0 {
				f = f[:rng.Intn(len(f))]
			}
			line.WriteString(f)
		}
		read := []int{1, 3, 7, 64, 4096, 32 << 10}[rng.Intn(6)]
		m := newSecretMasker(values)
		got := chunkLine(m, line.String()+"\nnext\n", read)
		if want := clusterMask(line.String(), values) + "\nnext\n"; got != want {
			t.Fatalf("case %d, values %q, reads of %d: the stream ends\n  %q\nwant the line as the cluster masks it, ending\n  %q",
				n, values, read, got[max(0, len(got)-300):], want[max(0, len(want)-300):])
		}
		for _, part := range parts {
			if strings.Contains(got, part) {
				t.Fatalf("case %d, values %q, reads of %d: the stream holds %q, a part of a value", n, values, read, part)
			}
		}
	}
}

// isLineBreakForTest is what ends a line of a value: a newline or a carriage
// return. It is the test's own, apart from the masker's isLineBreak, so the
// reference and the independent check do not lean on the code they check.
func isLineBreakForTest(r rune) bool { return r == '\n' || r == '\r' }

// clusterMask is the cluster's masking (the engine's pipelines.MaskSecrets),
// written out the way the engine writes it -- every occurrence of every form
// found, spans that overlap or touch merged, each replaced once -- as what
// the chunker's masking in parts is held to.
func clusterMask(text string, values []string) string {
	var forms []string
	seen := map[string]bool{}
	add := func(f string) {
		if len(f) >= minSecretLength && !seen[f] {
			seen[f] = true
			forms = append(forms, f)
		}
	}
	for _, v := range values {
		add(v)
		add(strings.TrimSpace(v))
		if strings.ContainsAny(v, "\r\n") {
			for _, l := range strings.FieldsFunc(v, isLineBreakForTest) {
				add(strings.TrimSpace(l))
			}
		}
	}
	type span struct{ start, end int }
	var spans []span
	for _, f := range forms {
		for from := 0; from+len(f) <= len(text); {
			i := strings.Index(text[from:], f)
			if i < 0 {
				break
			}
			spans = append(spans, span{from + i, from + i + len(f)})
			from += i + 1
		}
	}
	if len(spans) == 0 {
		return text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	written, cur := 0, spans[0]
	flush := func() {
		b.WriteString(text[written:cur.start])
		b.WriteString("***")
		written = cur.end
	}
	for _, s := range spans[1:] {
		if s.start <= cur.end {
			cur.end = max(cur.end, s.end)
			continue
		}
		flush()
		cur = s
	}
	flush()
	return b.String() + text[written:]
}

// TestOutputChunkerMasksTheStreamLineByLine: the stream is masked a line at a
// time, as the cluster masks the lines it reassembles from it -- so how the
// output happened to be read changes nothing, and a value whose stored form
// ends in a newline never takes the line end with it. Masked as one text, a
// key printed whole is one span that swallows its line ends when its lines
// arrive in one read, and is masked line by line when they do not.
func TestOutputChunkerMasksTheStreamLineByLine(t *testing.T) {
	key := "-----BEGIN KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcN\n-----END KEY-----\n"
	m := newSecretMasker([]string{key, "tok3n-value\n"})
	output := "the key:\n" + key + "Authorization: tok3n-value\nnext line\n"
	want := "the key:\n***\n***\n***\nAuthorization: ***\nnext line\n"
	for _, read := range []int{1, 7, len(output)} {
		if got := chunkLine(m, output, read); got != want {
			t.Errorf("reads of %d: the stream is %q, want %q", read, got, want)
		}
	}
}

// --- through the dispatcher ----------------------------------------------

func pipelineDispatch(t *testing.T, callID string, args map[string]any) *memqlv1.ToolDispatch {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"action": "pipeline_step", "pipeline_step": args})
	if err != nil {
		t.Fatal(err)
	}
	return &memqlv1.ToolDispatch{CallId: callID, Tool: "workerHost", Action: "pipeline_step", ArgsJson: raw}
}

func TestDispatcher_PipelineStepIsAdmittedByPolicyNotByTheConsentWindow(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	// No consent window is open: the gate refuses everything it is asked.
	gate := &fakeGate{decision: consent.Decision{Allowed: false, Reason: "no consent window"}}

	d := NewDispatcher(quietLogger(), policyWith(t, "shell:\n  allow: [ls]\n"), gate)
	_, fail := d.Dispatch(context.Background(), pipelineDispatch(t, "call-1", stepArgs(t, fx, "echo ran", nil)))
	if fail == nil || fail.GetErrorCode() != "denied_by_policy" {
		t.Fatalf("with no pipelines policy the step must be refused BY POLICY, got %+v", fail)
	}
	if gate.calls != 0 {
		t.Errorf("the consent window was consulted for a pipeline step (%d calls)", gate.calls)
	}

	p, _ := pipelineTestPolicy(t, "")
	d = NewDispatcher(quietLogger(), p, gate)
	success, fail := d.Dispatch(context.Background(), pipelineDispatch(t, "call-2", stepArgs(t, fx, "echo ran", nil)))
	if res := decodeStep(t, success, fail); res.ExitCode != 0 {
		t.Fatalf("an allowed step exited %d", res.ExitCode)
	}
	if gate.calls != 0 {
		t.Errorf("the consent window was consulted for a pipeline step (%d calls)", gate.calls)
	}

	// Every other action still asks the window.
	_, fail = d.Dispatch(context.Background(), &memqlv1.ToolDispatch{
		Tool: "workerHost", Action: "exec", ArgsJson: []byte(`{"exec":{"cmd":"ls"}}`), CallId: "call-3",
	})
	if fail == nil || fail.GetErrorCode() != "consent_required" || gate.calls != 1 {
		t.Errorf("exec must still go through the consent window: %+v (gate calls %d)", fail, gate.calls)
	}
}

// The agent the refusal is about is the one on the dispatch ENVELOPE, which
// the engine stamps and the arguments cannot set.
func TestDispatcher_PipelineStepFromAnAgentIsRefused(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	d := NewDispatcher(quietLogger(), p, nil)

	dispatch := pipelineDispatch(t, "call-agent", stepArgs(t, fx, "echo ran", nil))
	dispatch.AgentId = "v1:agents:agent:abc123"
	if _, fail := d.Dispatch(context.Background(), dispatch); fail == nil || fail.GetErrorCode() != "denied_by_policy" {
		t.Fatalf("a pipeline step an agent dispatched must be refused, got %+v", fail)
	}

	dispatch = pipelineDispatch(t, "call-runner", stepArgs(t, fx, "echo ran", nil))
	if res := decoded(t)(d.Dispatch(context.Background(), dispatch)); res.ExitCode != 0 {
		t.Fatalf("the runner's step exited %d", res.ExitCode)
	}
}

func TestDispatcher_StreamsAPipelineStepAsToolStreamChunks(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	d := NewDispatcher(quietLogger(), p, nil)
	var mu sync.Mutex
	var chunks []*memqlv1.ToolStream
	send := func(c *memqlv1.ToolStream) error {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, c)
		return nil
	}
	success, fail := d.DispatchStream(context.Background(),
		pipelineDispatch(t, "call-9", stepArgs(t, fx, "echo to-stdout; echo to-stderr >&2", nil)), send)
	if res := decodeStep(t, success, fail); res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	mu.Lock()
	defer mu.Unlock()
	var stdout, stderr string
	for _, c := range chunks {
		if c.GetCallId() != "call-9" {
			t.Errorf("a chunk carries call id %q, want call-9", c.GetCallId())
		}
		stdout += string(c.GetStdoutChunk())
		stderr += string(c.GetStderrChunk())
	}
	if !strings.Contains(stdout, "to-stdout") || !strings.Contains(stderr, "to-stderr") {
		t.Errorf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestRedactedArgsPreviewHidesThePipelineTokenAndSecrets(t *testing.T) {
	raw := []byte(`{"action":"pipeline_step","pipeline_step":{"cloneUrl":"https://github.com/o/r.git",` +
		`"sha":"0123456789012345678901234567890123456789","token":"ghs_live_token_value","repository":"o/r",` +
		`"command":"go test ./...","env":{"MEMQL_RUN_ID":"r1"},"secrets":{"DEPLOY_KEY":"hunter2-value"},"timeoutSec":1200}}`)
	preview := redactedArgsPreview(raw)
	for _, leak := range []string{"ghs_live_token_value", "hunter2-value"} {
		if strings.Contains(preview, leak) {
			t.Errorf("the args preview logs %q: %s", leak, preview)
		}
	}
	if !strings.Contains(preview, "[REDACTED]") {
		t.Errorf("nothing was redacted: %s", preview)
	}
}

// The engine snapshots plain regular files, including Unicode and long names,
// before storing verified receipts. PAX metadata is intentionally not admitted.
func TestPipelineStepArtifactHeadersFitVerifiedSnapshotContract(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	name := "dist/" + strings.Repeat("long-", 35) + "résumé %2F #.txt"
	res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx,
		`mkdir -p dist && printf 'verified bytes\n' > "$ARTIFACT_PATH"`,
		map[string]any{"artifacts": []any{"dist/*"}, "env": map[string]any{"ARTIFACT_PATH": name}}), p))
	if res.ExitCode != 0 || len(res.ArtifactsMissing) != 0 || res.ArtifactsTooLarge {
		t.Fatalf("artifact producer failed: %+v", res)
	}
	data, err := base64.StdEncoding.DecodeString(res.ArtifactsTgzBase64)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != name || h.Typeflag != tar.TypeReg || len(h.PAXRecords) != 0 || len(h.Xattrs) != 0 {
		t.Fatalf("artifact header violates engine snapshot contract: %+v", h)
	}
	body, err := io.ReadAll(tr)
	if err != nil || string(body) != "verified bytes\n" {
		t.Fatalf("wrong content: %q %v", body, err)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("unexpected remaining entry: %v", err)
	}
}
