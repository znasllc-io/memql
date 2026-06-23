package taskstamp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// defaultSelfPlanningTools are tools whose invocation creates its OWN
// v1:planner:plan (the real record of the work), so the taskstamp Stamper must
// NOT also wrap an ad-hoc call to them in a synthetic adHocAction Plan -- that
// produced a duplicate, empty wrapper task (memql#1192). produceArtifact is the
// canonical case: the Assistant's produceArtifact tool spawns a produceArtifact
// Plan that does the deliverable. Extendable via MEMQL_TASKSTAMP_SELF_PLANNING_TOOLS
// (comma-separated, ADDED to this default set).
var defaultSelfPlanningTools = map[string]bool{
	"produceArtifact": true,
}

// isSelfPlanningTool reports whether a tool spawns its own Plan (so an ad-hoc
// call to it should not be wrapped). The env override only ADDS names -- the
// produceArtifact default is always present.
func isSelfPlanningTool(toolName string) bool {
	if defaultSelfPlanningTools[toolName] {
		return true
	}
	for _, n := range strings.Split(os.Getenv("MEMQL_TASKSTAMP_SELF_PLANNING_TOOLS"), ",") {
		if strings.TrimSpace(n) == toolName && toolName != "" {
			return true
		}
	}
	return false
}

// Executor is the narrow interface the stamper depends on: the engine's
// existing ExecuteToolByName plus an Execute escape hatch for issuing
// mutations. Both are present on the real MemQLEngine; the interface
// lets us test the stamper without dragging the full engine in.
type Executor interface {
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
	Execute(ctx context.Context, query string) (any, error)
}

// Stamper wraps an Executor and auto-stamps v1:planner:task rows on
// every tool call when the caller has installed a PlanContext on the
// context. Callers without PlanContext see no behavior change.
//
// Per Q5: tool calls are observable. Per Q6: the stamped row is
// category='toolInvocation' with parentTaskId set to the semantic
// Task that was executing when the call fired. Per the ad-hoc path,
// the stamper materializes a synthetic Plan + semantic Task on the
// first call within a PlanContext that lacks them.
type Stamper struct {
	Engine Executor
	Logger *slog.Logger
}

// New constructs a Stamper. Logger may be nil; the stamper substitutes
// slog.Default in that case.
func New(engine Executor, logger *slog.Logger) *Stamper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Stamper{Engine: engine, Logger: logger}
}

// ExecuteToolByName is the stamped tool dispatcher. Mirrors the engine's
// ExecuteToolByName signature so callers can swap in the stamper with
// no API change.
//
// Behavior:
//
//   - No PlanContext on ctx -> straight passthrough to engine. No row
//     stamped. Caller's behavior unchanged.
//   - PlanContext present but missing PlanId/SemanticTaskId -> stamper
//     materializes synthetic rows (kind='adHocAction' Plan + a semantic
//     wrapper Task) before stamping the toolInvocation row.
//   - All present -> stamper writes the toolInvocation row, dispatches,
//     then updates the row with the result or error.
//
// Mutation failures are LOGGED but not propagated -- observability must
// never break a user-facing tool call. The result returned to the caller
// is whatever the underlying executor produced.
func (s *Stamper) ExecuteToolByName(ctx context.Context, toolName string, args map[string]any) (string, error) {
	pc, ok := PlanContextFrom(ctx)
	if !ok {
		// No PlanContext installed -- caller didn't opt in. Straight passthrough.
		return s.Engine.ExecuteToolByName(ctx, toolName, args)
	}

	// Did THIS call materialize the synthetic ad-hoc Plan? (PlanId empty before
	// ensure = the ad-hoc-chat-tool-call case.) If so we own its lifecycle and
	// must drive it to terminal after dispatch -- otherwise the synthetic
	// adHocAction Plan + callTool wrapper sit "running" forever (memql#1186).
	// A REAL caller-supplied Plan (PlanId set) is the planner's to finalize, so
	// we never touch its status here.
	createdSyntheticPlan := pc.PlanId == ""

	// SELF-PLANNING tools spawn their OWN Plan (e.g. produceArtifact creates a
	// produceArtifact Plan that does the real work). Wrapping such an ad-hoc
	// call in a synthetic adHocAction Plan produced a confusing DUPLICATE task
	// (one user request -> two tasks: the real produceArtifact Plan + an empty
	// adHocAction wrapper). When the call is ad-hoc (no caller Plan) AND the
	// tool self-plans, skip stamping entirely -- the tool's own Plan is the
	// authoritative record (memql#1192). A call under a REAL caller Plan still
	// stamps normally (it's a genuine sub-step of that Plan).
	if createdSyntheticPlan && isSelfPlanningTool(toolName) {
		return s.Engine.ExecuteToolByName(ctx, toolName, args)
	}

	ctx, pc, err := s.ensurePlanAndSemanticTask(ctx, pc)
	if err != nil {
		// Synthetic-Plan / semantic-Task materialization failed. Log and
		// fall through to the underlying executor so the user-facing
		// behavior is preserved. The audit trail loses this call.
		s.Logger.Warn("taskstamp: ensure plan+task failed; dispatching without stamping",
			"tool", toolName, "error", err)
		return s.Engine.ExecuteToolByName(ctx, toolName, args)
	}

	taskId := id.NewShortId()
	if err := s.stampPre(ctx, pc, taskId, toolName, args); err != nil {
		s.Logger.Warn("taskstamp: pre-dispatch stamp failed",
			"tool", toolName, "planId", pc.PlanId, "parentTaskId", pc.SemanticTaskId, "error", err)
		// Continue with dispatch -- the row just won't exist. Better
		// than dropping the user-facing tool call.
		return s.Engine.ExecuteToolByName(ctx, toolName, args)
	}

	startedAt := time.Now()
	result, execErr := s.Engine.ExecuteToolByName(ctx, toolName, args)
	completedAt := time.Now()

	if err := s.stampPost(ctx, taskId, result, execErr, completedAt); err != nil {
		s.Logger.Warn("taskstamp: post-dispatch stamp failed",
			"tool", toolName, "taskId", taskId, "error", err)
		// toolInvocation row will be stuck in 'running'; observability
		// surfaces can detect this as "stale running" and reconcile.
	}

	// Finalize the synthetic ad-hoc wrapper we created so it doesn't linger
	// "running". The ad-hoc tool call is self-contained: its toolInvocation
	// row already captured the outcome, so the wrapper Plan + semantic task
	// reflect that outcome (succeeded, or failed if the tool errored). A
	// best-effort write -- a finalize failure must never break the tool call.
	if createdSyntheticPlan {
		s.finalizeAdHocWrapper(ctx, pc, execErr, completedAt)
	}
	_ = startedAt // reserved for future per-call duration metrics

	return result, execErr
}

// finalizeAdHocWrapper drives the synthetic adHocAction Plan + its callTool
// semantic Task to a terminal status (succeeded, or failed when the wrapped
// tool call errored) so they don't sit "running" forever (memql#1186). Both
// writes are best-effort + logged: observability bookkeeping must never break
// the user-facing tool call. Only ever called for stamper-materialized
// synthetic plans -- never a caller-supplied real Plan.
func (s *Stamper) finalizeAdHocWrapper(ctx context.Context, pc PlanContext, execErr error, completedAt time.Time) {
	status := "succeeded"
	if execErr != nil {
		status = "failed"
	}
	ts := completedAt.UTC().Format(time.RFC3339)

	if pc.SemanticTaskId != "" {
		tq := fmt.Sprintf(
			`mutationUpdateTaskStatus({"taskId": %q, "status": %q, "completedAt": %q})`,
			pc.SemanticTaskId, status, ts,
		)
		if _, err := s.Engine.Execute(ctx, tq); err != nil {
			s.Logger.Warn("taskstamp: finalize ad-hoc semantic task failed",
				"taskId", pc.SemanticTaskId, "error", err)
		}
	}
	if pc.PlanId != "" {
		pq := fmt.Sprintf(
			`mutationUpdatePlanStatus({"planId": %q, "status": %q, "completedAt": %q})`,
			pc.PlanId, status, ts,
		)
		if _, err := s.Engine.Execute(ctx, pq); err != nil {
			s.Logger.Warn("taskstamp: finalize ad-hoc plan failed",
				"planId", pc.PlanId, "error", err)
		}
	}
}

// ensurePlanAndSemanticTask materializes the synthetic rows when the
// PlanContext lacks them. Returns the updated context (carrying the
// freshly-stamped ids) and the resolved PlanContext.
//
// Cases handled:
//
//   - PlanId empty + SemanticTaskId empty: create Plan + semantic Task.
//   - PlanId set + SemanticTaskId empty: create semantic Task only.
//   - Both set: no-op.
func (s *Stamper) ensurePlanAndSemanticTask(ctx context.Context, pc PlanContext) (context.Context, PlanContext, error) {
	if pc.PlanId == "" {
		planId, err := s.createAdHocPlan(ctx, pc)
		if err != nil {
			return ctx, pc, fmt.Errorf("create ad-hoc plan: %w", err)
		}
		pc.PlanId = planId
	}
	if pc.SemanticTaskId == "" {
		semId, err := s.createSemanticWrapper(ctx, pc)
		if err != nil {
			return ctx, pc, fmt.Errorf("create semantic wrapper: %w", err)
		}
		pc.SemanticTaskId = semId
	}
	return updatePlanContext(ctx, pc), pc, nil
}

// createAdHocPlan inserts a kind='adHocAction' Plan row using the
// mutationCreateAdHocPlan DSL function. Returns the new Plan id.
func (s *Stamper) createAdHocPlan(ctx context.Context, pc PlanContext) (string, error) {
	planId := id.NewShortId()
	goal := fmt.Sprintf("Ad-hoc tool actions by agent %s", pc.AgentId)
	q := fmt.Sprintf(
		`mutationCreateAdHocPlan({"planId": %q, "partitionId": %q, "agentId": %q, "ownerUserId": %q, "goal": %q})`,
		planId, pc.PartitionId, pc.AgentId, pc.OwnerUserId, goal,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return "", err
	}
	return planId, nil
}

// createSemanticWrapper inserts a category='semantic' Task that parents
// the upcoming toolInvocation rows. logicalStepId = the synthetic Task's
// own id (one attempt; the threshold model is for retries on a real
// step, not for ad-hoc grouping).
func (s *Stamper) createSemanticWrapper(ctx context.Context, pc PlanContext) (string, error) {
	taskId := id.NewShortId()
	q := fmt.Sprintf(
		`mutationCreateSemanticTask({"taskId": %q, "planId": %q, "kind": "callTool", "seq": 0, "logicalStepId": %q, "attemptNumber": 1, "agentId": %q, "input": {"adHoc": true}})`,
		taskId, pc.PlanId, taskId, pc.AgentId,
	)
	if _, err := s.Engine.Execute(ctx, q); err != nil {
		return "", err
	}
	return taskId, nil
}

// stampPre inserts the toolInvocation Task row before dispatch. The
// row's status is 'running' and toolResult is empty until stampPost
// fires after the underlying executor returns.
func (s *Stamper) stampPre(ctx context.Context, pc PlanContext, taskId, toolName string, args map[string]any) error {
	argsJSON := "{}"
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err == nil {
			argsJSON = string(b)
		}
	}
	q := fmt.Sprintf(
		`mutationCreateToolInvocationTask({"taskId": %q, "planId": %q, "parentTaskId": %q, "toolName": %q, "toolArgs": %s, "seq": 0})`,
		taskId, pc.PlanId, pc.SemanticTaskId, toolName, argsJSON,
	)
	_, err := s.Engine.Execute(ctx, q)
	return err
}

// stampPost updates the toolInvocation Task with the dispatch result.
// On success: status='succeeded' + toolResult populated. On error:
// status='failed' + errorMessage populated.
func (s *Stamper) stampPost(ctx context.Context, taskId, result string, execErr error, completedAt time.Time) error {
	completedAtStr := completedAt.UTC().Format(time.RFC3339)
	if execErr != nil {
		q := fmt.Sprintf(
			`mutationCompleteToolInvocation({"taskId": %q, "status": "failed", "errorMessage": %q, "completedAt": %q})`,
			taskId, execErr.Error(), completedAtStr,
		)
		_, err := s.Engine.Execute(ctx, q)
		return err
	}
	// Wrap the result string in a small envelope object. The toolResult
	// field is typed as object, not string; producing a small payload
	// the engine accepts.
	resultJSON, err := json.Marshal(map[string]string{"output": result})
	if err != nil {
		resultJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutationCompleteToolInvocation({"taskId": %q, "status": "succeeded", "toolResult": %s, "completedAt": %q})`,
		taskId, string(resultJSON), completedAtStr,
	)
	_, err = s.Engine.Execute(ctx, q)
	return err
}
