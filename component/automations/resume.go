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
// a completed step is served from the journal and never re-run; a step
// whose type has no external effect (query, shape, function, forEach,
// parallel, switch, automation) is re-run; a mutation, webhook, event or
// action step at the resume point needs AllowSideEffects, because the
// journal cannot yet tell whether its far side already holds a receipt.
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
	"github.com/znasllc-io/memql/core/id"
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
	ErrNonRetryableStep = errors.New("step is not safely retryable (mutation, webhook, event or action)")
)

// RunJournal is what resume needs from the rows: the run's envelope and
// the completed steps' trimmed results.
type RunJournal struct {
	RunId                 string
	AutomationName        string
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
}

// ResumeOptions configures resume behavior.
type ResumeOptions struct {
	// FromStep overrides the resume point (defaults to the failed step).
	FromStep string

	// AllowSideEffects permits retrying mutation/webhook/event/action steps.
	// Without this flag, resuming from a non-retryable step returns an error.
	AllowSideEffects bool
}

// IsStepRetryable reports whether a step type can be re-run with no
// external effect.
func IsStepRetryable(stepType StepType) bool {
	switch stepType {
	case StepTypeMutation, StepTypeWebhook, StepTypeEvent, StepTypeAction:
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
	j := &RunJournal{
		RunId:                 shortWorkId(stringField(run, "id")),
		AutomationName:        stringField(run, "automationName"),
		TemplateFingerprint:   stringField(run, "templateFingerprint"),
		TriggeredBy:           stringField(run, "triggeredBy"),
		Input:                 run["input"],
		InputFingerprint:      stringField(run, "inputFingerprint"),
		CallerSuppliedPayload: boolField(run, "callerSuppliedPayload"),
		ChainHead:             stringField(run, "chainHead"),
		InitialChainHead:      stringField(run, "initialChainHead"),
		Steps:                 map[string]*MinimalStepResult{},
	}
	if ev, ok := run["triggerEvent"].(map[string]any); ok {
		j.TriggerEvent = ev
	}
	if order, ok := run["stepOrder"].([]any); ok {
		for _, s := range order {
			if k, ok := s.(string); ok {
				j.StepOrder = append(j.StepOrder, k)
			}
		}
	}
	for _, row := range steps {
		key := stringField(row, "key")
		if key == "" {
			continue
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
	if j == nil {
		return ErrRunJournalInvalid
	}
	if j.RunId == "" || j.AutomationName == "" {
		return fmt.Errorf("%w: missing run id or automation name", ErrRunJournalInvalid)
	}
	if j.FailedStep == "" {
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
	if opts == nil {
		opts = &ResumeOptions{}
	}

	// Validate the journal against the current automation
	if err := ValidateRunJournal(journal, automation, fingerprintEngine); err != nil {
		return nil, err
	}

	// Determine the resume point
	resumeStepId := journal.FailedStep
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

	// Check if resume step is retryable
	if !IsStepRetryable(resumeStep.Type) && !opts.AllowSideEffects {
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

	// Set up evaluator
	evaluator := NewEvaluator()
	bindActorEnvelope(ctx, evaluator)
	evaluator.SetVariableResolver(e.createVariableResolver())
	evaluator.SetSystemVariableResolver(e.createSystemVariableResolver())
	evaluator.SetSecretResolver(e.createSecretResolver())
	evaluator.SetSystemSecretResolver(e.createSystemSecretResolver())
	evaluator.SetCanonicalIdResolver(e.createCanonicalIdResolver())
	evaluator.SetLogger(e.logger)
	evaluator.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))

	// Restore input from the run row
	if journal.Input != nil {
		exec.Input = journal.Input
		evaluator.SetInput(journal.Input)
	}

	// Restore the trigger context from the run row. When the run has
	// no triggering event (cron / manual / startup resume), seed a
	// synthetic object envelope so step args that pass `event: event`
	// still resolve to an object rather than the unresolved literal
	// `event` token (which the engine coerces to a string and the
	// receiving logic function rejects -- see executor.go / issue #418).
	if journal.TriggerEvent != nil {
		evaluator.SetCustom("event", journal.TriggerEvent)
	} else {
		evaluator.SetCustom("event", buildEventEnvelope(nil, "resume", "resume"))
	}

	// Rehydrate evaluator with the completed step results the journal holds
	for stepId, minResult := range journal.Steps {
		if minResult == nil {
			continue
		}
		// Convert MinimalStepResult to StepResult for evaluator
		fullResult := minimalToStepResult(minResult)
		evaluator.SetStepResult(stepId, fullResult)
		exec.AddStepResult(fullResult)
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
		)
	}

	// Publish automation resumed event
	e.publishEvent("automation.resumed", events.KindTelemetry, map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"runId":          journal.RunId,
		"resumeFromStep": resumeStepId,
		"restoredSteps":  len(journal.Steps),
	})

	// Chain tracking - start from the run row's chain head
	// Include completed steps from the journal in StepOrder for chain verification
	// Only copy steps BEFORE the resume point to avoid duplicates
	var chainHead string
	if e.chainTrackingEnabled {
		// Copy only steps before resumeIndex (not including the failed step)
		// The failed step will be added when it executes
		if len(journal.StepOrder) > 0 && resumeIndex > 0 {
			// Find how many steps from journal.StepOrder to keep
			// This is the minimum of resumeIndex and the journal's step count
			stepsToKeep := resumeIndex
			if stepsToKeep > len(journal.StepOrder) {
				stepsToKeep = len(journal.StepOrder)
			}
			exec.StepOrder = make([]string, stepsToKeep, len(automation.Steps))
			copy(exec.StepOrder, journal.StepOrder[:stepsToKeep])
		} else {
			exec.StepOrder = make([]string, 0, len(automation.Steps))
		}
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

	// Execute steps starting from resumeIndex
	for i := resumeIndex; i < len(automation.Steps); i++ {
		step := automation.Steps[i]

		// Track step order for chain verification
		if e.chainTrackingEnabled {
			exec.StepOrder = append(exec.StepOrder, step.ID)
		}

		// Check for cancellation
		select {
		case <-ctx.Done():
			exec.Cancel()
			writer.closeRun(ctx, exec, chainHead)
			return exec, ctx.Err()
		default:
		}

		// Evaluate step condition if present
		if step.Condition != "" {
			if e.logger != nil {
				e.logger.Debug("evaluating step condition",
					"step", step.ID,
					"condition", step.Condition,
				)
			}
			shouldRun, err := evaluator.EvaluateCondition(step.Condition)
			if err != nil {
				if e.logger != nil {
					e.logger.Warn("step condition evaluation failed",
						"step", step.ID,
						"condition", step.Condition,
						"error", err,
					)
				}
				shouldRun = false
			}
			if !shouldRun {
				// Record skipped step
				skipResult := &StepResult{
					StepId:      step.ID,
					Status:      "skipped",
					StartedAt:   time.Now(),
					CompletedAt: time.Now(),
				}
				exec.AddStepResult(skipResult)
				evaluator.SetStepResult(step.ID, skipResult)
				writer.stepSkipped(ctx, exec, step, i)
				continue
			}
		}

		// Set current chain head in context
		if e.chainTrackingEnabled {
			stepCtx.PreviousChainHead = chainHead
		}

		// Execute the step. A resumed step is by definition at least its
		// SECOND attempt at the resume point; the steps after it are running
		// for the first time in this run.
		attemptNo := 1
		if i == resumeIndex {
			attemptNo = 2
		}
		writer.stepRunning(ctx, exec, step, i, attemptNo)
		result, err := e.executeStep(ctx, step, stepCtx)
		if result != nil {
			// Compute chain linkage if tracking enabled
			if e.chainTrackingEnabled {
				result.PreviousChainHead = chainHead
				result.ContentId = StepDeterministicFingerprint(step, result)
				chainHead = string(fingerprintEngine.Combine(
					id.ID(chainHead),
					id.ID(result.ContentId),
				))
			}

			exec.AddStepResult(result)
			evaluator.SetStepResult(step.ID, result)
			writer.stepFinished(ctx, exec, step, result, chainHead)
		}

		if err != nil {
			// Handle error based on strategy
			switch step.OnError {
			case ErrorStrategyContinue:
				if e.logger != nil {
					e.logger.Warn("step failed, continuing",
						"component", ComponentName,
						"step", step.ID,
						"error", err,
					)
				}
				continue
			case ErrorStrategyRetry:
				// Retry logic
				retried := false
				for attempt := 1; attempt <= step.RetryCount && !retried; attempt++ {
					if e.logger != nil {
						e.logger.Info("retrying step",
							"component", ComponentName,
							"step", step.ID,
							"attempt", attempt,
						)
					}
					writer.stepRunning(ctx, exec, step, i, attemptNo+attempt)
					result, err = e.executeStep(ctx, step, stepCtx)
					if err == nil {
						if e.chainTrackingEnabled && result != nil {
							result.PreviousChainHead = chainHead
							result.ContentId = StepDeterministicFingerprint(step, result)
							chainHead = string(fingerprintEngine.Combine(
								id.ID(chainHead),
								id.ID(result.ContentId),
							))
						}
						exec.AddStepResult(result)
						evaluator.SetStepResult(step.ID, result)
						writer.stepFinished(ctx, exec, step, result, chainHead)
						retried = true
					}
				}
				if !retried {
					exec.Fail(err)
					writer.stepFinished(ctx, exec, step, result, chainHead)
					writer.closeRun(ctx, exec, chainHead)
					// The step's own rows already record the failure; the run
					// stays resumable from them.
					return exec, err
				}
			default: // ErrorStrategyStop
				exec.Fail(err)
				writer.closeRun(ctx, exec, chainHead)
				return exec, err
			}
		}
	}

	// Execute onComplete hook if defined
	if automation.OnComplete != nil {
		_, err := e.executeStep(ctx, automation.OnComplete, stepCtx)
		if err != nil && e.logger != nil {
			e.logger.Warn("onComplete hook failed",
				"component", ComponentName,
				"error", err,
			)
		}
	}

	exec.Complete()
	writer.closeRun(ctx, exec, chainHead)

	// Finalize chain tracking
	if e.chainTrackingEnabled {
		exec.ChainHead = chainHead
	}

	// Publish automation completed event
	completedPayload := map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"runId":          journal.RunId,
		"resumed":        true,
		"duration":       exec.Duration.Milliseconds(),
		"stepCount":      len(exec.Steps),
	}
	if e.chainTrackingEnabled && exec.ChainHead != "" {
		completedPayload["chainHead"] = exec.ChainHead
	}
	e.publishEvent(events.TopicAutomationCompleted, events.KindAutomationCompleted, completedPayload)

	if e.logger != nil {
		e.logger.Info("resumed automation execution completed",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"runId", journal.RunId,
			"status", exec.Status,
			"duration", exec.Duration,
		)
	}

	return exec, nil
}

// minimalToStepResult converts a MinimalStepResult back to a full StepResult.
// This is used to rehydrate the evaluator during resume.
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
				minResult.Result = result.Result
			}
		}

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
