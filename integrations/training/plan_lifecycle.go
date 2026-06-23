// Plan + Task lifecycle bracketing for the training pipeline.
//
// The trainAgent capability runs a 3-step Go pipeline (capabilities
// + chunk pre-warm; identity vector; distilled prompt) and the user
// needs to SEE that work happen. The product surface for that is
// the existing Plan + Task model: a Plan row is the top-level "thing
// running" the user can see on the canvas (plan.created card with
// estimate strip) and the Tasks page (status pill, agent assignment,
// duration). This file brackets the in-process pipeline with that
// lifecycle so:
//
//   1. `plan.created` canvas card lands on the active space the
//      moment Train is clicked, with a heuristic estimate so the
//      user has IMMEDIATE feedback.
//   2. Three Task rows (capabilities / identityVector / distillPrompt)
//      track the steps individually -- the user can watch them flip
//      to running / succeeded on the Tasks page.
//   3. `training.completed` canvas card emits on plan success with
//      importance: notify (the bell pings, since the user explicitly
//      kicked this off and walked away).
//
// The lifecycle is graceful-degradation: if the caller didn't pass
// partitionId / requestedBy (e.g. an automation or test), the helpers
// no-op and the pipeline runs without bookkeeping. The CoPresent
// Training studio always passes both.

package training

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// planLifecycle holds the ids the handler stamps as it walks the
// pipeline. Nil-safe: every method short-circuits when the receiver
// or planId is empty so the caller doesn't have to litter nil checks.
type planLifecycle struct {
	integ       *Integration
	planId      string
	taskIds     [3]string // [capabilities, identityVector, distillPrompt]
	partitionId     string
	requestedBy string
	startedAt   time.Time
}

// beginPlan creates the Plan + 3 Task rows + emits the plan.created
// canvas card. Returns a lifecycle handle the handler walks through
// each pipeline step, OR nil + nil when partitionId / requestedBy are
// absent (DSL caller chose to skip the bookkeeping). A failure to
// create the Plan / Tasks / card is logged but doesn't abort the
// caller -- the pipeline still runs; only the visibility is lost.
func (i *Integration) beginPlan(
	ctx context.Context,
	partitionId string,
	requestedBy string,
	agentId string,
	agentName string,
	addedDomainIds []string,
	removedDomainIds []string,
	tools []string,
) *planLifecycle {
	partitionId = strings.TrimSpace(partitionId)
	requestedBy = strings.TrimSpace(requestedBy)
	if partitionId == "" || requestedBy == "" {
		// Graceful degrade: caller opted out of the bookkeeping.
		return nil
	}
	if i.engine == nil {
		i.Logger.Warn("training.beginPlan: engine not configured, skipping")
		return nil
	}

	now := time.Now().UTC()
	planId := id.NewSystemNodeShortId("train", agentId)
	// Task ids derive deterministically from the (already bare) planId so
	// the three steps stay stably keyed to their plan; SystemNodeShortId
	// keeps them colon-free bare ids the engine accepts (issue #1712).
	taskIds := [3]string{
		id.SystemNodeShortId(planId, "t0-capabilities"),
		id.SystemNodeShortId(planId, "t1-identityVector"),
		id.SystemNodeShortId(planId, "t2-distillPrompt"),
	}

	addedDomainsCount := len(addedDomainIds)
	removedDomainsCount := len(removedDomainIds)

	displayName := strings.TrimSpace(agentName)
	if displayName == "" {
		displayName = "agent"
	}
	goal := fmt.Sprintf("Train %s", displayName)
	if addedDomainsCount > 0 || removedDomainsCount > 0 {
		parts := []string{}
		if addedDomainsCount > 0 {
			parts = append(parts, fmt.Sprintf("+%d domain%s", addedDomainsCount, pluralS(addedDomainsCount)))
		}
		if removedDomainsCount > 0 {
			parts = append(parts, fmt.Sprintf("-%d domain%s", removedDomainsCount, pluralS(removedDomainsCount)))
		}
		goal = fmt.Sprintf("Train %s (%s)", displayName, strings.Join(parts, ", "))
	}

	estP50, estP90 := heuristicEstimateTrain(addedDomainsCount)
	estimateBlock := fmt.Sprintf(
		`{"p50Ms": %d, "p90Ms": %d, "confidence": "heuristic"}`,
		estP50, estP90,
	)

	// Stuff actual lists into the Plan's input payload (not just
	// counts). Lets the Tasks-page expansion show a useful drawer
	// AND lets downstream tools (analytics, search) match training
	// runs by domain.
	inputJSON := mustJSON(map[string]any{
		"agentId":             agentId,
		"addedDomainsCount":   addedDomainsCount,
		"addedDomainIds":      addedDomainIds,
		"removedDomainsCount": removedDomainsCount,
		"removedDomainIds":    removedDomainIds,
		"tools":               tools,
	})

	// 1. Create Plan(queued).
	createPlanQ := fmt.Sprintf(
		`mutationCreatePlan({"planId": %s, "partitionId": %s, "kind": "trainAgent", "goal": %s, "requestedBy": %s, "triggerSource": "user.explicit", "input": %s})`,
		quoteString(planId),
		quoteString(partitionId),
		quoteString(goal),
		quoteString(requestedBy),
		inputJSON,
	)
	if _, err := i.engine.Execute(ctx, createPlanQ); err != nil {
		i.Logger.Warn("training.beginPlan: mutationCreatePlan failed; running without lifecycle", "err", err)
		return nil
	}

	// 2. Stamp the heuristic estimate so the canvas card has it on
	//    first render. Best-effort; the estimate is nice-to-have.
	if _, err := i.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %s, "status": "queued", "estimate": %s, "estimatedAt": %s})`,
		quoteString(planId),
		estimateBlock,
		quoteString(now.Format(time.RFC3339)),
	)); err != nil {
		i.Logger.Debug("training.beginPlan: estimate stamp failed", "err", err)
	}

	// 3. Emit the plan.created canvas card so the user sees Train
	//    landed somewhere immediately. Owner-private so only the
	//    user who clicked Train sees it. Importance: ambient -- the
	//    bell ping comes later on plan.completed (training.completed).
	createdStateId := planId + ":created"
	createdCardData := fmt.Sprintf(
		`{"variant": "plan.created", "planId": %s, "goal": %s, "estimate": %s}`,
		quoteString(planId),
		quoteString(goal),
		estimateBlock,
	)
	createdActor := fmt.Sprintf(`{"kind": "user", "userId": %s}`, quoteString(requestedBy))
	if _, err := i.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateCanvasState({"stateId": %s, "space": %s, "kind": "card", "data": %s, "visibility": "private", "forUserId": %s, "actor": %s, "importance": "ambient"})`,
		quoteString(createdStateId),
		quoteString(partitionId),
		createdCardData,
		quoteString(requestedBy),
		createdActor,
	)); err != nil {
		i.Logger.Debug("training.beginPlan: plan.created card emit failed", "err", err)
	}

	// 4. Create three Tasks under the Plan, all queued.
	taskKinds := [3]string{"trainAgentCapabilities", "trainAgentIdentityVector", "trainAgentDistillPrompt"}
	taskInputs := [3]string{
		fmt.Sprintf(`{"agentId": %s, "step": "capabilities"}`, quoteString(agentId)),
		fmt.Sprintf(`{"agentId": %s, "step": "identityVector"}`, quoteString(agentId)),
		fmt.Sprintf(`{"agentId": %s, "step": "distillPrompt"}`, quoteString(agentId)),
	}
	for idx := 0; idx < 3; idx++ {
		if _, err := i.engine.Execute(ctx, fmt.Sprintf(
			`mutationCreateTask({"taskId": %s, "planId": %s, "kind": %s, "seq": %d, "input": %s})`,
			quoteString(taskIds[idx]),
			quoteString(planId),
			quoteString(taskKinds[idx]),
			idx,
			taskInputs[idx],
		)); err != nil {
			i.Logger.Debug("training.beginPlan: createTask failed", "idx", idx, "err", err)
		}
	}

	// 5. Plan -> running (with startedAt). The handler is about to
	//    do the actual work; surface it on the Tasks page right away.
	if _, err := i.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %s, "status": "running", "startedAt": %s})`,
		quoteString(planId),
		quoteString(now.Format(time.RFC3339)),
	)); err != nil {
		i.Logger.Debug("training.beginPlan: plan->running failed", "err", err)
	}

	return &planLifecycle{
		integ:       i,
		planId:      planId,
		taskIds:     taskIds,
		partitionId:     partitionId,
		requestedBy: requestedBy,
		startedAt:   now,
	}
}

// markTaskRunning transitions task[idx] -> running with a startedAt.
// Nil-safe.
func (l *planLifecycle) markTaskRunning(ctx context.Context, idx int) {
	if l == nil || l.planId == "" {
		return
	}
	if idx < 0 || idx >= len(l.taskIds) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := l.integ.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdateTaskStatus({"taskId": %s, "status": "running", "startedAt": %s})`,
		quoteString(l.taskIds[idx]),
		quoteString(now),
	)); err != nil {
		l.integ.Logger.Debug("training.markTaskRunning failed", "idx", idx, "err", err)
	}
}

// markTaskSucceeded transitions task[idx] -> succeeded with the
// supplied output payload + completedAt. Nil-safe.
func (l *planLifecycle) markTaskSucceeded(ctx context.Context, idx int, output map[string]any) {
	if l == nil || l.planId == "" {
		return
	}
	if idx < 0 || idx >= len(l.taskIds) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	outputJSON := "{}"
	if output != nil {
		outputJSON = mustJSON(output)
	}
	if _, err := l.integ.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdateTaskStatus({"taskId": %s, "status": "succeeded", "output": %s, "completedAt": %s})`,
		quoteString(l.taskIds[idx]),
		outputJSON,
		quoteString(now),
	)); err != nil {
		l.integ.Logger.Debug("training.markTaskSucceeded failed", "idx", idx, "err", err)
	}
}

// markTaskFailed transitions task[idx] -> failed with errMsg + a
// completedAt. Used for best-effort steps (B + C) when they bubble
// an error -- the Task is failed but the parent Plan can still
// succeed since A is the only required step. Nil-safe.
func (l *planLifecycle) markTaskFailed(ctx context.Context, idx int, errMsg string) {
	if l == nil || l.planId == "" {
		return
	}
	if idx < 0 || idx >= len(l.taskIds) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := l.integ.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdateTaskStatus({"taskId": %s, "status": "failed", "errorMessage": %s, "completedAt": %s})`,
		quoteString(l.taskIds[idx]),
		quoteString(errMsg),
		quoteString(now),
	)); err != nil {
		l.integ.Logger.Debug("training.markTaskFailed failed", "idx", idx, "err", err)
	}
}

// completePlan transitions the Plan -> succeeded with the rolled-up
// summary as output, then emits the training.completed canvas card
// (importance: notify) so the bell pings on the active space. The
// card payload mirrors what the frontend used to publish so the
// existing TrainingCompletedCard renders unchanged. Nil-safe.
func (l *planLifecycle) completePlan(
	ctx context.Context,
	summary map[string]any,
	agentName string,
) {
	if l == nil || l.planId == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	outputJSON := "{}"
	if summary != nil {
		outputJSON = mustJSON(summary)
	}
	if _, err := l.integ.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %s, "status": "succeeded", "output": %s, "completedAt": %s})`,
		quoteString(l.planId),
		outputJSON,
		quoteString(now),
	)); err != nil {
		l.integ.Logger.Debug("training.completePlan: plan->succeeded failed", "err", err)
	}

	// Build the training.completed card payload from the summary.
	// Field names mirror what TrainingContext used to publish so
	// TrainingCompletedCard renders unchanged. Retry-plan ids let
	// the card render a "retry queued" badge against any step that
	// the in-handler retry couldn't recover -- background poll loop
	// will pick the retry up over the next 5min / 30min / 2h.
	cardPayload := map[string]any{
		"variant":             "training.completed",
		"planId":              l.planId, // lets the card link directly to the Tasks panel row
		"agentId":             summary["agentId"],
		"agentName":           agentName,
		"domainsAdded":        intFromAny(summary["domainsAdded"]),
		"domainsAddedList":    summary["domainsAddedList"], // actual ids, named on the card
		"domainsRemoved":      intFromAny(summary["domainsRemoved"]),
		"domainsRemovedList":  summary["domainsRemovedList"], // actual ids
		"domainsSeeded":       intFromAny(summary["domainsSeeded"]),
		"perDomainStats":      summary["perDomainStats"], // per-domain breakdown for the expanded view
		"toolsList":           summary["toolsList"],      // full tool set after training
		"chunksEmbedded":      intFromAny(summary["chunksEmbedded"]),
		"chunksAlready":       intFromAny(summary["chunksAlready"]),
		"identityVectorWrote": boolFromAny(summary["identityVectorWrote"]),
		"systemPromptWrote":   boolFromAny(summary["systemPromptWrote"]),
		"bridgeId":            stringFromAny(summary["bridgeId"]),
		"elapsedMs":           intFromAny(summary["elapsedMs"]),
		"identityRetryPlanId": stringFromAny(summary["identityRetryPlanId"]),
		"distillRetryPlanId":  stringFromAny(summary["distillRetryPlanId"]),
		// Partial-success surface. Frontend uses these to swap the
		// kicker copy ("Training complete" -> "Training partially
		// complete") and to render a per-domain failure section.
		// Empty list = fully successful.
		"failedSeedDomains": summary["failedSeedDomains"],
		"failedSeedReasons": summary["failedSeedReasons"],
	}
	cardJSON := mustJSON(cardPayload)
	stateId := l.planId + ":completed"
	actorJSON := fmt.Sprintf(`{"kind": "user", "userId": %s}`, quoteString(l.requestedBy))

	// importance: notify -- the user explicitly clicked Train and
	// walked away; the bell ping is exactly what they're waiting for.
	if _, err := l.integ.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateCanvasState({"stateId": %s, "space": %s, "kind": "card", "data": %s, "visibility": "private", "forUserId": %s, "actor": %s, "importance": "notify"})`,
		quoteString(stateId),
		quoteString(l.partitionId),
		cardJSON,
		quoteString(l.requestedBy),
		actorJSON,
	)); err != nil {
		l.integ.Logger.Warn("training.completePlan: training.completed card emit failed", "err", err)
	}
}

// failPlan transitions the Plan -> failed with errorMessage + a
// completedAt. Called when step A (the required step) errors --
// the agent's edit didn't land, so the Plan is genuinely failed.
// Nil-safe.
func (l *planLifecycle) failPlan(ctx context.Context, errMsg string) {
	if l == nil || l.planId == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := l.integ.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %s, "status": "failed", "errorMessage": %s, "completedAt": %s})`,
		quoteString(l.planId),
		quoteString(errMsg),
		quoteString(now),
	)); err != nil {
		l.integ.Logger.Debug("training.failPlan: plan->failed failed", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Retry helper
// ---------------------------------------------------------------------------

// retryWithBackoff runs fn up to attempts times, sleeping `backoff`
// between attempts. The first attempt fires immediately; the
// optional retries fire after the backoff. Returns the last result
// + error. Cancellation-aware: if ctx is done while sleeping, we
// return the prior error rather than waiting out the backoff.
//
// Used to soak up transient external-API failures on training steps
// B (embedding API) and C (LLM distill) -- the bulk of real
// failures here are rate limits / brief 5xx / network blips that
// resolve on a second try a couple seconds later. Permanent errors
// (auth, billing, malformed request) still surface after the
// retries are exhausted; the caller treats those as best-effort
// failures and queues a longer-horizon background retry plan
// (see trainAgentRetryStep).
func retryWithBackoff[T any](
	ctx context.Context,
	attempts int,
	backoff time.Duration,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	var (
		result T
		err    error
	)
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Sleep the backoff, but bail if the caller's context
			// gives up first -- we'd rather return the prior error
			// than hang past the request deadline.
			select {
			case <-ctx.Done():
				return result, err
			case <-time.After(backoff):
			}
		}
		result, err = fn(ctx)
		if err == nil {
			return result, nil
		}
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// heuristicEstimateTrain returns a quick p50/p90 in ms based on the
// number of NEW domains. Steps B + C have ~baseline costs (~800ms
// for embed + ~3s for distill); step A scales with the number of
// new domains because each domain's chunk pre-warm dominates.
//
// Numbers are intentionally conservative so the user isn't
// disappointed by an overly-optimistic ETA. Refined per Q5 once the
// historical bucket has enough samples to take over from the
// heuristic.
func heuristicEstimateTrain(addedDomainsCount int) (p50Ms, p90Ms int64) {
	const baselineMs int64 = 4000     // B + C combined baseline
	const perNewDomainMs int64 = 3000 // step A per added domain
	p50 := baselineMs + perNewDomainMs*int64(addedDomainsCount)
	p90 := p50 * 2
	return p50, p90
}

// (stripIdPrefix lives in training.go -- shared helper.)

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func intFromAny(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case float32:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

func boolFromAny(v any) bool {
	b, _ := v.(bool)
	return b
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}
