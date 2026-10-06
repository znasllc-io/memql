package planner

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

const reactiveSystemActor = "system:reactive-loop"

type responsibilityGoals interface {
	HasLiveGoalForResponsibility(ctx context.Context, ownerUserId, responsibilityId string) (bool, error)
	OpenResponsibilityGoal(ctx context.Context, g workintegration.ResponsibilityGoal) (string, string, error)
	// OpenDirectGoal opens a goal whose template is already known -- what the
	// refresh cadence and the approved-training gate use (memql#5051). It is
	// on this interface rather than a second one because a node either has the
	// work spine or it does not; splitting them would make "has the reactive
	// loop's half but not training's" representable, and nothing can be in
	// that state.
	OpenDirectGoal(ctx context.Context, g workintegration.DirectGoal) (string, string, error)
}

type ReactiveLoop struct {
	engine Engine
	logger plannerLogger
	// goals is the work spine. Nil on a node whose work plug-in did not
	// materialize, which is LOUD rather than silent: without it a due
	// responsibility spawns nothing at all, and "my standing directive
	// stopped running" names nothing on its own.
	goals responsibilityGoals

	mu        sync.Mutex
	claims    workflowClaimer
	writeGate func(context.Context, string) (func(), error)
}

type plannerLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

func (r *ReactiveLoop) SetWorkGoals(g responsibilityGoals) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.goals = g
}

func NewReactiveLoop(engine Engine, logger plannerLogger) *ReactiveLoop {
	return &ReactiveLoop{engine: engine, logger: logger}
}

func (r *ReactiveLoop) tick(ctx context.Context) {
	if err := r.run(ctx); err != nil && r.logger != nil {
		r.logger.Warn("responsibility workflow failed", "error", err)
	}
}

func (r *ReactiveLoop) run(ctx context.Context) error {
	if r.engine == nil {
		return fmt.Errorf("responsibility sweep has no engine")
	}
	byUser := map[string][]map[string]any{}
	_, err := workflowhost.Run(ctx, "plannerResponsibilitySweep", nil, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"plannerResponsibilityOwners": func(ctx context.Context, _ map[string]any) (any, error) {
			rows, err := r.sweepResponsibilities(ctx)
			if err != nil {
				return nil, err
			}
			owners := []string{}
			for _, row := range rows {
				owner := getString(row, "ownerUserId")
				if owner == "" {
					continue
				}
				if _, ok := byUser[owner]; !ok {
					owners = append(owners, owner)
				}
				byUser[owner] = append(byUser[owner], row)
			}
			return owners, nil
		},
		"plannerProcessOwner": func(ctx context.Context, a map[string]any) (any, error) {
			owner := getString(a, "owner")
			rows, ok := byUser[owner]
			if !ok {
				return nil, fmt.Errorf("owner is outside the authorized sweep")
			}
			ownerCtx := ownerActorContext(ctx, owner)
			selected := map[string]map[string]any{}
			ids := []string{}
			for _, row := range rows {
				id := getString(row, "id")
				selected[id] = row
				ids = append(ids, id)
			}
			return workflowhost.Run(ownerCtx, "plannerOwnerWorkflow", map[string]any{"ids": ids}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
				"plannerProcessResponsibility": func(ctx context.Context, a map[string]any) (any, error) {
					row, ok := selected[getString(a, "id")]
					if !ok {
						return nil, fmt.Errorf("responsibility outside owner scope")
					}
					return r.runRow(ctx, owner, row, "plannerResponsibilityWorkflow", time.Now().UTC(), nil)
				},
				"plannerConverge": func(ctx context.Context, _ map[string]any) (any, error) {
					return nil, r.converge(ctx, owner, rows, time.Now().UTC())
				},
			}})
		},
	}})
	return err
}

func (r *ReactiveLoop) sweepResponsibilities(ctx context.Context) ([]map[string]any, error) {
	res, err := r.engine.Execute(reactiveSystemActorContext(ctx),
		`query activeResponsibilitiesAcrossUsers()`)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

func (r *ReactiveLoop) processResponsibility(ctx context.Context, userId string, row map[string]any, now time.Time) bool {
	value, err := r.runRow(ctx, userId, row, "plannerResponsibilityWorkflow", now, nil)
	if err != nil && r.logger != nil {
		r.logger.Warn("responsibility workflow failed", "responsibilityId", getString(row, "id"), "error", err)
	}
	honored, _ := value.(bool)
	return honored
}

func (r *ReactiveLoop) isDue(trigger string, row map[string]any, now time.Time) bool {
	value, err := workflowhost.Run(context.Background(), "plannerResponsibilityDue", map[string]any{"trigger": trigger, "recurringDue": r.recurringIsDue(row, now)}, workflowhost.Options{})
	due, _ := value.(bool)
	return err == nil && due
}

func (r *ReactiveLoop) recurringIsDue(row map[string]any, now time.Time) bool {
	schedule := strings.TrimSpace(getString(row, "schedule"))
	if schedule == "" {
		return false
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		// Unparseable cron -- don't wedge or spam. Log-once-ish via Debug
		// and treat as not-due (a malformed schedule shouldn't fire every
		// tick).
		r.logger.Debug("planner reactive loop: bad cron schedule",
			"schedule", schedule, "error", err)
		return false
	}

	// Anchor the "since" point. Look back from one occurrence before now
	// to find the previous fire time, then check it's after when we last
	// evaluated (and recent enough to count as "now").
	last := parseTimeOrZero(getString(row, "lastEvaluatedAt"))
	// cron.Schedule only exposes Next(); to find the latest occurrence
	// <= now we step Next() forward from a point safely before now and
	// keep the last one that's still <= now.
	prev := latestOccurrenceBefore(sched, now)
	if prev.IsZero() {
		return false
	}
	return last.IsZero() || last.Before(prev)
}

func (r *ReactiveLoop) hasLiveGoal(ctx context.Context, userId, respId string) bool {
	r.mu.Lock()
	goals := r.goals
	r.mu.Unlock()
	if goals == nil {
		return false
	}
	live, err := goals.HasLiveGoalForResponsibility(ctx, userId, respId)
	if err != nil {
		r.logger.Debug("planner reactive loop: live-goal check failed",
			"responsibilityId", respId, "error", err)
		return false
	}
	return live
}

func (r *ReactiveLoop) routeResponsibility(ctx context.Context, userId string, row map[string]any) (string, error) {
	value, err := r.runRow(ctx, userId, row, "plannerRouteResponsibility", time.Now().UTC(), nil)
	agent, _ := value.(string)
	return agent, err
}

func (r *ReactiveLoop) resolveAssistant(ctx context.Context, userId string) string {
	q := fmt.Sprintf(`query assistantAgentForUser(ownerUserId:%s)`, langparser.QuoteString(userId))
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("planner reactive loop: resolveAssistant failed",
			"userId", userId, "error", err)
		return ""
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return ""
	}
	return getString(rows[0], "id")
}

func (r *ReactiveLoop) mintOrExtendSpecialist(ctx context.Context, userId string, row map[string]any) (string, error) {
	goal := getString(row, "statement")
	if goal == "" {
		return "", fmt.Errorf("responsibility has no statement to route on")
	}
	partitionId := getString(row, "scopePartitionId")
	call := fmt.Sprintf(
		`builtin ensureAgentForGoal(goal:%s, ownerUserId:%s, partitionId:%s)`,
		langparser.QuoteString(goal), langparser.QuoteString(userId), langparser.QuoteString(partitionId),
	)
	res, err := r.engine.Execute(ctx, call)
	if err != nil {
		return "", fmt.Errorf("ensureAgentForGoal: %w", err)
	}
	agentId, action := extractAgentFactoryResult(res)
	r.logger.Info("planner reactive loop: factory resolved specialist",
		"responsibilityId", getString(row, "id"), "agentId", agentId,
		"factoryAction", action)
	return agentId, nil
}

func (r *ReactiveLoop) assign(ctx context.Context, respId, targetKind, agentId, roleSlug string) {
	args := map[string]any{
		"responsibilityId": respId,
		"targetKind":       targetKind,
	}
	if agentId != "" {
		args["assignedAgentId"] = agentId
	}
	if roleSlug != "" {
		args["assignedRoleSlug"] = roleSlug
	}
	q := fmt.Sprintf(`assignResponsibility(%s)`, encodeArgs(args))
	if _, err := r.engine.Execute(ctx, q); err != nil {
		r.logger.Warn("planner reactive loop: assign failed",
			"responsibilityId", respId, "agentId", agentId, "error", err)
	}
}

func (r *ReactiveLoop) honorResponsibility(ctx context.Context, userId string, row map[string]any, agentId string) (string, error) {
	value, err := r.runRow(ctx, userId, row, "plannerHonorResponsibility", time.Now().UTC(), map[string]any{"agentId": agentId})
	result, _ := value.(string)
	return result, err
}

func (r *ReactiveLoop) openResponsibilityGoal(ctx context.Context, userId string, row map[string]any, agentId string) (string, error) {
	value, err := r.runRow(ctx, userId, row, "plannerOpenResponsibility", time.Now().UTC(), map[string]any{"agentId": agentId})
	id, _ := value.(string)
	return id, err
}

func (r *ReactiveLoop) maybeInjectStanding(ctx context.Context, row map[string]any, agentId string) {
	_, err := r.runRow(ctx, "", row, "plannerInjectStanding", time.Now().UTC(), map[string]any{"agentId": agentId})
	if err != nil && r.logger != nil {
		r.logger.Warn("standing directive injection failed", "error", err)
	}
}

func (r *ReactiveLoop) appendDirective(ctx context.Context, row map[string]any, agentId string) error {
	directive := strings.TrimSpace(getString(row, "statement"))
	if directive == "" || agentId == "" {
		return nil
	}
	if r.writeGate == nil {
		return fmt.Errorf("standing directive requires shared write coordination")
	}
	release, err := r.writeGate(ctx, agentId)
	if err != nil {
		return err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	agent := r.loadAgent(ctx, agentId)
	if agent == nil {
		r.logger.Debug("planner reactive loop: standing inject -- agent not found",
			"agentId", agentId, "responsibilityId", getString(row, "id"))
		return nil
	}
	version, _ := agent["createdAt"].(time.Time)
	if version.IsZero() {
		version = parseTimeOrZero(getString(agent, "createdAt"))
	}
	if version.IsZero() {
		return fmt.Errorf("standing directive requires the observed agent version")
	}
	ctx = memql.ContextWithRowVersionFence(ctx, "v1:agents:agent", agentId, version)
	lineage, _ := agent["lineage"].(map[string]any)
	goals := toStringList(mapGet(lineage, "extensionGoals"))
	for _, g := range goals {
		if strings.EqualFold(strings.TrimSpace(g), directive) {
			// Already injected -- nothing to do.

			return nil
		}
	}
	goals = append(goals, directive)
	// Partial-update only the lineage.extensionGoals path. updateAgent
	// merges the payload object onto the prior row.
	updatedLineage := make(map[string]any, len(lineage)+1)
	for key, value := range lineage {
		updatedLineage[key] = value
	}
	updatedLineage["extensionGoals"] = goals
	payload := map[string]any{"lineage": updatedLineage}
	call := fmt.Sprintf(`mutation updateAgent(agentId:%s, payload:%s)`,
		langparser.QuoteString(agentId), mustJSONObject(payload))
	if _, err := r.engine.Execute(ctx, call); err != nil {
		r.logger.Warn("planner reactive loop: standing inject failed",
			"agentId", agentId, "responsibilityId", getString(row, "id"), "error", err)
		return err
	}

	r.logger.Info("planner reactive loop: injected standing directive",
		"agentId", agentId, "responsibilityId", getString(row, "id"))
	return nil
}

func (r *ReactiveLoop) recordEvaluation(ctx context.Context, respId, result string) {
	q := fmt.Sprintf(
		`mutation recordResponsibilityEvaluation(responsibilityId:%s, lastResult:%s)`,
		langparser.QuoteString(respId), langparser.QuoteString(truncate(result, 280)),
	)
	if _, err := r.engine.Execute(ctx, q); err != nil {
		r.logger.Warn("planner reactive loop: recordEvaluation failed",
			"responsibilityId", respId, "error", err)
	}
}

func (r *ReactiveLoop) maybeConverge(ctx context.Context, userId string, rows []map[string]any, now time.Time) {
	if err := r.converge(ctx, userId, rows, now); err != nil && r.logger != nil {
		r.logger.Debug("planner convergence failed", "userId", userId, "error", err)
	}
}

func (r *ReactiveLoop) dispatchConvergenceAction(ctx context.Context, userId string, a convergenceAction, now time.Time) {
	if err := r.dispatch(ctx, userId, a, now); err != nil && r.logger != nil {
		r.logger.Warn("planner convergence action failed", "error", err)
	}
}

func (r *ReactiveLoop) loadSpaceGoals(ctx context.Context, userId string) []map[string]any {
	q := fmt.Sprintf(`query queryActiveSpaces(userId:%s)`, langparser.QuoteString(userId))
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("planner reactive loop: loadSpaceGoals failed",
			"userId", userId, "error", err)
		return nil
	}
	out := []map[string]any{}
	for _, sp := range memql.MaterializeRows(res) {
		goal, _ := sp["goal"].(map[string]any)
		statement := strings.TrimSpace(getString(goal, "statement"))
		if statement == "" {
			continue
		}
		out = append(out, map[string]any{
			"partitionId": getString(sp, "id"),
			"name":        getString(sp, "name"),
			"statement":   statement,
			"timeframe":   getString(goal, "timeframe"),
		})
	}
	return out
}

func (r *ReactiveLoop) loadRecentMemory(ctx context.Context, text string, k int) []map[string]any {
	q := fmt.Sprintf(`builtin recall(text:%s, k:%d)`,
		langparser.QuoteString(text), k)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("planner reactive loop: recall failed (harness likely unwired)",
			"error", err)
		return nil
	}
	rows := memql.MaterializeRows(res)
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		entry := map[string]any{
			"content":   getString(m, "content"),
			"createdAt": getString(m, "createdAt"),
		}
		out = append(out, entry)
	}
	return out
}

func (r *ReactiveLoop) loadAgent(ctx context.Context, agentId string) map[string]any {
	q := fmt.Sprintf(`query agentById(agentId:%s)`, langparser.QuoteString(agentId))
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		return nil
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}
