//go:build agent

package pipelinesteps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeLibrary records every file stored.
type fakeLibrary struct {
	mu    sync.Mutex
	files []RunFile
	omit  map[string]string
	fail  map[string]error
}

func (l *fakeLibrary) StoreRunFile(_ context.Context, f RunFile) (StoredFile, error) {
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
	d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
		return worker.Result{OK: false, ErrorCode: "kill_switch_engaged", ErrorMessage: "computer use is currently disabled by the user"}, nil
	}}
	f, _, _, _ := newTestFleet(t, d)
	wantFailure(t, runFleet(t, f, fleetReq()), pl.OutcomeRefused, pl.CodeFleetDisabled)
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
	log, ok := lib.named(runLogFileName(req.StepKey))
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
		lib.fail = map[string]error{runLogFileName("tests.go-tests#2"): errors.New("blob storage timed out")}
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
}

// gateStore is the slice of the dispatcher's store its gate and router read:
// the owner's machines, no routing policy, and the kill switch.
type gateStore struct {
	mu          sync.Mutex
	machines    []worker.Candidate
	computerUse bool
	invocations []workerservice.InvocationRow
}

func (s *gateStore) WorkersForOwner(context.Context, string) ([]worker.Candidate, error) {
	return s.machines, nil
}
func (s *gateStore) RoutingPolicyForOwner(context.Context, string) (*worker.Policy, error) {
	return nil, nil
}
func (s *gateStore) TouchWorkerSelected(context.Context, string, string) error { return nil }
func (s *gateStore) UserPreferences(context.Context, string) (worker.Preferences, error) {
	return worker.Preferences{ComputerUseEnabled: s.computerUse}, nil
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
		computerUse bool
		status      pl.Outcome
		code        string
	}{
		{"an owner with no machines", nil, true, pl.OutcomeRefused, pl.CodeNoMachineForNeed},
		{"a machine held by a sibling replica this node cannot reach", []worker.Candidate{builder}, true, pl.OutcomeRefused, pl.CodeNoMachineForNeed},
		{"an owner whose computer use is off", []worker.Candidate{builder}, false, pl.OutcomeRefused, pl.CodeFleetDisabled},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &gateStore{machines: c.machines, computerUse: c.computerUse}
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
	log, ok := lib.named(runLogFileName(req.StepKey))
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
