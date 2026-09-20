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
// CockpitAppExecutor.Run -- the SAME executor a delegated task uses -- because
// a routed step and a delegated task are the same act: opening an app session
// on somebody's machine with the owner's consent gates in front of it. A second
// path here would be a second set of gates, and the two would disagree exactly
// once, on the case nobody tested. That executor is also the one thing in this
// tree that had no production caller on the delegation path; this file is that
// caller.
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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/planner"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/common"
)

// appSessionTemplate names the child run's template. It is what a client
// filters the Work feed by, so it is a stable word rather than the app's id --
// "an app session ran this step" is the category; which app is a field.
const appSessionTemplate = "appSession"

// appSessionStepKey is the child run's single step.
const appSessionStepKey = "session"

// stepExecutor is the narrow half of CockpitAppExecutor this file needs.
//
// Declared as an interface so the test can substitute a recorder without
// standing up a dispatcher, a router, a registry and a session runner -- four
// collaborators none of which this file's logic is about.
type stepExecutor interface {
	Run(ctx context.Context, req planner.ExecutorRequest, progress planner.ProgressCallback) (planner.ExecutorResult, error)
}

// stepStamper writes the parent step's childRunId. It is the engine, narrowed
// to the one call this file makes.
type stepStamper interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// AppSessionDelegate fills component/memql's step-handover seam.
type AppSessionDelegate struct {
	exec    stepExecutor
	journal *workjournal.Journal
	engine  stepStamper
	logger  *slog.Logger
}

// NewAppSessionDelegate builds the seam implementation over the executor the
// agent node already has, so there is ONE path onto somebody's machine rather
// than a second set of consent gates that could disagree with the first.
//
// A nil journal or engine degrades honestly: the session still runs, and the
// step simply carries no childRunId. Refusing the run because the bookkeeping
// is unwired would take the door down for a fault in the wrong layer.
func NewAppSessionDelegate(exec *CockpitAppExecutor, journal *workjournal.Journal, engine *memqlengine.MemQLEngine, logger *slog.Logger) *AppSessionDelegate {
	var stamper stepStamper
	if engine != nil {
		stamper = engine
	}
	return newAppSessionDelegateFor(exec, journal, stamper, logger)
}

func newAppSessionDelegateFor(exec stepExecutor, journal *workjournal.Journal, engine stepStamper, logger *slog.Logger) *AppSessionDelegate {
	if logger == nil {
		logger = slog.Default()
	}
	return &AppSessionDelegate{exec: exec, journal: journal, engine: engine, logger: logger}
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
	if owner == "" {
		// The same refusal CockpitAppExecutor.Run makes, made earlier so the
		// child run is never opened under a blank actor -- a row written that
		// way is readable by nobody, including the operator asking what ran.
		return memqlengine.AppSessionOutcome{}, fmt.Errorf(
			"app session: the step has no owner; a machine-touching door cannot run unattributed")
	}

	child := d.beginChildRun(ctx, h, owner)
	childRunId := child.RunID()
	if childRunId != "" {
		d.stampParentStep(ctx, owner, h.StepId, childRunId)
	}

	var childStep *workjournal.Step
	if child != nil {
		childStep = child.Step(ctx, appSessionStepKey)
	}

	res, runErr := d.exec.Run(ctx, planner.ExecutorRequest{
		StepId:      h.StepId,
		RunId:       h.RunId,
		AgentId:     h.AgentId,
		OwnerUserId: owner,
		Kind:        appSessionTemplate,
		Inputs:      h.Inputs,
		Input: map[string]any{
			"executorBackend": planner.BackendCockpitApp + ":" + h.AppId,
			"prompt":          h.Prompt,
			"level":           h.Level,
			"model":           h.Model,
			"responseSchema":  schemaJSON(h.ResponseSchema),
		},
	}, nil)

	out := memqlengine.AppSessionOutcome{
		ChildRunId:  childRunId,
		SessionId:   outputString(res.Output, "sessionId"),
		ArtifactIds: res.ArtifactIds,
		Model:       outputString(res.Output, "model"),
		Effort:      outputString(res.Output, "effort"),
		Billing:     res.Billing,
		Usage: memqlengine.AppUsage{
			OutputTokens: int64(res.TokensSpent),
			Known:        res.TokensSpent > 0,
		},
		ExecutionSurface: surfaceFor(h.AppId, outputString(res.Output, "workerId")),
	}
	// THE STRUCTURED ANSWER AND THE TRANSCRIPT ARE BOTH KEPT, and neither
	// substitutes for the other: a harness can answer the schema and still
	// exit non-zero, and a session that answered no schema still produced a
	// transcript. Content is the TEXT answer either way, because that is what
	// a chat surface renders.
	out.Result = resultJSON(res.Output["result"])
	out.Content = outputString(res.Output, "transcript")
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
func (d *AppSessionDelegate) beginChildRun(ctx context.Context, h memqlengine.AppSessionHandover, owner string) *workjournal.Run {
	if d.journal == nil {
		return nil
	}
	run, err := d.journal.Begin(ctx, workjournal.Work{
		OwnerUserID: owner,
		Template:    appSessionTemplate,
		Statement:   fmt.Sprintf("run this step in %s", h.AppId),
		// The GOAL is stable per parent step, so a retried step is two runs of
		// one goal rather than two goals -- which is what makes "this step was
		// delegated twice" readable.
		GoalKey: h.StepId,
		RunKey:  fmt.Sprintf("%s-%d", h.StepId, time.Now().UTC().UnixNano()),
		Input: map[string]any{
			"app":          h.AppId,
			"level":        h.Level,
			"model":        h.Model,
			"parentRunId":  h.RunId,
			"parentStepId": h.StepId,
		},
		// ONE step, and it is `reasoning`: an app session is an agent taking
		// its own decisions, which is the definition the journal's two kinds
		// draw the line on.
		Steps:        []workjournal.StepDecl{{Key: appSessionStepKey, Kind: workjournal.KindReasoning}},
		RequestedVia: "router",
	})
	if err != nil {
		d.logger.Warn("app session: could not open the child run; the step will carry no childRunId",
			"parent_step_id", h.StepId, "error", err)
		return nil
	}
	return run
}

// stampParentStep records the subrun on the step that was handed over.
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
