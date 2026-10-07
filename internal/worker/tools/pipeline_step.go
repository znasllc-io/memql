package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// pipeline_step.go -- workerHost.pipeline_step (memql#5494): one CI pipeline
// step, run on this machine with the clone-and-command contract the cluster's
// Kubernetes Job has, for a step that names a need the cluster cannot meet.
//
//	{"action":"pipeline_step","pipeline_step":{
//	  "cloneUrl":"https://github.com/o/r.git","sha":"<40 hex>","token":"<short-lived or empty>",
//	  "repository":"o/r","command":"go test ./...","env":{"MEMQL_RUN_ID":"..."},
//	  "execution":"native","platform":"darwin/arm64",
//	  "secrets":{"NAME":"value"},"artifacts":["dist/report.xml"],"timeoutSec":1200}}
//
// answered with
//
//	{"exitCode":N,"durationMs":...,"artifactsTgzBase64":"...","artifactsMissing":[...]}
//
// plus "artifactsTooLarge":true, only when the artifacts were left out for
// their size.
//
// THE ORDER IS THE CONTRACT. A step an agent dispatched is refused (only the
// cluster's pipeline runner dispatches one, and it names no agent); then
// admission by this machine's policy (pipelines.allow and pipelines.repos),
// and a clone URL that names the admitted repository; then a fresh directory
// under the pipelines workspace root; `git init`, a depth-1 fetch of the sha
// and a checkout of FETCH_HEAD; `/bin/sh -c command` there; the artifacts
// packed; the directory removed, whatever happened. Output streams while it
// runs.
//
// The command is whatever the cluster sends, run with no consent window: the
// policy is the owner's standing consent to the PIPELINE RUNNER, for the
// repositories it names, and these refusals are what hold a step to that.
//
// THE TOKEN NEVER TOUCHES DISK OR ARGV. It travels to git as an
// http.extraheader in GIT_CONFIG_COUNT/KEY/VALUE environment variables, which
// git reads as command-line configuration and writes nowhere; an empty token is
// an anonymous fetch of a public repository -- anonymous meaning this machine's
// own credentials are kept out of it too: no credential helper, no global or
// system gitconfig (no insteadOf onto the owner's SSH key), none of the
// machine's GIT_ variables, no prompt.
//
// A NATIVE COMMAND INHERITS THE MACHINE'S ENVIRONMENT, unlike exec, which replaces
// it: a CI step needs this machine's PATH and toolchains. The request's env and
// secrets are laid over it. The one thing held back is the worker's own
// credential (MEMQL_WORKER_TOKEN), so a test that prints its environment does
// not print it into a log. This is not a sandbox: the step runs as the user
// the worker runs as, which is what pipelines.allow consents to. Container
// commands instead follow the explicit image/platform contract in pipeline_container.go.
//
// SECRETS ARE MASKED as the cluster masks them -- "***" for every value in
// every form it is printed in: as stored, without the whitespace around it,
// and each line of a multi-line one likewise (a line ends at a newline or a
// carriage return), four bytes or more; values that overlap where printed are
// one mask -- in every chunk streamed and in the result. The stream is masked
// a line at a time, as the cluster masks the lines it reassembles, and cut
// into chunks only at line ends -- or, for a line longer than
// maxPendingOutput, where its masking is decided, holding a value that may
// still be arriving from its first byte until all of it can be seen -- so a
// value split across two reads, or across many, is whole when it is masked.

const (
	// defaultPipelineStepTimeoutSec is a step's timeout when the request names
	// none: the engine's own per-step default, so a step gets as long here as it
	// would as a Job.
	defaultPipelineStepTimeoutSec = 1200

	// maxPendingOutput bounds how much of one unfinished line the output
	// chunker holds before it sends part of it.
	maxPendingOutput = 64 << 10

	// pipelineOutputDrain bounds the wait for a step's output once it has
	// ended and what it left in its process group has been killed: a process
	// that left the group (setsid) can hold the pipes open indefinitely.
	pipelineOutputDrain = 5 * time.Second

	// pipelineFailureTail is how much of git's stderr a clone failure quotes.
	pipelineFailureTail = 2 << 10

	// minSecretLength is the shortest form of a value that is masked. Masking
	// "abc" -- or the indent-only line of a key -- would shred ordinary
	// output; the cluster draws the same line.
	minSecretLength = 4

	// workerTokenVariable is the worker's own credential when it is passed in
	// the environment (cli.go reads it). A step never inherits it.
	workerTokenVariable = "MEMQL_WORKER_TOKEN"
)

var (
	// pipelineStopGrace is how long a step stopped for its timeout gets between
	// SIGTERM and SIGKILL. A variable only so a test can shorten it.
	pipelineStopGrace = 10 * time.Second

	// pipelineArtifactMaxBytes caps a step's artifacts at the cluster's own
	// limit for a Job's: 64 MiB of files.
	pipelineArtifactMaxBytes int64 = 64 << 20

	// pipelineArtifactWireBytes caps the COMPRESSED archive at what one result
	// can carry. The result travels in a single ToolResult on the worker
	// stream, whose messages are capped at 32 MiB in both directions (the SDK's
	// DefaultMaxMessageSize, the agent's maxWorkerMessageSize), and base64
	// costs a third again: 20 MiB of archive is under 27 MiB on the wire, with
	// room left for the envelope. An archive over it is left out and reported,
	// never sent -- a message over the cap fails the send, and the result with
	// it.
	pipelineArtifactWireBytes int64 = 20 << 20

	// gitBinary is the git a step fetches with. A variable only so a test can
	// stand a recording wrapper in front of the real one.
	gitBinary = "git"

	// localCloneSources admits file:// clone URLs. Production refuses them:
	// the cluster naming a path on this machine to fetch from is not part of
	// the contract (the Job clones over https), and https is what keeps the
	// fetch on the step's token or nothing. The tests open it to clone from a
	// bare repository on disk.
	localCloneSources = false
)

var (
	shaPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// outputEmitter receives a running call's output, masked, in order per
// stream. The dispatcher makes each piece one ToolStream frame.
type outputEmitter func(stderr bool, data []byte)

// pipelineStepRequest is one decoded and validated pipeline_step.
type pipelineStepRequest struct {
	cloneURL   string
	sha        string
	token      string
	repository string
	command    string
	env        map[string]string
	secrets    map[string]string
	artifacts  []string
	timeoutSec int
	execution  string
	platform   string
	image      string
	dockerID   string
	needs      []string
	services   []pipelineService
	caches     []string
	cacheScope string
	cacheHome  string
	cacheDir   string
}

// runPipelineStep implements workerHost.pipeline_step. agentID is the
// dispatch envelope's agent_id; emit may be nil, which drops the output.
func runPipelineStep(ctx context.Context, agentID string, args map[string]any, policy *Policy, emit outputEmitter) (result *memqlv1.Success, failed *memqlv1.Failure) {
	started := time.Now()
	if emit == nil {
		emit = func(bool, []byte) {}
	}
	// The masker is built first, from whatever the request carries, so even a
	// refusal's sentence cannot repeat a value.
	mask := newSecretMasker(secretValuesOf(args))
	refuse := func(code string, err error) (*memqlv1.Success, *memqlv1.Failure) {
		return nil, failure(code, mask.mask(err.Error()))
	}

	// ONLY THE CLUSTER'S PIPELINE RUNNER DISPATCHES A STEP, and it dispatches
	// with no agent; an agent's tool loop always names its agent. A step is
	// a command run with no consent window, so one an agent asked for is
	// refused whatever the policy says -- pipelines.allow is consent to the
	// pipeline runner, not to every caller that can name the action.
	if agent := strings.TrimSpace(agentID); agent != "" {
		return refuse("denied_by_policy", fmt.Errorf("pipeline_step: pipeline steps come only from the cluster's pipeline runner, and this one was dispatched by agent %s", agent))
	}

	settings := policy.Pipelines()
	if err := settings.Check(argString(args, "repository")); err != nil {
		return refuse("denied_by_policy", err)
	}
	req, err := parsePipelineStep(args)
	if err != nil {
		return refuse("bad_request", err)
	}
	if len(req.caches) > 0 {
		req.cacheHome, _ = ctx.Value(pipelineCacheHomeKey{}).(string)
		if req.cacheHome == "" {
			return refuse("bad_request", errors.New("pipeline_step: cache reuse requires a locally bound cluster enrollment"))
		}
	}
	if err := gitUnusable(); err != nil {
		return refuse("pipeline_clone_failed", err)
	}

	timeout := pipelineStepTimeout(req.timeoutSec, settings.MaxTimeoutSec)
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reservation, err := acquirePipelineReservation()
	if err != nil {
		code := "pipeline_capacity_unavailable"
		if errors.Is(err, errPipelineBusy) {
			code = "pipeline_capacity_busy"
		}
		return refuse(code, err)
	}
	defer reservation.close()
	if reservation.dirty {
		if err := reconcilePipelineReservation(stepCtx, reservation); err != nil {
			return refuse("pipeline_recovery_required", err)
		}
	}
	if err := checkPipelineRuntime(stepCtx, &req); err != nil {
		return refuse("pipeline_runtime_unavailable", err)
	}
	if len(req.caches) > 0 {
		if err := preparePipelineCache(&req); err != nil {
			return refuse("pipeline_cache_unavailable", err)
		}
		defer func() {
			// Uncertain containers may still be writing: never prune beneath
			// them or release the capacity reservation to another build.
			if !reservation.dirty {
				if err := prunePipelineCaches(); err != nil {
					emit(true, []byte("memql: cache retention could not complete; the next cached build will recheck it\n"))
				}
			}
		}()
	}

	if err := os.MkdirAll(settings.WorkspaceRoot, 0o700); err != nil {
		return refuse("exec_failed", fmt.Errorf("pipeline_step: cannot make the pipelines workspace root %s (pipelines.workspace_root): %w", settings.WorkspaceRoot, err))
	}
	dir, err := makePipelineWorkspace(reservation, settings.WorkspaceRoot)
	if err != nil {
		return refuse("exec_failed", fmt.Errorf("pipeline_step: cannot make a step directory under %s: %w", settings.WorkspaceRoot, err))
	}
	defer func() {
		if err := finishPipelineWorkspace(reservation, dir); err != nil {
			result = nil
			failed = failure("pipeline_cleanup_uncertain", mask.mask("pipeline_step: workspace cleanup could not be confirmed; capacity remains reserved: "+err.Error()))
		}
	}()

	run := &pipelineRun{req: req, dir: dir, mask: mask, emit: emit, started: started, reservation: reservation,
		containerMemoryMiB: settings.ContainerMemoryMiB}
	if fail := run.fetch(stepCtx); fail != nil {
		return nil, fail
	}
	exitCode, fail := run.command(stepCtx)
	if fail != nil {
		return nil, fail
	}
	pack := run.artifacts()

	payload := map[string]any{
		"exitCode":           exitCode,
		"durationMs":         time.Since(started).Milliseconds(),
		"artifactsTgzBase64": base64.StdEncoding.EncodeToString(pack.tgz),
		"artifactsMissing":   pack.missing,
	}
	if pack.tooLarge {
		payload["artifactsTooLarge"] = true
	}
	preview := mask.mask(fmt.Sprintf("pipeline step %s@%s exited %d", req.repository, req.sha[:12], exitCode))
	return successJSON(payload, exitCode, 0, int(run.bytes.Load()), preview), nil
}

// pipelineStepTimeout is the timeout a step runs under: the one it asked
// for, or the engine's per-step default when it asked for none, never more
// than the policy's max_timeout_sec.
func pipelineStepTimeout(requested, ceiling int) time.Duration {
	if ceiling <= 0 {
		ceiling = DefaultPipelineMaxTimeoutSec
	}
	sec := requested
	if sec <= 0 {
		sec = defaultPipelineStepTimeoutSec
	}
	if sec > ceiling {
		sec = ceiling
	}
	return time.Duration(sec) * time.Second
}

// secretValuesOf is every value the request asks to have masked: each
// secret, the token, and the header the token becomes. Read from the raw
// arguments, before they are validated, so a refusal is masked as well.
func secretValuesOf(args map[string]any) []string {
	var values []string
	if secrets, ok := args["secrets"].(map[string]any); ok {
		for _, v := range secrets {
			if s, ok := v.(string); ok {
				values = append(values, s)
			}
		}
	}
	if token := strings.TrimSpace(argString(args, "token")); token != "" {
		values = append(values, token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	}
	return values
}

// parsePipelineStep decodes and validates the request. Its errors name the
// field, never a value that could be a secret.
func parsePipelineStep(args map[string]any) (pipelineStepRequest, error) {
	req := pipelineStepRequest{
		cloneURL:   strings.TrimSpace(argString(args, "cloneUrl")),
		sha:        strings.ToLower(strings.TrimSpace(argString(args, "sha"))),
		token:      strings.TrimSpace(argString(args, "token")),
		repository: strings.TrimSpace(argString(args, "repository")),
		command:    argString(args, "command"),
		timeoutSec: argInt(args, "timeoutSec", 0),
	}
	if err := checkCloneURL(req.cloneURL); err != nil {
		return req, err
	}
	// The repository the policy admitted (pipelines.repos) must be the one
	// that is cloned: a listed name over another repository's URL would make
	// the list a filter on what the request SAYS rather than on what runs.
	if req.repository == "" {
		return req, errors.New("pipeline_step: repository required")
	}
	if !cloneNamesRepository(req.cloneURL, req.repository) {
		named := ""
		if u, err := url.Parse(req.cloneURL); err == nil {
			named = clonedRepository(u)
		}
		return req, fmt.Errorf("pipeline_step: cloneUrl names %s, not the step's repository %s; the repository this machine admits is the one it clones", named, req.repository)
	}
	if !shaPattern.MatchString(req.sha) {
		return req, errors.New("pipeline_step: sha must be a full 40-character commit id")
	}
	if strings.TrimSpace(req.command) == "" {
		return req, errors.New("pipeline_step: command required")
	}
	if err := parsePipelineExecution(args, &req); err != nil {
		return req, err
	}
	var err error
	if req.env, err = stringMap(args, "env"); err != nil {
		return req, err
	}
	if req.secrets, err = stringMap(args, "secrets"); err != nil {
		return req, err
	}
	for name := range req.secrets {
		if _, clash := req.env[name]; clash {
			return req, fmt.Errorf("pipeline_step: %s is both an env variable and a secret", name)
		}
	}
	if req.execution != "container" {
		for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
			_, envSet := req.env[name]
			_, secretSet := req.secrets[name]
			if envSet || secretSet {
				return req, fmt.Errorf("pipeline_step: %s is owned by the native runner's scratch cleanup", name)
			}
		}
	}
	for _, cache := range req.caches {
		for name := range pipelineCacheEnvironment[cache] {
			if _, found := req.env[name]; found {
				return req, errors.New("pipeline_step: environment overrides a declared cache")
			}
			if _, found := req.secrets[name]; found {
				return req, errors.New("pipeline_step: secret overrides a declared cache")
			}
		}
	}
	if req.artifacts, err = artifactList(args); err != nil {
		return req, err
	}
	return req, nil
}

// checkCloneURL admits an https URL with a host and no credentials in it --
// the token travels on its own, never in the URL, which git would write into
// FETCH_HEAD. ssh and scp-like URLs are refused because they would fetch with
// this machine's own SSH key; plain http because it would send the token in
// the clear.
func checkCloneURL(raw string) error {
	if raw == "" {
		return errors.New("pipeline_step: cloneUrl required")
	}
	u, err := url.Parse(raw)
	if err == nil {
		switch strings.ToLower(u.Scheme) {
		case "https":
			if u.User != nil {
				return errors.New("pipeline_step: cloneUrl carries credentials; the token travels in token, never in the URL")
			}
			if u.Host != "" {
				return nil
			}
		case "file":
			if localCloneSources {
				return nil
			}
		}
	}
	return errors.New("pipeline_step: cloneUrl must be an https URL -- the fetch uses the step's token or nothing, never this machine's own credentials")
}

// cloneNamesRepository reports whether a clone URL's path names repository,
// both read as owner/name without regard to case or a .git suffix. An https
// URL names it with its whole path -- https://github.com/o/r.git names o/r,
// and https://host/scm/o/r.git names scm/o/r, which is not o/r. A file:// URL
// (the tests' door) names it by ending in it, since a local path has no
// owner/name of its own.
func cloneNamesRepository(rawURL, repository string) bool {
	want := normalRepository(repository)
	if want == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	named := clonedRepository(u)
	if strings.EqualFold(u.Scheme, "file") {
		return named == want || strings.HasSuffix(named, "/"+want)
	}
	return named == want
}

// clonedRepository is the owner/name a clone URL's path names.
func clonedRepository(u *url.URL) string {
	return normalRepository(strings.Trim(u.Path, "/"))
}

// stringMap reads an object of environment variable names to strings.
func stringMap(args map[string]any, key string) (map[string]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pipeline_step: %s must be an object of names to strings", key)
	}
	out := make(map[string]string, len(m))
	for name, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("pipeline_step: %s.%s must be a string", key, name)
		}
		if !envNamePattern.MatchString(name) {
			return nil, fmt.Errorf("pipeline_step: %q in %s is not an environment variable name", name, key)
		}
		if strings.ContainsRune(s, 0) {
			return nil, fmt.Errorf("pipeline_step: %s.%s contains a NUL byte", key, name)
		}
		out[name] = s
	}
	return out, nil
}

// artifactList reads the declared artifact paths, refusing what would leave
// the step directory -- the same paths the cluster's Job refuses.
func artifactList(args map[string]any) ([]string, error) {
	v, ok := args["artifacts"]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("pipeline_step: artifacts must be a list of paths relative to the checkout")
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		p, ok := item.(string)
		if !ok {
			return nil, errors.New("pipeline_step: artifacts must be a list of paths relative to the checkout")
		}
		if err := checkArtifactPath(p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func checkArtifactPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("pipeline_step: an artifact path is empty")
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return fmt.Errorf("pipeline_step: artifact %q is absolute; artifacts are paths relative to the checkout", p)
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return fmt.Errorf("pipeline_step: artifact %q leaves the checkout", p)
		}
	}
	if _, err := filepath.Match(p, ""); err != nil {
		return fmt.Errorf("pipeline_step: artifact %q is not a usable path or glob: %v", p, err)
	}
	return nil
}

// gitUnusable reports why this machine cannot fetch a step, or nil.
//
// On a Mac without the command-line developer tools, /usr/bin/git is a stub
// that answers by raising an install dialog -- from a LaunchAgent, on a
// machine whose owner may not be at it -- so it is never run. The same rule
// keeps it out of the app-session fingerprint (appsession/fingerprint.go).
func gitUnusable() error {
	bin, err := exec.LookPath(gitBinary)
	if err != nil {
		return fmt.Errorf("pipeline_step: git is not installed on this machine, or not on the worker's PATH: %v", err)
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(bin, "/usr/bin/") && !macDeveloperToolsPresent() {
		return errors.New("pipeline_step: git on this machine is the macOS install stub; install the command-line developer tools (xcode-select --install)")
	}
	return nil
}

// macDeveloperToolsPresent reports whether the command-line developer tools
// or Xcode are installed, which is what turns /usr/bin/git from an
// install-dialog stub into git.
func macDeveloperToolsPresent() bool {
	for _, dir := range []string{
		"/Library/Developer/CommandLineTools/usr/bin",
		"/Applications/Xcode.app/Contents/Developer/usr/bin",
	} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// pipelineRun is one step in flight.
type pipelineRun struct {
	req         pipelineStepRequest
	dir         string
	mask        *secretMasker
	emit        outputEmitter
	started     time.Time
	reservation *pipelineReservation
	// Local policy snapshot, never populated from cluster request arguments.
	containerMemoryMiB int
	// bytes is the output the step produced, before masking.
	bytes atomic.Int64
}

// note writes one line of the worker's own into the step's output, the way
// the Job's clone script says what it checked out.
func (r *pipelineRun) note(stderr bool, line string) {
	r.emit(stderr, []byte(r.mask.mask(line)))
}

// fetch makes the step directory a checkout of the sha: init, a depth-1 fetch
// of exactly that commit, FETCH_HEAD checked out -- the Job's clone script.
func (r *pipelineRun) fetch(ctx context.Context) (failed *memqlv1.Failure) {
	if err := r.reservation.begin(pipelineAttemptRecord{Execution: "checkout", Workspace: r.dir}); err != nil {
		return failure("pipeline_recovery_required", "pipeline_step: could not record checkout ownership")
	}
	defer func() {
		// runGroup has joined each Git process and stopped its group. Only
		// this normal return proves that no checkout command is still writing.
		if err := r.reservation.cleanupReady(r.dir); err != nil {
			failed = failure("pipeline_recovery_required", "pipeline_step: checkout ended but cleanup could not be recorded")
		}
	}()
	env := gitEnvironment(os.Environ(), r.req.token)
	for _, args := range [][]string{
		{"init", "-q"},
		// "--" so nothing the cluster sends can be read as an option.
		{"fetch", "-q", "--depth=1", "--", r.req.cloneURL, r.req.sha},
		{"checkout", "-q", "FETCH_HEAD"},
	} {
		cmd := exec.Command(gitBinary, args...)
		cmd.Dir = r.dir
		cmd.Env = env
		res, err := runGroup(ctx, cmd, r.mask, r.emit)
		r.bytes.Add(res.bytes)
		if res.stopped {
			return r.stopped(ctx)
		}
		if err != nil {
			return failure("pipeline_clone_failed", r.mask.mask(fmt.Sprintf("pipeline_step: git %s could not run: %v", args[0], err)))
		}
		if res.exitCode != 0 {
			msg := fmt.Sprintf("pipeline_step: git %s exited %d", args[0], res.exitCode)
			if tail := strings.TrimSpace(res.stderrTail); tail != "" {
				msg += ": " + tail
			}
			return failure("pipeline_clone_failed", msg)
		}
	}
	r.note(false, "memql: checked out "+r.req.sha+"\n")
	return nil
}

// command runs the step's command in the checkout and reports its exit
// status: its own code, or 128 plus the signal that ended it.
func (r *pipelineRun) command(ctx context.Context) (int, *memqlv1.Failure) {
	if r.req.execution == "container" {
		return r.containerCommand(ctx)
	}
	// The checkout already has durable ownership. Keep native tool scratch
	// under it so failed packaging/compilation cannot strand files in the
	// operator's global temp directory. Git's metadata directory keeps these
	// files out of the source status and the build's VCS-dirty stamp.
	scratch, err := os.MkdirTemp(filepath.Join(r.dir, ".git"), "memql-scratch-")
	if err != nil {
		return 0, failure("exec_failed", "pipeline_step: could not create owned native scratch")
	}
	if err := r.reservation.begin(pipelineAttemptRecord{Execution: "native", Workspace: r.dir}); err != nil {
		return 0, failure("pipeline_recovery_required", err.Error())
	}
	cmd := exec.Command("/bin/sh", "-c", r.req.command)
	cmd.Dir = r.dir
	cmd.Env = stepEnvironment(os.Environ(), r.req.env, r.req.secrets)
	cmd.Env = stepEnvironment(cmd.Env, map[string]string{"TMPDIR": scratch, "TMP": scratch, "TEMP": scratch}, nil)
	res, err := runGroup(ctx, cmd, r.mask, r.emit)
	r.bytes.Add(res.bytes)
	if err := r.reservation.cleanupReady(r.dir); err != nil {
		return 0, failure("pipeline_recovery_required", "pipeline_step: could not record completed command cleanup")
	}
	if res.stopped {
		return 0, r.stopped(ctx)
	}
	if err != nil {
		return 0, failure("exec_failed", r.mask.mask(fmt.Sprintf("pipeline_step: the command could not run: %v", err)))
	}
	return res.exitCode, nil
}

// stopped is the failure for a step whose context ended before it did: past
// its timeout, or cancelled -- by the cluster, by the loss of the stream it
// arrived on, or by the worker shutting down. The log says which first.
func (r *pipelineRun) stopped(ctx context.Context) *memqlv1.Failure {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		limit := "its timeout"
		if deadline, ok := ctx.Deadline(); ok {
			limit = fmt.Sprintf("its %s timeout", deadline.Sub(r.started).Round(time.Second))
		}
		r.note(true, "memql: stopped: the step ran past "+limit+"\n")
		return failure("timeout", "pipeline_step: the step ran past "+limit+" and was stopped")
	}
	r.note(true, "memql: stopped: the step was cancelled before it finished\n")
	return failure("cancelled", "pipeline_step: the step was cancelled before it finished (by the cluster, by the loss of its stream, or by the worker shutting down)")
}

// artifacts packs the declared paths and says in the log when they were left
// out for their size.
func (r *pipelineRun) artifacts() artifactPack {
	pack := packArtifacts(r.dir, r.req.artifacts)
	for i, m := range pack.missing {
		pack.missing[i] = r.mask.mask(m)
	}
	if pack.tooLarge {
		r.note(true, "memql: artifacts not returned: "+pack.reason+"\n")
	}
	return pack
}

// gitEnvironment is the environment the fetch runs in: the machine's, minus
// every GIT_ variable and the worker's own credential, plus configuration
// that keeps this machine's own credentials out of it -- and, when there is a
// token, the header that carries it.
func gitEnvironment(machine []string, token string) []string {
	env := make([]string, 0, len(machine)+12)
	for _, kv := range machine {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_") || name == workerTokenVariable {
			continue
		}
		env = append(env, kv)
	}
	var config [][2]string
	if token != "" {
		header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
		config = append(config, [2]string{"http.extraheader", header})
	}
	// An empty credential.helper RESETS the list, so no helper of the owner's
	// (osxkeychain, a manager) can answer for a repository the token cannot
	// reach.
	config = append(config, [2]string{"credential.helper", ""})
	env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		// Set and empty: git then asks no askpass program, SSH_ASKPASS included.
		"GIT_ASKPASS=",
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(config)),
	)
	for i, kv := range config {
		env = append(env,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	return env
}

// stepEnvironment is the environment the command runs in: the machine's,
// minus the worker's own credential, with the request's env and then its
// secrets laid over it (exec takes the last of a repeated name).
func stepEnvironment(machine []string, env, secrets map[string]string) []string {
	out := make([]string, 0, len(machine)+len(env)+len(secrets))
	for _, kv := range machine {
		name, _, _ := strings.Cut(kv, "=")
		if name == workerTokenVariable {
			continue
		}
		out = append(out, kv)
	}
	for _, name := range sortedKeys(env) {
		out = append(out, name+"="+env[name])
	}
	for _, name := range sortedKeys(secrets) {
		out = append(out, name+"="+secrets[name])
	}
	return out
}

// removeStepDir removes a step's directory whatever the step left in it. A
// tree the step made read-only -- a Go module cache does exactly that --
// refuses RemoveAll, so its directories are made writable and the removal is
// tried again.
func removeStepDir(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// A successful command is not a completed attempt until its scratch tree is
// gone. Keep a durable cleanup-only record while packing artifacts and deleting
// the workspace, so a crash or a filesystem error cannot silently leak it.
// An uncertain running container/native process is deliberately left alone.
func finishPipelineWorkspace(reservation *pipelineReservation, dir string) error {
	if reservation.dirty {
		record, err := reservation.read()
		if err != nil {
			return err
		}
		if record.Execution != "cleanup" {
			return nil
		}
		if record.Workspace != dir {
			return errPipelineUnreconciled
		}
	} else if err := reservation.cleanupReady(dir); err != nil {
		// Fetch may fail before a command reserves the runtime. Persist the
		// directory even on this path before attempting its removal.
		return err
	}
	if err := removePipelineWorkspace(dir); err != nil {
		return err
	}
	return reservation.clear()
}

// Injectable filesystem boundary for cleanup failure and recovery tests.
var removePipelineWorkspace = removeStepDir

// Record the scratch identity before creating it, including the crash window
// before Git starts. A crash during Git leaves an explicit checkout attempt,
// never an unrecorded directory that a later build can silently abandon.
func makePipelineWorkspace(reservation *pipelineReservation, root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(root, "step-"+hex.EncodeToString(nonce[:]))
	if err := reservation.cleanupReady(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		// The directory was not created by this call. Never remove an
		// existing namesake or infer ownership from a failed mkdir.
		return "", errors.Join(err, reservation.clear())
	}
	return dir, nil
}

// --- running a process group --------------------------------------------

// groupResult is how one process run through runGroup ended.
type groupResult struct {
	exitCode int
	// stopped: the context ended first, and the group was stopped for it.
	stopped bool
	// bytes is the output read, before masking.
	bytes int64
	// stderrTail is the end of what it wrote to stderr, masked.
	stderrTail string
}

// runGroup runs cmd as the leader of a process group of its own, carries its
// stdout and stderr to emit as masked chunks while it runs, and returns once it
// has exited and its output is drained.
//
// The group is what everything is done to. When ctx ends it gets SIGTERM, and
// SIGKILL after pipelineStopGrace. When the command exits, whatever it left
// running in its group is killed -- the way a pod's teardown ends what its
// container started -- so a background process can neither hold the step's
// output open nor outlive it on somebody's machine.
//
// The pipes are the worker's own rather than exec's, so Wait waits for the
// process and nothing else; the output is drained afterwards under a bound,
// because a process that left the group can hold them open forever.
func runGroup(ctx context.Context, cmd *exec.Cmd, mask *secretMasker, emit outputEmitter) (groupResult, error) {
	var res groupResult
	if ctx.Err() != nil {
		// Already over -- cancelled or out of time between two commands.
		// Nothing is started only to be stopped.
		res.stopped = true
		return res, nil
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return res, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return res, err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	ownProcessGroup(cmd)
	startErr := cmd.Start()
	// The child holds its own copies of the write ends. Ours must go, or the
	// readers never see the end of the output.
	outW.Close()
	errW.Close()
	if startErr != nil {
		outR.Close()
		errR.Close()
		return res, startErr
	}

	var total atomic.Int64
	tail := &tailBuffer{max: pipelineFailureTail}
	var readers sync.WaitGroup
	readers.Add(2)
	go pump(outR, newLineChunker(mask, func(b []byte) { emit(false, b) }), &total, &readers)
	go pump(errR, newLineChunker(mask, func(b []byte) {
		tail.write(b)
		emit(true, b)
	}), &total, &readers)

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		res.stopped = true
		signalProcessGroup(cmd, false)
		select {
		case waitErr = <-waited:
		case <-time.After(pipelineStopGrace):
			signalProcessGroup(cmd, true)
			waitErr = <-waited
		}
	}
	signalProcessGroup(cmd, true)

	drained := make(chan struct{})
	go func() {
		readers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(pipelineOutputDrain):
		// Something outside the group still holds the pipes. Closing our ends
		// ends the reads; a reader still inside emit is left to finish.
		outR.Close()
		errR.Close()
		select {
		case <-drained:
		case <-time.After(pipelineOutputDrain):
		}
	}
	outR.Close()
	errR.Close()
	res.bytes = total.Load()
	res.stderrTail = tail.String()
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return res, waitErr
		}
	}
	res.exitCode = exitStatusOf(cmd.ProcessState)
	return res, nil
}

// pump reads one of a process's pipes to its end, into a chunker.
func pump(r *os.File, c *lineChunker, total *atomic.Int64, done *sync.WaitGroup) {
	defer done.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			total.Add(int64(n))
			c.write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	c.flush()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) write(b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// --- masking -------------------------------------------------------------

// secretMasker replaces every secret in text with "***" as the cluster masks
// it (the engine's pipelines.MaskSecrets): each value in every form it is
// printed in -- as stored, without the whitespace around it, and, for a value
// over several lines, each line without the whitespace around it, a line
// ending at a newline or a carriage return -- of minSecretLength bytes or
// more. Occurrences that overlap or touch, of one value or of two, are one
// span under one mask, so no byte of either survives between two masks. An
// indent-only line of a key trims to nothing, and masks nothing; each part of
// a value a bare carriage return breaks ("user\rpass") is masked alone.
type secretMasker struct {
	starts  [256]bool         // the bytes a form starts with
	byFirst map[byte][]string // the forms by their first byte
}

func newSecretMasker(values []string) *secretMasker {
	m := &secretMasker{byFirst: map[byte][]string{}}
	seen := map[string]bool{}
	add := func(form string) {
		if len(form) < minSecretLength || seen[form] {
			return
		}
		seen[form] = true
		m.starts[form[0]] = true
		m.byFirst[form[0]] = append(m.byFirst[form[0]], form)
	}
	for _, v := range values {
		add(v)
		add(strings.TrimSpace(v))
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, isLineBreak) {
				add(strings.TrimSpace(line))
			}
		}
	}
	return m
}

// isLineBreak is what ends a line of a value: a newline or a carriage return.
func isLineBreak(r rune) bool { return r == '\n' || r == '\r' }

// mask is s with every secret masked, s taken as one text.
func (m *secretMasker) mask(s string) string {
	if m == nil || len(m.byFirst) == 0 {
		return s
	}
	out, _ := m.scan(s, &maskState{}, true)
	return out
}

// maskState is where a scan of a line stopped: inside a span whose mask it
// wrote, or where one ends -- which a value occurring there carries on --
// with the span reaching reach bytes past the stop.
type maskState struct {
	span  bool
	reach int
}

// scan masks s from where st says the last scan of its line stopped, and
// answers the masked text and how much of s it covers. A span's mask is
// written where the span starts, and how far it reaches is carried in st, so
// a line can be masked in parts that join into the line masked whole. It
// stops before the first position it cannot decide yet -- one where a value
// may occur that s ends inside of -- unless complete says s ends where its
// line does.
func (m *secretMasker) scan(s string, st *maskState, complete bool) (string, int) {
	if m == nil {
		return s, len(s)
	}
	var out strings.Builder
	out.Grow(len(s))
	cover := st.reach // where the open span ends, in s
	i := 0
scan:
	for i < len(s) {
		if st.span && i < cover {
			// Inside the span: nothing is written, and a value occurring
			// here may carry the span further.
			if m.starts[s[i]] {
				end, decided := m.at(s, i, complete)
				if !decided {
					break
				}
				cover = max(cover, end)
			}
			i++
			continue
		}
		if !m.starts[s[i]] {
			// The text's own bytes, up to where a value could start; a span
			// that ended here is over.
			st.span = false
			j := i + 1
			for j < len(s) && !m.starts[s[j]] {
				j++
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}
		end, decided := m.at(s, i, complete)
		switch {
		case !decided:
			break scan
		case end >= 0 && st.span:
			// A value occurring where the span ends carries it on.
			cover = end
		case end >= 0:
			// A span starts here: its mask.
			out.WriteString("***")
			st.span, cover = true, end
		default:
			st.span = false
			out.WriteByte(s[i])
		}
		i++
	}
	st.reach = 0
	if st.span {
		st.reach = cover - i
	}
	return out.String(), i
}

// at is where the longest form occurring at s[i] ends, or -1 when none does.
// decided is false when a form s ends inside of may still occur there: s is
// cut short of its line (complete is false), and that form's end decides how
// far a span reaches.
func (m *secretMasker) at(s string, i int, complete bool) (end int, decided bool) {
	end, rest := -1, s[i:]
	for _, form := range m.byFirst[s[i]] {
		switch {
		case strings.HasPrefix(rest, form):
			end = max(end, i+len(form))
		case !complete && len(rest) < len(form) && strings.HasPrefix(form, rest):
			return -1, false
		}
	}
	return end, true
}

// lineChunker cuts one output stream into masked chunks. The stream is
// masked a line at a time, as the cluster masks the lines it reassembles from
// these chunks: how the output happened to be read changes nothing, and a
// value stored with a line end never takes a line end with it. A chunk ends
// at a line end -- or, for a line longer than maxPendingOutput, where the
// line's masking is decided: a span's mask goes out where the span starts,
// how far the span reaches is carried to the next part, and only a value that
// may still be arriving is held, from its first byte, until all of it can be
// seen. A value split across two reads, or across many, is masked whole.
type lineChunker struct {
	mask *secretMasker
	emit func([]byte)
	// pending is the current line, not yet sent; it holds no line end.
	pending []byte
	// state is where the current line was cut when part of it was sent.
	state maskState
}

func newLineChunker(mask *secretMasker, emit func([]byte)) *lineChunker {
	return &lineChunker{mask: mask, emit: emit}
}

func (c *lineChunker) write(p []byte) {
	c.pending = append(c.pending, p...)
	var out []byte
	if bytes.IndexByte(p, '\n') >= 0 {
		// pending held no line end before p: every line end is in p.
		rest := c.pending
		for {
			i := bytes.IndexByte(rest, '\n')
			if i < 0 {
				break
			}
			line, _ := c.masked(rest[:i], true)
			out = append(append(out, line...), '\n')
			rest = rest[i+1:]
		}
		c.pending = append(c.pending[:0], rest...)
	}
	if len(c.pending) > maxPendingOutput {
		part, used := c.masked(c.pending, false)
		out = append(out, part...)
		c.pending = append(c.pending[:0], c.pending[used:]...)
	}
	if len(out) > 0 {
		c.emit(out)
	}
}

// flush sends what is left, line end or not: the stream has ended.
func (c *lineChunker) flush() {
	if rest, _ := c.masked(c.pending, true); rest != "" {
		c.emit([]byte(rest))
	}
	c.pending = c.pending[:0]
}

// masked masks b, the current line from where its last part was cut, and
// answers that and how much of b it covers. complete says the line ends with
// b: the next starts afresh.
func (c *lineChunker) masked(b []byte, complete bool) (string, int) {
	out, used := c.mask.scan(string(b), &c.state, complete)
	if complete {
		c.state = maskState{}
	}
	return out, used
}

// --- artifacts -----------------------------------------------------------

// artifactPack is a step's declared artifacts, packed.
type artifactPack struct {
	// tgz is a gzipped tar of regular files named relative to the checkout;
	// nil when there is nothing to send.
	tgz []byte
	// missing is every declared path that contributed no file, in the
	// declared order. Never nil: the result says [] rather than null.
	missing []string
	// tooLarge: the files were left out for their size; reason says which
	// limit.
	tooLarge bool
	reason   string
}

var errArtifactsTooLarge = errors.New("artifacts over the limit")

// packArtifacts gathers the declared paths -- literal paths or globs,
// directories taken whole -- from the step directory into a tar.gz.
//
// Only regular files travel, and only ones that are inside the step
// directory once every link in their path is followed: a link out of the
// checkout is not the step's artifact, and a link to / would otherwise walk
// the disk. Links themselves, devices and the like are left out, as the
// cluster's extractor leaves them out.
func packArtifacts(dir string, declared []string) artifactPack {
	pack := artifactPack{missing: []string{}}
	if len(declared) == 0 {
		return pack
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		pack.missing = append(pack.missing, declared...)
		return pack
	}
	type artifactFile struct {
		name, path string
		size       int64
		mode       fs.FileMode
		mod        time.Time
	}
	var files []artifactFile
	seen := map[string]bool{}
	checkout := os.DirFS(dir)
	for _, pattern := range declared {
		// Globbed over the CHECKOUT, so the path it lives at -- a workspace
		// root named "ci [main]" -- is never read as a pattern. Cleaned, so
		// "dist/" is the directory and "./x" is x; checkArtifactPath has
		// already refused anything Clean could turn into "..".
		matches, _ := fs.Glob(checkout, path.Clean(filepath.ToSlash(pattern)))
		sort.Strings(matches)
		contributed := 0
		for _, match := range matches {
			_ = filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(match)), func(file string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() || !insideDir(realDir, file) {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				rel, err := filepath.Rel(dir, file)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return nil
				}
				name := filepath.ToSlash(rel)
				contributed++
				if !seen[name] {
					seen[name] = true
					files = append(files, artifactFile{name: name, path: file, size: info.Size(), mode: info.Mode().Perm(), mod: info.ModTime()})
				}
				return nil
			})
		}
		if contributed == 0 {
			pack.missing = append(pack.missing, pattern)
		}
	}
	if len(files) == 0 {
		return pack
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	var total int64
	for _, f := range files {
		total += f.size
	}
	if total > pipelineArtifactMaxBytes {
		pack.tooLarge = true
		pack.reason = fmt.Sprintf("%d bytes of files is over this machine's %d-byte artifact limit", total, pipelineArtifactMaxBytes)
		return pack
	}

	var buf bytes.Buffer
	out := &limitedWriter{w: &buf, left: pipelineArtifactWireBytes}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	err = func() error {
		for _, f := range files {
			data, err := readArtifact(f.path, f.size)
			if err != nil {
				// Gone or unreadable since the walk: it is not sent.
				continue
			}
			// GNU encodes long/Unicode names without PAX metadata. The engine
			// accepts only plain regular-file snapshots before issuing receipts.
			hdr := &tar.Header{Format: tar.FormatGNU, Name: f.name, Mode: int64(f.mode), Size: int64(len(data)), ModTime: f.mod, Typeflag: tar.TypeReg}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}()
	if err != nil || out.exceeded {
		pack.tooLarge = true
		pack.reason = fmt.Sprintf("the archive is over the %d bytes one result can carry", pipelineArtifactWireBytes)
		return pack
	}
	pack.tgz = buf.Bytes()
	return pack
}

// readArtifact reads at most size bytes of a file -- the size the walk saw,
// so a file still growing cannot carry the archive past its limit.
func readArtifact(file string, size int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, size))
}

// insideDir reports whether path, every link in it followed, is inside
// realDir (itself already resolved).
func insideDir(realDir, file string) bool {
	real, err := filepath.EvalSymlinks(file)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realDir, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// limitedWriter fails the first write that would take it past left bytes.
type limitedWriter struct {
	w        io.Writer
	left     int64
	exceeded bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		l.exceeded = true
		return 0, errArtifactsTooLarge
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}
