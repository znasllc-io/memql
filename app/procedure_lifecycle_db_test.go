package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// procedure_lifecycle_db_test.go -- the certification ladder end to end,
// against a REAL engine and a REAL database (epic memql#5408, #5410, #5411).
//
// Every row is the real one: the recordings are written by the session
// writer a delegated app session uses, the procedure is lifted and Gate 1
// compiles it, the ladder's values are the seeded singleton's row, the
// promotion is a v1:work:approval a person decides through integrations/work's
// own decide handler, and every replay writes its run and its steps through
// the @serverOnly work mutations. Only the three seams a node installs are
// fakes -- a dispatcher over an in-memory workspace, a prober, and the app
// taking a goal over -- because they are the parallel production adapters'
// to prove.
//
// The one thing a unit test cannot say, this says: that the rows these
// statements write are rows the schema ACCEPTS -- an outcome of lists and
// prose, binding digests keyed by dotted hole ids, an approval's subject and
// options -- and that each rung reads back exactly what the last one wrote.

// lifecycleDispatcher runs the fixture procedure's two steps against an
// in-memory workspace per replay run, reporting what any executor would: a
// clean exit, and the bytes a write left, by digest.
type lifecycleDispatcher struct {
	mu    sync.Mutex
	calls []procedure.DispatchRequest
	// unavailable answers every step as the TARGET not finishing it (no
	// workbench peer); timeout answers every step as having timed out.
	unavailable bool
	timeout     bool
	files       map[string]map[string]string
}

func (d *lifecycleDispatcher) Dispatch(_ context.Context, req procedure.DispatchRequest) (procedure.DispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, req)
	if d.unavailable || d.timeout {
		failed := true
		res := procedure.DispatchResult{Observation: work.StepObservation{IsError: &failed}, Output: map[string]any{"errorCode": "timeout"}}
		if d.unavailable {
			res.Output, res.Unavailable = map[string]any{"errorCode": "no_workbench_peer"}, true
		}
		return res, nil
	}
	if d.files == nil {
		d.files = map[string]map[string]string{}
	}
	ws := d.files[req.RunId]
	if ws == nil {
		ws = map[string]string{}
		d.files[req.RunId] = ws
	}
	no := false
	res := procedure.DispatchResult{Observation: work.StepObservation{IsError: &no}}
	switch req.Tool {
	case "exec":
		zero := 0
		res.Observation.ExitCode = &zero
		res.Observation.ResultType = work.InferTextType("")
		if cmd, _ := req.Args["command"].(string); cmd != "" {
			words := strings.Fields(cmd)
			ws[path.Clean(words[len(words)-1])] = "hello\n"
		}
	case "fs_write":
		p, _ := req.Args["file_path"].(string)
		content, _ := req.Args["content"].(string)
		p = path.Clean(p)
		ws[p] = content
		sum := sha256.Sum256([]byte(content))
		res.Observation.Contents = []work.ContentDigest{{Op: "write", Path: p, Digest: hex.EncodeToString(sum[:])}}
	}
	return res, nil
}

func (d *lifecycleDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// lifecycleProber reports the workbench's tools and an empty workspace.
type lifecycleProber struct {
	mu    sync.Mutex
	tools map[string]string
}

func (p *lifecycleProber) Probe(context.Context, work.ReplayTarget, string, string, procedure.Preconditions) (procedure.Preconditions, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tools := make(map[string]string, len(p.tools))
	for k, v := range p.tools {
		tools[k] = v
	}
	empty := true
	return procedure.Preconditions{Tools: tools, EmptyWorkspace: &empty}, nil
}

func (p *lifecycleProber) set(tool, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools[tool] = version
}

// lifecycleFallback is the app taking a goal over.
type lifecycleFallback struct {
	mu    sync.Mutex
	calls []procedure.FallbackRequest
}

func (f *lifecycleFallback) Handover(_ context.Context, req procedure.FallbackRequest) (procedure.FallbackOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return procedure.FallbackOutcome{ChildRunId: "v1:work:run:" + id.NewShortId(), Content: "the app did it"}, nil
}

func (f *lifecycleFallback) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// setLadderPolicy writes v1:authoring:ladderPolicy:primary through the seed's
// own create mutation, under internal origin and a cluster owner -- the only
// writer the concept's tier admits.
func setLadderPolicy(t *testing.T, e *memql.MemQLEngine, p work.LadderPolicy) {
	t.Helper()
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "ladder-policy-lifecycle", Role: auth.RoleOwner})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "ladder-policy-lifecycle"})
	templateMutation(t, e, auth.ContextWithInternalOrigin(ctx), "createLadderPolicy", map[string]any{
		"ladderPolicyId": "primary", "shadowMatches": p.ShadowMatches, "distinctBindings": p.DistinctBindings,
		"canaryMatches": p.CanaryMatches, "failuresToDemote": p.FailuresToDemote,
		"insufficientToDemote": p.InsufficientToDemote, "retireAfterDays": p.RetireAfterDays,
	})
}

// openGoalRun is a goal compile served from the ladder: its run names the
// replay template and carries the goal's input and the construct.
func openGoalRun(t *testing.T, e *memql.MemQLEngine, ownerCtx context.Context, statement, sig, file, constructId string) string {
	t.Helper()
	internal := auth.ContextWithInternalOrigin(ownerCtx)
	goalId, runId := id.NewShortId(), id.NewShortId()
	templateMutation(t, e, internal, "createWorkGoal", map[string]any{
		"goalId": goalId, "statement": statement, "origin": "user", "input": map[string]any{"file": file},
	})
	templateMutation(t, e, internal, "createWorkRun", map[string]any{
		"runId": runId, "goalId": goalId, "automationName": "replayLearnedProcedure",
		"templateFingerprint": "replayLearnedProcedure", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339),
		"goalSignature": sig, "variables": map[string]any{"file": file, "procedureConstructId": constructId},
	})
	return runId
}

func queryRows(t *testing.T, e *memql.MemQLEngine, ctx context.Context, q string) []map[string]any {
	t.Helper()
	res, err := e.Execute(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return memql.MaterializeRows(res)
}

func numberOf(row map[string]any, key string) float64 {
	switch v := row[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// warnings is a slog handler that keeps every WARN and ERROR. The runner LOGS
// a write the engine refused and carries on -- a replay must not fail a goal
// over its own bookkeeping -- so a test that discarded the log would pass with
// every step row, heartbeat and receipt refused. Any warning fails this one.
type warnings struct {
	mu   *sync.Mutex
	seen *[]string
}

func (w warnings) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (w warnings) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	*w.seen = append(*w.seen, b.String())
	return nil
}
func (w warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w warnings) WithGroup(string) slog.Handler      { return w }

func TestProcedureLifecycleDB_RecordedToTrustedAndBackToShadow(t *testing.T) {
	e, db := workTemplateDBEngineAndDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var (
		warnMu sync.Mutex
		warned []string
	)
	procLogger := slog.New(warnings{mu: &warnMu, seen: &warned})
	t.Cleanup(func() {
		warnMu.Lock()
		defer warnMu.Unlock()
		for _, w := range warned {
			t.Errorf("the procedure integration warned -- a row it wrote may have been refused: %s", w)
		}
	})
	owner := "dbtest-life-" + strings.ReplaceAll(id.NewShortId(), "-", "")
	ownerCtx := auth.ContextWithUserActor(context.Background(), owner)
	statement := "Write the greeting file " + owner
	sig := work.GoalSignature(statement, []string{"file"})
	name := "learnedProcedure_" + sig[:12] + "_l1"
	writer := workspine.NewSessionWriter(e, logger)

	d := &lifecycleDispatcher{}
	prober := &lifecycleProber{tools: map[string]string{"mkdir": "9.4", "echo": "9.4"}}
	fb := &lifecycleFallback{}
	integ := procedure.New(e, procLogger)
	integ.SetCompiler(&CognitionEngineAdapter{Engine: e})
	integ.SetDispatcher(work.TargetWorkbench, d)
	integ.SetProber(prober)
	integ.SetAppFallback(fb)

	construct := func() map[string]any { return procedureRow(t, e, ownerCtx, name) }

	// --- 1. Two recordings, two bindings: lifted into shadow. ---------------
	var recordings []string
	for n, file := range []string{"a.txt", "b.txt"} {
		recordings = append(recordings, recordSession(t, e, writer, ownerCtx, owner, statement, sig, file, n))
	}
	lifted, err := integ.LearnFromRun(ownerCtx, recordings[1])
	if err != nil {
		t.Fatalf("LearnFromRun: %v", err)
	}
	if lifted.Lift != procedure.LiftCreated || lifted.Rung != work.RungShadow {
		t.Fatalf("the lift = %+v, want a new construct in shadow", lifted)
	}
	row := construct()
	constructId, _ := row["id"].(string)

	// --- 2. The ladder's values: m=2, k=2, one clean canary to trust. -------
	setLadderPolicy(t, e, work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 1, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 30})
	t.Cleanup(func() { setLadderPolicy(t, e, work.DefaultLadderPolicy()) })

	// --- 3. Two more recordings, two new bindings, each compared beside the
	// app: exactly ONE promotion is raised, on the second.
	for n, file := range []string{"c.txt", "d.txt"} {
		rec := recordSession(t, e, writer, ownerCtx, owner, statement, sig, file, n+2)
		outs, err := integ.ShadowCompare(ownerCtx, rec)
		if err != nil {
			t.Fatalf("ShadowCompare %s: %v", file, err)
		}
		if len(outs) != 1 || !outs[0].Match {
			t.Fatalf("comparing %s = %+v, want one match", file, outs)
		}
		if got := numberOf(construct(), "shadowMatches"); got != float64(n+1) {
			t.Fatalf("after %s shadowMatches = %v, want %d", file, got, n+1)
		}
		if n == 0 && outs[0].ApprovalId != "" {
			t.Fatalf("one match proposed a promotion: %s", outs[0].ApprovalId)
		}
	}
	var promotions []map[string]any
	for _, a := range queryRows(t, e, ownerCtx, "query workApprovalsForOwner()") {
		if a["kind"] == work.ApprovalKindProcedurePromotion {
			promotions = append(promotions, a)
		}
	}
	if len(promotions) != 1 {
		t.Fatalf("%d procedurePromotion approvals are pending, want exactly one", len(promotions))
	}
	approvalId, _ := promotions[0]["id"].(string)
	row = construct()
	if memql.BareShortId(fmt.Sprint(row["promotionApprovalId"])) != memql.BareShortId(approvalId) {
		t.Fatalf("the construct waits on %v, want the raised approval %s", row["promotionApprovalId"], approvalId)
	}
	if promotions[0]["artifactHash"] != row["procedureHash"] {
		t.Fatalf("the approval pins %v, want the construct's version %v", promotions[0]["artifactHash"], row["procedureHash"])
	}

	// --- 4. The owner approves it through integrations/work's REAL decide
	// handler, and the promotion decision moves shadow to canary.
	decided := false
	for _, c := range workspine.New(e, logger, func() *bun.DB { return db }).Capabilities() {
		if c.Name != "decideApproval" {
			continue
		}
		person := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
		if _, err := c.Handler(person, map[string]any{"approvalId": approvalId, "decision": "approved"}, 0); err != nil {
			t.Fatalf("decideApproval: %v", err)
		}
		decided = true
	}
	if !decided {
		t.Fatal("integrations/work offers no decideApproval capability")
	}
	tr, err := integ.DecidePromotion(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("onProcedurePromotionDecided")), approvalId)
	if err != nil {
		t.Fatalf("DecidePromotion: %v", err)
	}
	if tr.To != work.RungCanary || construct()["ladder"] != "canary" {
		t.Fatalf("the decision moved the ladder to %s (stored %v), want canary", tr.To, construct()["ladder"])
	}

	// --- 5. One clean canary replay: trusted. --------------------------------
	serve := func(file string) procedure.ReplayOutcome {
		t.Helper()
		goalRun := openGoalRun(t, e, ownerCtx, statement, sig, file, constructId)
		out, err := integ.Replay(ownerCtx, procedure.ReplayRequest{
			OwnerUserId: owner, ConstructId: constructId, Mode: procedure.ReplayTrusted,
			GoalRunId: goalRun, Input: map[string]any{"file": file},
		})
		if err != nil {
			t.Fatalf("Replay %s: %v", file, err)
		}
		return out
	}
	if out := serve("e.txt"); !out.Served || out.Transition.To != work.RungTrusted {
		t.Fatalf("the canary replay = %+v, want served and trusted", out)
	}
	if construct()["ladder"] != "trusted" {
		t.Fatalf("stored rung %v, want trusted", construct()["ladder"])
	}

	// --- 6. A trusted replay serves with NO model and NO app. ----------------
	out := serve("f.txt")
	if !out.Served || out.FellBack || out.ModelCalls != 0 || fb.count() != 0 {
		t.Fatalf("the trusted replay = %+v with %d hand-backs, want served by the procedure alone", out, fb.count())
	}
	if calls := queryRows(t, e, ownerCtx, `query workModelCallsForOwnerRun(runId: "`+out.ReplayRunId+`")`); len(calls) != 0 {
		t.Fatalf("the trusted replay journaled %d model calls", len(calls))
	}
	replayRun := queryRows(t, e, ownerCtx, `query workRunForOwner(runId: "`+out.ReplayRunId+`")`)
	if len(replayRun) != 1 || replayRun[0]["status"] != "succeeded" || replayRun[0]["triggeredBy"] != "procedure:trusted" {
		t.Fatalf("the replay run = %v", replayRun)
	}

	// --- 7. The workbench's tool moved: the start is refused, the app serves
	// the goal exactly once, and the mismatch is on the replay run.
	prober.set("mkdir", "9.3")
	out = serve("g.txt")
	if !out.StartRefused || out.Served || fb.count() != 1 {
		t.Fatalf("the mismatched replay = %+v with %d hand-backs, want the start refused and one hand-back", out, fb.count())
	}
	refusedRun := queryRows(t, e, ownerCtx, `query workRunForOwner(runId: "`+out.ReplayRunId+`")`)
	if len(refusedRun) != 1 || !strings.Contains(fmt.Sprint(refusedRun[0]["outcome"]), "tools.mkdir: recorded 9.4, found 9.3") {
		t.Fatalf("the replay run's outcome does not name the mismatch: %v", refusedRun)
	}
	if got := numberOf(construct(), "failures"); got != 1 || construct()["ladder"] != "trusted" {
		t.Fatalf("after a refused start: failures %v, rung %v -- want one failure, still trusted", got, construct()["ladder"])
	}

	// --- 8. A clean replay clears the failures. A target that could not
	// finish the step (no workbench peer) is NOT the procedure failing: the
	// goal goes to the app and the ladder counts nothing. Two replays whose
	// first step timed out -- an ordinary failure -- demote it.
	prober.set("mkdir", "9.4")
	if out := serve("h.txt"); !out.Served || numberOf(construct(), "failures") != 0 {
		t.Fatalf("the clean replay = %+v, failures %v", out, numberOf(construct(), "failures"))
	}
	d.mu.Lock()
	d.unavailable = true
	d.mu.Unlock()
	if out := serve("h2.txt"); out.Served || construct()["ladder"] != "trusted" || numberOf(construct(), "failures") != 0 {
		t.Fatalf("an unavailable workbench = %+v, rung %v, failures %v -- want the app served it and nothing counted", out, construct()["ladder"], numberOf(construct(), "failures"))
	}
	d.mu.Lock()
	d.unavailable, d.timeout = false, true
	d.mu.Unlock()
	if out := serve("i.txt"); out.Served || construct()["ladder"] != "trusted" {
		t.Fatalf("one failed replay = %+v, rung %v -- want still trusted", out, construct()["ladder"])
	}
	if out := serve("j.txt"); !out.Transition.Demoted || construct()["ladder"] != "shadow" {
		t.Fatalf("the second failed replay = %+v, rung %v -- want demoted to shadow", out.Transition, construct()["ladder"])
	}
	d.mu.Lock()
	d.timeout = false
	d.mu.Unlock()
	if fb.count() != 4 {
		t.Fatalf("%d hand-backs, want one per goal the procedure did not serve (4)", fb.count())
	}

	// --- 9. A machine-local procedure sent to the workbench is refused
	// before its first step -- nothing dispatched anywhere.
	row = construct()
	payload, _ := row["procedure"].(map[string]any)
	payload["footprint"] = map[string]any{"files": true, "machine": true}
	payload["target"] = "workbench"
	internal := auth.ContextWithInternalOrigin(ownerCtx)
	templateMutation(t, e, internal, "recordProcedure", map[string]any{
		"constructId": constructId, "source": row["source"], "procedure": payload,
		"preconditions": row["preconditions"], "procedureHash": row["procedureHash"],
	})
	templateMutation(t, e, internal, "recordConstructLadder", map[string]any{"constructId": constructId, "ladder": "trusted"})
	before := d.count()
	out = serve("k.txt")
	if !out.StartRefused || !strings.Contains(out.Diagnosis, "workbench") || d.count() != before {
		t.Fatalf("the machine-local replay = %+v with %d new dispatches, want it refused before the first step", out, d.count()-before)
	}
}
