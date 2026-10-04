package proving

// procedure_harness.go -- the learned-procedure half of the platform, opened
// over a REAL engine and a REAL database for the lifecycle driver (epic
// memql#5408, task memql#5413).
//
// EVERYTHING HERE IS THE PLATFORM'S OWN, except three seams. The recordings
// are written by integrations/work's session writer -- the path a delegated
// app session takes. The goals are opened, and closed, by integrations/work.
// The lift, the shadow comparison, the promotion decision and every replay are
// integrations/procedure's, with Gate 1 compiling the lifted source through
// the engine. The promotion is a v1:work:approval a person decides through
// integrations/work's OWN decide handler, and the ladder's values are the
// seeded singleton's row. What a node would install is the only thing
// supplied: a dispatcher and a prober over the proving world, and the fixture
// app standing where an app session stands. That is the boundary
// app/procedure_lifecycle_db_test.go draws too, and for the same reason.
//
// EACH CALL IS MADE AS THE ONE PRODUCTION MAKES IT. The learn, the comparison
// and every replay as the recording's or the goal's OWNER -- the completion
// trigger borrows the owner the event names, and the runner refuses anybody
// else. The promotion decision as onProcedurePromotionDecided's maintenance
// principal, under the internal origin the automation runtime gives every
// automation from the registered tree. The person's decision as that person.
//
// A person stepping into a goal -- the goal's run executed, a step run again,
// a verdict -- is procedure_harness_steps.go's, over the same engine.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/num"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// ladderPolicyId is the ladder's one row, as its seed names it (the seed's
// name is its id: v1:authoring:ladderPolicy:primary).
const ladderPolicyId = "primary"

// provingGoalTemplate is what a goal the APP serves names as its template. In
// production that is whatever compile chose for a goal that delegates to an
// app; nothing dispatches a goal run in the bench process, so the name only has
// to say what served it. A goal the ladder serves names
// replayLearnedProcedure, exactly as compile writes it.
const (
	provingGoalTemplate  = "provingFixtureGoal"
	replayGoalTemplate   = "replayLearnedProcedure"
	provingGoalTrigger   = "proving:lifecycle"
	promotionAutomation  = "onProcedurePromotionDecided"
	goalNotServedErrCode = "proving_goal_not_served"
)

// engineLifecycle is the LifecycleHarness over the engine.
type engineLifecycle struct {
	e      *memqlengine.MemQLEngine
	db     *bun.DB
	logger *slog.Logger

	// The work spine as a node materializes it, registered on the engine the
	// first time a lifecycle opens (procedure_harness_steps.go workActs), and
	// the switch that routes its log to the lifecycle running now.
	once    sync.Once
	work    memqlengine.IntegrationProvider
	workErr error
	workLog *logSwitch
}

// NewEngineLifecycle is the harness cmd/memql-bench installs on the Runner.
// db is the engine's own database handle: the work spine's version history
// reads it directly, as it does on a node.
func NewEngineLifecycle(e *memqlengine.MemQLEngine, db *bun.DB, logger *slog.Logger) LifecycleHarness {
	return &engineLifecycle{e: e, db: db, logger: logger, workLog: &logSwitch{fallback: logger}}
}

// Open writes the ladder's values the run is to climb under, proves the
// OWNER reads them back -- the runner reads the policy as the owner, and a
// read that silently fell back to the defaults would run a lifecycle that
// never proposes -- and builds the platform over this engine.
func (h *engineLifecycle) Open(ctx context.Context, owner string, overlay work.LadderPolicy) (*LifecyclePlatform, error) {
	if h == nil || h.e == nil {
		return nil, fmt.Errorf("proving: the learned-procedure harness has no engine")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("proving: a lifecycle needs an owner; every row it writes is somebody's")
	}
	// Before anything is written: a harness that cannot be the platform must
	// not leave the ladder's row on this run's values, and Close runs only
	// after an Open that succeeded.
	acts, err := h.workActs()
	if err != nil {
		return nil, err
	}
	current, _, err := h.readPolicy(ctx, owner)
	if err != nil {
		return nil, err
	}
	want := overlayPolicy(current, overlay)
	if err := h.writePolicy(ctx, want); err != nil {
		return nil, err
	}
	got, found, err := h.readPolicy(ctx, owner)
	switch {
	case err != nil:
		return nil, err
	case !found || got != want:
		return nil, fmt.Errorf("proving: the ladder's values were written as %+v and the lifecycle's owner reads %+v (row found: %v); "+
			"a lifecycle that climbed under the defaults would pass or fail for a reason nobody wrote down", want, got, found)
	}

	log := newWarningLog(h.logger)
	h.workLog.set(log)
	spine := &engineSpine{e: h.e, wi: workspine.New(h.e, log.logger(), func() *bun.DB { return h.db }), acts: acts, log: log.logger()}
	return &LifecyclePlatform{
		Recorder: workspine.NewSessionWriter(h.e, log.logger()),
		Spine:    spine,
		NewLadder: func(w *ProcedureWorld, app AppHandover) (ProcedureLadder, error) {
			integ := procedure.New(h.e, log.logger())
			integ.SetCompiler(engineCompileGate{e: h.e})
			integ.SetDispatcher(work.TargetWorkbench, worldDispatcher{w: w})
			integ.SetProber(worldProber{w: w})
			integ.SetAppFallback(appFallback{app: app})
			return &engineLadder{e: h.e, integ: integ}, nil
		},
		// The SEEDED defaults, not the row Open found: no later scenario may
		// inherit this one's values, and the seed is what every node
		// re-asserts on its next boot anyway.
		Close: func(ctx context.Context) error {
			h.workLog.set(nil)
			return h.writePolicy(ctx, work.DefaultLadderPolicy())
		},
		Warnings: log.warnings,
	}, nil
}

// readPolicy reads v1:authoring:ladderPolicy:primary as the owner reads it.
func (h *engineLifecycle) readPolicy(ctx context.Context, owner string) (work.LadderPolicy, bool, error) {
	q, err := langparser.RenderCall("ladderPolicyCurrent", nil)
	if err != nil {
		return work.LadderPolicy{}, false, err
	}
	rows, err := engineRows(auth.ContextWithUserActor(ctx, owner), h.e, "query "+q)
	if err != nil {
		return work.LadderPolicy{}, false, fmt.Errorf("proving: reading the ladder's values as the lifecycle's owner: %w", err)
	}
	if len(rows) == 0 {
		return work.LadderPolicy{}, false, nil
	}
	r := rows[0]
	return work.LadderPolicy{
		ShadowMatches: intField(r, "shadowMatches"), DistinctBindings: intField(r, "distinctBindings"),
		CanaryMatches: intField(r, "canaryMatches"), FailuresToDemote: intField(r, "failuresToDemote"),
		InsufficientToDemote: intField(r, "insufficientToDemote"), RetireAfterDays: intField(r, "retireAfterDays"),
	}, true, nil
}

// writePolicy writes the ladder's row through the seed's own create mutation.
//
// @serverOnly, and the concept's write rule is the cluster owner's, so the
// write goes under the bench's cluster principal with the internal-origin
// stamp -- the ONE writer the tier admits, and the one this package's entry in
// call_origin_conformance_test.go names for it.
func (h *engineLifecycle) writePolicy(ctx context.Context, p work.LadderPolicy) error {
	call, err := langparser.RenderCall("createLadderPolicy", map[string]any{
		"ladderPolicyId": ladderPolicyId, "shadowMatches": p.ShadowMatches, "distinctBindings": p.DistinctBindings,
		"canaryMatches": p.CanaryMatches, "failuresToDemote": p.FailuresToDemote,
		"insufficientToDemote": p.InsufficientToDemote, "retireAfterDays": p.RetireAfterDays,
	})
	if err != nil {
		return fmt.Errorf("proving: rendering the ladder's values: %w", err)
	}
	if _, err := h.e.Execute(benchContext(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("proving: writing the ladder's values: %w", err)
	}
	return nil
}

// overlayPolicy is the row the run climbs under: the overlay's non-zero values
// over the deployment's current row, and the defaults where there is no row.
func overlayPolicy(current, overlay work.LadderPolicy) work.LadderPolicy {
	out := current.Normalize()
	for _, f := range []struct {
		dst *int
		v   int
	}{
		{&out.ShadowMatches, overlay.ShadowMatches}, {&out.DistinctBindings, overlay.DistinctBindings},
		{&out.CanaryMatches, overlay.CanaryMatches}, {&out.FailuresToDemote, overlay.FailuresToDemote},
		{&out.InsufficientToDemote, overlay.InsufficientToDemote}, {&out.RetireAfterDays, overlay.RetireAfterDays},
	} {
		if f.v > 0 {
			*f.dst = f.v
		}
	}
	return out
}

// --- the work spine ------------------------------------------------------------------

// engineSpine is WorkSpine over integrations/work.
type engineSpine struct {
	e  *memqlengine.MemQLEngine
	wi *workspine.Integration
	// acts is the work spine as the engine has it registered -- a node's
	// materialization, with its database handle and row-admission gate --
	// which the person's acts are called on (procedure_harness_steps.go).
	acts memqlengine.IntegrationProvider
	// log is the lifecycle's, for the executor a goal's run is executed by.
	log *slog.Logger
}

func (s *engineSpine) OpenGoal(ctx context.Context, g GoalOrder) (string, string, error) {
	template := provingGoalTemplate
	variables := map[string]any{}
	for k, v := range g.Input {
		variables[k] = v
	}
	if g.ProcedureConstructId != "" {
		// What compile writes for a goal the ladder serves: the replay
		// template, and the construct among the run's variables.
		template = replayGoalTemplate
		variables["procedureConstructId"] = g.ProcedureConstructId
	}
	goalId, runId, err := s.wi.OpenDirectGoal(ctx, workspine.DirectGoal{
		OwnerUserId: g.OwnerUserId, Statement: g.Statement, AutomationName: template,
		Input: g.Input, RequestedVia: "api", TriggeredBy: provingGoalTrigger,
	})
	if err != nil {
		return "", "", err
	}
	// THE GOAL SIGNATURE, on the run, as compile records it: it is what the
	// app session's recording inherits, and a recording without one belongs
	// to no corpus.
	if err := s.wi.RecordCompileOutcome(ctx, g.OwnerUserId, runId, map[string]any{
		"goalSignature": g.GoalSignature, "variables": variables,
	}); err != nil {
		return "", "", fmt.Errorf("recording the goal signature on %s: %w", runId, err)
	}
	return goalId, runId, nil
}

func (s *engineSpine) CloseGoal(ctx context.Context, owner, runId string, served bool, why string) error {
	if !served {
		if strings.TrimSpace(why) == "" {
			why = "the goal was not served"
		}
		return s.wi.FailRun(ctx, owner, runId, goalNotServedErrCode, why)
	}
	return s.wi.RecordCompileOutcome(ctx, owner, runId, map[string]any{
		"status": "succeeded", "finishedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

// DecideApproval is integrations/work's own decide handler, as the person:
// the owner check and the artifact-hash gate run exactly as they would for a
// click in Approvals.
func (s *engineSpine) DecideApproval(ctx context.Context, owner, approvalId, decision string) error {
	for _, c := range s.wi.Capabilities() {
		if c.Name != "decideApproval" {
			continue
		}
		// This fixture represents the person's click, not the borrowed actor
		// used by a job. Production refuses synthetic/unranked/job decisions.
		person := common.ContextWithRun(ctx, common.RunContext{})
		person = auth.ContextWithToken(person, &auth.TokenInfo{Subject: owner})
		person = auth.ContextWithAccess(person, &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
		_, err := c.Handler(person, map[string]any{"approvalId": approvalId, "decision": decision}, 0)
		return err
	}
	return fmt.Errorf("proving: integrations/work offers no decideApproval capability")
}

// PromotionApprovals reads the owner's promotions back: every one still
// pending, as the owner's own list shows it, and each the lifecycle saw
// raised, by id -- a decided approval leaves the owner's list, and only the
// by-id read (server-only, the cluster's) still answers what it became.
func (s *engineSpine) PromotionApprovals(ctx context.Context, owner string, known []string) ([]ApprovalState, error) {
	byId := map[string]ApprovalState{}
	q, err := langparser.RenderCall("workApprovalsForOwner", nil)
	if err != nil {
		return nil, err
	}
	pending, err := engineRows(auth.ContextWithUserActor(ctx, owner), s.e, "query "+q)
	if err != nil {
		return nil, fmt.Errorf("reading the owner's pending approvals: %w", err)
	}
	for _, r := range pending {
		if a, ok := promotionState(r); ok {
			byId[memqlengine.BareShortId(a.ApprovalId)] = a
		}
	}
	for _, id := range known {
		q, err := langparser.RenderCall("workApprovalById", map[string]any{"approvalId": id})
		if err != nil {
			return nil, err
		}
		rows, err := engineRows(benchContext(ctx), s.e, "query "+q)
		if err != nil {
			return nil, fmt.Errorf("reading approval %s back: %w", id, err)
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("approval %s was raised and is not readable back", id)
		}
		if a, ok := promotionState(rows[0]); ok {
			byId[memqlengine.BareShortId(a.ApprovalId)] = a
		}
	}
	out := make([]ApprovalState, 0, len(byId))
	for _, a := range byId {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ApprovalId < out[j].ApprovalId })
	return out, nil
}

func promotionState(r map[string]any) (ApprovalState, bool) {
	if strField(r, "kind") != work.ApprovalKindProcedurePromotion {
		return ApprovalState{}, false
	}
	subject, _ := r["subject"].(map[string]any)
	return ApprovalState{
		ApprovalId:  strField(r, "id"),
		ConstructId: strField(subject, "constructId"),
		Decision:    strField(r, "decision"),
	}, true
}

// --- the runner ------------------------------------------------------------------------

// engineLadder is ProcedureLadder over integrations/procedure.
type engineLadder struct {
	e     *memqlengine.MemQLEngine
	integ *procedure.Integration
}

func (l *engineLadder) LearnFromRun(ctx context.Context, owner, runId string) (LearnReport, error) {
	res, err := l.integ.LearnFromRun(auth.ContextWithUserActor(ctx, owner), runId)
	if err != nil {
		return LearnReport{}, err
	}
	return LearnReport{ConstructId: res.ConstructId, Lift: string(res.Lift), Rung: string(res.Rung), Reason: res.Reason}, nil
}

func (l *engineLadder) ShadowCompare(ctx context.Context, owner, runId string) ([]ReplayReport, error) {
	outs, err := l.integ.ShadowCompare(auth.ContextWithUserActor(ctx, owner), runId)
	if err != nil {
		return nil, err
	}
	reports := make([]ReplayReport, 0, len(outs))
	for _, o := range outs {
		reports = append(reports, replayReportOf("", string(procedure.ReplayShadow), o))
	}
	return reports, nil
}

func (l *engineLadder) DecidePromotion(ctx context.Context, _ string, approvalId string) (LadderMove, error) {
	actor := auth.MaintenanceActor(promotionAutomation)
	if actor == nil {
		return LadderMove{}, fmt.Errorf("proving: %s is not a maintenance automation, so nothing can apply a promotion decision as it", promotionAutomation)
	}
	t, err := l.integ.DecidePromotion(auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, actor)), approvalId)
	if err != nil {
		return LadderMove{}, err
	}
	return LadderMove{From: string(t.From), To: string(t.To), Reason: t.Reason}, nil
}

func (l *engineLadder) Replay(ctx context.Context, o ReplayOrder) (ReplayReport, error) {
	out, err := l.integ.Replay(auth.ContextWithUserActor(ctx, o.OwnerUserId), procedure.ReplayRequest{
		OwnerUserId: o.OwnerUserId, ConstructId: o.ConstructId, Mode: procedure.ReplayMode(o.Mode),
		GoalRunId: o.GoalRunId, GoalId: o.GoalId, Statement: o.Statement, StepKey: o.StepKey, Input: o.Input,
	})
	if err != nil {
		return ReplayReport{}, err
	}
	return replayReportOf(o.ConstructId, o.Mode, out), nil
}

func replayReportOf(constructId, mode string, o procedure.ReplayOutcome) ReplayReport {
	rep := ReplayReport{
		ConstructId: constructId, Mode: mode,
		Served: o.Served, Match: o.Match, Diverged: o.Diverged, DivergedStep: o.DivergedStep,
		Diagnosis: o.Diagnosis, StartRefused: o.StartRefused, FellBack: o.FellBack, ModelCalls: o.ModelCalls,
		Proposed: o.ApprovalId != "", PromotionApprovalId: o.ApprovalId,
		From: string(o.Transition.From), To: string(o.Transition.To), Rung: string(o.Rung),
		ReplayRunId: o.ReplayRunId, Code: o.Code, NotCompared: o.NotCompared,
	}
	for _, c := range o.Completed {
		rep.Completed = append(rep.Completed, CompletedReport{Index: c.Index, Tool: c.Tool, IdempotencyKey: c.IdempotencyKey, SideEffect: c.SideEffect})
	}
	return rep
}

// ProcedureFor is compile's exact tier for one signature: the owner's learned
// constructs on a comparing or serving rung, the one that serves first --
// trusted, then canary, then shadow -- as compile ranks them.
func (l *engineLadder) ProcedureFor(ctx context.Context, owner, sig string) (ConstructState, bool, error) {
	q, err := langparser.RenderCall("procedureConstructsForGoalSignature", map[string]any{"goalSignature": sig})
	if err != nil {
		return ConstructState{}, false, err
	}
	rows, err := engineRows(auth.ContextWithUserActor(ctx, owner), l.e, "query "+q)
	if err != nil {
		return ConstructState{}, false, err
	}
	rank := map[string]int{string(work.RungTrusted): 0, string(work.RungCanary): 1, string(work.RungShadow): 2}
	var best *ConstructState
	for _, r := range rows {
		cs := constructStateOf(r)
		if cs.GoalSignature != sig {
			continue
		}
		if _, on := rank[cs.Rung]; !on {
			continue
		}
		if best == nil || rank[cs.Rung] < rank[best.Rung] {
			c := cs
			best = &c
		}
	}
	if best == nil {
		return ConstructState{}, false, nil
	}
	return *best, true, nil
}

func (l *engineLadder) Constructs(ctx context.Context, owner string) ([]ConstructState, error) {
	q, err := langparser.RenderCall("learnedProceduresForOwner", nil)
	if err != nil {
		return nil, err
	}
	rows, err := engineRows(auth.ContextWithUserActor(ctx, owner), l.e, "query "+q)
	if err != nil {
		return nil, err
	}
	out := make([]ConstructState, 0, len(rows))
	for _, r := range rows {
		out = append(out, withProcedure(constructStateOf(r), r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConstructId < out[j].ConstructId })
	return out, nil
}

func constructStateOf(r map[string]any) ConstructState {
	return ConstructState{
		ConstructId: strField(r, "id"), GoalSignature: strField(r, "goalSignature"),
		Rung: strField(r, "ladder"), PromotionApprovalId: strField(r, "promotionApprovalId"),
	}
}

// --- the seams a node installs, over the proving world -----------------------------

// engineCompileGate is Gate 1 over the engine -- the call app's
// CognitionEngineAdapter makes. component/automations links the automation
// compiler into this binary (its init), so an automation is compiled, not
// reported skipped.
type engineCompileGate struct{ e *memqlengine.MemQLEngine }

func (g engineCompileGate) CompileBundle(cs []memqlengine.SandboxConstruct) memqlengine.SandboxReport {
	return memqlengine.SandboxCompileBundleWithEngine(cs, g.e)
}

// worldDispatcher runs a replay's step on the proving world's machine.
type worldDispatcher struct{ w *ProcedureWorld }

func (d worldDispatcher) Dispatch(_ context.Context, req procedure.DispatchRequest) (procedure.DispatchResult, error) {
	if req.Tool != "exec" {
		// A Go error is "the step did not run": the fake world has a machine
		// and nothing else, so it cannot run anything but a command.
		return procedure.DispatchResult{}, fmt.Errorf("the proving world runs commands only, and this step is %s", req.Tool)
	}
	var ans execAnswer
	switch c := req.Args["command"].(type) {
	case string:
		ans = d.w.exec(c, req.IdempotencyKey, req.Sandbox)
	case []any:
		argv := make([]string, 0, len(c))
		for _, e := range c {
			argv = append(argv, fmt.Sprint(e))
		}
		ans = d.w.execArgv(argv, req.IdempotencyKey, req.Sandbox)
	default:
		return procedure.DispatchResult{}, fmt.Errorf("the step names no command")
	}
	isError, exit := ans.IsError, ans.ExitCode
	return procedure.DispatchResult{
		Observation: work.StepObservation{IsError: &isError, ExitCode: &exit, ResultType: work.InferTextType(ans.Stdout)},
		Output:      ans.Stdout,
		// The fake machine is the world outside the replay's workspace: a
		// command it counted reached it, and the app must be told so.
		Delivered: ans.Delivered,
	}, nil
}

// worldProber answers what the proving world's machine IS: its platform, each
// script it declares at its version, and the fresh workspace every replay
// starts in. A learned tool the machine does not have is left out -- unmeasured,
// which never holds.
type worldProber struct{ w *ProcedureWorld }

func (p worldProber) Probe(_ context.Context, _ work.ReplayTarget, _, _ string, learned procedure.Preconditions) (procedure.Preconditions, error) {
	tools := map[string]string{}
	for name := range learned.Tools {
		if _, ok := p.w.scripts[name]; ok {
			tools[name] = provingToolVersion
		}
	}
	empty := true
	return procedure.Preconditions{
		Platform:       map[string]string{"os": provingMachineOS, "arch": provingMachineArch},
		Tools:          tools,
		EmptyWorkspace: &empty,
	}, nil
}

// appFallback hands a goal the replay could not finish to the fixture app.
type appFallback struct{ app AppHandover }

func (f appFallback) Handover(ctx context.Context, req procedure.FallbackRequest) (procedure.FallbackOutcome, error) {
	completed := make([]CompletedReport, 0, len(req.Guidance.Completed))
	for _, c := range req.Guidance.Completed {
		completed = append(completed, CompletedReport{Index: c.Index, Tool: c.Tool, IdempotencyKey: c.IdempotencyKey, SideEffect: c.SideEffect})
	}
	res, err := f.app.Handover(ctx, HandoverOrder{
		OwnerUserId: req.OwnerUserId, GoalRunId: req.RunId, Completed: completed,
		Diagnosis: req.Guidance.Diagnosis, Prompt: req.Guidance.Prompt,
	})
	if err != nil {
		return procedure.FallbackOutcome{}, err
	}
	return procedure.FallbackOutcome{ChildRunId: res.ChildRunId, SessionId: res.SessionId, Content: "the fixture app took the goal over"}, nil
}

// --- helpers --------------------------------------------------------------------------

// warningLog keeps every record at WARN or above that the platform's pieces
// log during one lifecycle, and passes everything to the bench's own logger.
type warningLog struct {
	next *slog.Logger
	mu   sync.Mutex
	seen []string
}

func newWarningLog(next *slog.Logger) *warningLog { return &warningLog{next: next} }

func (w *warningLog) logger() *slog.Logger { return slog.New(warningHandler{w: w}) }

func (w *warningLog) warnings() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

type warningHandler struct {
	w     *warningLog
	attrs []slog.Attr
}

func (h warningHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || (h.w.next != nil && h.w.next.Enabled(ctx, l))
}

func (h warningHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		var b strings.Builder
		b.WriteString(r.Message)
		for _, a := range h.attrs {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
		}
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		h.w.mu.Lock()
		h.w.seen = append(h.w.seen, b.String())
		h.w.mu.Unlock()
	}
	if h.w.next != nil && h.w.next.Enabled(ctx, r.Level) {
		return h.w.next.Handler().WithAttrs(h.attrs).Handle(ctx, r)
	}
	return nil
}

func (h warningHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return warningHandler{w: h.w, attrs: append(append([]slog.Attr(nil), h.attrs...), as...)}
}

func (h warningHandler) WithGroup(string) slog.Handler { return h }

// EVERY CALL HERE IS RENDERED BY langparser.RenderCall WITH ITS CONSTRUCT
// NAMED AS A LITERAL, and that is for a gate rather than for taste:
// test/dslconformance's server-only-callers gate finds a Go caller of a
// @serverOnly construct by that spelling (or a `mutation name(` literal), and
// holds the file to a stamp. A name passed through a variable -- this package's
// own renderCall, which the bench rows use -- is invisible to it.

// engineRows runs one read and materializes its rows.
func engineRows(ctx context.Context, e *memqlengine.MemQLEngine, q string) ([]map[string]any, error) {
	res, err := e.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	return memqlengine.MaterializeRows(res), nil
}

func strField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// intField reads one of the ladder's values: a small whole number the schema
// holds at one or more. The ZERO answer (core/num): a value that does not fit,
// or is not whole, lands where an absent field does -- and the read-back
// comparison refuses a zero, because the schema never stores one.
func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		if v != math.Trunc(v) {
			return 0
		}
		return num.Float64OrZero(v)
	case int:
		return v
	case int64:
		return num.Int64OrZero(v)
	}
	return 0
}
