// Background retry of best-effort training steps.
//
// Steps B (identity vector) and C (distilled system prompt) call
// out to external SI providers (embedding API + LLM API) and CAN
// fail for transient reasons -- rate limits, brief 5xx, network
// blips. The handler already does one in-handler retry (~1.5s
// backoff) which catches the bulk of those. When that's still not
// enough, we'd rather not punish the user's training click for an
// OpenAI hiccup that resolves 5 minutes later. So we queue a
// trainAgentRetryStep Plan and a background goroutine in this
// package picks it up later, runs the failed step, and emits a
// small canvas card when the retry lands (success or final fail).
//
// Architecture:
//
//   trainAgent handler           --> step B / C fails after 1 retry
//      |
//      | enqueueRetryPlan(...)
//      v
//   trainAgentRetryStep Plan     status=queued, attempt=0,
//   (input.nextAttemptAt = now + 5min)
//      |
//      | poll loop wakes every 60s, finds the Plan via
//      | queryDueTrainAgentRetryPlans
//      v
//   processRetryPlan(...)
//      |
//      | runs step B or C ONCE (no in-loop retry -- the in-handler
//      |   retry already happened, this is the longer-horizon one)
//      v
//   on success: Plan -> succeeded; emit training.retryCompleted card
//   on failure:
//     attempt++; if attempt < 3 -> bump nextAttemptAt by exp backoff
//                  (5min -> 30min -> 2h), keep status=queued
//                if attempt >= 3 -> Plan -> failed; emit
//                  training.retryFailed card so the user knows the
//                  enhancement won't auto-recover
//
// The poll loop runs forever inside the integration goroutine.
// Cleanup on process exit is implicit (goroutine dies with the
// process); in-flight plans the loop didn't get to are picked up
// on the next process boot since the schedule lives in the DB.

package training

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

const (
	retryStepIdentityVector = "identityVector"
	retryStepDistillPrompt  = "distillPrompt"

	// Backoff schedule for the background poll loop. attempts beyond
	// the schedule terminate the retry-Plan as failed.
	retryMaxAttempts = 3
)

// retryBackoff returns the wait duration after attempt N (0-indexed).
// Schedule: 5min -> 30min -> 2h. After attempt 2 the retry-Plan is
// terminated (failed) so this is only called for attempts 0/1/2.
func retryBackoff(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 5 * time.Minute
	case 1:
		return 30 * time.Minute
	case 2:
		return 2 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// enqueueRetryPlan inserts a trainAgentRetryStep Plan in status
// 'queued' with input.nextAttemptAt set to now + initial backoff.
// The training poll loop picks it up after that window. Best-effort:
// a failure to enqueue is logged, not returned -- the original
// training run already succeeded for the user; missing the auto-
// retry is a degraded experience but not a hard error.
func (i *Integration) enqueueRetryPlan(
	ctx context.Context,
	originalPlanId string,
	spaceId string,
	requestedBy string,
	agentId string,
	step string,
	failureReason string,
) string {
	if i.engine == nil {
		return ""
	}
	if strings.TrimSpace(spaceId) == "" || strings.TrimSpace(requestedBy) == "" {
		// No bookkeeping context -- caller opted out (or never
		// passed spaceId / requestedBy). Skip.
		return ""
	}

	now := time.Now().UTC()
	nextAttemptAt := now.Add(retryBackoff(0))

	planId := fmt.Sprintf("trainretry:%s:%s:%d", stripIdPrefix(agentId), step, now.UnixNano())
	goal := fmt.Sprintf("Retry training step (%s)", humanStep(step))

	inputJSON := mustJSON(map[string]any{
		"originalPlanId": originalPlanId,
		"agentId":        agentId,
		"step":           step,
		"attempt":        0,
		"nextAttemptAt":  nextAttemptAt.Format(time.RFC3339),
		"failureReason":  failureReason,
	})

	createQ := fmt.Sprintf(
		`mutationCreatePlan({"planId": %s, "spaceId": %s, "kind": "trainAgentRetryStep", "goal": %s, "requestedBy": %s, "triggerSource": "system", "input": %s})`,
		quoteString(planId),
		quoteString(spaceId),
		quoteString(goal),
		quoteString(requestedBy),
		inputJSON,
	)
	if _, err := i.engine.Execute(ctx, createQ); err != nil {
		i.Logger.Warn("training.enqueueRetryPlan: createPlan failed",
			"planId", planId, "err", err)
		return ""
	}
	i.Logger.Info("training.enqueueRetryPlan: scheduled",
		"planId", planId,
		"originalPlanId", originalPlanId,
		"agentId", agentId,
		"step", step,
		"nextAttemptAt", nextAttemptAt.Format(time.RFC3339),
	)
	return planId
}

// trainAgentRetryStepHandler runs the single failed step against the
// agent's CURRENT capabilities (we re-read the agent so we operate
// against the latest state, not a stale snapshot from when the
// original train ran). Synchronous, returns the same v1:training:result
// shape the main trainAgent capability returns; caller (poll loop or
// frontend) handles the Plan-status transitions around it.
func (i *Integration) trainAgentRetryStepHandler(
	ctx context.Context,
	args map[string]any,
	_ int,
) ([]memorynodes.MemoryNode, error) {
	handlerStart := time.Now()

	agentId, _ := args["agentId"].(string)
	step, _ := args["step"].(string)
	if agentId == "" || step == "" {
		return nil, fmt.Errorf("training.trainAgentRetryStep: agentId + step are required")
	}
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = defaultEmbeddingProviderName
	}

	if i.engine == nil || i.db() == nil || i.embeddingProvider == nil {
		return nil, fmt.Errorf("training.trainAgentRetryStep: integration not fully wired")
	}

	agent, _, _, err := i.readAgent(ctx, agentId)
	if err != nil {
		return nil, fmt.Errorf("training.trainAgentRetryStep: read agent: %w", err)
	}
	caps, _ := agent["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	domains := toStringSlice(caps["domains"])
	tools := toStringSlice(caps["tools"])
	domainLabelMap, _ := i.fetchDomainLabels(ctx, domains)
	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("training.trainAgentRetryStep: resolve provider %q: %w", providerName, err)
	}

	identityVectorWrote := false
	systemPromptWrote := false

	switch step {
	case retryStepIdentityVector:
		profileText := buildAgentProfileText(agent, domains, tools, domainLabelMap)
		if strings.TrimSpace(profileText) == "" {
			return nil, fmt.Errorf("training.trainAgentRetryStep: profile text is empty (no capabilities to embed)")
		}
		vec, err := provider.Embed(ctx, profileText)
		if err != nil {
			return nil, fmt.Errorf("training.trainAgentRetryStep: identity-embed: %w", err)
		}
		if err := i.storeVector(ctx, agentId, "v1:agents:agent", vec); err != nil {
			return nil, fmt.Errorf("training.trainAgentRetryStep: identity-vector store: %w", err)
		}
		identityVectorWrote = true

	case retryStepDistillPrompt:
		distilled, err := i.distillSystemPrompt(ctx, agent, domains, tools, domainLabelMap)
		if err != nil {
			return nil, fmt.Errorf("training.trainAgentRetryStep: distill: %w", err)
		}
		if distilled == "" {
			return nil, fmt.Errorf("training.trainAgentRetryStep: distill returned empty")
		}
		latest, _, _, err := i.readAgent(ctx, agentId)
		if err != nil {
			return nil, fmt.Errorf("training.trainAgentRetryStep: re-read agent: %w", err)
		}
		latest["systemPrompt"] = distilled
		if err := i.writeAgent(ctx, agentId, latest); err != nil {
			return nil, fmt.Errorf("training.trainAgentRetryStep: systemPrompt write: %w", err)
		}
		systemPromptWrote = true

	default:
		return nil, fmt.Errorf("training.trainAgentRetryStep: unknown step %q", step)
	}

	elapsed := time.Since(handlerStart)
	i.Logger.Info("training.trainAgentRetryStep: completed",
		"agentId", agentId,
		"step", step,
		"identityVectorWrote", identityVectorWrote,
		"systemPromptWrote", systemPromptWrote,
		"elapsedMs", elapsed.Milliseconds(),
	)

	summary := map[string]any{
		"agentId":             agentId,
		"step":                step,
		"identityVectorWrote": identityVectorWrote,
		"systemPromptWrote":   systemPromptWrote,
		"elapsedMs":           elapsed.Milliseconds(),
	}
	summaryJSON, _ := json.Marshal(summary)
	return []memorynodes.MemoryNode{
		{
			ID:        fmt.Sprintf("training-retry:%s:%d", agentId, time.Now().UnixNano()),
			Concept:   "v1:copresent:trainingresult",
			Payload:   summaryJSON,
			CreatedAt: time.Now(),
		},
	}, nil
}

// startRetryLoop spawns the background poll goroutine. Called once
// from the plug-in factory (plugin.go) after the integration's
// engine + DB getter are wired. The goroutine runs for the lifetime
// of the process; in-flight retry-Plans are picked up on the next
// process boot since their schedule lives in the DB.
func (i *Integration) startRetryLoop(ctx context.Context) {
	const pollInterval = 60 * time.Second
	go func() {
		i.Logger.Info("training.retryLoop: started", "pollInterval", pollInterval.String())
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				i.Logger.Info("training.retryLoop: stopped")
				return
			case <-ticker.C:
				i.pollRetries(ctx)
			}
		}
	}()
}

// pollRetries fetches due retry-Plans and processes them in
// sequence. Failures inside one plan don't affect the others.
func (i *Integration) pollRetries(ctx context.Context) {
	if i.engine == nil {
		return
	}
	res, err := i.engine.Execute(ctx, "queryDueTrainAgentRetryPlans({})")
	if err != nil {
		i.Logger.Debug("training.pollRetries: query failed (likely engine not ready yet)", "err", err)
		return
	}
	if res == nil || res.Bundle == nil {
		return
	}
	plans := res.Bundle.Nodes
	if len(plans) == 0 {
		return
	}
	// Time filter happens in Go (not the query) -- memQL's
	// comparison evaluator doesn't accept now() as a comparison RHS,
	// so the query returns ALL queued trainAgentRetryStep Plans and
	// we drop the ones whose nextAttemptAt is still in the future.
	now := time.Now().UTC()
	dueCount := 0
	for _, p := range plans {
		if p == nil || p.Payload == nil {
			continue
		}
		payloadJSON, err := p.Payload.MarshalJSON()
		if err != nil {
			i.Logger.Warn("training.pollRetries: payload marshal failed", "planId", p.Id, "err", err)
			continue
		}
		if !retryPlanIsDue(payloadJSON, now) {
			continue
		}
		dueCount++
		i.processRetryPlan(ctx, p.Id, payloadJSON)
	}
	if dueCount > 0 {
		i.Logger.Info("training.pollRetries: processed due retry plans", "due", dueCount, "total_queued", len(plans))
	}
}

// retryPlanIsDue returns true when the plan's input.nextAttemptAt
// is in the past (or absent / unparseable -- treat as due so we
// don't wedge a malformed retry Plan in the queue forever).
func retryPlanIsDue(payloadJSON []byte, now time.Time) bool {
	var plan map[string]any
	if err := json.Unmarshal(payloadJSON, &plan); err != nil {
		return true
	}
	input, _ := plan["input"].(map[string]any)
	if input == nil {
		return true
	}
	nextStr, _ := input["nextAttemptAt"].(string)
	if nextStr == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, nextStr)
	if err != nil {
		return true
	}
	return !t.After(now)
}

// processRetryPlan runs ONE attempt of the retry. On success the
// plan transitions to 'succeeded' and a small training.retryCompleted
// canvas card lands. On failure attempt is bumped + nextAttemptAt is
// re-scheduled (or the plan is failed if attempts are exhausted).
func (i *Integration) processRetryPlan(ctx context.Context, planId string, payload []byte) {
	var plan map[string]any
	if err := json.Unmarshal(payload, &plan); err != nil {
		i.Logger.Warn("training.processRetryPlan: payload decode failed", "planId", planId, "err", err)
		return
	}
	input, _ := plan["input"].(map[string]any)
	if input == nil {
		i.Logger.Warn("training.processRetryPlan: input missing", "planId", planId)
		return
	}
	agentId, _ := input["agentId"].(string)
	step, _ := input["step"].(string)
	originalPlanId, _ := input["originalPlanId"].(string)
	attempt := int(intFromAny(input["attempt"]))
	spaceId, _ := plan["spaceId"].(string)
	requestedBy, _ := plan["requestedBy"].(string)

	if agentId == "" || step == "" {
		i.Logger.Warn("training.processRetryPlan: bad input", "planId", planId, "input", input)
		return
	}

	// Mark Plan running (so a concurrent poll cycle on a slower
	// node doesn't double-execute) -- best-effort.
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = i.engine.Execute(ctx, fmt.Sprintf(
		`mutationUpdatePlanStatus({"planId": %s, "status": "running", "startedAt": %s})`,
		quoteString(planId), quoteString(now),
	))

	// Run the step. ONE attempt -- the in-handler retry already
	// happened on the original training run; this loop is the
	// longer-horizon backoff.
	args := map[string]any{
		"agentId":        agentId,
		"step":           step,
		"retryPlanId":    planId,
		"originalPlanId": originalPlanId,
	}
	_, runErr := i.trainAgentRetryStepHandler(ctx, args, 0)

	if runErr == nil {
		// Success path: Plan -> succeeded with the result summary,
		// emit retry-success canvas card.
		completedAt := time.Now().UTC().Format(time.RFC3339)
		successOutput := mustJSON(map[string]any{
			"success":        true,
			"attempt":        attempt,
			"step":           step,
			"originalPlanId": originalPlanId,
		})
		_, _ = i.engine.Execute(ctx, fmt.Sprintf(
			`mutationUpdatePlanStatus({"planId": %s, "status": "succeeded", "output": %s, "completedAt": %s})`,
			quoteString(planId), successOutput, quoteString(completedAt),
		))
		i.emitRetryCompletedCard(ctx, planId, originalPlanId, spaceId, requestedBy, agentId, step, attempt)
		i.Logger.Info("training.processRetryPlan: succeeded",
			"planId", planId, "agentId", agentId, "step", step, "attempt", attempt)
		return
	}

	// Failure path: bump attempt + reschedule, OR terminate.
	nextAttempt := attempt + 1
	if nextAttempt >= retryMaxAttempts {
		// Terminate -- emit retry-failed card so the user knows the
		// enhancement won't auto-recover.
		failedAt := time.Now().UTC().Format(time.RFC3339)
		errMsg := fmt.Sprintf("attempt %d/%d failed: %v", nextAttempt, retryMaxAttempts, runErr)
		_, _ = i.engine.Execute(ctx, fmt.Sprintf(
			`mutationUpdatePlanStatus({"planId": %s, "status": "failed", "errorMessage": %s, "completedAt": %s})`,
			quoteString(planId), quoteString(errMsg), quoteString(failedAt),
		))
		i.emitRetryFailedCard(ctx, planId, originalPlanId, spaceId, requestedBy, agentId, step, retryMaxAttempts, errMsg)
		i.Logger.Warn("training.processRetryPlan: exhausted attempts; failing",
			"planId", planId, "agentId", agentId, "step", step, "attempts", retryMaxAttempts, "err", runErr)
		return
	}

	// Reschedule. Update input.attempt + input.nextAttemptAt and
	// flip status back to 'queued' so the next poll cycle picks
	// it up after the backoff window.
	nextAt := time.Now().UTC().Add(retryBackoff(nextAttempt)).Format(time.RFC3339)
	updatedInput := map[string]any{
		"originalPlanId": originalPlanId,
		"agentId":        agentId,
		"step":           step,
		"attempt":        nextAttempt,
		"nextAttemptAt":  nextAt,
		"failureReason":  runErr.Error(),
	}
	// Re-create the plan row with status=queued + bumped input.
	// memQL Plans are time-series; mutationCreatePlan with the
	// existing planId inserts a fresh row that supersedes the prior
	// (cache(ttl=0) on the concept reads latest only). The
	// status-update mutation above set it to running; this brings
	// it back to queued for the next poll window.
	_, _ = i.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreatePlan({"planId": %s, "spaceId": %s, "kind": "trainAgentRetryStep", "goal": %s, "requestedBy": %s, "triggerSource": "system", "input": %s})`,
		quoteString(planId),
		quoteString(spaceId),
		quoteString(fmt.Sprintf("Retry training step (%s) -- attempt %d/%d", humanStep(step), nextAttempt+1, retryMaxAttempts)),
		quoteString(requestedBy),
		mustJSON(updatedInput),
	))
	i.Logger.Info("training.processRetryPlan: re-queued",
		"planId", planId, "agentId", agentId, "step", step,
		"nextAttempt", nextAttempt+1, "nextAttemptAt", nextAt, "err", runErr)
}

// emitRetryCompletedCard publishes a small canvas card the moment a
// background retry succeeds, so the user sees the enhancement
// landed without having to look at the Tasks page.
func (i *Integration) emitRetryCompletedCard(
	ctx context.Context,
	retryPlanId string,
	originalPlanId string,
	spaceId string,
	requestedBy string,
	agentId string,
	step string,
	attempt int,
) {
	if strings.TrimSpace(spaceId) == "" || strings.TrimSpace(requestedBy) == "" {
		return
	}
	cardData := mustJSON(map[string]any{
		"variant":        "training.retryCompleted",
		"originalPlanId": originalPlanId,
		"retryPlanId":    retryPlanId,
		"agentId":        agentId,
		"step":           step,
		"attempt":        attempt,
	})
	stateId := retryPlanId + ":completed"
	actorJSON := fmt.Sprintf(`{"kind": "user", "userId": %s}`, quoteString(requestedBy))
	if _, err := i.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateCanvasState({"stateId": %s, "space": %s, "kind": "card", "data": %s, "visibility": "private", "forUserId": %s, "actor": %s, "importance": "ambient"})`,
		quoteString(stateId),
		quoteString(spaceId),
		cardData,
		quoteString(requestedBy),
		actorJSON,
	)); err != nil {
		i.Logger.Debug("training.emitRetryCompletedCard: publish failed", "err", err)
	}
}

// emitRetryFailedCard publishes a card after exhausted attempts so
// the user knows the enhancement won't auto-recover and can choose
// to re-train manually if it matters to them.
func (i *Integration) emitRetryFailedCard(
	ctx context.Context,
	retryPlanId string,
	originalPlanId string,
	spaceId string,
	requestedBy string,
	agentId string,
	step string,
	totalAttempts int,
	finalError string,
) {
	if strings.TrimSpace(spaceId) == "" || strings.TrimSpace(requestedBy) == "" {
		return
	}
	cardData := mustJSON(map[string]any{
		"variant":        "training.retryFailed",
		"originalPlanId": originalPlanId,
		"retryPlanId":    retryPlanId,
		"agentId":        agentId,
		"step":           step,
		"totalAttempts":  totalAttempts,
		"finalError":     finalError,
	})
	stateId := retryPlanId + ":failed"
	actorJSON := fmt.Sprintf(`{"kind": "user", "userId": %s}`, quoteString(requestedBy))
	if _, err := i.engine.Execute(ctx, fmt.Sprintf(
		`mutationCreateCanvasState({"stateId": %s, "space": %s, "kind": "card", "data": %s, "visibility": "private", "forUserId": %s, "actor": %s, "importance": "notify"})`,
		quoteString(stateId),
		quoteString(spaceId),
		cardData,
		quoteString(requestedBy),
		actorJSON,
	)); err != nil {
		i.Logger.Debug("training.emitRetryFailedCard: publish failed", "err", err)
	}
}

func humanStep(step string) string {
	switch step {
	case retryStepIdentityVector:
		return "identity vector"
	case retryStepDistillPrompt:
		return "system prompt"
	default:
		return step
	}
}
