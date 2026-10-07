package planner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// RefreshCron retains the native work-goal port. Its cadence, selection,
// stale threshold and training recipe are authored in dsl/knowledge.
// The shared strict claim is fail-closed and survives a planner handoff.
type RefreshCron struct {
	engine Engine
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
		Debug(string, ...any)
	}
	mu     sync.Mutex
	goals  responsibilityGoals
	claims workflowClaimer
}

type workflowClaimer interface {
	ClaimWithTTL(context.Context, string, string, time.Duration) bool
}

func NewRefreshCron(engine Engine, logger interface {
	Info(string, ...any)
	Warn(string, ...any)
	Debug(string, ...any)
}) *RefreshCron {
	return &RefreshCron{engine: engine, logger: logger}
}
func (c *RefreshCron) SetWorkGoals(g responsibilityGoals) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.goals == nil {
		c.goals = g
	}
}
func (c *RefreshCron) goalsRef() responsibilityGoals {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.goals
}

func (c *RefreshCron) run(ctx context.Context) error {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return fmt.Errorf("knowledge refresh requires the trusted scheduler")
	}
	if c.engine == nil {
		return fmt.Errorf("knowledge refresh has no engine")
	}
	_, err := workflowhost.Run(ctx, "knowledgeRefreshSweep", nil, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"knowledgeRefreshCandidates": func(ctx context.Context, _ map[string]any) (any, error) {
			call, err := parser.RenderCall("knowledgeDomainsDueRefresh", nil)
			if err != nil {
				return nil, err
			}
			result, err := c.engine.Execute(systemActorContext(ctx), call)
			if err != nil {
				return nil, err
			}
			return memql.MaterializeRows(result), nil
		},
		"knowledgeRefreshCandidate": func(ctx context.Context, a map[string]any) (any, error) {
			row, _ := a["domain"].(map[string]any)
			return nil, c.refreshCandidate(ctx, row, false, time.Now().UTC())
		},
	}})
	return err
}

func (c *RefreshCron) HandleDomainUpdated(ev events.Event) {
	if c == nil || ev.Payload == nil {
		return
	}
	row, _ := ev.Payload["payload"].(map[string]any)
	if row == nil {
		return
	}
	copy := map[string]any{}
	for k, v := range row {
		copy[k] = v
	}
	copy["id"] = ev.Payload["id"]
	if err := c.refreshCandidate(context.Background(), copy, true, time.Now().UTC()); err != nil && c.logger != nil {
		c.logger.Warn("knowledge stale refresh failed", "error", err)
	}
}

func (c *RefreshCron) refreshCandidate(ctx context.Context, row map[string]any, stale bool, now time.Time) error {
	id := getString(row, "id")
	if id == "" {
		return nil
	}
	args := refreshFacts(row, now)
	args["staleEvent"] = stale
	_, err := workflowhost.Run(ctx, "knowledgeRefreshCandidateWorkflow", args, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"knowledgeClaimRefresh": func(ctx context.Context, a map[string]any) (any, error) {
			if c.claims == nil {
				return nil, fmt.Errorf("knowledge refresh requires a durable claim store")
			}
			hours := numberField(a["hours"])
			if hours <= 0 || hours > 168 {
				return nil, fmt.Errorf("invalid refresh claim duration")
			}
			return c.claims.ClaimWithTTL(ctx, "knowledge.refresh", id, time.Duration(hours*float64(time.Hour))), nil
		},
		"knowledgeOpenRefresh": func(ctx context.Context, _ map[string]any) (any, error) { return nil, c.spawnRefreshPlan(ctx, row) },
	}})
	return err
}

func refreshFacts(row map[string]any, now time.Time) map[string]any {
	age := 0.0
	valid := false
	if stamp, err := time.Parse(time.RFC3339, getString(row, "lastSeededAt")); err == nil {
		age = now.Sub(stamp).Hours() / 24
		valid = true
	}
	return map[string]any{"cadence": row["refreshCadenceDays"], "ageDays": age, "validSeedTime": valid, "staleSignalCount": row["staleSignalCount"]}
}

// Kept as a small pure policy entry for deterministic due-date tests; the rule is DSL.
func (c *RefreshCron) domainIsDue(row map[string]any, now time.Time) bool {
	value, err := workflowhost.Run(context.Background(), "knowledgeRefreshDue", refreshFacts(row, now), workflowhost.Options{})
	if err != nil {
		return false
	}
	due, _ := value.(bool)
	return due
}

func (c *RefreshCron) spawnRefreshPlan(ctx context.Context, row map[string]any) error {
	goals := c.goalsRef()
	if goals == nil {
		return fmt.Errorf("refresh cron: no work-goal surface on this node, so a due domain cannot be refreshed")
	}
	_, err := workflowhost.Run(ctx, "knowledgeOpenRefreshWorkflow", map[string]any{"domainId": getString(row, "id"), "name": getString(row, "name"), "owner": getString(row, "ownerId")}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"knowledgeDispatchRefresh": func(ctx context.Context, a map[string]any) (any, error) {
			input, _ := a["input"].(map[string]any)
			_, _, err := goals.OpenDirectGoal(ctx, workintegration.DirectGoal{OwnerUserId: getString(a, "owner"), Statement: getString(a, "statement"), AutomationName: getString(a, "automation"), Input: input, RequestedVia: getString(a, "requestedVia"), TriggeredBy: getString(a, "triggeredBy")})
			return nil, err
		},
	}})
	return err
}

func numberField(v any) float64 {
	switch v := v.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
