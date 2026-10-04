//go:build agent

package pipelinesteps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	workerservice "github.com/znasllc-io/memql/component/worker"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// fleet_test.go -- a step naming a need runs on one of its owner's machines
// through the agent's dispatcher (epic memql#5478, #5494). Agent-tagged: the
// dispatcher is.

// fleetReq is a step that needs docker and a GPU on a pipeline whose owner
// consented to the fleet.
func fleetReq() pl.StepRequest {
	req := exRequest()
	req.Compute = pl.ComputeClusterAndFleet
	req.Step.Needs = []string{"docker", "gpu"}
	req.Step.Artifacts = []string{"coverage.out", "dist/report.json", "missing.txt"}
	return req
}

func fleetRun(req pl.StepRequest) StepRun { return stepRunFor(req, 900, pl.CodeStepTimeout) }

// fakeDispatcher records every dispatch and answers with the test's script.
type fakeDispatcher struct {
	mu     sync.Mutex
	reqs   []worker.Request
	ctxs   []context.Context
	answer func(ctx context.Context, req worker.Request) (worker.Result, error)
}

func (d *fakeDispatcher) Dispatch(ctx context.Context, req worker.Request) (worker.Result, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.ctxs = append(d.ctxs, ctx)
	answer := d.answer
	d.mu.Unlock()
	if answer == nil {
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
	}
	return answer(ctx, req)
}

func (d *fakeDispatcher) only(t *testing.T) (worker.Request, context.Context) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(d.reqs))
	}
	return d.reqs[0], d.ctxs[0]
}

// fakeLibrary records every file stored. Like the app's store -- whose quota
// read and upload both run under it -- it answers its context: one already
// ended stores nothing.
type fakeLibrary struct {
	mu    sync.Mutex
	files []RunFile
	omit  map[string]string
	fail  map[string]error
}

func (l *fakeLibrary) StoreRunFile(ctx context.Context, f RunFile) (StoredFile, error) {
	if err := ctx.Err(); err != nil {
		return StoredFile{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason, ok := l.omit[f.Name]; ok {
		return StoredFile{Omitted: reason}, nil
	}
	if err, ok := l.fail[f.Name]; ok {
		return StoredFile{}, err
	}
	l.files = append(l.files, f)
	return StoredFile{FileID: fmt.Sprintf("file-%d", len(l.files))}, nil
}

func (l *fakeLibrary) named(name string) (RunFile, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		if f.Name == name {
			return f, true
		}
	}
	return RunFile{}, false
}

// fakeTokens mints a token and remembers who asked for one.
type fakeTokens struct {
	mu    sync.Mutex
	asked []string
	token string
	err   error
}

func (m *fakeTokens) CloneToken(_ context.Context, installationID int64, owner, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, fmt.Sprintf("%d %s/%s", installationID, owner, name))
	return m.token, m.err
}

const fleetToken = "ghs_" + "fleetclonetokenvalue"

// newTestFleet is a fleet over the given dispatcher, with a capture whose
// store bucket is its own (the node's is shared by every test in the
// process) and a sink the test can read.
func newTestFleet(t *testing.T, d FleetDispatcher) (*Fleet, *fakeLibrary, *fakeTokens, *captureTestSink) {
	t.Helper()
	lib := &fakeLibrary{}
	tokens := &fakeTokens{token: fleetToken}
	sink := &captureTestSink{}
	cfg := exConfig()
	cfg.LogStoreMaxLines, cfg.ArchiveMaxBytes, cfg.ArtifactMaxBytes = 1000, 1<<20, 1<<20
	f := NewFleet(cfg, d, lib, tokens, func() LineSink { return sink }, quietLogger())
	f.tempDir = t.TempDir()
	f.now = func() time.Time { return exNow }
	f.openCapture = func(o CaptureOptions) (*Capture, error) {
		return newCapture(o, func() time.Time { return exNow }, newCaptureBucket(1<<20, exNow))
	}
	return f, lib, tokens, sink
}

func runFleet(t *testing.T, f *Fleet, req pl.StepRequest) pl.StepResult {
	t.Helper()
	res, err := f.RunStep(context.Background(), req, fleetRun(req))
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	return res
}

// TestFleetStepRoutesByNeedLabelsAndPipelinesLabel: the dispatch is the
// pipeline purpose's -- no agent, the owner's machines, the needs as exact
// labels plus the machine owner's consent label -- under internal origin and
// the owner's own forwarded authority, with the cockpit's whole contract.
func TestFleetStepRoutesByNeedLabelsAndPipelinesLabel(t *testing.T) {
	d := &fakeDispatcher{}
	f, _, tokens, _ := newTestFleet(t, d)
	req := fleetReq()
	if res := runFleet(t, f, req); res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want success", res)
	}
	got, ctx := d.only(t)

	if got.Tool != "workerHost" || got.Action != worker.PipelineStepAction || got.Purpose != worker.PurposePipeline {
		t.Errorf("dispatch %s.%s purpose %q, want workerHost.%s under the pipeline purpose",
			got.Tool, got.Action, got.Purpose, worker.PipelineStepAction)
	}
	if got.AgentId != "" {
		t.Errorf("AgentId = %q: a pipeline step names no agent, and the cockpit refuses one that does", got.AgentId)
	}
	if got.OwnerUserId != req.OwnerUserID || got.RunId != req.WorkRunID || got.StepId != req.StepKey {
		t.Errorf("owner/run/step = %q/%q/%q, want %q/%q/%q", got.OwnerUserId, got.RunId, got.StepId,
			req.OwnerUserID, req.WorkRunID, req.StepKey)
	}
	wantLabels := map[string]string{"docker": "true", "gpu": "true", worker.PipelinesLabel: worker.PipelinesAllowed}
	if !maps.Equal(got.RequireLabels, wantLabels) {
		t.Errorf("RequireLabels = %v, want %v", got.RequireLabels, wantLabels)
	}
	if got.Timeout != 900*time.Second+5*time.Minute {
		t.Errorf("Timeout = %v, want the step's 15m plus 5m for the clone and the artifacts", got.Timeout)
	}
	if got.OnStreamChunk == nil {
		t.Error("no OnStreamChunk: the machine's output would reach neither the log store nor the Library")
	}

	// INTERNAL ORIGIN: only the engine's own executor may claim the purpose.
	if !auth.OriginFromContext(ctx).IsInternal() {
		t.Error("the dispatch is not under internal origin; the dispatcher refuses the pipeline purpose without it")
	}
	// THE OWNER'S FORWARDED AUTHORITY: a machine held by a sibling replica is
	// reached over a forward that checks the machine against this subject.
	a, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok || a.Subject != req.OwnerUserID || a.CredentialClass != auth.ForwardedClassUser {
		t.Errorf("forwarded authority = %+v (%v), want the owner's", a, ok)
	}
	if _, err := auth.VerifyForwardedAuthority(a, time.Now()); err != nil {
		t.Errorf("the owner's assertion does not verify: %v", err)
	}

	// THE COCKPIT'S CONTRACT.
	run := fleetRun(req)
	if got.Args["cloneUrl"] != "https://github.com/acme/widget.git" || got.Args["repository"] != "acme/widget" {
		t.Errorf("cloneUrl/repository = %v/%v, want both from the one repository record", got.Args["cloneUrl"], got.Args["repository"])
	}
	if got.Args["sha"] != req.SHA || got.Args["command"] != req.Step.Run || got.Args["timeoutSec"] != 900 {
		t.Errorf("sha/command/timeoutSec = %v/%v/%v", got.Args["sha"], got.Args["command"], got.Args["timeoutSec"])
	}
	if got.Args["token"] != fleetToken || !slices.Equal(tokens.asked, []string{"42 acme/widget"}) {
		t.Errorf("token = %v (minted for %v), want one minted for the step's repository", got.Args["token"], tokens.asked)
	}
	if env, _ := got.Args["env"].(map[string]string); !maps.Equal(env, run.Env) {
		t.Errorf("env = %v, want the contract environment %v", got.Args["env"], run.Env)
	}
	if secrets, _ := got.Args["secrets"].(map[string]string); !maps.Equal(secrets, run.Secrets) {
		t.Errorf("secrets = %v, want the resolved values, separately", got.Args["secrets"])
	}
	if artifacts, _ := got.Args["artifacts"].([]string); !slices.Equal(artifacts, req.Step.Artifacts) {
		t.Errorf("artifacts = %v, want %v", got.Args["artifacts"], req.Step.Artifacts)
	}
	// The whole dispatch survives the JSON the envelope is: the shape the
	// dispatcher's credential check reads.
	if _, err := json.Marshal(got.Args); err != nil {
		t.Errorf("the args do not marshal: %v", err)
	}

	t.Run("the needs map to their labels", func(t *testing.T) {
		for _, c := range []struct {
			needs []string
			want  map[string]string
		}{
			{[]string{"macos_tooling"}, map[string]string{"os": "darwin", "pipelines": "allowed"}},
			{[]string{"user_files"}, map[string]string{"pipelines": "allowed"}},
			{[]string{"display"}, map[string]string{"display": "true", "pipelines": "allowed"}},
		} {
			d := &fakeDispatcher{}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			req.Step.Needs = c.needs
			runFleet(t, f, req)
			if got, _ := d.only(t); !maps.Equal(got.RequireLabels, c.want) {
				t.Errorf("needs %v: RequireLabels = %v, want %v", c.needs, got.RequireLabels, c.want)
			}
		}
	})

	t.Run("the clone URL keeps the record's host and nothing of its path", func(t *testing.T) {
		for _, c := range []struct{ cloneURL, want string }{
			{"https://ghe.example.com/acme/widget.git", "https://ghe.example.com/acme/widget.git"},
			{"https://ghe.example.com/scm/acme/widget.git", "https://ghe.example.com/acme/widget.git"},
			{"", "https://github.com/acme/widget.git"},
			{"git@github.com:acme/widget.git", "https://github.com/acme/widget.git"},
		} {
			d := &fakeDispatcher{}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			req.Repository.CloneURL = c.cloneURL
			runFleet(t, f, req)
			if got, _ := d.only(t); got.Args["cloneUrl"] != c.want || got.Args["repository"] != "acme/widget" {
				t.Errorf("record %q: cloneUrl = %v, want %s", c.cloneURL, got.Args["cloneUrl"], c.want)
			}
		}
	})

	t.Run("a public repository clones anonymously", func(t *testing.T) {
		d := &fakeDispatcher{}
		f, _, tokens, _ := newTestFleet(t, d)
		req := fleetReq()
		req.InstallationID = 0
		runFleet(t, f, req)
		if got, _ := d.only(t); got.Args["token"] != "" || len(tokens.asked) != 0 {
			t.Errorf("token = %v (asked %v), want none minted for installation 0", got.Args["token"], tokens.asked)
		}
	})

	t.Run("a token that cannot be minted runs nothing", func(t *testing.T) {
		d := &fakeDispatcher{}
		f, _, tokens, _ := newTestFleet(t, d)
		tokens.err = errors.New("the installation was suspended")
		res := runFleet(t, f, fleetReq())
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeCloneFailed)
		if !strings.Contains(res.Failure.Message, "suspended") {
			t.Errorf("message %q does not say why", res.Failure.Message)
		}
		if len(d.reqs) != 0 {
			t.Fatal("the step was dispatched without the token it needs to clone")
		}
	})
}

// TestFleetStepWithNoMachineIsTyped: every way of nothing having run because
// no machine could take the step reads as pipeline_no_machine_for_need, a
// refusal -- never as the step's command failing.
func TestFleetStepWithNoMachineIsTyped(t *testing.T) {
	// THE DISPATCHER'S VERDICT DECIDES, not the code. These are this
	// replica's own refusals and the ones a sibling replica answers with --
	// which travel through verbatim, so no list of codes here could keep up
	// with them -- each carried with RefusedBeforeStart.
	for _, code := range []string{
		"no_worker_available", "worker_busy", "worker_unreachable", "pipelines_not_allowed", "worker_disconnected",
		"owner_mismatch", "registration_refused", "forwarded_authority_refused", "decode_args",
		"denied_pipeline_purpose", "a_code_this_engine_has_never_seen",
	} {
		t.Run(code, func(t *testing.T) {
			labels := map[string]string{"docker": "true", "gpu": "true", "pipelines": "allowed"}
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				return worker.Result{OK: false, ErrorCode: code, ErrorMessage: "none of the 2 paired machine(s) can take it",
					RefusedBeforeStart: true, WorkerId: "reg-9", NodeId: "agent-b", Labels: labels}, nil
			}}
			f, lib, _, _ := newTestFleet(t, d)
			res := runFleet(t, f, fleetReq())
			wantFailure(t, res, pl.OutcomeRefused, pl.CodeNoMachineForNeed)
			for _, need := range []string{"docker", "gpu"} {
				if !strings.Contains(res.Failure.Message, need) {
					t.Errorf("message %q does not name the need %s", res.Failure.Message, need)
				}
			}
			if len(lib.files) != 0 {
				t.Errorf("a step that ran nowhere stored %d Library file(s)", len(lib.files))
			}
			// The machine the dispatcher named is where the step was last
			// looked for; a refusal keeps it.
			if res.Where.Surface != "fleet" || res.Where.WorkerID != "reg-9" || res.Where.NodeID != "agent-b" ||
				!maps.Equal(res.Where.MachineLabels, labels) {
				t.Errorf("Where = %+v, want the fleet and the machine the dispatcher named", res.Where)
			}
		})
	}

	t.Run("the same code without the verdict is a step that may have run", func(t *testing.T) {
		// worker_disconnected is both: a stale row refused before start, and a
		// connection that ended mid-call. Only the flag tells them apart.
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: "stream reset",
				WorkerId: "reg-9", NodeId: "agent-b"}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		wantFailure(t, runFleet(t, f, fleetReq()), pl.OutcomeFailed, pl.CodeNodeLost)
	})

	t.Run("a machine whose own policy refuses the repository fails the step", func(t *testing.T) {
		// Not a refusal before start: the machine was reached and said no,
		// so the dispatcher tried no other -- the step failed.
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{OK: false, ErrorCode: "denied_by_policy", ErrorMessage: "pipelines.repos does not list acme/widget",
				WorkerId: "reg-1", NodeId: "agent-a"}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		res := runFleet(t, f, fleetReq())
		wantFailure(t, res, pl.OutcomeFailed, pl.CodeNoMachineForNeed)
		if !strings.Contains(res.Failure.Message, "pipelines.repos") {
			t.Errorf("message %q drops the machine's own reason", res.Failure.Message)
		}
	})
}

// TestFleetKillSwitchIsTyped: the owner's off switch for their machines is
// pipeline_fleet_disabled, which tells a person what to turn back on.
func TestFleetKillSwitchIsTyped(t *testing.T) {
	// The off switch is the engine's own gate refusing, flagged as the real
	// dispatcher flags it -- so its own code has to be read first.
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		return worker.Result{OK: false, ErrorCode: "kill_switch_engaged", ErrorMessage: "computer use is currently disabled by the user",
			RefusedBeforeStart: true, RefusedByGate: true}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	wantFailure(t, runFleet(t, f, fleetReq()), pl.OutcomeRefused, pl.CodeFleetDisabled)
}

// TestFleetStepTheEnginesGateRefusalIsAnExecutorError (ruling R33b): a
// refusal by THIS engine's own gate before any routing is a decision about
// the request -- its origin, its shape, a read the gate could not make -- not
// a fact about the owner's machines, so it is pipeline_executor_error
// carrying the gate's code and sentence, never "no machine offers this need".
// Read off the dispatcher's RefusedByGate, not a list of the gate's codes.
func TestFleetStepTheEnginesGateRefusalIsAnExecutorError(t *testing.T) {
	for _, code := range []string{
		"denied_pipeline_purpose", "unknown_purpose", "bad_request", "preferences_lookup_failed",
		"authorization_lookup_failed", "unknown_action", "a_gate_code_this_engine_has_never_seen",
	} {
		t.Run(code, func(t *testing.T) {
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				return worker.Result{OK: false, ErrorCode: code, ErrorMessage: "the gate's own sentence",
					RefusedBeforeStart: true, RefusedByGate: true}, nil
			}}
			f, lib, _, _ := newTestFleet(t, d)
			res := runFleet(t, f, fleetReq())
			wantFailure(t, res, pl.OutcomeRefused, pl.CodeExecutorError)
			if !strings.Contains(res.Failure.Message, code) || !strings.Contains(res.Failure.Message, "the gate's own sentence") {
				t.Errorf("message %q does not carry the gate's code and sentence", res.Failure.Message)
			}
			if len(lib.files) != 0 {
				t.Errorf("a step the gate refused stored %d Library file(s)", len(lib.files))
			}
		})
	}

	t.Run("the same code from a sibling replica is a routing refusal", func(t *testing.T) {
		// Rule 0 re-decided on the replica holding the machine -- the
		// version-skew case of a rolling deploy -- is not this engine's gate:
		// no machine took the step.
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{OK: false, ErrorCode: "denied_pipeline_purpose", ErrorMessage: "unknown purpose there",
				RefusedBeforeStart: true, WorkerId: "reg-9", NodeId: "agent-b"}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		wantFailure(t, runFleet(t, f, fleetReq()), pl.OutcomeRefused, pl.CodeNoMachineForNeed)
	})
}

// TestFleetRecordsTheMachine: the request carries no machine (design D4), so
// where the step ran comes back on the result -- the registration, the
// replica holding its stream, the labels it was matched on.
func TestFleetRecordsTheMachine(t *testing.T) {
	labels := map[string]string{"docker": "true", "gpu": "true", "pipelines": "allowed", "os": "linux"}
	for _, exit := range []int{0, 3} {
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{OK: true, OutputJSON: fmt.Sprintf(`{"exitCode":%d,"durationMs":4000}`, exit),
				WorkerId: "reg-7", NodeId: "agent-b", Labels: labels}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		res := runFleet(t, f, fleetReq())
		want := pl.Where{Surface: "fleet", WorkerID: "reg-7", NodeID: "agent-b", MachineLabels: labels}
		if res.Where.Surface != want.Surface || res.Where.WorkerID != want.WorkerID || res.Where.NodeID != want.NodeID ||
			!maps.Equal(res.Where.MachineLabels, labels) {
			t.Errorf("exit %d: Where = %+v, want %+v", exit, res.Where, want)
		}
		if res.ExitCode != exit {
			t.Errorf("ExitCode = %d, want the command's %d", res.ExitCode, exit)
		}
		wantStatus := pl.OutcomeSucceeded
		if exit != 0 {
			wantStatus = pl.OutcomeFailed
		}
		if res.Status != wantStatus || (exit != 0 && res.Failure != nil) {
			t.Errorf("exit %d: status %s failure %+v; a non-zero exit fails with no code, the exit code is the answer",
				exit, res.Status, res.Failure)
		}
		if res.StartedAt == "" || res.FinishedAt == "" {
			t.Errorf("exit %d: no start or finish time", exit)
		}
	}
}

// chunk is one stream chunk as the dispatcher delivers it.
func chunk(stream string, text string) *nodev1.WorkerForwardStream {
	switch stream {
	case "stderr":
		return &nodev1.WorkerForwardStream{RequestId: "r", Payload: &nodev1.WorkerForwardStream_StderrChunk{StderrChunk: []byte(text)}}
	case "data":
		return &nodev1.WorkerForwardStream{RequestId: "r", Payload: &nodev1.WorkerForwardStream_DataChunk{DataChunk: []byte(text)}}
	}
	return &nodev1.WorkerForwardStream{RequestId: "r", Payload: &nodev1.WorkerForwardStream_StdoutChunk{StdoutChunk: []byte(text)}}
}

// TestFleetStepCapturesTheMachinesOutput: the machine's stream is the step's
// log. Chunks are cut where the machine pleased, so lines are assembled
// whole, each stream on its own; they carry no kubelet timestamp, so a line
// beginning with one keeps it; every line is masked, stored under the run,
// archived to the Library and tailed -- and a chunk that arrives after the
// dispatch returned is dropped rather than racing the archive.
func TestFleetStepCapturesTheMachinesOutput(t *testing.T) {
	var late func()
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		for _, c := range []*nodev1.WorkerForwardStream{
			chunk("stdout", "line one\nline t"),
			chunk("stderr", "warn: x"),
			chunk("stdout", "wo\n2026-01-01T00:00:00Z a line that starts like a stamp\n"),
			chunk("data", "not output\n"),
			chunk("stderr", "yz\nthe secret is "+plantedNPM+"\n"),
			chunk("stdout", "no newline at the end"),
		} {
			req.OnStreamChunk(c)
		}
		late = func() { req.OnStreamChunk(chunk("stdout", "after the result\n")) }
		return worker.Result{OK: true, OutputJSON: `{"exitCode":1,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
	}}
	f, lib, _, sink := newTestFleet(t, d)
	req := fleetReq()
	req.Step.Artifacts = nil
	res := runFleet(t, f, req)
	late()

	want := []string{"line one", "line two", "2026-01-01T00:00:00Z a line that starts like a stamp",
		"warn: xyz", "the secret is ***", "no newline at the end"}
	log, ok := lib.named(logFileName(req.StepKey))
	if !ok {
		t.Fatalf("no log archive among %v", lib.files)
	}
	archive := string(log.Bytes)
	var output []string
	for _, line := range strings.Split(strings.TrimSuffix(archive, "\n"), "\n") {
		if !strings.HasPrefix(line, "memql: ") {
			output = append(output, line)
		}
	}
	if strings.Join(output, "|") != strings.Join(want, "|") {
		t.Errorf("archived output = %q\nwant %q", output, want)
	}
	if !strings.Contains(archive, "reg-1") {
		t.Errorf("the archive does not say which machine ran the step:\n%s", archive)
	}
	if strings.Contains(archive, plantedNPM) || strings.Contains(res.LogTail, plantedNPM) {
		t.Fatal("a secret the step echoed reached the archive or the tail unmasked")
	}
	if strings.Contains(archive, "after the result") || strings.Contains(archive, "not output") {
		t.Errorf("a late or data chunk reached the archive:\n%s", archive)
	}
	if log.OwnerUserID != req.OwnerUserID || log.WorkRunID != req.WorkRunID || log.StepKey != req.StepKey ||
		log.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("log file = %+v, want the owner's, bound to the work run and step, as text", log)
	}
	if res.LogFileID == "" || res.LogLines != len(want) || res.ExitCode != 1 || res.Status != pl.OutcomeFailed {
		t.Errorf("result = %+v, want the log's id, %d lines, the command's exit 1", res, len(want))
	}
	if !strings.HasSuffix(res.LogTail, "no newline at the end") {
		t.Errorf("tail = %q, want the step's last words", res.LogTail)
	}
	stored := sink.messages()
	if strings.Join(stored, "|") != strings.Join(want, "|") {
		t.Errorf("store = %q, want %q", stored, want)
	}
	for _, l := range sink.all() {
		if l.Subject != req.RunID || l.SubjectConcept != "v1:pipelines:run" {
			t.Errorf("store line bound to %s %s, want the pipelines run", l.SubjectConcept, l.Subject)
		}
	}
}

// TestFleetStepStoresItsArtifactsAndNotes: the machine packs the declared
// paths; they land in the owner's Library under the same limits as the
// cluster's, and what did not arrive is a note beside the step, never a
// failure of a step that passed.
func TestFleetStepStoresItsArtifactsAndNotes(t *testing.T) {
	tgz := extractTestTgz(t, []extractTestEntry{
		{name: "coverage.out", body: "mode: set\n"},
		{name: "dist/report.json", body: `{"passed":3}`},
	})
	output := func(extra string) string {
		return `{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"` + base64.StdEncoding.EncodeToString(tgz) +
			`","artifactsMissing":["missing.txt"]` + extra + `}`
	}
	answer := func(out string) func(context.Context, worker.Request) (worker.Result, error) {
		return func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{OK: true, OutputJSON: out, WorkerId: "reg-1", NodeId: "agent-b"}, nil
		}
	}

	t.Run("the declared files", func(t *testing.T) {
		d := &fakeDispatcher{answer: answer(output(""))}
		f, lib, _, _ := newTestFleet(t, d)
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeSucceeded || res.Failure != nil {
			t.Fatalf("result = %+v, want a success", res)
		}
		cov, ok1 := lib.named("coverage.out")
		rep, ok2 := lib.named("dist__report.json")
		if !ok1 || !ok2 || string(cov.Bytes) != "mode: set\n" || rep.MimeType != "application/json" {
			t.Fatalf("Library = %+v, want coverage.out and dist__report.json (application/json)", lib.files)
		}
		if cov.OwnerUserID != "user-5d1e" || cov.WorkRunID != "work-91c2" || cov.StepKey != "tests.go-tests#2" {
			t.Errorf("artifact = %+v, want it the owner's and bound to the run and step", cov)
		}
		if len(res.ArtifactFileIDs) != 2 {
			t.Errorf("ArtifactFileIDs = %v, want two", res.ArtifactFileIDs)
		}
		if len(res.Notes) != 1 || res.Notes[0].Code != pl.CodeArtifactMissing || !strings.Contains(res.Notes[0].Message, "missing.txt") {
			t.Errorf("notes = %+v, want one %s naming missing.txt", res.Notes, pl.CodeArtifactMissing)
		}
	})

	t.Run("too large on the machine fails a passing step", func(t *testing.T) {
		d := &fakeDispatcher{answer: answer(`{"exitCode":0,"durationMs":10,"artifactsTooLarge":true,"artifactsMissing":[]}`)}
		f, _, _, _ := newTestFleet(t, d)
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeFailed || res.Failure == nil || res.Failure.Code != pl.CodeArtifactTooLarge || res.ExitCode != 0 {
			t.Fatalf("result = %+v (failure %+v), want failed %s with the command's own exit 0",
				res, res.Failure, pl.CodeArtifactTooLarge)
		}
	})

	t.Run("too large under this node's cap", func(t *testing.T) {
		d := &fakeDispatcher{answer: answer(output(""))}
		f, lib, _, _ := newTestFleet(t, d)
		f.cfg.ArtifactMaxBytes = 4
		res := runFleet(t, f, fleetReq())
		if res.Failure == nil || res.Failure.Code != pl.CodeArtifactTooLarge {
			t.Fatalf("result = %+v, want %s", res, pl.CodeArtifactTooLarge)
		}
		if _, ok := lib.named("coverage.out"); ok {
			t.Error("an archive past the cap was stored anyway")
		}
	})

	t.Run("what the Library would not keep is a note", func(t *testing.T) {
		d := &fakeDispatcher{answer: answer(output(""))}
		f, lib, _, _ := newTestFleet(t, d)
		lib.omit = map[string]string{"coverage.out": "the owner's Library is over its quota"}
		lib.fail = map[string]error{logFileName("tests.go-tests#2"): errors.New("blob storage timed out")}
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeSucceeded || res.LogFileID != "" {
			t.Fatalf("result = %+v, want a success with no log file", res)
		}
		var said []string
		for _, n := range res.Notes {
			if n.Code != pl.CodeArtifactMissing {
				t.Errorf("note %+v: a file the Library did not keep is %s", n, pl.CodeArtifactMissing)
			}
			said = append(said, n.Message)
		}
		all := strings.Join(said, " | ")
		for _, want := range []string{"over its quota", "blob storage timed out", "log"} {
			if !strings.Contains(all, want) {
				t.Errorf("notes %q do not say %q", all, want)
			}
		}
	})

	t.Run("an archive that is not an archive is a note", func(t *testing.T) {
		d := &fakeDispatcher{answer: answer(`{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"bm90IGEgdGFyLmd6","artifactsMissing":[]}`)}
		f, lib, _, _ := newTestFleet(t, d)
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeSucceeded || len(res.Notes) == 0 || res.Notes[0].Code != pl.CodeArtifactMissing {
			t.Fatalf("result = %+v, want a success with a %s note", res, pl.CodeArtifactMissing)
		}
		if len(lib.files) != 1 {
			t.Errorf("Library = %d files, want only the log", len(lib.files))
		}
	})

	// No archive: which declared paths matched nothing is the machine's word
	// when it gives one (artifactsMissing), and every declared path only when
	// it says nothing.
	for _, c := range []struct {
		name, out string
		missing   []string
	}{
		{"no archive: the paths the machine says matched nothing",
			`{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"","artifactsMissing":["missing.txt"]}`, []string{"missing.txt"}},
		{"no archive from a failed command that packed nothing and names nothing",
			`{"exitCode":2,"durationMs":10,"artifactsTgzBase64":"","artifactsMissing":[]}`, nil},
		{"no archive and no word from the machine: every declared path",
			`{"exitCode":0,"durationMs":10,"artifactsTgzBase64":""}`, fleetReq().Step.Artifacts},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDispatcher{answer: answer(c.out)}
			f, lib, _, _ := newTestFleet(t, d)
			res := runFleet(t, f, fleetReq())
			if len(lib.files) != 1 || res.Failure != nil {
				t.Fatalf("result = %+v with %d Library files, want no failure and only the log", res, len(lib.files))
			}
			if len(res.Notes) != len(c.missing) {
				t.Fatalf("notes = %+v, want one for each of %q", res.Notes, c.missing)
			}
			for i, p := range c.missing {
				if n := res.Notes[i]; n.Code != pl.CodeArtifactMissing || !strings.Contains(n.Message, strconv.Quote(p)) {
					t.Errorf("note %d = %+v, want the %s note naming %q", i, n, pl.CodeArtifactMissing, p)
				}
			}
		})
	}

	t.Run("a command that failed keeps its exit code beside the failure", func(t *testing.T) {
		// As on the cluster: the artifacts' failure-class code is the step's
		// failure, and the command's exit status stays as it chose.
		d := &fakeDispatcher{answer: answer(`{"exitCode":2,"durationMs":10,"artifactsTooLarge":true,"artifactsMissing":[]}`)}
		f, _, _, _ := newTestFleet(t, d)
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeFailed || res.ExitCode != 2 || res.Failure == nil || res.Failure.Code != pl.CodeArtifactTooLarge {
			t.Fatalf("result = %+v (failure %+v), want failed %s with the command's exit 2", res, res.Failure, pl.CodeArtifactTooLarge)
		}
	})

	t.Run("an archive past the cap is refused before it is decoded", func(t *testing.T) {
		// Base64 of zeros: no gzip at all, so only the size check can say it is
		// too large -- decoded, it would read as an archive that is not one.
		d := &fakeDispatcher{answer: answer(`{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"` +
			strings.Repeat("A", 90000) + `","artifactsMissing":[]}`)}
		f, _, _, _ := newTestFleet(t, d)
		f.cfg.ArtifactMaxBytes = 4
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeFailed || res.Failure == nil || res.Failure.Code != pl.CodeArtifactTooLarge {
			t.Fatalf("result = %+v (failure %+v), want failed %s", res, res.Failure, pl.CodeArtifactTooLarge)
		}
	})

	t.Run("an agent node with no Library notes each file it could not store", func(t *testing.T) {
		d := &fakeDispatcher{answer: answer(output(""))}
		f, _, _, _ := newTestFleet(t, d)
		f.library = nil
		res := runFleet(t, f, fleetReq())
		if res.Status != pl.OutcomeSucceeded || res.LogFileID != "" || len(res.ArtifactFileIDs) != 0 {
			t.Fatalf("result = %+v, want a success with no files", res)
		}
		var said []string
		for _, n := range res.Notes {
			said = append(said, n.Message)
		}
		all := strings.Join(said, " | ")
		for _, want := range []string{"the step's log was not stored: this agent node has no Library to store it in",
			`the artifact "coverage.out" was not stored: this agent node has no Library to store it in`} {
			if !strings.Contains(all, want) {
				t.Errorf("notes %q do not say %q", all, want)
			}
		}
	})

	t.Run("a log past the store's cap says the Library has all of it", func(t *testing.T) {
		d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
			req.OnStreamChunk(chunk("stdout", "one\ntwo\nthree\n"))
			return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
		}}
		f, lib, _, _ := newTestFleet(t, d)
		f.cfg.LogStoreMaxLines = 2
		req := fleetReq()
		req.Step.Artifacts = nil
		res := runFleet(t, f, req)
		if !res.LogCapped || len(res.Notes) != 1 || res.Notes[0].Code != pl.CodeLogCapped ||
			res.Notes[0].Message != "the live log of this step stops at 2 lines; the complete log is archived to the Library" {
			t.Fatalf("result = %+v, want the log capped and one note saying the Library has the whole log", res)
		}
		if log, ok := lib.named(logFileName(req.StepKey)); !ok || !strings.Contains(string(log.Bytes), "three") {
			t.Errorf("the archived log lacks the line past the store's cap")
		}
	})

	t.Run("the outcome is bounded as the cluster's is", func(t *testing.T) {
		const files = 400
		var entries []extractTestEntry
		for i := 0; i < files; i++ {
			entries = append(entries, extractTestEntry{name: fmt.Sprintf("dist/f%04d.txt", i), body: "x"})
		}
		many := base64.StdEncoding.EncodeToString(extractTestTgz(t, entries))
		d := &fakeDispatcher{answer: answer(`{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"` + many + `","artifactsMissing":[]}`)}
		f, _, _, _ := newTestFleet(t, d)
		f.library = &rtLibrary{idPad: "-" + strings.Repeat("i", 800)}
		req := fleetReq()
		req.Step.Artifacts = []string{"dist/*"}
		res := runFleet(t, f, req)
		if kept := len(res.ArtifactFileIDs); kept == 0 || kept == files {
			t.Fatalf("%d of %d artifact file ids kept, want as many as fit", kept, files)
		}
		if n := outcomeBytes(res); n > outcomeMaxBytes {
			t.Errorf("the outcome is %d bytes, over outcomeMaxBytes (%d)", n, outcomeMaxBytes)
		}
		if len(res.Notes) == 0 || res.Notes[0].Code != pl.CodeOutcomeTrimmed || !strings.Contains(res.Notes[0].Message, "artifact file ids") {
			t.Errorf("notes = %+v, want the first to say how many artifact file ids were left out", res.Notes)
		}
	})
}

// TestFleetStepMasksTheMachinesWordsForEveryFormOfASecret: what a machine says
// about a step -- the error it answered with -- is masked as the capture masks
// a line of the step's log, its repair included (a NUL the capture drops
// cannot keep a secret's bytes apart), and for every form of a secret the seam
// masks besides (a stored value's trimmed form).
func TestFleetStepMasksTheMachinesWordsForEveryFormOfASecret(t *testing.T) {
	padded := "  padded-" + strings.Repeat("p", 12) + "  "
	for _, c := range []struct{ name, said, secret string }{
		{"a secret a NUL splits", "git said npm-zz\x00" + strings.Repeat("z", 10) + " was rejected", plantedNPM},
		{"a stored value's trimmed form", "git said " + strings.TrimSpace(padded) + " was rejected", strings.TrimSpace(padded)},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				return worker.Result{OK: false, ErrorCode: "exec_failed", ErrorMessage: c.said, WorkerId: "reg-1", NodeId: "agent-b"}, nil
			}}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			req.Secrets["PADDED_TOKEN"] = padded
			res := runFleet(t, f, req)
			wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutorError)
			if strings.Contains(strings.ReplaceAll(res.Failure.Message, "\x00", ""), c.secret) || !strings.Contains(res.Failure.Message, "***") {
				t.Errorf("failure %q, want the machine's words with the secret %q masked", res.Failure.Message, c.secret)
			}
		})
	}
}

// TestFleetStepMasksASecretInAnArtifactEntryName: an artifact entry's name is
// the step's to choose, and the note beside the step quotes a refused one --
// a note that rides the step's result to the run's rows and its check run.
// The fleet files a machine's artifacts through the runner's own helpers
// (settle.go), so the note is masked as the cluster's is: the name masked
// before it is cut, and the note masked whole once it is written, quoting and
// all.
func TestFleetStepMasksASecretInAnArtifactEntryName(t *testing.T) {
	// A secret holding what quoting writes: a name carrying its unescaped
	// form is spelled as the secret once the note quotes it.
	quoted := `quo\"ted-` + strings.Repeat("q", 12)
	for _, c := range []struct {
		name   string
		entry  string
		secret string // what must appear in no note
		masked string // what the note says instead
	}{
		{"a secret in a refused entry's name", "secrets/" + plantedNPM + ".txt", plantedNPM, "secrets/***.txt"},
		// The secret straddles the cut at 256 bytes: cut first, and the six
		// bytes of it before the cut survive where no masker can know them.
		{"a secret straddling the cut of a long name", "dist/" + strings.Repeat("n", 245) + plantedNPM + strings.Repeat("n", 20000),
			plantedNPM[:6], "nnn***nnn"},
		{"a secret the note's quoting spells out", `secrets/quo"ted-` + strings.Repeat("q", 12), quoted, `"secrets/***"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			tgz := extractTestTgz(t, []extractTestEntry{
				{name: "coverage.out", body: "mode: set\n"},
				{name: c.entry, body: "x"},
			})
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				return worker.Result{OK: true, WorkerId: "reg-1", NodeId: "agent-b", OutputJSON: `{"exitCode":0,"durationMs":10,` +
					`"artifactsTgzBase64":"` + base64.StdEncoding.EncodeToString(tgz) + `","artifactsMissing":[]}`}, nil
			}}
			f, lib, _, _ := newTestFleet(t, d)
			req := fleetReq()
			req.Step.Artifacts = []string{"coverage.out"}
			req.Secrets["QUOTED_TOKEN"] = quoted

			res := runFleet(t, f, req)

			if res.Status != pl.OutcomeSucceeded || res.Failure != nil {
				t.Fatalf("result = %+v (failure %+v), want a success: a refused entry is a note", res, res.Failure)
			}
			if _, ok := lib.named("coverage.out"); !ok {
				t.Errorf("the declared artifact beside the refused entry was not stored: %+v", lib.files)
			}
			var said []string
			for _, n := range res.Notes {
				said = append(said, n.Message)
			}
			all := strings.Join(said, " | ")
			if strings.Contains(all, c.secret) {
				t.Errorf("notes %.600q... carry the secret %q", all, c.secret)
			}
			if !strings.Contains(all, c.masked) {
				t.Errorf("notes %.600q... do not quote the refused entry as %q", all, c.masked)
			}
		})
	}
}

// TestFleetStepFailuresAreTyped: how a machine-side ending reads.
func TestFleetStepFailuresAreTyped(t *testing.T) {
	for _, c := range []struct {
		code   string
		status pl.Outcome
		want   string
	}{
		{"timeout", pl.OutcomeFailed, pl.CodeRunCeiling}, // the StepRun's own deadline code
		{"worker_disconnected", pl.OutcomeFailed, pl.CodeNodeLost},
		{"pipeline_clone_failed", pl.OutcomeFailed, pl.CodeCloneFailed},
		{"cancelled", pl.OutcomeCancelled, pl.CodeStepCancelled},
		{"exec_failed", pl.OutcomeFailed, pl.CodeExecutorError},
		{"bad_request", pl.OutcomeFailed, pl.CodeExecutorError},
		{"preferences_lookup_failed", pl.OutcomeFailed, pl.CodeExecutorError},
		{"something_new", pl.OutcomeFailed, pl.CodeExecutorError},
	} {
		t.Run(c.code, func(t *testing.T) {
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				return worker.Result{OK: false, ErrorCode: c.code, ErrorMessage: "git said " + fleetToken + " was rejected",
					WorkerId: "reg-1", NodeId: "agent-b"}, nil
			}}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			res, err := f.RunStep(context.Background(), req, stepRunFor(req, 600, pl.CodeRunCeiling))
			if err != nil {
				t.Fatalf("RunStep: %v", err)
			}
			wantFailure(t, res, c.status, c.want)
			if strings.Contains(res.Failure.Message, fleetToken) {
				t.Error("the machine's message carried the clone token into the result unmasked")
			}
		})
	}

	t.Run("a step cancelled under it is cancelled, whatever the dispatcher saw", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			cancel()
			return worker.Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: "context canceled"}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		req := fleetReq()
		res, err := f.RunStep(ctx, req, fleetRun(req))
		if err != nil {
			t.Fatalf("RunStep: %v", err)
		}
		wantFailure(t, res, pl.OutcomeCancelled, pl.CodeStepCancelled)
	})

	t.Run("an output that is not the contract", func(t *testing.T) {
		d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
			return worker.Result{OK: true, OutputJSON: `{"durationMs":10}`}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		wantFailure(t, runFleet(t, f, fleetReq()), pl.OutcomeFailed, pl.CodeExecutorError)
	})
}

// TestFleetStepCutByItsMachinesCapSaysSo (final review, M7): a machine runs a
// pipeline step for no longer than its own policy allows
// (pipelines.max_timeout_sec, 3600 seconds unless its policy.yaml says
// otherwise), whatever the step asks for, and when it stops one it says the
// timeout it ran it under: "the step ran past its 1h0m0s timeout"
// (memql-cockpit, internal/worker/tools/pipeline_step.go). When that is less
// than the step's own deadline, the machine's cap is what stopped the step
// and what to raise: the failure names it, and is the step's timeout -- the
// run's ceiling did not end it.
func TestFleetStepCutByItsMachinesCapSaysSo(t *testing.T) {
	const capped = "pipeline_step: the step ran past its 1h0m0s timeout and was stopped"
	for _, c := range []struct {
		name     string
		words    string // what the machine said
		deadline int    // the step's effective timeout, in seconds
		code     string // the bound the StepRun names
		wantCode string
		says     []string
		saysNot  []string
	}{
		{"the machine's cap was less than the step's deadline", capped, 7200, pl.CodeStepTimeout, pl.CodeStepTimeout,
			[]string{"1h0m0s", "max_timeout_sec", "reg-1", "2h0m0s"}, []string{"within its 2h0m0s deadline"}},
		{"so it is the step's timeout, though the run's ceiling bound its deadline", capped, 5400, pl.CodeRunCeiling, pl.CodeStepTimeout,
			[]string{"1h0m0s", "max_timeout_sec", "1h30m0s"}, nil},
		// The reachable positives: the machine's words are read, and only a
		// cap below the step's own deadline changes the sentence.
		{"the machine stopped it at the step's own deadline", "pipeline_step: the step ran past its 2h0m0s timeout and was stopped", 7200, pl.CodeRunCeiling, pl.CodeRunCeiling,
			[]string{"within its 2h0m0s deadline on reg-1"}, []string{"max_timeout_sec"}},
		{"the dispatcher timed the call out itself", "worker call timed out", 7200, pl.CodeStepTimeout, pl.CodeStepTimeout,
			[]string{"within its 2h0m0s deadline on reg-1"}, []string{"max_timeout_sec"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
				return worker.Result{OK: false, ErrorCode: "timeout", ErrorMessage: c.words, WorkerId: "reg-1", NodeId: "agent-b"}, nil
			}}
			f, _, _, _ := newTestFleet(t, d)
			req := fleetReq()
			res, err := f.RunStep(context.Background(), req, stepRunFor(req, c.deadline, c.code))
			if err != nil {
				t.Fatalf("RunStep: %v", err)
			}
			wantFailure(t, res, pl.OutcomeFailed, c.wantCode)
			for _, want := range c.says {
				if !strings.Contains(res.Failure.Message, want) {
					t.Errorf("failure %q, want it to say %q", res.Failure.Message, want)
				}
			}
			for _, not := range c.saysNot {
				if strings.Contains(res.Failure.Message, not) {
					t.Errorf("failure %q says %q", res.Failure.Message, not)
				}
			}
		})
	}
}

// driverStepGrace is component/pipelinerun/driver.go's stepGrace: how long
// past a step's run's ceiling the pipeline driver waits for Execute before it
// stops waiting (ruling R31b). The driver does not export it, so it is pinned
// here -- change the two together -- and
// TestAFleetStepsGraceStaysInsideTheDrivers reads driver.go to check the pin
// still says what the driver does.
const driverStepGrace = 10 * time.Minute

// TestAFleetStepsGraceStaysInsideTheDrivers: a fleet step can take its
// effective timeout and fleetGrace past it -- the machine's clone and packing,
// then the Library window, which no deadline of the step's ends -- while the
// driver stops waiting driverStepGrace past the step's run's ceiling, which
// the effective timeout, counted from the hand-over, never passes. A step
// still filing its files must never read as one that never reported.
func TestAFleetStepsGraceStaysInsideTheDrivers(t *testing.T) {
	if fleetGrace >= driverStepGrace {
		t.Errorf("fleetGrace = %v (clone and packing %v + Library window %v), want it inside the driver's stepGrace %v",
			fleetGrace, fleetCloneSlack, libraryPhaseTimeout, driverStepGrace)
	}
	// The fleet as built files within the grace it names.
	if f := NewFleet(exConfig(), &fakeDispatcher{}, nil, nil, nil, quietLogger()); fleetCloneSlack+f.libraryTimeout > fleetGrace {
		t.Errorf("a fleet's Library window is %v, past the %v fleetGrace leaves it after the clone and packing", f.libraryTimeout, fleetGrace-fleetCloneSlack)
	}
	// The pin is the driver's.
	src, err := os.ReadFile(filepath.Join("..", "..", "component", "pipelinerun", "driver.go"))
	if err != nil {
		t.Fatalf("reading the driver to check the pinned stepGrace: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*stepGrace\s*=\s*(\d+)\s*\*\s*time\.Minute\s*$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("component/pipelinerun/driver.go no longer declares stepGrace as N * time.Minute: re-pin driverStepGrace by hand")
	}
	if minutes, _ := strconv.Atoi(string(m[1])); time.Duration(minutes)*time.Minute != driverStepGrace {
		t.Errorf("the driver's stepGrace is %d minutes, the pin says %v: change driverStepGrace with it", minutes, driverStepGrace)
	}
}

// TestFleetStepFilesWhatACancelledStepCaptured: a run's cancel ends the
// machine's call and the step is cancelled, but what it captured is still
// filed -- its log, and any artifacts the machine returned -- as the cluster's
// runner files a cancelled step's log. The Library is written in a window of
// its own, which no cancel ends and its own bound does: a Library that does
// not answer costs the step its files, never more than the window.
func TestFleetStepFilesWhatACancelledStepCaptured(t *testing.T) {
	// fleetCancelled runs a step whose run is cancelled while the machine
	// runs it: answer streams its output, and the run's cancel lands.
	fleetCancelled := func(t *testing.T, f *Fleet, req pl.StepRequest, ctx context.Context) pl.StepResult {
		t.Helper()
		res, err := f.RunStep(ctx, req, fleetRun(req))
		if err != nil {
			t.Fatalf("RunStep: %v", err)
		}
		if res.Status != pl.OutcomeCancelled || res.Failure == nil || res.Failure.Code != pl.CodeStepCancelled ||
			res.Failure.Message != runCancelledMessage {
			t.Fatalf("result = %+v (failure %+v), want the run's cancel", res, res.Failure)
		}
		return res
	}

	t.Run("cancelled mid-stream, its log is stored", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		d := &fakeDispatcher{answer: func(callCtx context.Context, req worker.Request) (worker.Result, error) {
			req.OnStreamChunk(chunk("stdout", "compiling\nlinking\n"))
			cancel(errRunCancelled)
			<-callCtx.Done()
			// As the real dispatcher answers a call whose context ended.
			return worker.Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: "context canceled",
				WorkerId: "reg-1", NodeId: "agent-b"}, nil
		}}
		f, lib, _, _ := newTestFleet(t, d)
		req := fleetReq()
		req.Step.Artifacts = nil

		res := fleetCancelled(t, f, req, ctx)

		log, ok := lib.named(logFileName(req.StepKey))
		if !ok || res.LogFileID == "" || !strings.Contains(string(log.Bytes), "compiling\nlinking\n") {
			t.Fatalf("Library = %+v, outcome log file %q (notes %+v); want the cancelled step's log stored with what it printed",
				lib.files, res.LogFileID, res.Notes)
		}
		if res.LogLines != 2 || len(res.Notes) != 0 {
			t.Errorf("LogLines = %d, notes = %+v; want the two lines and nothing left unstored", res.LogLines, res.Notes)
		}
	})

	t.Run("cancelled as the machine answered, its artifacts are stored too", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		tgz := extractTestTgz(t, []extractTestEntry{{name: "coverage.out", body: "mode: set\n"}})
		d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
			req.OnStreamChunk(chunk("stdout", "PASS\n"))
			cancel(errRunCancelled) // lands as the machine sends its answer
			return worker.Result{OK: true, WorkerId: "reg-1", NodeId: "agent-b", OutputJSON: `{"exitCode":0,"durationMs":10,` +
				`"artifactsTgzBase64":"` + base64.StdEncoding.EncodeToString(tgz) + `","artifactsMissing":[]}`}, nil
		}}
		f, lib, _, _ := newTestFleet(t, d)
		req := fleetReq()
		req.Step.Artifacts = []string{"coverage.out"}

		res := fleetCancelled(t, f, req, ctx)

		if _, ok := lib.named(logFileName(req.StepKey)); !ok || res.LogFileID == "" {
			t.Errorf("Library = %+v, want the cancelled step's log", lib.files)
		}
		if cov, ok := lib.named("coverage.out"); !ok || string(cov.Bytes) != "mode: set\n" || len(res.ArtifactFileIDs) != 1 {
			t.Errorf("Library = %+v, artifact ids %v; want the artifact the machine returned", lib.files, res.ArtifactFileIDs)
		}
	})

	// The run's cancel is a cancelled step's answer (Executor.Cancel's
	// contract), so artifacts too large to keep are a note beside it, never a
	// failure that turns it into a failed step -- and a note carries a
	// note-class code (pl.StepResult.Notes): artifacts missing, too large to
	// keep, never the failure's own code.
	for _, c := range []struct{ name, out string }{
		{"cancelled as the machine answered its artifacts too large, it stays cancelled",
			`{"exitCode":0,"durationMs":10,"artifactsTooLarge":true}`},
		{"cancelled as the machine answered an archive past the cap, it stays cancelled",
			`{"exitCode":0,"durationMs":10,"artifactsTgzBase64":"` + strings.Repeat("A", 90000) + `"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
				req.OnStreamChunk(chunk("stdout", "PASS\n"))
				cancel(errRunCancelled) // lands as the machine sends its answer
				return worker.Result{OK: true, WorkerId: "reg-1", NodeId: "agent-b", OutputJSON: c.out}, nil
			}}
			f, lib, _, _ := newTestFleet(t, d)
			f.cfg.ArtifactMaxBytes = 4
			req := fleetReq()
			req.Step.Artifacts = []string{"coverage.out"}

			res := fleetCancelled(t, f, req, ctx)

			var said []string
			for _, n := range res.Notes {
				if class, _ := pl.ClassOf(n.Code); class != pl.ClassNote {
					t.Errorf("a note carries %s, a %s-class code: %+v", n.Code, class, n)
				}
				if n.Code == pl.CodeArtifactMissing && strings.Contains(n.Message, "too large to keep") {
					said = append(said, n.Message)
				}
			}
			if len(said) != 1 || res.ExitCode != -1 {
				t.Errorf("exit %d, notes %+v; want the cancel's -1 and one %s note saying the artifacts were too large to keep",
					res.ExitCode, res.Notes, pl.CodeArtifactMissing)
			}
			if _, ok := lib.named(logFileName(req.StepKey)); !ok || res.LogFileID == "" {
				t.Errorf("Library = %+v, want the cancelled step's log", lib.files)
			}
		})
	}

	t.Run("a Library that does not answer costs the step its files, within the window", func(t *testing.T) {
		d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
			req.OnStreamChunk(chunk("stdout", "ok\n"))
			return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1", NodeId: "agent-b"}, nil
		}}
		f, _, _, _ := newTestFleet(t, d)
		f.library = &rtLibrary{block: true}
		f.libraryTimeout = 50 * time.Millisecond
		req := fleetReq()
		req.Step.Artifacts = nil
		done := make(chan pl.StepResult, 1)
		go func() {
			res, _ := f.RunStep(context.Background(), req, fleetRun(req))
			done <- res
		}()
		select {
		case res := <-done:
			if res.Status != pl.OutcomeSucceeded || res.LogFileID != "" || len(res.Notes) != 1 ||
				!strings.Contains(res.Notes[0].Message, "deadline") {
				t.Fatalf("result = %+v, want a success with no log file and a note saying the Library ran out of time", res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("RunStep did not return: a Library that does not answer held the step past its window")
		}
	})
}

// TestFleetStepBoundsItsFailureAsTheClusterDoes: a machine's words, a sibling
// replica's code, the dispatcher's own error and an answer that is not the
// contract are as long as they like, and the failure sentence quoting them is
// cut where it is made, to failureMaxBytes, as the cluster's runner cuts one
// -- so the outcome stays within outcomeMaxBytes whatever was said. The words
// are runes of three bytes after a head of one, two or three bytes, so under
// one head or another a rune lies across failureMaxBytes, and the cut must
// leave it whole.
func TestFleetStepBoundsItsFailureAsTheClusterDoes(t *testing.T) {
	longWith := func(head int) string {
		return "boom" + strings.Repeat("!", head) + " " + strings.Repeat("€", 100<<10)
	}
	for _, c := range []struct {
		name   string
		answer func(long string) (worker.Result, error)
		status pl.Outcome
		code   string
		// runes says the sentence quotes the words, whose runes reach the
		// cut; an exit code is digits.
		runes bool
	}{
		{"the machine's error", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: "exec_failed", ErrorMessage: long}, nil
		}, pl.OutcomeFailed, pl.CodeExecutorError, true},
		{"a code nobody bounded", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: long, ErrorMessage: "no"}, nil
		}, pl.OutcomeFailed, pl.CodeExecutorError, true},
		{"the machine's own policy", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: "denied_by_policy", ErrorMessage: long}, nil
		}, pl.OutcomeFailed, pl.CodeNoMachineForNeed, true},
		{"a connection lost mid-run", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: "worker_disconnected", ErrorMessage: long}, nil
		}, pl.OutcomeFailed, pl.CodeNodeLost, true},
		{"a failed clone", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: pl.CodeCloneFailed, ErrorMessage: long}, nil
		}, pl.OutcomeFailed, pl.CodeCloneFailed, true},
		{"no machine could take it", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: "worker_busy", ErrorMessage: long, RefusedBeforeStart: true}, nil
		}, pl.OutcomeRefused, pl.CodeNoMachineForNeed, true},
		{"the engine's own gate", func(long string) (worker.Result, error) {
			return worker.Result{ErrorCode: "bad_request", ErrorMessage: long, RefusedBeforeStart: true, RefusedByGate: true}, nil
		}, pl.OutcomeRefused, pl.CodeExecutorError, true},
		{"the dispatcher could not take it", func(long string) (worker.Result, error) {
			return worker.Result{}, errors.New(long)
		}, pl.OutcomeFailed, pl.CodeRunnerUnavailable, true},
		// The JSON decoder's error quotes the number whole: 300 KiB of it.
		{"an exit code no int holds, in an answer otherwise the contract", func(string) (worker.Result, error) {
			return worker.Result{OK: true, OutputJSON: `{"exitCode":` + strings.Repeat("9", 300<<10) + `,"durationMs":10}`}, nil
		}, pl.OutcomeFailed, pl.CodeExecutorError, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			straddled, heads := false, 3
			if !c.runes {
				heads = 1
			}
			for head := 1; head <= heads; head++ {
				long := longWith(head)
				d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
					r, err := c.answer(long)
					r.WorkerId, r.NodeId = "reg-1", "agent-b"
					return r, err
				}}
				f, _, _, _ := newTestFleet(t, d)
				res := runFleet(t, f, fleetReq())
				wantFailure(t, res, c.status, c.code)
				msg := res.Failure.Message
				if n := len(msg); n > failureMaxBytes {
					t.Errorf("head %d: the failure sentence is %d bytes, over failureMaxBytes (%d)", head, n, failureMaxBytes)
				}
				if n := outcomeBytes(res); n > outcomeMaxBytes {
					t.Errorf("head %d: the outcome is %d bytes, over outcomeMaxBytes (%d)", head, n, outcomeMaxBytes)
				}
				if !utf8.ValidString(msg) {
					t.Errorf("head %d: the failure sentence was cut inside a rune", head)
				}
				// A rune left whole across the cut ends the sentence short
				// of failureMaxBytes.
				straddled = straddled || len(msg) < failureMaxBytes
			}
			if c.runes && !straddled {
				t.Error("no head put a rune across the cut: the check above never met a rune boundary")
			}
		})
	}

	t.Run("the token minter's error", func(t *testing.T) {
		straddled := false
		for head := 1; head <= 3; head++ {
			d := &fakeDispatcher{}
			f, _, tokens, _ := newTestFleet(t, d)
			tokens.err = errors.New(longWith(head))
			res := runFleet(t, f, fleetReq())
			wantFailure(t, res, pl.OutcomeFailed, pl.CodeCloneFailed)
			msg := res.Failure.Message
			if n := len(msg); n > failureMaxBytes || len(d.reqs) != 0 {
				t.Errorf("head %d: the failure sentence is %d bytes (failureMaxBytes %d), %d dispatch(es); want it bounded and nothing run",
					head, n, failureMaxBytes, len(d.reqs))
			}
			if !utf8.ValidString(msg) {
				t.Errorf("head %d: the failure sentence was cut inside a rune", head)
			}
			straddled = straddled || len(msg) < failureMaxBytes
		}
		if !straddled {
			t.Error("no head put a rune across the cut: the check above never met a rune boundary")
		}
	})
}

// TestFleetStepMasksItsFailureBeforeCuttingIt (the Task 12b re-review): a
// failure sentence quoting the machine is masked whole, and only then cut to
// failureMaxBytes. Cut first, a secret straddling the cut keeps its head,
// which no masker can know for the secret.
func TestFleetStepMasksItsFailureBeforeCuttingIt(t *testing.T) {
	const secret = "S3CRET-straddling-the-failure-cut"
	for _, c := range []struct {
		name   string
		answer func(said string) worker.Result
		status pl.Outcome
		code   string
	}{
		{"a failed step", func(said string) worker.Result {
			return worker.Result{ErrorCode: "exec_failed", ErrorMessage: said}
		}, pl.OutcomeFailed, pl.CodeExecutorError},
		{"a refused step", func(said string) worker.Result {
			return worker.Result{ErrorCode: "worker_busy", ErrorMessage: said, RefusedBeforeStart: true}
		}, pl.OutcomeRefused, pl.CodeNoMachineForNeed},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(said string) string {
				d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
					r := c.answer(said)
					r.WorkerId, r.NodeId = "reg-1", "agent-b"
					return r, nil
				}}
				f, _, _, _ := newTestFleet(t, d)
				req := fleetReq()
				req.Secrets["STRADDLING_TOKEN"] = secret
				res := runFleet(t, f, req)
				wantFailure(t, res, c.status, c.code)
				return res.Failure.Message
			}
			// Where the machine's words begin in the sentence, measured, so
			// the secret can begin six bytes before the cut.
			const probe = "probe"
			head := len(run(probe)) - len(probe)
			msg := run(strings.Repeat("x", failureMaxBytes-head-6) + secret + strings.Repeat("y", 64))

			for n := 1; n <= len(secret); n++ {
				if strings.HasSuffix(msg, secret[:n]) {
					t.Fatalf("the sentence ends %q, the head of the secret: it was cut before it was masked", secret[:n])
				}
			}
			if !strings.Contains(msg, "***") || len(msg) > failureMaxBytes {
				t.Errorf("the sentence (%d bytes) ends %q; want the secret masked in it, within failureMaxBytes (%d)",
					len(msg), msg[max(0, len(msg)-40):], failureMaxBytes)
			}
		})
	}
}

// TestFleetStepReadsGoTimings: a Go test step's passing packages' times come
// back for the driver's timing table, read from the archived output.
func TestFleetStepReadsGoTimings(t *testing.T) {
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		req.OnStreamChunk(chunk("stdout", "ok  \tgithub.com/acme/widget/a\t1.50s\nok  \tgithub.com/acme/widget/b\t(cached)\n"))
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	req := fleetReq()
	req.Step.Artifacts = nil
	res := runFleet(t, f, req)
	if len(res.Timings) != 1 || res.Timings["github.com/acme/widget/a"] != 1.5 {
		t.Fatalf("Timings = %v, want a: 1.5", res.Timings)
	}

	req.Step.Run, req.Step.Packages = "npm test", nil
	d2 := &fakeDispatcher{answer: d.answer}
	f2, _, _, _ := newTestFleet(t, d2)
	if res := runFleet(t, f2, req); len(res.Timings) != 0 {
		t.Errorf("Timings = %v for a step that is not a Go test step", res.Timings)
	}

	// A result line the reader cannot take is a note, as on the cluster path
	// (fix round 2, minor 3) -- never the step's failure, and masked like
	// everything the step printed.
	d3 := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		req.OnStreamChunk(chunk("stdout", "ok  \tgithub.com/acme/widget/"+plantedNPM+"\t"+strings.Repeat("9", 400)+"s\n"))
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`}, nil
	}}
	f3, _, _, _ := newTestFleet(t, d3)
	req3 := fleetReq()
	req3.Step.Artifacts = nil
	res = runFleet(t, f3, req3)
	var note *pl.Failure
	for i := range res.Notes {
		if res.Notes[i].Code == pl.CodeTimingsUnreadable {
			note = &res.Notes[i]
		}
	}
	if res.Status != pl.OutcomeSucceeded || note == nil || !strings.Contains(note.Message, "Go test timings") || strings.Contains(note.Message, plantedNPM) {
		t.Errorf("result %s with notes %+v, want a success with a masked %s note", res.Status, res.Notes, pl.CodeTimingsUnreadable)
	}
}

// gateStore is the slice of the dispatcher's store its gate and router read:
// the owner's machines (or the error reading them), no routing policy, and the
// kill switch.
type gateStore struct {
	mu          sync.Mutex
	machines    []worker.Candidate
	machinesErr error
	computerUse bool
	prefsErr    error
	invocations []workerservice.InvocationRow
}

func (s *gateStore) WorkersForOwner(context.Context, string) ([]worker.Candidate, error) {
	return s.machines, s.machinesErr
}
func (s *gateStore) RoutingPolicyForOwner(context.Context, string) (*worker.Policy, error) {
	return nil, nil
}
func (s *gateStore) TouchWorkerSelected(context.Context, string, string) error { return nil }
func (s *gateStore) UserPreferences(context.Context, string) (worker.Preferences, error) {
	return worker.Preferences{KillSwitchEngaged: !s.computerUse}, s.prefsErr
}
func (s *gateStore) AgentAuthorization(context.Context, string, string) (*worker.Authorization, error) {
	return nil, nil
}
func (s *gateStore) WriteInvocation(_ context.Context, row workerservice.InvocationRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invocations = append(s.invocations, row)
	return nil
}

// TestFleetStepPassesTheRealDispatchersPipelineGate: the request this path
// builds is one the REAL dispatcher's pipeline gate admits -- internal
// origin, the run and the owner named, the consent label required, the
// credentials in a shape it can mask -- so what comes back is the router's
// answer about the owner's machines, not denied_pipeline_purpose. A fake
// dispatcher cannot say that.
func TestFleetStepPassesTheRealDispatchersPipelineGate(t *testing.T) {
	builder := worker.Candidate{
		RegistrationId:  "reg-builder",
		Name:            "builder",
		Capabilities:    []string{workerservice.CapabilityHeadless},
		Labels:          map[string]string{"docker": "true", "gpu": "true", "pipelines": "allowed"},
		ConnectedNodeId: "agent-b",
	}
	for _, c := range []struct {
		name        string
		machines    []worker.Candidate
		machinesErr error
		computerUse bool
		prefsErr    error
		status      pl.Outcome
		code        string
		says        string // what the failure must carry, when anything
	}{
		{"an owner with no machines", nil, nil, true, nil, pl.OutcomeRefused, pl.CodeNoMachineForNeed, ""},
		{"a machine held by a sibling replica this node cannot reach", []worker.Candidate{builder}, nil, true, nil, pl.OutcomeRefused, pl.CodeNoMachineForNeed, ""},
		{"an owner whose computer use is off", []worker.Candidate{builder}, nil, false, nil, pl.OutcomeRefused, pl.CodeFleetDisabled, ""},
		// The real gate refusing for a reason of its own: an executor error.
		{"an owner whose computer-use setting cannot be read", []worker.Candidate{builder}, nil, true, errors.New("db down"), pl.OutcomeRefused, pl.CodeExecutorError, ""},
		// The router unable to read the owner's machines (ruling R33c): the
		// engine's own fault, not "no machine of yours offers this need" --
		// an executor error carrying the router's words.
		{"an owner whose machines cannot be read", []worker.Candidate{builder}, errors.New("the fleet read timed out"), true, nil, pl.OutcomeRefused, pl.CodeExecutorError, "the fleet read timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &gateStore{machines: c.machines, machinesErr: c.machinesErr, computerUse: c.computerUse, prefsErr: c.prefsErr}
			d, err := worker.NewDispatcher(worker.Options{
				Logger:     quietLogger(),
				Registry:   workerservice.NewRegistry(quietLogger(), nil),
				Store:      store,
				SelfNodeId: exAgent,
			})
			if err != nil {
				t.Fatalf("NewDispatcher: %v", err)
			}
			f, _, _, _ := newTestFleet(t, d)
			res := runFleet(t, f, fleetReq())
			wantFailure(t, res, c.status, c.code)
			if strings.Contains(res.Failure.Message, "denied_pipeline_purpose") {
				t.Fatalf("the real gate refused the request this path builds: %s", res.Failure.Message)
			}
			if !strings.Contains(res.Failure.Message, c.says) {
				t.Errorf("failure %q, want it to carry %q", res.Failure.Message, c.says)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			for _, row := range store.invocations {
				if row.AgentId != "" || row.RunId != "work-91c2" {
					t.Errorf("invocation agent %q run %q, want no agent and the work run", row.AgentId, row.RunId)
				}
				b, _ := json.Marshal(row.ArgsRedacted)
				if strings.Contains(string(b), fleetToken) || strings.Contains(string(b), plantedNPM) {
					t.Errorf("the invocation record carries a credential: %s", b)
				}
			}
		})
	}
}

// TestFleetStepFeedsAnOverlongPartialLineAsALineOfItsOwn: a partial line is
// held for its newline, but not without bound. The cockpit cuts a line past
// 64 KiB where no secret straddles the cut, so a partial that grows past the
// bound is fed as a line of its own at exactly such a cut -- and what follows
// the cut's newline is a line of its own, never glued to it.
func TestFleetStepFeedsAnOverlongPartialLineAsALineOfItsOwn(t *testing.T) {
	first, second := strings.Repeat("a", 40<<10), strings.Repeat("b", 40<<10)
	d := &fakeDispatcher{answer: func(_ context.Context, req worker.Request) (worker.Result, error) {
		req.OnStreamChunk(chunk("stdout", first))  // held: under the bound
		req.OnStreamChunk(chunk("stdout", second)) // past it: fed whole, at the cockpit's cut
		req.OnStreamChunk(chunk("stdout", "tail\n"))
		return worker.Result{OK: true, OutputJSON: `{"exitCode":0,"durationMs":10}`, WorkerId: "reg-1"}, nil
	}}
	f, lib, _, _ := newTestFleet(t, d)
	req := fleetReq()
	req.Step.Artifacts = nil
	res := runFleet(t, f, req)
	log, ok := lib.named(logFileName(req.StepKey))
	if !ok {
		t.Fatal("no log archive")
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(log.Bytes), "\n"), "\n") {
		if !strings.HasPrefix(line, "memql: ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || lines[0] != first+second || lines[1] != "tail" {
		lens := make([]int, len(lines))
		for i, l := range lines {
			lens[i] = len(l)
		}
		t.Fatalf("archived lines of lengths %v, want %d then the 4-byte \"tail\": a partial past the bound is a line "+
			"of its own", lens, len(first+second))
	}
	if res.LogLines != 2 || !strings.HasSuffix(res.LogTail, "tail") {
		t.Errorf("LogLines = %d, tail ends %q; want 2 lines ending in tail", res.LogLines, res.LogTail[max(len(res.LogTail)-10, 0):])
	}
}
