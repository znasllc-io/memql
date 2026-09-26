package procedure

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// replay_harness_test.go -- the seams a replay runs through, faked, and a
// small in-memory work store, so every runner test drives the PRODUCTION
// sequence against state that moves the way the engine's rows do.
//
// The seams record what they were asked, because the claims these tests
// exist for are about CALLS: that nothing was dispatched before a refusal,
// that a step with a receipt was not dispatched again, that the app was
// handed the goal exactly once, and that no model was reached.

// --- the seams -----------------------------------------------------------------

// fakeDispatcher runs steps against an in-memory workspace per replay run: a
// write stores its bytes, a read returns them, a command reports a clean
// exit. What it reports is in executor-independent terms, digests included,
// computed the way any executor would compute them.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []DispatchRequest
	files map[string]map[string]string // replay run -> workspace path -> content
	// fail makes a step's dispatch fail, by step key.
	fail map[string]error
	// alter rewrites what a step reports, by step key: the world answering
	// differently from every recording.
	alter map[string]func(*DispatchResult)
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{files: map[string]map[string]string{}, fail: map[string]error{}, alter: map[string]func(*DispatchResult){}}
}

func (d *fakeDispatcher) Dispatch(_ context.Context, req DispatchRequest) (DispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, req)
	if err := d.fail[req.StepKey]; err != nil {
		return DispatchResult{}, err
	}
	ws := d.files[req.RunId]
	if ws == nil {
		ws = map[string]string{}
		d.files[req.RunId] = ws
	}
	no := false
	res := DispatchResult{Observation: work.StepObservation{IsError: &no}}
	switch req.Tool {
	case "exec":
		zero := 0
		res.Observation.ExitCode = &zero
		res.Observation.ResultType = work.InferTextType("")
		res.Output = map[string]any{"stdout": "", "exitCode": 0}
	case "fs_write":
		p := path.Clean(str(req.Args, "file_path"))
		content := str(req.Args, "content")
		ws[p] = content
		res.Observation.Contents = []work.ContentDigest{{Op: "write", Path: p, Digest: proc.Digest([]byte(content))}}
		res.Output = map[string]any{"written": p}
	case "fs_read":
		p := path.Clean(str(req.Args, "file_path"))
		res.Observation.Contents = []work.ContentDigest{{Op: "read", Path: p, Digest: proc.Digest([]byte(ws[p]))}}
		res.Output = ws[p]
	}
	if fn := d.alter[req.StepKey]; fn != nil {
		fn(&res)
	}
	return res, nil
}

func (d *fakeDispatcher) recorded() []DispatchRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DispatchRequest(nil), d.calls...)
}

// fakeProber reports a fixed environment, and counts the probes.
type fakeProber struct {
	mu       sync.Mutex
	observed proc.Preconditions
	err      error
	calls    int
}

func (p *fakeProber) Probe(context.Context, work.ReplayTarget, string, string, proc.Preconditions) (proc.Preconditions, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.observed, p.err
}

// matchingProber reports exactly what the fixture corpus learned: the two
// tools its commands use, at the versions every recording ran, in an empty
// workspace. The platform and the variables are not compared on the
// workbench, so they are not reported.
func matchingProber() *fakeProber {
	empty := true
	return &fakeProber{observed: proc.Preconditions{
		Tools:          map[string]string{"mkdir": "9.4", "echo": "9.4"},
		EmptyWorkspace: &empty,
	}}
}

// fakeFallback is the app taking the goal over: it records every hand-back
// and answers a repaired run.
type fakeFallback struct {
	mu    sync.Mutex
	calls []FallbackRequest
	err   error
}

func (f *fakeFallback) Handover(_ context.Context, req FallbackRequest) (FallbackOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return FallbackOutcome{}, f.err
	}
	return FallbackOutcome{ChildRunId: fmt.Sprintf("v1:work:run:repair-%d", len(f.calls)), Content: "done by the app"}, nil
}

func (f *fakeFallback) recorded() []FallbackRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FallbackRequest(nil), f.calls...)
}

// --- live rows -------------------------------------------------------------------

// copyRow is a row as a fresh read answers it: its own maps, nothing shared
// with the store's.
func copyRow(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// liveConstruct is a construct row that moves with what is written to it,
// through the REAL parser: a write the parser would read differently is a
// write the row does not get.
type liveConstruct struct {
	mu  sync.Mutex
	row map[string]any
}

func installConstruct(t *testing.T, eng *fakeEngine, row map[string]any) *liveConstruct {
	t.Helper()
	lc := &liveConstruct{row: row}
	eng.answer("authoringConstructById", func(recordedCall, string) ([]map[string]any, string) {
		lc.mu.Lock()
		defer lc.mu.Unlock()
		return []map[string]any{copyRow(t, lc.row)}, ""
	})
	apply := func(c recordedCall) {
		args := parseCallArgs(t, c.Query)
		lc.mu.Lock()
		defer lc.mu.Unlock()
		for k, v := range args {
			if k != "constructId" {
				lc.row[k] = v
			}
		}
	}
	eng.onWrite("recordConstructLadder", apply)
	eng.onWrite("recordConstructReliability", apply)
	return lc
}

func (lc *liveConstruct) get(key string) any {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.row[key]
}

func (lc *liveConstruct) set(key string, v any) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.row[key] = v
}

// liveWork is the work rows a replay writes and reads back -- runs and steps,
// merged version over version the way a read-merge does.
type liveWork struct {
	mu    sync.Mutex
	runs  map[string]map[string]any
	steps map[string]map[string]any
}

func installWork(t *testing.T, eng *fakeEngine) *liveWork {
	t.Helper()
	lw := &liveWork{runs: map[string]map[string]any{}, steps: map[string]map[string]any{}}
	merge := func(into map[string]map[string]any, idKey string) func(recordedCall) {
		return func(c recordedCall) {
			args := parseCallArgs(t, c.Query)
			id, _ := args[idKey].(string)
			lw.mu.Lock()
			defer lw.mu.Unlock()
			row := into[id]
			if row == nil {
				row = map[string]any{"id": id, "ownerUserId": c.Actor}
				into[id] = row
			}
			for k, v := range args {
				if k != idKey {
					row[k] = v
				}
			}
		}
	}
	eng.onWrite("createWorkRun", merge(lw.runs, "runId"))
	eng.onWrite("updateWorkRun", merge(lw.runs, "runId"))
	eng.onWrite("createWorkStep", merge(lw.steps, "stepId"))
	eng.onWrite("updateWorkStep", merge(lw.steps, "stepId"))
	// A run or a step the store holds is answered from it -- to its owner
	// alone, as the owned tier answers -- and one it does not hold falls
	// through to the fixture replies: a recording seeded by seedCorpus is
	// read exactly as the corpus loader reads it.
	eng.answerSome("workRunForOwner", func(c recordedCall, _ string) ([]map[string]any, string, bool) {
		id, _ := parseCallArgs(t, c.Query)["runId"].(string)
		lw.mu.Lock()
		defer lw.mu.Unlock()
		row, ok := lw.runs[id]
		if !ok {
			return nil, "", false
		}
		if !sameUser(str(row, "ownerUserId"), c.Actor) {
			return nil, "", true
		}
		return []map[string]any{copyRow(t, row)}, "", true
	})
	eng.answerSome("workStepsForOwnerRun", func(c recordedCall, _ string) ([]map[string]any, string, bool) {
		runId, _ := parseCallArgs(t, c.Query)["runId"].(string)
		lw.mu.Lock()
		defer lw.mu.Unlock()
		var out []map[string]any
		held := false
		for _, row := range lw.steps {
			if str(row, "runId") != runId {
				continue
			}
			held = true
			if sameUser(str(row, "ownerUserId"), c.Actor) {
				out = append(out, copyRow(t, row))
			}
		}
		return out, "", held
	})
	return lw
}

func (lw *liveWork) run(id string) map[string]any {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.runs[id]
}

func (lw *liveWork) putRun(row map[string]any) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.runs[str(row, "id")] = row
}

func (lw *liveWork) putStep(row map[string]any) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.steps[str(row, "id")] = row
}

// stepsOf are a run's steps, by key.
func (lw *liveWork) stepsOf(runId string) map[string]map[string]any {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	out := map[string]map[string]any{}
	for _, row := range lw.steps {
		if str(row, "runId") == runId {
			out[str(row, "key")] = row
		}
	}
	return out
}

// --- the world a replay runs in --------------------------------------------------

const (
	goalRunId   = "v1:work:run:goal-e"
	goalId      = "v1:work:goal:goal-e"
	goalFile    = "e.txt"
	replayOwner = testOwner
)

type replayWorld struct {
	eng  *fakeEngine
	i    *Integration
	lc   *liveConstruct
	work *liveWork
	d    *fakeDispatcher
	p    *fakeProber
	f    *fakeFallback
	// constructId, name and hash are the lifted construct's.
	constructId, name, hash string
}

// newReplayWorld lifts the fixture corpus for REAL -- the payload a replay
// executes is the one the lift writes, read back through the parser -- and
// puts the construct on a rung, with every seam installed.
func newReplayWorld(t *testing.T, rung string) *replayWorld {
	t.Helper()
	lifted, _ := liftFixture(t, twoRecordings()...)
	row := storedConstruct(t, lifted, rung)
	row["shadowMatches"] = float64(0)
	row["canaryMatches"] = float64(0)
	row["failures"] = float64(0)
	row["insufficient"] = float64(0)
	row["distinctBindings"] = map[string]any{}
	row["ownerUserId"] = replayOwner
	row["createdAt"] = testNow.Add(-72 * time.Hour).Format(timeLayout)

	eng := newFakeEngine()
	w := &replayWorld{
		eng: eng, d: newFakeDispatcher(), p: matchingProber(), f: &fakeFallback{},
		constructId: str(row, "id"), name: str(row, "name"), hash: str(row, "procedureHash"),
	}
	w.lc = installConstruct(t, eng, row)
	eng.ownedRead("authoringConstructById", replayOwner)
	w.work = installWork(t, eng)
	w.work.putRun(map[string]any{
		"id": goalRunId, "ownerUserId": replayOwner, "goalId": goalId, "status": "running",
		"goalSignature": testSignature, "variables": map[string]any{"file": goalFile, "procedureConstructId": w.constructId},
	})
	eng.replyWhen("workGoalForOwner", `"`+goalId+`"`, map[string]any{
		"id": goalId, "statement": testStatement, "input": map[string]any{"file": goalFile},
	})
	w.i = newTestIntegration(eng)
	w.i.SetDispatcher(work.TargetWorkbench, w.d)
	w.i.SetProber(w.p)
	w.i.SetAppFallback(w.f)
	return w
}

// policy sets the ladder policy row the replay reads.
func (w *replayWorld) policy(p work.LadderPolicy) {
	w.eng.reply("ladderPolicyCurrent", map[string]any{
		"id": "v1:authoring:ladderPolicy:primary", "shadowMatches": float64(p.ShadowMatches),
		"distinctBindings": float64(p.DistinctBindings), "canaryMatches": float64(p.CanaryMatches),
		"failuresToDemote": float64(p.FailuresToDemote), "insufficientToDemote": float64(p.InsufficientToDemote),
		"retireAfterDays": float64(p.RetireAfterDays),
	})
}

// serve is a trusted or canary replay of the fixture goal.
func (w *replayWorld) serve(t *testing.T, mode ReplayMode, input map[string]any) ReplayOutcome {
	t.Helper()
	out, err := w.i.Replay(context.Background(), ReplayRequest{
		OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: mode, GoalRunId: goalRunId, Input: input,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return out
}

// withProcedure rewrites the stored payload, as a tampered or stale row would
// carry it.
func (w *replayWorld) withProcedure(t *testing.T, edit func(p map[string]any)) {
	t.Helper()
	p := copyRow(t, w.lc.get("procedure").(map[string]any))
	edit(p)
	w.lc.set("procedure", p)
}

// withDecoded rewrites the stored payload through its Go value.
func (w *replayWorld) withDecoded(t *testing.T, edit func(p *Procedure)) {
	t.Helper()
	p, err := DecodeProcedure(w.lc.get("procedure"))
	if err != nil {
		t.Fatal(err)
	}
	edit(&p)
	obj, err := asObject(p)
	if err != nil {
		t.Fatal(err)
	}
	w.lc.set("procedure", obj)
}

// appObservations are what the app's two actions reported in a recording of
// the fixture goal: the command's clean exit and the report's bytes.
func appObservations() []work.StepObservation {
	no, zero := false, 0
	return []work.StepObservation{
		{IsError: &no, ExitCode: &zero, ResultType: "string"},
		{IsError: &no, Contents: []work.ContentDigest{{Op: "write", Path: "out/report.txt", Digest: helloDigest}}},
	}
}

// shadowOf is a shadow comparison of a recording that wrote file.
func (w *replayWorld) shadowOf(t *testing.T, recordingRunId, file string) ReplayOutcome {
	t.Helper()
	out, err := w.i.Replay(context.Background(), ReplayRequest{
		OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayShadow, GoalRunId: recordingRunId,
		Bindings:   map[string]string{"s0.command.7": file},
		AppActions: appObservations(),
		AppArgs: []map[string]any{
			{"command": "mkdir -p out && echo hello > " + file},
			{"file_path": relativeReportPath, "content": "hello\n"},
		},
	})
	if err != nil {
		t.Fatalf("Replay (shadow): %v", err)
	}
	return out
}

// ladderWrites are the ladder writes the engine recorded, parsed.
func ladderWrites(t *testing.T, eng *fakeEngine) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range eng.callsTo("recordConstructLadder") {
		out = append(out, parseCallArgs(t, c.Query))
	}
	return out
}

func dispatchedKeys(calls []DispatchRequest) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.StepKey)
	}
	return out
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
