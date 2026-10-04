package automations

// resume.go -- resume a run from the work journal (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D).
//
// The journal rows replace the checkpoint side-record: a run's own row
// carries what the checkpoint carried (the input envelope, the trigger,
// the caller-supplied flag, the chain heads, the step order, the
// automation fingerprint) and each step row carries its trimmed result.
// LoadRunJournal reads them under the journal's own cluster actor, AFTER
// the caller's handler has enforced who may resume -- the rows are the
// deployment's, and an admin who may resume must not be refused by the
// tier on the read.
//
// THE SECURITY RULE IS UNCHANGED (memql#2888, memql#2890): internal
// origin on resume requires a TRUSTED source AND a trigger payload the
// caller did not supply. CallerSuppliedPayload rides on the run row for
// exactly that reason.
//
// THE RETRYABLE RULE IS THE IDEMPOTENCY RULE'S A1 FORM (spec section D):
// a completed step is served from the journal and never re-run; a step with
// no external effect (a query, logic or builtin call, a for, a parallel, a
// sub-automation) is re-run; a `mutation` call, a publish or an action at the
// resume point needs AllowSideEffects (stepRetryable), because the journal
// cannot yet tell whether its far side already holds a receipt. The body
// resumes by running again over its recorded values (resume_statements.go).
// Epic A2 wires the receipts and narrows this to "retried when
// idempotent by key".
//
// THE RESUMED RUN KEEPS ITS RUN ID. A resume is the same work continuing,
// not a new execution that happens to share a prefix -- so the rows it
// writes are new VERSIONS of the same run and the same steps, with
// attempt incremented, and a reader asking "what happened to run X" gets
// one story rather than two.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

var (
	// ErrRunNotFound is returned when no v1:work:run row carries the id.
	ErrRunNotFound = errors.New("work run not found")
	// ErrRunJournalInvalid is returned when the journal cannot be resumed
	// from: no run, no automation name, or no unfinished step.
	ErrRunJournalInvalid = errors.New("work run journal is invalid")
	// ErrAutomationChanged is returned when the automation's definition
	// fingerprint moved since the run started.
	ErrAutomationChanged = errors.New("automation definition changed since the run started")
	// ErrNonRetryableStep is returned when the resume point has an external
	// effect and AllowSideEffects was not set.
	ErrNonRetryableStep = errors.New("step is not safely retryable (a mutation call, a publish or an action)")
)

// RunJournal is what resume needs from the rows: the run's envelope and
// the completed steps' trimmed results.
type RunJournal struct {
	ExecutionAuthority    map[string]any
	HeartbeatAt           time.Time
	HasRunningStep        bool
	GoalId                string
	Status                string
	OwnerUserId           string
	Variables             map[string]any
	Mode                  string
	ReplayPolicy          string
	ForkedFromRunId       string
	ForkAtStepKey         string
	WaitingOn             map[string]any
	RunId                 string
	AutomationName        string
	TemplateVersion       string
	TemplateConstructId   string
	TemplateFingerprint   string
	TriggeredBy           string
	Input                 any
	InputFingerprint      string
	TriggerEvent          map[string]any
	CallerSuppliedPayload bool
	ChainHead             string
	InitialChainHead      string
	StepOrder             []string
	// Steps holds the completed (done) steps by key, in the MinimalStepResult
	// shape the evaluator is rehydrated from.
	Steps map[string]*MinimalStepResult
	// FailedStep is the key of the step at `failed` or `running` with no
	// receipt -- the default resume point.
	FailedStep string
	// StepStates is every step's latest row, by key: its status and the
	// attempt it recorded. A statement body resumes from these (epic
	// memql#5370): a `failed` statement that carries `on error continue` was
	// continued past, and stays so.
	StepStates map[string]StepState
	// MaxAttempt is the highest version each top-level step's newest row
	// records, by key: its attempt or its version, whichever is higher. A
	// step executed again runs one past it (epic memql#5414). It is read off
	// the collapsed newest rows, so after a head move re-asserted an earlier
	// version it is that version's number -- which is why a re-run takes the
	// plan's numbers as a floor (RerunSpec.Versions).
	MaxAttempt map[string]int
	// Head is the run's stored head (epic memql#5414, design D18): the
	// current version of every top-level step, an entry naming another run
	// when that version lives there. Nil on a run written before it existed.
	Head work.Head
	// StaleSteps are the steps whose current version was computed against an
	// upstream that is no longer current: written by a person's act, and
	// taken off one by one as a re-run gives each a finished new version.
	StaleSteps []string
	// Rerun is the pending re-run request, decoded from run.rerun; nil when
	// absent or cleared to {}.
	Rerun *RerunSpec
	// StepOverrides is the override each top-level step's newest row
	// records, by key: the override its head version ran with (the newest
	// row IS the head, a head move re-asserting the version it makes
	// current). Only steps somebody overrode appear. A replay of this run
	// applies them (ResumeOptions.Overrides, RunAdoption.Overrides), because
	// the model calls its journal holds were made with them.
	StepOverrides map[string]*common.StepOverride
	// Routing is the owner's routing choice for every model call of the run
	// (run.routing, the Ask route picker's), read off the row so the agent
	// that executes the run needs nothing from the node that took the turn.
	// The zero value routes by the rules.
	Routing common.RouteChoice
}

// StepState is one step's latest journal row: its status and its attempt.
type StepState struct {
	Status  string
	Attempt int
	// Version is the version the row belongs to (epic memql#5414); 0 on a
	// row written before the field existed, whose attempt is its version.
	Version int
}

// ResumeOptions configures resume behavior.
type ResumeOptions struct {
	// FromStep overrides the resume point (defaults to the failed step).
	FromStep string

	// AllowSideEffects permits retrying a mutation call, a publish or an
	// action. Without this flag, resuming from a non-retryable step returns
	// an error.
	AllowSideEffects bool

	// Rerun serves a re-run request (epic memql#5414, task memql#5415): the
	// run need not have a failed step, every step from the resume point on
	// runs as a new version, the steps before it are served from what they
	// recorded, and the override reaches the targeted step alone. FromStep
	// empty resumes where the request stands (rerunResumePoint);
	// PrepareRerun builds the journal and the options together.
	Rerun *RerunSpec

	// Overrides are the overrides a REPLAY applies, by top-level step key:
	// the replayed run's StepOverrides. Each reaches its own step alone, as a
	// re-run's does (withRunContext). A re-run's own request wins for the
	// step it targets.
	Overrides map[string]*common.StepOverride
}

// IsStepRetryable reports whether a step type can be re-run with no
// external effect.
func IsStepRetryable(stepType StepType) bool {
	switch stepType {
	case StepTypeEvent, StepTypeAction:
		return false
	}
	return true
}

// LoadRunJournal reads one run and its steps, under the journal's own
// synthetic cluster actor. The caller decides WHO may resume before
// calling this; the actor here exists because the rows are the
// deployment's and the reads carry `actor.isClusterOwner==true`.
func LoadRunJournal(ctx context.Context, exec journalExecutor, runId string) (*RunJournal, error) {
	if exec == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	runId = strings.TrimSpace(runId)
	if runId == "" {
		return nil, fmt.Errorf("%w: empty run id", ErrRunJournalInvalid)
	}
	jctx := journalContext(ctx)
	runCall, err := journalArgs("workRunById", map[string]any{"runId": runId})
	if err != nil {
		return nil, err
	}
	res, err := exec.Execute(jctx, "query "+runCall)
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runId, err)
	}
	runs := memql.MaterializeRows(res)
	if len(runs) == 0 {
		return nil, ErrRunNotFound
	}
	stepCall, err := journalArgs("workStepsForRun", map[string]any{"runId": runId})
	if err != nil {
		return nil, err
	}
	res, err = exec.Execute(jctx, "query "+stepCall)
	if err != nil {
		return nil, fmt.Errorf("load steps of run %s: %w", runId, err)
	}
	return runJournalFromRows(runs[0], memql.MaterializeRows(res))
}

// runJournalFromRows folds one run row and its step rows into a RunJournal.
// Row ids arrive canonical (v1:work:run:<short>); the short id is what the
// executor minted, and what a later write has to address.
func runJournalFromRows(run map[string]any, steps []map[string]any) (*RunJournal, error) {
	if run == nil {
		return nil, ErrRunNotFound
	}
	authority, _ := run["executionAuthority"].(map[string]any)
	j := &RunJournal{
		RunId:                 shortWorkId(stringField(run, "id")),
		GoalId:                stringField(run, "goalId"),
		Status:                stringField(run, "status"),
		OwnerUserId:           stringField(run, "ownerUserId"),
		ExecutionAuthority:    authority,
		Mode:                  stringField(run, "mode"),
		ReplayPolicy:          stringField(run, "replayPolicy"),
		ForkedFromRunId:       stringField(run, "forkedFromRunId"),
		ForkAtStepKey:         stringField(run, "forkAtStepKey"),
		AutomationName:        stringField(run, "automationName"),
		TemplateFingerprint:   stringField(run, "templateFingerprint"),
		TemplateConstructId:   stringField(run, "templateConstructId"),
		TemplateVersion:       stringField(run, "templateVersion"),
		TriggeredBy:           stringField(run, "triggeredBy"),
		Input:                 run["input"],
		InputFingerprint:      stringField(run, "inputFingerprint"),
		CallerSuppliedPayload: boolField(run, "callerSuppliedPayload"),
		ChainHead:             stringField(run, "chainHead"),
		InitialChainHead:      stringField(run, "initialChainHead"),
		Steps:                 map[string]*MinimalStepResult{},
		StepStates:            map[string]StepState{},
		MaxAttempt:            map[string]int{},
		Head:                  work.ParseHead(run["head"]),
		StaleSteps:            rowStringList(run["staleSteps"]),
		Rerun:                 rerunSpecFrom(run["rerun"]),
		Routing:               common.RouteChoiceFrom(run["routing"]),
	}
	j.HeartbeatAt, _ = time.Parse(time.RFC3339Nano, stringField(run, "heartbeatAt"))
	j.WaitingOn, _ = run["waitingOn"].(map[string]any)
	j.Variables, _ = run["variables"].(map[string]any)
	if ev, ok := run["triggerEvent"].(map[string]any); ok {
		j.TriggerEvent = ev
	}
	j.StepOrder = rowStringList(run["stepOrder"])
	for _, row := range steps {
		key := stringField(row, "key")
		if key == "" || isNestedStepKey(key) {
			// A nested key is a row of a logic a statement called (a logic's
			// statements journal under the calling statement's key): resume
			// re-runs that statement, never into it.
			continue
		}
		state := StepState{Status: stringField(row, "status"), Attempt: intField(row, "attempt"), Version: intField(row, "version")}
		j.StepStates[key] = state
		j.MaxAttempt[key] = max(state.Attempt, state.Version)
		if record, ok := row["override"].(map[string]any); ok {
			if o := stepOverrideFrom(record); o != nil {
				if j.StepOverrides == nil {
					j.StepOverrides = map[string]*common.StepOverride{}
				}
				j.StepOverrides[key] = o
			}
		}
		if stringField(row, "status") == "running" {
			j.HasRunningStep = true
		}
		switch stringField(row, "status") {
		case "done":
			m := &MinimalStepResult{StepId: key, Status: "completed"}
			if r, ok := row["result"].(map[string]any); ok {
				_ = mapStructFromPayload(r, m)
				m.StepId = key
			}
			j.Steps[key] = m
		case "failed", "running":
			// `running` with no later receipt is a step the executor reached
			// and never finished -- a crash mid-step -- and it resumes from
			// exactly where a `failed` one does.
			if j.FailedStep == "" {
				j.FailedStep = key
			}
		}
	}
	return j, nil
}

// ValidateRunJournal is the resume precondition: a run, a resume point,
// and an automation that has not changed underneath it.
func ValidateRunJournal(j *RunJournal, automation *Automation, idEngine *id.Engine) error {
	return validateRunJournal(j, automation, idEngine, false)
}

// validateRunJournal is ValidateRunJournal, with the resume point waived for a
// re-run: a finished run is exactly what a person runs a step of again, and its
// resume point is the step the request names rather than one that failed.
func validateRunJournal(j *RunJournal, automation *Automation, idEngine *id.Engine, rerun bool) error {
	if j == nil {
		return ErrRunJournalInvalid
	}
	if j.RunId == "" || j.AutomationName == "" {
		return fmt.Errorf("%w: missing run id or automation name", ErrRunJournalInvalid)
	}
	if j.FailedStep == "" && !rerun {
		return fmt.Errorf("%w: run %s has no failed or unfinished step to resume from", ErrRunJournalInvalid, j.RunId)
	}
	if j.TemplateFingerprint != "" && automation != nil && idEngine != nil {
		if current := automation.DefinitionFingerprint(idEngine); current != "" && current != j.TemplateFingerprint {
			return ErrAutomationChanged
		}
	}
	return nil
}

// shortWorkId strips the canonical prefix a read returns, leaving the short
// id the executor minted.
func shortWorkId(canonical string) string {
	if i := strings.LastIndex(canonical, ":"); i >= 0 {
		return canonical[i+1:]
	}
	return canonical
}

func stringField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func boolField(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// rowStringList reads a row's string list however it decoded: []any off a
// read, []string when a caller built the row.
func rowStringList(v any) []string {
	switch list := v.(type) {
	case []string:
		return append([]string(nil), list...)
	case []any:
		var out []string
		for _, s := range list {
			if k, ok := s.(string); ok {
				out = append(out, k)
			}
		}
		return out
	}
	return nil
}

// intField reads a step row's attempt however the row decoded it.
//
// narrowing: ZERO -- the one caller reads 0 as "no attempt recorded" and
// resumes as the second attempt (attemptBase); a number out of range is no
// attempt this executor wrote, and saturating would claim an attempt count
// nobody made.
func intField(m map[string]any, k string) int {
	switch n := m[k].(type) {
	case int:
		return n
	case int64:
		return num.Int64OrZero(n)
	case float64:
		return num.Float64OrZero(n)
	}
	return 0
}

// isNestedStepKey reports whether a step key belongs to a list inside a step
// -- `decide/a`, `for_x/0/touch` -- rather than to the run's own list.
func isNestedStepKey(key string) bool {
	return strings.Contains(key, "/")
}

// ResumeFrom resumes execution from the work journal.
// It rehydrates the evaluator with the completed step results the journal
// holds, then continues execution from the specified step (or the
// unfinished one if not specified), on the SAME run id.
func (e *Executor) ResumeFrom(
	ctx context.Context,
	journal *RunJournal,
	automation *Automation,
	opts *ResumeOptions,
) (*AutomationExecution, error) {
	if journal == nil {
		return nil, ErrRunJournalInvalid
	}
	if automation == nil {
		return nil, fmt.Errorf("automation is nil")
	}
	if err := ensurePrepared(automation); err != nil {
		return nil, err
	}
	if opts == nil {
		opts = &ResumeOptions{}
	}

	// A re-run (epic memql#5414) resumes a run that may have finished: the
	// failed-step requirement is waived, and the step it targets must be one
	// of the automation's own.
	rerun := opts.Rerun
	if err := validateRunJournal(journal, automation, fingerprintEngine, rerun != nil); err != nil {
		return nil, err
	}
	target := -1
	if rerun != nil {
		var err error
		if target, err = rerunTargetIndex(automation, rerun, journal.ForkAtStepKey); err != nil {
			return nil, err
		}
		// The spec names its target as the step it resolved to, so a branch
		// that named its fork point only as forkAtStepKey still aims its
		// override at that step. A copy: the caller's spec is theirs.
		spec := *rerun
		spec.StepKey = automation.Steps[target].ID
		rerun = &spec
	}

	// Determine the resume point
	resumeStepId := statementResumePoint(journal, automation)
	if rerun != nil {
		resumeStepId = rerunResumePoint(journal, automation, rerun, target)
	}
	if opts.FromStep != "" {
		resumeStepId = opts.FromStep
	}

	// Find the step index to resume from
	resumeIndex := -1
	var resumeStep *Step
	for i, step := range automation.Steps {
		if step.ID == resumeStepId {
			resumeIndex = i
			resumeStep = step
			break
		}
	}
	if resumeIndex == -1 {
		return nil, fmt.Errorf("step %q not found in automation", resumeStepId)
	}
	// Every step before a re-run's target is served from what it recorded, so
	// resuming earlier would run one of them again -- in a branch, a step
	// whose version lives in another run.
	if rerun != nil && resumeIndex < target {
		return nil, fmt.Errorf("%w: the resume point %q is before the targeted step %q", ErrRerunStepInvalid, resumeStepId, rerun.StepKey)
	}

	// Check if resume step is retryable
	if !stepRetryable(resumeStep) && !opts.AllowSideEffects {
		return nil, fmt.Errorf("%w: step %q is type %s, set AllowSideEffects to retry",
			ErrNonRetryableStep, resumeStepId, resumeStep.Type)
	}

	// Inject system actor for automation execution
	ctx = contextWithSystemActor(ctx, automation.Name)

	// Create new execution tracking the resume
	triggeredBy := fmt.Sprintf("resumed:%s", journal.RunId)
	exec := NewExecution(automation.Name, triggeredBy)
	// The resumed run IS the original run: same id, new versions of its rows.
	exec.ID = journal.RunId
	// The SAME rule as executeWithEvent: internal origin requires a trusted
	// SOURCE and a trigger payload the caller did not supply (memql#2888).
	//
	// Reading automation.Trusted alone here made the origin downgrade
	// BYPASSABLE, and the bypass was handed to the attacker by the fix itself:
	//
	//   1. MCP run_automation with a chosen payload -> client origin (correct)
	//   2. the body hits a @serverOnly construct -> refused -> the step errors
	//   3. ErrorStrategyStop saves a CHECKPOINT, which persists
	//      TriggerContext.Event -- the attacker's payload
	//   4. POST /automations/resume replays it, and this line restored
	//      SourceTrusted = true
	//   5. the steps re-dispatch at INTERNAL origin, with the attacker's
	//      payload, and the loop runs to the end so the write step executes too
	//
	// Measured end to end: leg 1 origins=[client client], leg 2
	// origins=[internal internal] carrying the same event. The refusal in step
	// 2 is what MINTS the token in step 3.
	exec.SourceTrusted = automation.Trusted && !journal.CallerSuppliedPayload
	exec.CallerSuppliedPayload = journal.CallerSuppliedPayload
	// The head the journal writes at every receipt starts where the run's rows
	// stand, and a re-run's request rides the execution rather than the
	// context, so it reaches this run's steps and journal writes and no other
	// run's (a sub-automation a step starts is an execution of its own).
	exec.rerun = rerun
	exec.overrides = opts.Overrides
	var stale []string
	if rerun != nil && len(journal.StaleSteps) > 0 {
		stale = journal.StaleSteps
	}
	exec.head = newRunHead(resumeHead(journal), stale)

	// The resumed run keeps its place in its causal chain (epic memql#5380):
	// the parent its first attempt recorded on triggerEvent, one deeper, under
	// the same run id -- so what it writes and publishes carries the depth
	// the first attempt's did. See journalRunCause.
	ctx = events.ContextWithCause(ctx, journalRunCause(ctx, automation, exec.ID, journal.TriggerEvent))

	// Set up evaluator
	evaluator := NewEvaluator()
	bindActorEnvelope(ctx, evaluator)
	evaluator.SetVariableResolver(e.createVariableResolver())
	evaluator.SetSystemVariableResolver(e.createSystemVariableResolver())
	evaluator.SetSecretResolver(e.createSecretResolver())
	evaluator.SetSystemSecretResolver(e.createSystemSecretResolver())
	evaluator.SetCanonicalIdResolver(e.createCanonicalIdResolver())
	evaluator.SetLogger(e.logger)
	evaluator.SetCustom("now", time.Now().UTC().Format(time.RFC3339))
	// Resume restores the same declared arguments and validation used at
	// first execution. Variables on a goal run are authoritative; ordinary
	// scheduled/event runs take their saved event payload.
	payload := journal.Variables
	if payload == nil && journal.TriggerEvent != nil {
		payload, _ = journal.TriggerEvent["payload"].(map[string]any)
	}
	boundArgs, _, bindErr := bindEventArgs(automation, &events.Event{Payload: payload})
	if bindErr != nil {
		return nil, fmt.Errorf("resume args contract violation: %w", bindErr)
	}
	if boundArgs != nil {
		evaluator.SetCustom("args", boundArgs)
	}
	bindRunAmbient(ctx, e.engine, evaluator)

	// Restore the run's input record from the run row.
	if journal.Input != nil {
		exec.Input = journal.Input
	}

	// Restore the trigger context from the run row. When the run has no
	// triggering event (cron / manual / startup resume), seed a synthetic
	// object envelope so a call that passes `event: event` still passes an
	// object (see executor.go / issue #418).
	if journal.TriggerEvent != nil {
		evaluator.SetCustom("event", journal.TriggerEvent)
	} else {
		evaluator.SetCustom("event", buildEventEnvelope(nil, "resume", "resume"))
	}

	// The run's record of the steps the journal holds. Their values reach the
	// resumed statements through resumedStatements.
	for _, minResult := range journal.Steps {
		if minResult != nil {
			exec.AddStepResult(minimalToStepResult(minResult))
		}
	}

	resumedPayload := map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"runId":          journal.RunId,
		"resumeFromStep": resumeStepId,
		"restoredSteps":  len(journal.Steps),
	}
	if rerun != nil {
		resumedPayload["rerunReason"] = rerun.Reason
		resumedPayload["rerunRequestId"] = rerun.RequestId
		resumedPayload["rerunStepKey"] = rerun.StepKey
	}
	if e.logger != nil {
		e.logger.Info("resuming automation execution",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"runId", journal.RunId,
			"resumeFromStep", resumeStepId,
			"resumeIndex", resumeIndex,
			"restoredSteps", len(journal.Steps),
			"rerun", rerun != nil,
		)
	}

	// Publish automation resumed event
	e.publishEvent(ctx, "automation.resumed", events.KindTelemetry, resumedPayload)

	// Chain tracking starts from the run row's chain head. The body runs again
	// from its first statement over names rehydrated from the journal; the
	// statement order is the body's, so it is not copied from the journal.
	var chainHead string
	exec.StepOrder = make([]string, 0, len(automation.Steps))
	if e.chainTrackingEnabled {
		exec.InitialChainHead = journal.InitialChainHead
		chainHead = journal.ChainHead // Resume from the run row's chain position
		if chainHead == "" {
			chainHead = journal.InitialChainHead
		}
	}

	// Reopen the run: it goes back to `running`, and the retried steps write
	// new versions with attempt incremented. A reader watching the run sees
	// one story rather than a second execution sharing a prefix.
	writer := e.journal
	if journalSkipsAutomation(automation) {
		writer = nil
	}
	writer.reopenRun(ctx, exec)
	ctx = withRunJournal(ctx, exec.ID, writer)

	// Set up step context.
	//
	// A resumed step must not read a stale cached result. That requirement is
	// recorded here as prose rather than as a StepContext.SkipCache field
	// (removed in memql#2941): the field was written here and read nowhere
	// once memql#2899 deleted the step cache, and an inert flag on a struct
	// invites the next reader to believe it still guarantees freshness. If a
	// step cache is ever reintroduced, this is the site that needs the opt-out.
	stepCtx := &StepContext{
		Logger:               e.logger,
		Engine:               e.engine,
		EventBus:             e.eventBus,
		Evaluator:            evaluator,
		Execution:            exec,
		AutomationTrigger:    e.automationTrigger,
		ChainTrackingEnabled: e.chainTrackingEnabled,
	}

	return e.runStatementAutomation(ctx, automation, exec, nil, writer, stepCtx, chainHead, resumedStatements(journal, automation, resumeIndex, rerun))
}

// minimalToStepResult converts a MinimalStepResult back to a full StepResult,
// the run's record of a step a resume did not run again.
func minimalToStepResult(min *MinimalStepResult) *StepResult {
	if min == nil {
		return nil
	}

	result := &StepResult{
		StepId:    min.StepId,
		Status:    min.Status,
		Result:    min.Result,
		Error:     min.Error,
		ContentId: min.ContentId,
	}

	// Copy metadata
	if min.Metadata != nil {
		result.Metadata = make(map[string]any, len(min.Metadata))
		for k, v := range min.Metadata {
			result.Metadata[k] = v
		}
	}

	return result
}

// ---------------------------------------------------------------------
// The step-result trimming the journal writes and resume reads back.
// Moved here verbatim from checkpoint.go when the checkpoint side-record
// was retired: the shape is the same, only its home row changed.
// ---------------------------------------------------------------------

// ToMinimalStepResults converts a map of full StepResults to MinimalStepResults.
// This reduces checkpoint size by omitting large payloads while preserving
// the data needed for evaluator rehydration.
func ToMinimalStepResults(steps map[string]*StepResult) map[string]*MinimalStepResult {
	if steps == nil {
		return nil
	}

	minimal := make(map[string]*MinimalStepResult, len(steps))
	for stepId, result := range steps {
		if result == nil {
			continue
		}

		minResult := &MinimalStepResult{
			StepId:    result.StepId,
			Status:    string(result.Status),
			Error:     result.Error,
			ContentId: result.ContentId,
		}

		// Include result if it's not too large
		// For queries with many nodes, we omit the result to save space
		if result.Result != nil {
			if shouldIncludeResult(result) {
				// ExecuteResult holds flat builtin/logic output in a private
				// field. Serializing its wrapper discards the returned value,
				// so rehydrate the same value downstream evaluation sees.
				minResult.Result = UnwrapStepResult(result.Result)
			}
		}
		minResult.Value = result.Bound

		// Extract key metadata for evaluator
		if result.Metadata != nil {
			minResult.Metadata = make(map[string]any)
			// Copy essential metadata fields
			for _, key := range []string{"itemCount", "query", "resultQuery", "topic", "url", "statusCode"} {
				if v, ok := result.Metadata[key]; ok {
					minResult.Metadata[key] = v
				}
			}
		}

		minimal[stepId] = minResult
	}

	return minimal
}

// shouldIncludeResult determines if a step result should be stored in the checkpoint.
// Large results (many nodes) are omitted to keep checkpoint size reasonable.
func shouldIncludeResult(result *StepResult) bool {
	if result == nil || result.Result == nil {
		return false
	}

	// Check if result is a MemQL result with many nodes
	if resultMap, ok := result.Result.(map[string]any); ok {
		if bundle, ok := resultMap["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				// Omit results with more than 100 nodes
				if len(nodes) > 100 {
					return false
				}
			}
		}
	}

	// Include by default for non-query results
	return true
}

// extractJournalPayload extracts the payload from a MemQL query result.
func extractJournalPayload(result any) (map[string]any, error) {
	if result == nil {
		return nil, fmt.Errorf("result is nil")
	}

	// Handle *memql.ExecuteResult directly (from engine.Execute)
	if er, ok := result.(*memql.ExecuteResult); ok {
		if er.Bundle == nil || len(er.Bundle.Nodes) == 0 {
			return nil, fmt.Errorf("no nodes found")
		}
		node := er.Bundle.Nodes[0]
		if node == nil || node.Payload == nil {
			return nil, fmt.Errorf("no payload in node")
		}
		return node.Payload.AsMap(), nil
	}

	// Fallback: handle map[string]any (e.g., from WebSocket responses)
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	// Navigate to result.bundle.nodes[0].payload
	bundle, ok := resultMap["bundle"].(map[string]any)
	if !ok {
		// Try result.Bundle for Go struct
		bundle, ok = resultMap["Bundle"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("no bundle in result")
		}
	}

	nodes, ok := bundle["nodes"].([]any)
	if !ok {
		// Try bundle.Nodes for Go struct
		nodes, ok = bundle["Nodes"].([]any)
		if !ok {
			return nil, fmt.Errorf("no nodes in bundle")
		}
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes found")
	}

	node, ok := nodes[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected node type: %T", nodes[0])
	}

	payload, ok := node["payload"].(map[string]any)
	if !ok {
		// Try node.Payload for Go struct
		payload, ok = node["Payload"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("no payload in node")
		}
	}

	return payload, nil
}

// mapStructFromPayload unmarshals a map into a struct via JSON.
func mapStructFromPayload(m map[string]any, v any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// jsonString returns a JSON-encoded string value.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
