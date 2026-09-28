//go:build agent

package worker

// app_session_delegate.go -- the engine-side half of the `session` door (epic
// memql#5391, task memql#5392, design D7).
//
// It is the AppSessionDelegate twin of app_inference.go and sits beside it for
// the same reason: a session travels over the WorkerService stream, which
// terminates on the agent node, so the code that can open one is behind
// `//go:build agent` while the contract lives in component/memql.
//
// WHAT IT REUSES, AND WHY THAT IS THE DESIGN. The run goes through
// CockpitAppExecutor -- the SAME executor a delegated task uses -- because a
// routed step and a delegated task are the same act: opening an app session on
// somebody's machine with the owner's app gate (app_gate.go) in front of it. A
// second path here would be a second set of gates, and the two would disagree
// exactly once, on the case nobody tested. This file asks that gate itself,
// with the pin the router handed over, before it opens the child run -- then
// runs the admitted step through the executor. That executor is also the one
// thing in this tree that had no production caller on the delegation path;
// this file is that caller.
//
// THE SUBRUN IS A REAL RUN, and that is what makes `childRunId` worth reading.
// A step that says "delegated" and points at nothing leaves the reader with
// nowhere to go; a step that points at a v1:work:run they can open answers the
// question the field exists to answer. The child run is opened before the
// session and closed after it, on every outcome -- a run that burned an hour of
// somebody's subscription and then failed still happened.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/planner"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// appSessionTemplate names the child run's template. It is what a client
// filters the Work feed by, so it is a stable word rather than the app's id --
// "an app session ran this step" is the category; which app is a field.
const appSessionTemplate = "appSession"

// appSessionStepKey is the child run's single step.
const appSessionStepKey = "session"

// snapshotRestorePreamble opens the prompt of a session started from a
// workspace snapshot (epic memql#5414, design D19). The cockpit lands every
// input FLAT in the working directory under its Library file name, so the app
// is told, file by file, where each one belongs; one line per file follows it,
// `<landed name> -> <path>`.
const snapshotRestorePreamble = "Before you start, restore the workspace: each file below was delivered into " +
	"the working directory under the name on the left; move it to the path on the right (create directories as needed)."

// A recorded file's index row was written when the recording landed it, so it
// should already be there; the wait only covers promotion still in flight on
// another replica. It is short because a missing row is a refusal, and waiting
// longer only makes the refusal slower.
const (
	snapshotPromotionWait = 3 * time.Second
	snapshotPromotionPoll = 100 * time.Millisecond
)

// stepExecutor is the narrow half of CockpitAppExecutor this file needs: the
// app gate, asked with the door's pin before anything is opened, and the run
// of a step the gate admitted.
//
// Declared as an interface so the test can substitute a recorder without
// standing up a dispatcher, a router, a registry and a session runner -- four
// collaborators none of which this file's logic is about.
type stepExecutor interface {
	admitSession(ctx context.Context, ownerUserId string, pin memqlengine.AppDoorPin) *appGateRefusal
	runAdmitted(ctx context.Context, req planner.ExecutorRequest, progress planner.ProgressCallback) (planner.ExecutorResult, error)
}

// stepStamper writes the parent step's childRunId and makes the owner-scoped
// reads this file needs (the delegating run, a snapshot's Library rows). It is
// the engine, narrowed to Execute.
type stepStamper interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// AppSessionDelegate fills component/memql's step-handover seam.
type AppSessionDelegate struct {
	exec    stepExecutor
	journal *workjournal.Journal
	engine  stepStamper
	logger  *slog.Logger
	// snapshotWait and snapshotPoll bound the wait for a snapshot file's
	// index row; fields so a test about the timeout does not take seconds.
	snapshotWait time.Duration
	snapshotPoll time.Duration
}

// NewAppSessionDelegate builds the seam implementation over the executor the
// agent node already has, so there is ONE path onto somebody's machine rather
// than a second set of consent gates that could disagree with the first.
//
// A nil journal or engine degrades honestly: the session still runs, and the
// step simply carries no childRunId. Refusing the run because the bookkeeping
// is unwired would take the door down for a fault in the wrong layer.
func NewAppSessionDelegate(exec *CockpitAppExecutor, journal *workjournal.Journal, engine *memqlengine.MemQLEngine, logger *slog.Logger) *AppSessionDelegate {
	// A nil executor stays a nil INTERFACE, so RunStep refuses it as unwired
	// rather than asking the gate through a nil pointer.
	var executor stepExecutor
	if exec != nil {
		executor = exec
	}
	var stamper stepStamper
	if engine != nil {
		stamper = engine
	}
	return newAppSessionDelegateFor(executor, journal, stamper, logger)
}

func newAppSessionDelegateFor(exec stepExecutor, journal *workjournal.Journal, engine stepStamper, logger *slog.Logger) *AppSessionDelegate {
	if logger == nil {
		logger = slog.Default()
	}
	return &AppSessionDelegate{
		exec: exec, journal: journal, engine: engine, logger: logger,
		snapshotWait: snapshotPromotionWait, snapshotPoll: snapshotPromotionPoll,
	}
}

var _ memqlengine.AppSessionDelegate = (*AppSessionDelegate)(nil)

// RunStep opens a session for the handed-over step and runs it to completion.
func (d *AppSessionDelegate) RunStep(ctx context.Context, h memqlengine.AppSessionHandover) (memqlengine.AppSessionOutcome, error) {
	if d == nil || d.exec == nil {
		return memqlengine.AppSessionOutcome{}, fmt.Errorf(
			"app session: this node has no cockpit-app executor wired; a step can only be handed to an app " +
				"on an agent node running WorkerService")
	}
	owner := strings.TrimSpace(h.ActingUserId)
	if auth.NamesNoPerson(owner) {
		// The same refusal the app gate makes, made earlier so the child run
		// is never opened under an actor that names nobody -- a row written
		// that way is readable by nobody, including the operator asking what
		// ran.
		return memqlengine.AppSessionOutcome{}, fmt.Errorf(
			"app session: the step has %s; a machine-touching door cannot run unattributed", memqlengine.AppNoOwnerReason)
	}

	// THE APP GATE, BEFORE ANYTHING IS OPENED (app_gate.go): a pin the owner
	// did not make, and the kill switch. A refused step leaves no child run
	// behind and no childRunId on the parent step, and its refusal keeps its
	// own code rather than becoming a child run failed as app_session_failed
	// with the reason in its text.
	if refusal := d.exec.admitSession(ctx, owner, h.Pin); refusal != nil {
		return memqlengine.AppSessionOutcome{}, fmt.Errorf("app session: %w", refusal)
	}

	// THE STEP ROW this session serves. The handover names the run and the
	// step KEY, as the run context does; the journal's row is the two composed,
	// and it is the row the subrun is stamped on and the child goal is keyed
	// by. Stamping the key named no row, so a delegated step never carried its
	// childRunId.
	stepRowId := parentStepRowId(h.RunId, h.StepId)

	// WHAT THE SESSION IS STARTED WITH, decided before anything is opened: a
	// snapshot whose files cannot be resolved refuses the step, and a refusal
	// must not leave a child run behind for a session that never started.
	plan, err := d.planSession(ctx, owner, h)
	if err != nil {
		return memqlengine.AppSessionOutcome{}, err
	}

	child := d.beginChildRun(ctx, h, owner, stepRowId, plan)
	childRunId := child.RunID()
	if childRunId != "" {
		d.stampParentStep(ctx, owner, stepRowId, childRunId)
	}

	var childStep *workjournal.Step
	if child != nil {
		childStep = child.Step(ctx, appSessionStepKey)
	}

	res, runErr := d.exec.runAdmitted(ctx, planner.ExecutorRequest{
		StepId:      h.StepId,
		RunId:       h.RunId,
		AgentId:     h.AgentId,
		OwnerUserId: owner,
		Kind:        appSessionTemplate,
		Inputs:      plan.inputs,
		Input: map[string]any{
			"executorBackend": planner.BackendCockpitApp + ":" + h.AppId,
			"prompt":          plan.prompt,
			"level":           plan.level,
			"model":           plan.model,
			"effort":          plan.effort,
			// A directory NAME under the owner's workspace root, not a path:
			// sessionWorkspace composes it, exactly as it composes the run's
			// own directory when no fresh one was asked for.
			"freshWorkspace": plan.workspace,
			"responseSchema": schemaJSON(h.ResponseSchema),
			// THE SUBRUN OPENED ABOVE IS WHERE THE SESSION IS RECORDED (epic
			// memql#5396). It is the run childRunId now points at, so the
			// actions have to land in THAT one: a recording in a second run
			// would leave the pointer a reader follows aimed at a run holding
			// one step and no actions, which is the "points at nothing"
			// failure this file's own header warns about. Empty when the
			// journal is unwired, and the recorder then opens its own.
			"recordingRunId": childRunId,
		},
	}, nil)

	out := memqlengine.AppSessionOutcome{
		ChildRunId:       childRunId,
		SessionId:        outputString(res.Output, "sessionId"),
		ArtifactIds:      res.ArtifactIds,
		Model:            outputString(res.Output, "model"),
		Effort:           outputString(res.Output, "effort"),
		Billing:          res.Billing,
		ExecutionSurface: surfaceFor(h.AppId, outputString(res.Output, "workerId")),
	}
	// THE STRUCTURED ANSWER AND THE TRANSCRIPT ARE BOTH KEPT, and neither
	// substitutes for the other: a harness can answer the schema and still
	// exit non-zero, and a session that answered no schema still produced a
	// transcript. Content is the TEXT answer either way, because that is what
	// a chat surface renders -- the app's own output, not the transcript,
	// whose stderr carries the cockpit's diagnostics.
	out.Result = resultJSON(res.Output["result"])
	out.Content = outputString(res.Output, "answer")
	if out.Content == "" && len(out.Result) > 0 {
		out.Content = string(out.Result)
	}

	if runErr != nil {
		childStep.Failed(ctx, "app_session_failed", runErr.Error())
		child.Failed(ctx, "app_session_failed", runErr.Error())
		return out, runErr
	}
	childStep.Done(ctx, map[string]any{
		"sessionId":   out.SessionId,
		"app":         h.AppId,
		"artifactIds": out.ArtifactIds,
	})
	child.Succeeded(ctx, map[string]any{"sessionId": out.SessionId})
	return out, nil
}

// beginChildRun opens the subrun. A nil journal, or a begin that fails, yields
// a nil run whose methods are no-ops -- the session still runs, and the step
// carries no childRunId rather than the run being refused for a bookkeeping
// fault.
func (d *AppSessionDelegate) beginChildRun(ctx context.Context, h memqlengine.AppSessionHandover, owner, stepRowId string, plan sessionPlan) *workjournal.Run {
	if d.journal == nil {
		return nil
	}
	// The GOAL is stable per parent step ROW, so a retried step is two runs of
	// one goal rather than two goals -- which is what makes "this step was
	// delegated twice" readable. Keyed on the ROW and not the key: two runs
	// that each have a step called `draft` are two different steps, and a goal
	// keyed on the key alone merged their sessions into one goal. A handover
	// that names no run has no row, and keeps the key.
	goalKey := stepRowId
	if goalKey == "" {
		goalKey = strings.TrimSpace(h.StepId)
	}
	// THE CHILD INHERITS ITS PARENT'S GOAL (epic memql#5408, gap G2). The
	// session is recorded into this run, and procedure learning mines the
	// recordings of ONE goal signature: a child run that does not carry its
	// parent's belongs to no corpus, so the app's work on this goal could never
	// become a procedure. The parent's variables ride along for the same
	// reason -- they are the goal's input, and the lift maps each free
	// parameter to the input key that supplied it.
	signature, variables := d.parentRunInheritance(ctx, owner, h.RunId)
	run, err := d.journal.Begin(ctx, workjournal.Work{
		OwnerUserID: owner,
		Template:    appSessionTemplate,
		Statement:   fmt.Sprintf("run this step in %s", h.AppId),
		GoalKey:     goalKey,
		RunKey:      fmt.Sprintf("%s-%d", goalKey, time.Now().UTC().UnixNano()),
		Input: map[string]any{
			"app":           h.AppId,
			"level":         plan.level,
			"model":         plan.model,
			"effort":        plan.effort,
			"parentRunId":   h.RunId,
			"parentStepId":  goalKey,
			"parentStepKey": h.StepId,
		},
		// ONE step, and it is `reasoning`: an app session is an agent taking
		// its own decisions, which is the definition the journal's two kinds
		// draw the line on.
		Steps:         []workjournal.StepDecl{{Key: appSessionStepKey, Kind: workjournal.KindReasoning}},
		RequestedVia:  "router",
		GoalSignature: signature,
		ParentRunID:   strings.TrimSpace(h.RunId),
		Variables:     variables,
	})
	if err != nil {
		d.logger.Warn("app session: could not open the child run; the step will carry no childRunId",
			"parent_step_id", h.StepId, "error", err)
		return nil
	}
	return run
}

// parentRunInheritance reads the delegating run under its OWNER's actor --
// the composite tier answers anybody else zero rows and no error -- and
// returns the goal signature and variables the child run inherits. The
// variables are the GOAL'S INPUT (component/work.GoalInput): when a learned
// procedure served the parent and diverged, this session IS the app taking the
// goal back, and the replay's own procedure id is not an input anybody gave.
//
// BEST-EFFORT, for stampParentStep's reason: the session is about to run on
// somebody's machine either way. A parent that cannot be read costs the
// recording its place in a corpus and is logged, because a recording that
// silently belongs to no goal is the gap this exists to close.
func (d *AppSessionDelegate) parentRunInheritance(ctx context.Context, owner, parentRunId string) (string, map[string]any) {
	parentRunId = strings.TrimSpace(parentRunId)
	if d.engine == nil || parentRunId == "" {
		return "", nil
	}
	query, err := langparser.RenderCall("workRunForOwner", map[string]any{"runId": parentRunId})
	if err != nil {
		return "", nil
	}
	res, err := d.engine.Execute(auth.ContextWithUserActor(ctx, owner), "query "+query)
	if err != nil {
		d.logger.Warn("app session: could not read the delegating run; the recording will carry no goal signature",
			"parent_run_id", parentRunId, "error", err)
		return "", nil
	}
	rows := memqlengine.MaterializeRows(res)
	if len(rows) == 0 {
		d.logger.Warn("app session: the delegating run is not readable as its owner; the recording will carry no goal signature",
			"parent_run_id", parentRunId)
		return "", nil
	}
	signature, _ := rows[0]["goalSignature"].(string)
	variables, _ := rows[0]["variables"].(map[string]any)
	return strings.TrimSpace(signature), workstate.GoalInput(variables)
}

// stampParentStep records the subrun on the step ROW that was handed over --
// parentStepRowId's id, never the step key, which names no row.
//
// A FAILURE HERE IS A WARNING, NOT A REFUSAL. The session is about to run on
// somebody's machine either way; losing the back-pointer costs a reader a
// journey, and refusing the work would cost them the work.
func (d *AppSessionDelegate) stampParentStep(ctx context.Context, owner, stepId, childRunId string) {
	if d.engine == nil || strings.TrimSpace(stepId) == "" {
		return
	}
	// updateWorkStep is @serverOnly, so the call needs internal origin; and it
	// is owner-tiered, so it needs the owner's actor. Both, or the write is
	// refused and the step silently keeps no back-pointer.
	stampCtx := auth.ContextWithUserActor(auth.ContextWithInternalOrigin(ctx), owner)
	query := fmt.Sprintf("mutation updateWorkStep(stepId: %s, childRunId: %s)",
		langparser.QuoteString(stepId), langparser.QuoteString(childRunId))
	if _, err := d.engine.Execute(stampCtx, query); err != nil {
		d.logger.Warn("app session: could not record childRunId on the parent step",
			"step_id", stepId, "child_run_id", childRunId, "error", err)
	}
}

// parentStepRowId is the v1:work:step row a handed-over step is: the journal's
// composition of the run and the step key (automations.WorkStepId, the one
// definition of that id). "" when either is missing -- a handover that names
// no run has no row to point at.
func parentStepRowId(runId, stepKey string) string {
	runId, stepKey = strings.TrimSpace(runId), strings.TrimSpace(stepKey)
	if runId == "" || stepKey == "" {
		return ""
	}
	return automations.WorkStepId(runId, stepKey)
}

// sessionPlan is what a session is started WITH.
type sessionPlan struct {
	prompt string
	level  string
	model  string
	effort string
	inputs []string
	// workspace is a directory NAME under the owner's workspace root; empty
	// means the run's own directory.
	workspace string
}

// planSession decides what the session is started with: the handover as the
// door delivered it, then -- when a person re-ran or branched this step (epic
// memql#5414) -- their override (design D20), the execution's fresh
// workspace, and the snapshot the targeted step is restored from (D19).
//
// Everything is read off the RUN CONTEXT, which is the executor's to set: the
// override and the snapshot only on the one step a re-run targets, the fresh
// workspace on every step of that execution, so every session step of one
// re-run shares one new directory.
func (d *AppSessionDelegate) planSession(ctx context.Context, owner string, h memqlengine.AppSessionHandover) (sessionPlan, error) {
	plan := sessionPlan{
		prompt: h.Prompt,
		level:  strings.TrimSpace(h.Level),
		model:  strings.TrimSpace(h.Model),
		effort: strings.TrimSpace(h.Effort),
		inputs: append([]string(nil), h.Inputs...),
	}
	rc, inRun := common.RunFromContext(ctx)
	if !inRun {
		return plan, nil
	}
	if ov := rc.Override; ov != nil {
		if err := d.applyOverride(ctx, &plan, h, ov); err != nil {
			return plan, err
		}
	}
	if ws := strings.TrimSpace(rc.Workspace); ws != "" {
		// A name that is not one plain directory name would put the workspace
		// outside the root the owner chose, and silently falling back to the
		// run's own directory would run the new session in the tree the
		// previous version's later steps changed -- the thing the fresh
		// workspace exists to avoid. Refused, naming it.
		if !isWorkspaceDirName(ws) {
			return plan, fmt.Errorf("app session: the fresh workspace %q named for step %q is not a directory name", ws, h.StepId)
		}
		plan.workspace = ws
	}
	if snap := rc.Snapshot; snap != nil && len(snap.Files) > 0 {
		if plan.workspace == "" {
			return plan, fmt.Errorf("app session: step %q is restored from a workspace snapshot, which has to land "+
				"in a fresh workspace, and its execution names none", h.StepId)
		}
		restore, err := d.restoreSnapshot(ctx, owner, snap)
		if err != nil {
			return plan, err
		}
		plan.inputs = append(plan.inputs, restore.artifactIds...)
		plan.prompt = restore.preamble + "\n\n" + plan.prompt
	}
	return plan, nil
}

// applyOverride puts a person's override onto the session (design D20).
//
// THE PROMPT IS REPLACED ONLY WHEN IT IS THE WHOLE PROMPT. When a session
// answered the version being replaced, its prompt is recorded on
// v1:worker:appSession.prompt, which is what the person was shown and edited,
// so their text is the whole prompt the new session runs with and the
// flattened conversation the door handed over goes with it. When anything
// else answered it -- a model, or a learned procedure -- the person wrote
// INSTRUCTIONS, and a session handed only those would lose its goal: the
// handed-over prompt is kept, and the instructions are added unless the seam
// already put them in it (the tool loops do; a procedure's hand-back does not).
//
// THE KNOBS go through ApplyStepOverride, the function every prompt seam uses,
// so a level means one thing everywhere and an invalid one refuses here rather
// than on the machine after a session was opened. The model is the part after
// `app:<id>:` when the override names THIS app; any other door is one the
// router would have chosen instead, and reaching here with it means the
// request bypassed the seam -- logged, and the door's own model kept.
func (d *AppSessionDelegate) applyOverride(ctx context.Context, plan *sessionPlan, h memqlengine.AppSessionHandover, ov *common.StepOverride) error {
	req, err := memqlengine.ApplyStepOverride(ctx, airoute.ResolveRequest{
		Level:  airoute.Level(plan.level),
		Effort: plan.effort,
	})
	if err != nil {
		return fmt.Errorf("app session: %w", err)
	}
	plan.level, plan.effort = string(req.Level), req.Effort
	if pinned := strings.TrimSpace(ov.Model); pinned != "" {
		appId, model, isApp := memqlengine.SplitAppReference(pinned)
		if isApp && appId == h.AppId {
			if model != "" {
				plan.model = model
			}
		} else {
			d.logger.Warn("app session: the step's override pins a door other than the app this session runs in; the override's model is not applied",
				"step_id", h.StepId, "app", h.AppId, "override_model", pinned)
		}
	}
	if strings.TrimSpace(ov.Prompt) != "" {
		if ov.WholePrompt {
			plan.prompt = ov.Prompt
		} else if instructions := memqlengine.StepOverrideInstructions(ov); !strings.Contains(plan.prompt, instructions) {
			plan.prompt = strings.TrimRight(plan.prompt, "\n") + "\n\n" + instructions
		}
	}
	// The guidance is appended ONCE. Without a replaced prompt the handed-over
	// conversation already carries it (the seam appended it before the door
	// flattened the turn), and saying it twice would weigh one complaint as
	// two.
	if guidance := memqlengine.StepOverrideGuidance(ov); guidance != "" && !strings.Contains(plan.prompt, guidance) {
		plan.prompt = strings.TrimRight(plan.prompt, "\n") + "\n\n" + guidance
	}
	return nil
}

// snapshotRestore is what a workspace snapshot contributes to a session: the
// artifacts the cockpit lands, and the preamble saying where each belongs.
type snapshotRestore struct {
	artifactIds []string
	preamble    string
}

// restoreSnapshot resolves every file of a workspace snapshot to the Library
// ARTIFACT the cockpit can pull -- AppSessionStart.inputs takes artifact ids,
// and the snapshot holds file ids -- and to the NAME the file lands under,
// which is the file row's own because the content route's Content-Disposition
// carries it. The preamble maps each landed name to the path it came from.
//
// Every read is the OWNER's: the Library is owner-tiered, and a snapshot read
// as anybody else would answer zero rows and look like a file that was never
// recorded. A file that cannot be resolved REFUSES the step, naming it: a
// session restored from a partial snapshot diverges silently, which is the
// same reason the act that builds a snapshot refuses one with an omitted file.
func (d *AppSessionDelegate) restoreSnapshot(ctx context.Context, owner string, snap *common.WorkspaceSnapshot) (snapshotRestore, error) {
	if d.engine == nil {
		return snapshotRestore{}, fmt.Errorf(
			"app session: the step is restored from a workspace snapshot and this node has no engine to resolve its files through")
	}
	readCtx := auth.ContextWithUserActor(ctx, owner)
	var out snapshotRestore
	var b strings.Builder
	b.WriteString(snapshotRestorePreamble)
	for _, f := range snap.Files {
		path := strings.TrimSpace(f.Path)
		fileId := memqlengine.BareShortId(strings.TrimSpace(f.FileId))
		if path == "" || fileId == "" {
			return snapshotRestore{}, fmt.Errorf(
				"app session: the workspace snapshot has an entry with no path or no file (path %q, file %q)", f.Path, f.FileId)
		}
		name, err := d.snapshotFileName(readCtx, fileId, path)
		if err != nil {
			return snapshotRestore{}, err
		}
		artifactId, err := d.snapshotArtifactId(readCtx, fileId, path)
		if err != nil {
			return snapshotRestore{}, err
		}
		out.artifactIds = append(out.artifactIds, artifactId)
		b.WriteString("\n" + name + " -> " + path)
	}
	out.preamble = b.String()
	return out, nil
}

// snapshotFileName reads the name a snapshot file lands under.
func (d *AppSessionDelegate) snapshotFileName(ctx context.Context, fileId, path string) (string, error) {
	query, err := langparser.RenderCall("libraryFileById", map[string]any{"fileId": fileId})
	if err != nil {
		return "", err
	}
	res, err := d.engine.Execute(ctx, "query "+query)
	if err != nil {
		return "", fmt.Errorf("app session: reading the snapshot's file for %s (%s): %w", path, fileId, err)
	}
	for _, row := range memqlengine.MaterializeRows(res) {
		id, _ := row["id"].(string)
		if memqlengine.BareShortId(strings.TrimSpace(id)) != fileId {
			continue
		}
		if name, _ := row["name"].(string); strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name), nil
		}
	}
	return "", fmt.Errorf("app session: the snapshot's file for %s (%s) is not in the owner's Library, so the workspace cannot be restored", path, fileId)
}

// snapshotArtifactId resolves a snapshot file to its Library index row, the
// way the vision stager's waitForPromotion does -- the id is read, never
// derived, because deriving it would put a copy of a DSL expression in Go.
// The wait is short: a recorded file was promoted when the recording landed
// it, and an absent row is a refusal.
func (d *AppSessionDelegate) snapshotArtifactId(ctx context.Context, fileId, path string) (string, error) {
	query, err := langparser.RenderCall("libraryArtifactBySourceConceptRef",
		map[string]any{"sourceConceptRef": "v1:library:file:" + fileId})
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(d.snapshotWait)
	for {
		res, err := d.engine.Execute(ctx, "query "+query)
		if err != nil {
			return "", fmt.Errorf("app session: resolving the Library artifact of the snapshot's file for %s (%s): %w", path, fileId, err)
		}
		for _, row := range memqlengine.MaterializeRows(res) {
			if id, _ := row["id"].(string); strings.TrimSpace(id) != "" {
				return memqlengine.BareShortId(strings.TrimSpace(id)), nil
			}
		}
		if !time.Now().Add(d.snapshotPoll).Before(deadline) {
			return "", fmt.Errorf("app session: the snapshot's file for %s (%s) has no Library artifact to hand the "+
				"session within %s, so the workspace cannot be restored", path, fileId, d.snapshotWait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(d.snapshotPoll):
		}
	}
}

// surfaceFor names where the session ran, in the form the ledger stores for a
// delegated run. It is `cockpit-app:<appId>` rather than app_inference.go's
// `app:<appId>@<registrationId>`, and the difference is deliberate: that one is
// one TURN inside MemQL's own reasoning, this one is a whole step handed over,
// and one surface string for both would make the ledger unable to tell them
// apart.
func surfaceFor(appId, workerId string) string {
	if strings.TrimSpace(appId) == "" {
		return ""
	}
	surface := planner.BackendCockpitApp + ":" + appId
	if w := strings.TrimSpace(workerId); w != "" {
		surface += "@" + w
	}
	return surface
}

// schemaJSON renders the output contract for the executor's input map. An
// absent schema is the EMPTY STRING, which is what "none was asked for" means
// on AppSessionStart -- distinct from asking and getting nothing back.
func schemaJSON(schema *common.StructuredSchema) string {
	if schema == nil || len(schema.Schema) == 0 {
		return ""
	}
	return string(schema.Schema)
}

// resultJSON re-encodes the executor's decoded structured answer.
//
// The executor decodes it for its own output map and this re-encodes it, which
// looks like waste and is not: the seam's contract is raw JSON, and a caller
// handed `any` would have to know whether the harness answered an object, a
// string or nothing -- the three cases the executor's own decoder collapses.
func resultJSON(v any) []byte {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// outputString reads a string off the executor's output map. A missing key and
// a non-string value both read as empty, which is the honest answer: the
// executor writes what it knows, and what it did not write is not known.
func outputString(out map[string]any, key string) string {
	if out == nil {
		return ""
	}
	s, _ := out[key].(string)
	return strings.TrimSpace(s)
}
