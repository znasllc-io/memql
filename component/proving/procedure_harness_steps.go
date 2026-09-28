package proving

// procedure_harness_steps.go -- a person stepping into a goal, over the REAL
// engine (epic memql#5414, task memql#5420): the goal's run executed through
// the automation executor, the re-run and the verdict through the work spine's
// own acts, and every version read back through the step timeline's read.
//
// WHAT IS THE PLATFORM'S OWN. The executor is component/automations' -- the
// journal writes the step's versions, the receipts and the run's head, and
// closes the run. The acts are integrations/work's rerunStep and
// recordFeedback, called as the person, exactly as DecideApproval calls
// decideApproval. The re-run is served the way the agent's dispatcher serves
// one (app/integrations_work_dispatch.go dispatchRerun): the run's journal
// read back, its prefix sources loaded, automations.PrepareRerun, and
// ResumeFrom with the same automation and step registry. The recording is the
// session writer's, which stamps the step row with the recording as its
// childRunId -- the session delegate's stamp, same mutation, same actor, same
// origin -- because the fixture app opens its recording naming that row.
//
// WHAT IS THE HARNESS'S. Three things a node would have: the automation the
// goal's run names (one step, handed to the app), the step registry that runs
// that step by asking the fixture app, and the execution context the agent's
// dispatcher builds from the run row (app/work_execution_context.go). The
// last is the only code here that mirrors code elsewhere, and it is kept to
// what that function does for a goal-owned run that forks nothing.
//
// THE WORK SPINE IS REGISTERED ON THE ENGINE, once. The learning corpus judges
// a recording by its parent step version through the workStepVersions BUILTIN
// (integrations/procedure parent_version.go), and a builtin is answered only
// by an integration the engine has registered -- which a node does for every
// plug-in at boot (app.materializePlugins) and the bench did for none. Without
// it the corpus reads nothing about a parent, logs at Info, and learns from a
// recording a person disliked: the correction this harness exists to measure
// would pass or fail for a reason no figure could see.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/core/common"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// appStepFunction is what the goal's one step calls, as its journal row's call
// names it: the app the step is handed to.
const appStepFunction = "fixtureApp"

// workIntegrationName is the work spine's plug-in, as it registers itself.
const workIntegrationName = "work"

// --- the work spine, as a node materializes it --------------------------------

// workActs is the work spine's integration as a node materializes it, registered
// on the engine the first time a lifecycle opens. Built by the plug-in's OWN
// factory with the plug-in context a node gives it -- the engine, the database
// handle, the row-admission gate -- so the acts and the builtins behave as on a
// node, the version-history read included, which refuses without the last two.
func (h *engineLifecycle) workActs() (memqlengine.IntegrationProvider, error) {
	h.once.Do(func() {
		if p := h.e.IntegrationByName(workIntegrationName); p != nil {
			h.work = p
			return
		}
		if h.db == nil {
			h.workErr = fmt.Errorf("proving: the learned-procedure harness has no database handle, so the work spine's version history cannot be read")
			return
		}
		for _, p := range memqlengine.RegisteredPlugins() {
			if p.Name != workIntegrationName {
				continue
			}
			if err := p.ValidateContract(); err != nil {
				h.workErr = err
				return
			}
			db := h.db
			prov, err := p.Factory(memqlengine.PluginContext{
				Logger:         slog.New(switchHandler{s: h.workLog}),
				Engine:         h.e,
				BunDB:          func() *bun.DB { return db },
				DirectBunDB:    func() *bun.DB { return db },
				AdmitSourceRow: memqlengine.AdmitSourceRow,
			})
			switch {
			case err != nil:
				h.workErr = fmt.Errorf("proving: materializing the work spine: %w", err)
			case prov == nil:
				h.workErr = fmt.Errorf("proving: the work spine's plug-in opted out of this engine")
			default:
				if err := h.e.RegisterIntegration(prov); err != nil {
					h.workErr = fmt.Errorf("proving: registering the work spine on the engine: %w", err)
					return
				}
				h.work = prov
			}
			return
		}
		h.workErr = fmt.Errorf("proving: no %q plug-in is registered in this binary", workIntegrationName)
	})
	return h.work, h.workErr
}

// logSwitch routes the registered work spine's log to the lifecycle running
// now. The integration is materialized once per engine and outlives every
// lifecycle, and a warning it logs belongs to whichever lifecycle made it.
type logSwitch struct {
	mu       sync.Mutex
	current  *warningLog
	fallback *slog.Logger
}

func (s *logSwitch) set(w *warningLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = w
}

func (s *logSwitch) handler() slog.Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.current != nil:
		return warningHandler{w: s.current}
	case s.fallback != nil:
		return s.fallback.Handler()
	}
	return discardHandler{}
}

// switchHandler is a slog.Handler over a logSwitch, keeping the attributes a
// logger was built with so they reach whichever handler is current.
type switchHandler struct {
	s     *logSwitch
	attrs []slog.Attr
}

func (h switchHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.s.handler().Enabled(ctx, l)
}

func (h switchHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.s.handler().WithAttrs(h.attrs).Handle(ctx, r)
}

func (h switchHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return switchHandler{s: h.s, attrs: append(append([]slog.Attr(nil), h.attrs...), as...)}
}

func (h switchHandler) WithGroup(string) slog.Handler { return h }

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

// --- the goal's run, executed -------------------------------------------------

// ExecuteAppGoal executes the goal's run as the agent's dispatcher executes a
// compiled run with no step rows yet: ExecuteAdopted, on the run's own id, with
// the run's variables, in the context the run row gives it.
func (s *engineSpine) ExecuteAppGoal(ctx context.Context, step GoalStep, serve AppStepServer) (StepRun, error) {
	journal, err := automations.LoadRunJournal(ctx, s.e, step.RunId)
	if err != nil {
		return StepRun{}, fmt.Errorf("reading the goal's run back: %w", err)
	}
	if len(journal.Steps) > 0 || journal.FailedStep != "" {
		return StepRun{}, fmt.Errorf("the goal's run %s has already started; a goal is executed once and changed only by a person's act", step.RunId)
	}
	ectx, err := s.executionContext(ctx, journal)
	if err != nil {
		return StepRun{}, err
	}
	exec := s.executor(serve)
	defer exec.Close()
	run, err := exec.ExecuteAdopted(ectx, appGoalAutomation(step.StepKey), automations.RunAdoption{
		RunId:       step.RunId,
		TriggeredBy: "compiled",
		Variables:   journal.Variables,
		Journal:     journal,
	})
	if run == nil {
		return StepRun{}, fmt.Errorf("executing the goal's run %s: %w", step.RunId, err)
	}
	return s.closedAs(ctx, step.OwnerUserId, step.RunId, 1)
}

// RerunStep is the person's rerunStep act, then the request it wrote served as
// the agent's dispatcher serves one (dispatchRerun): nothing the act knew is
// carried across but the run row, exactly as between two nodes.
func (s *engineSpine) RerunStep(ctx context.Context, o RerunOrder, serve AppStepServer) (StepRun, error) {
	args := map[string]any{"runId": o.RunId, "stepKey": o.StepKey}
	if o.Level != "" {
		args["level"] = o.Level
	}
	reply, err := s.act(ctx, o.OwnerUserId, "rerunStep", args)
	if err != nil {
		return StepRun{}, err
	}
	version := 0
	if len(reply) > 0 {
		version = intField(reply[0], "version")
	}
	if version < 1 {
		return StepRun{}, fmt.Errorf("the re-run act answered no version (%v)", reply)
	}

	journal, err := automations.LoadRunJournal(ctx, s.e, o.RunId)
	if err != nil {
		return StepRun{}, fmt.Errorf("reading the re-run's run back: %w", err)
	}
	if journal.Rerun == nil {
		return StepRun{}, fmt.Errorf("the re-run act wrote no request on run %s", o.RunId)
	}
	if journal.Rerun.Reason != automations.RerunReasonRerun {
		return StepRun{}, fmt.Errorf("the re-run act wrote a %s request, not a re-run", journal.Rerun.Reason)
	}
	auto := appGoalAutomation(o.StepKey)
	var sources []*automations.RunJournal
	for _, id := range automations.RerunSources(journal, auto) {
		src, err := automations.LoadRunJournal(ctx, s.e, id)
		if err != nil {
			return StepRun{}, fmt.Errorf("reading run %s, which the re-run's prefix is served from: %w", id, err)
		}
		if src.OwnerUserId != journal.OwnerUserId || memqlengine.BareShortId(src.GoalId) != memqlengine.BareShortId(journal.GoalId) {
			return StepRun{}, fmt.Errorf("run %s, which the re-run's prefix would be served from, is not this goal's and owner's", id)
		}
		sources = append(sources, src)
	}
	resume, opts, err := automations.PrepareRerun(journal, sources, auto)
	if err != nil {
		return StepRun{}, fmt.Errorf("preparing the re-run: %w", err)
	}
	ectx, err := s.executionContext(ctx, journal)
	if err != nil {
		return StepRun{}, err
	}
	exec := s.executor(serve)
	defer exec.Close()
	run, err := exec.ResumeFrom(ectx, resume, auto, opts)
	if run == nil {
		return StepRun{}, fmt.Errorf("serving the re-run of %s: %w", o.RunId, err)
	}
	return s.closedAs(ctx, o.OwnerUserId, o.RunId, version)
}

// RecordFeedback is the person's recordFeedback act.
func (s *engineSpine) RecordFeedback(ctx context.Context, o FeedbackOrder) (string, error) {
	args := map[string]any{
		"runId": o.RunId, "stepKey": o.StepKey, "version": o.Version,
		"verdict": o.Verdict, "reason": o.Reason,
	}
	for _, axis := range o.Axes {
		args[axis] = true
	}
	reply, err := s.act(ctx, o.OwnerUserId, "recordFeedback", args)
	if err != nil {
		return "", err
	}
	if len(reply) == 0 || strField(reply[0], "observationId") == "" {
		return "", fmt.Errorf("the feedback act answered no observation (%v)", reply)
	}
	return strField(reply[0], "observationId"), nil
}

// StepVersions reads every version of every step of the run through the
// workStepVersions BUILTIN, through the engine, as the run's owner -- the read
// the step timeline makes, and the one the learning corpus judges a recording
// by. Through the engine rather than the capability, so the read is the one a
// caller of the engine gets.
func (s *engineSpine) StepVersions(ctx context.Context, owner, runId string) ([]StepVersionState, error) {
	q, err := langparser.RenderCall("workStepVersions", map[string]any{"runId": runId})
	if err != nil {
		return nil, err
	}
	rows, err := engineRows(auth.ContextWithUserActor(ctx, owner), s.e, "builtin "+q)
	if err != nil {
		return nil, fmt.Errorf("reading the versions of run %s: %w", runId, err)
	}
	out := make([]StepVersionState, 0, len(rows))
	for _, r := range rows {
		current, _ := r["current"].(bool)
		override, _ := r["override"].(map[string]any)
		out = append(out, StepVersionState{
			StepKey: strField(r, "key"), Version: intField(r, "version"), Status: strField(r, "status"),
			Current: current, ChildRunId: strField(r, "childRunId"), Level: strField(override, "level"),
		})
	}
	return out, nil
}

// act calls one of the work spine's capabilities as the person, as
// DecideApproval calls decideApproval: the owner check and every refusal run
// exactly as they would for a click in the Work app. It answers each node the
// act replied with, its payload laid over its id.
func (s *engineSpine) act(ctx context.Context, owner, name string, args map[string]any) ([]map[string]any, error) {
	if s.acts == nil {
		return nil, fmt.Errorf("proving: the work spine is not registered on this engine, so %s cannot be called", name)
	}
	for _, c := range s.acts.Capabilities() {
		if c.Name != name {
			continue
		}
		nodes, err := c.Handler(auth.ContextWithUserActor(ctx, owner), args, 0)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			m := map[string]any{}
			if len(n.Payload) > 0 {
				if err := json.Unmarshal(n.Payload, &m); err != nil {
					return nil, fmt.Errorf("decoding %s's reply: %w", name, err)
				}
			}
			m["id"] = n.ID
			out = append(out, m)
		}
		return out, nil
	}
	return nil, fmt.Errorf("proving: integrations/work offers no %s capability", name)
}

// closedAs reads how an execution left the run, as the run's owner reads it.
func (s *engineSpine) closedAs(ctx context.Context, owner, runId string, version int) (StepRun, error) {
	q, err := langparser.RenderCall("workRunForOwner", map[string]any{"runId": runId})
	if err != nil {
		return StepRun{}, err
	}
	rows, err := engineRows(auth.ContextWithUserActor(ctx, owner), s.e, "query "+q)
	if err != nil {
		return StepRun{}, fmt.Errorf("reading the goal's run back as its owner: %w", err)
	}
	if len(rows) == 0 {
		return StepRun{}, fmt.Errorf("the goal's run %s is not readable as its owner", runId)
	}
	return StepRun{Version: version, Status: strField(rows[0], "status")}, nil
}

// executor is an automation executor over this engine, with the app as its
// only step and the lifecycle's log as its logger -- so a journal write the
// engine refuses, which the executor logs and survives, is the lifecycle's
// warning and so a verifier failure.
func (s *engineSpine) executor(serve AppStepServer) *automations.Executor {
	return automations.NewExecutor(automations.ExecutorOptions{
		Engine:       s.e,
		StepRegistry: appStepRegistry{serve: serve},
		Logger:       s.log,
	})
}

// executionContext is the context the agent's dispatcher executes a run in,
// built from the run row alone (app/work_execution_context.go
// workExecutionContext, for a goal-owned run that is not a fork): the run on
// the context, so the journal writes it and its steps as the owner; the owner's
// persisted authority, so the steps run as them; and the run's budget scope.
func (s *engineSpine) executionContext(ctx context.Context, j *automations.RunJournal) (context.Context, error) {
	if strings.TrimSpace(j.OwnerUserId) == "" || strings.TrimSpace(j.GoalId) == "" {
		return nil, fmt.Errorf("run %s names no owner or no goal; a goal's run is executed as its owner", j.RunId)
	}
	if strings.TrimSpace(j.ForkedFromRunId) != "" {
		// A branch executes against its source's lineage, which this mirror
		// does not carry; executing one without it would serve its prefix
		// from nowhere.
		return nil, fmt.Errorf("run %s is a branch of %s, which the proving harness does not execute", j.RunId, j.ForkedFromRunId)
	}
	run := common.RunContext{RunId: j.RunId, GoalId: j.GoalId, OwnerUserId: j.OwnerUserId, Mode: j.Mode, ReplayPolicy: j.ReplayPolicy, ForkAtStepKey: j.ForkAtStepKey, Routing: j.Routing}
	if run.Mode == "" {
		run.Mode = common.RunModeLive
	}
	resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(ctx context.Context, query string) (any, error) {
		result, err := s.e.Execute(ctx, query)
		if err != nil || result == nil {
			return nil, err
		}
		return result.OutputPayload(), nil
	}), s.log)
	ctx, err := auth.ContextWithPersistedOwner(ctx, j.OwnerUserId, j.ExecutionAuthority, resolver)
	if err != nil {
		return nil, fmt.Errorf("binding the run's owner: %w", err)
	}
	ctx = common.ContextWithRun(ctx, run)
	return memqlengine.ContextWithBudgetScope(ctx, memqlengine.BudgetScopeId("run", j.RunId), memqlengine.BudgetScopeId("goal", j.GoalId)), nil
}

// appGoalAutomation is the automation an app-served goal's run names: one
// step, handed to the app. Built the same way every time, so the re-run finds
// the fingerprint the first execution recorded.
func appGoalAutomation(stepKey string) *automations.Automation {
	return &automations.Automation{
		Name: provingGoalTemplate,
		Steps: []*automations.Step{{
			ID:       stepKey,
			Name:     stepKey,
			Type:     automations.StepTypeFunction,
			OnError:  automations.ErrorStrategyStop,
			Function: &automations.FunctionStepConfig{Name: appStepFunction},
		}},
	}
}

// appStepRegistry runs the goal's step by handing it to the app -- the one
// thing a node's step registry does that the proving world cannot: reach an
// app. Everything around it is the executor's.
type appStepRegistry struct{ serve AppStepServer }

func (r appStepRegistry) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	started := time.Now()
	fail := func(msg string) (*automations.StepResult, error) {
		return &automations.StepResult{StepId: step.ID, Status: "failed", Error: msg, StartedAt: started, CompletedAt: time.Now()}, fmt.Errorf("%s", msg)
	}
	if stepCtx == nil || stepCtx.Execution == nil {
		return fail("proving: the app's step ran outside an execution, so there is no run to record it beneath")
	}
	runId := stepCtx.Execution.ID
	ans, err := r.serve(ctx, AppStep{RunId: runId, StepKey: step.ID, StepId: automations.WorkStepId(runId, step.ID)})
	switch {
	case err != nil:
		return fail(err.Error())
	case ans.Failed:
		return fail(wordOr(ans.Error, "the app's session failed"))
	}
	return &automations.StepResult{
		StepId: step.ID, Status: "completed",
		Result:    map[string]any{"childRunId": ans.ChildRunId, "sessionId": ans.SessionId},
		StartedAt: started, CompletedAt: time.Now(),
	}, nil
}

// --- the learned procedure, read back -----------------------------------------

// withProcedure adds to a construct read back what its version replays: the
// recordings it was generalized from, and its steps as the commands a replay
// runs. The procedure is decoded by the runner's own reader
// (integrations/procedure.DecodeProcedure) and each step written out by the
// runner's own writer (component/procedure.Materialize), a free parameter
// bound to the placeholder of the goal input that supplies it -- so a step
// reads as the scenario writes an action.
func withProcedure(cs ConstructState, row map[string]any) ConstructState {
	raw, ok := row["procedure"]
	if !ok || raw == nil {
		cs.Unreadable = "the construct carries no procedure"
		return cs
	}
	p, err := procedure.DecodeProcedure(raw)
	if err != nil {
		cs.Unreadable = err.Error()
		return cs
	}
	cs.RecordedFrom = append([]string(nil), p.RecordedFrom.RunIds...)
	if cs.GoalSignature == "" {
		// A version that is not re-runnable keeps no signature on its row;
		// the payload still names the goal it was learned for.
		cs.GoalSignature = p.GoalSignature
	}
	values := map[string]string{}
	for _, h := range p.Holes {
		switch h.Class {
		case proc.HoleConstant:
			values[h.Id] = h.Const
		case proc.HoleFree:
			if key, ok := p.InputMap[h.Id]; ok {
				values[h.Id] = "{{" + key + "}}"
				continue
			}
			values[h.Id] = "{{?" + h.Id + "}}"
		default:
			values[h.Id] = "{{?" + h.Id + "}}"
		}
	}
	for i, st := range p.Steps {
		v, err := proc.Materialize(st.Args, values)
		if err != nil {
			cs.Unreadable = fmt.Sprintf("step %d cannot be written out: %v", i, err)
			return cs
		}
		m, _ := v.(map[string]any)
		command, _ := m["command"].(string)
		if st.Tool != "exec" || command == "" {
			encoded, _ := json.Marshal(v)
			command = st.Tool + " " + string(encoded)
		}
		cs.Steps = append(cs.Steps, command)
	}
	return cs
}
