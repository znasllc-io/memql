//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	memqlengine "github.com/visionarys-io/memql/component/memql"
	workerservice "github.com/visionarys-io/memql/component/worker"
)

// EngineStore is the production Store implementation. Reads + writes
// are routed through the MemQL engine; the queries already exist
// (User by id, agent authorization, plan by id) so we just shape the
// projections.
type EngineStore struct {
	Engine *memqlengine.MemQLEngine
}

// UserPreferences resolves the user's computerUseEnabled flag.
// Any error or missing row defaults to enabled=true so the kill
// switch defaults to "permitted" (Q13: opt-in to disable).
func (s *EngineStore) UserPreferences(ctx context.Context, userId string) (Preferences, error) {
	if s == nil || s.Engine == nil {
		return Preferences{ComputerUseEnabled: true}, nil
	}
	if strings.TrimSpace(userId) == "" {
		return Preferences{ComputerUseEnabled: true}, nil
	}
	query := fmt.Sprintf(`queryUserById({"userId":%q})`, userId)
	res, err := s.Engine.Execute(ctx, query)
	if err != nil {
		return Preferences{ComputerUseEnabled: true}, fmt.Errorf("user lookup: %w", err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return Preferences{ComputerUseEnabled: true}, nil
	}
	prefs := nestedObject(res.Bundle.Nodes[0], "preferences")
	if prefs == nil {
		return Preferences{ComputerUseEnabled: true}, nil
	}
	enabled := true
	if v, ok := prefs["computerUseEnabled"].(bool); ok {
		enabled = v
	}
	return Preferences{ComputerUseEnabled: enabled}, nil
}

// AgentAuthorization resolves the standing agentAuthorization for
// (agentId, userId). Returns nil + no error when none exists --
// the dispatcher treats nil as "no scope" and rejects.
//
// Reads via queryAgentAuthorizationsForUser({userId}) -- a shape()
// query, so results land on res.OutputPayload (the Data axis) not
// res.Bundle.Nodes. Walks the projected rows and matches on
// agentId tolerantly (bare-slug or canonical-form), because the
// agentAuthorization concept has no @relationship on agentId yet --
// auto-canon doesn't fire and the row's stored agentId can be
// either form depending on which writer landed it. The frontend's
// PlanScopeElevationCard.allow path uses the same tolerant
// matcher when locating the row to update; this read path mirrors
// it so the round-trip stays consistent.
//
// Without this implementation, the dispatcher always saw
// standingScope="" and rejected every workerHost / workerComputer
// call with "action requires scope X, agent has """ -- the
// "I clicked Allow three times and Sofia keeps saying she doesn't
// have full" loop the operator hit.
func (s *EngineStore) AgentAuthorization(ctx context.Context, agentId, ownerUserId string) (*Authorization, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	if strings.TrimSpace(agentId) == "" || strings.TrimSpace(ownerUserId) == "" {
		return nil, nil
	}
	query := fmt.Sprintf(`queryAgentAuthorizationsForUser({userId:%q})`, ownerUserId)
	res, err := s.Engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("agent authorization lookup: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	targetSuffix := agentId
	if i := strings.LastIndex(agentId, ":"); i >= 0 {
		targetSuffix = agentId[i+1:]
	}
	for _, row := range outputPayloadRows(res.OutputPayload()) {
		if row == nil {
			continue
		}
		rowAgent, _ := row["agentId"].(string)
		if rowAgent == "" {
			continue
		}
		rowSuffix := rowAgent
		if i := strings.LastIndex(rowAgent, ":"); i >= 0 {
			rowSuffix = rowAgent[i+1:]
		}
		if rowAgent != agentId && rowSuffix != targetSuffix {
			continue
		}
		auth := &Authorization{
			AgentId: rowAgent,
			UserId:  ownerUserId,
		}
		if id, ok := row["id"].(string); ok {
			auth.ID = id
		}
		if scope, ok := row["computerUseScope"].(string); ok {
			auth.ComputerUseScope = strings.TrimSpace(scope)
		}
		return auth, nil
	}
	return nil, nil
}

// outputPayloadRows lives in integration.go (same package); the
// store reuses that helper rather than duplicating it.

// PlanScope resolves Plan.computerUseScope for the supplied plan id.
// Empty string means the Plan didn't declare a scope; dispatch
// falls back to the agent's standing scope.
func (s *EngineStore) PlanScope(ctx context.Context, planId string) (string, error) {
	if s == nil || s.Engine == nil {
		return "", nil
	}
	if strings.TrimSpace(planId) == "" {
		return "", nil
	}
	// Re-uses the existing planFull query.
	query := fmt.Sprintf(`queryPlanById({"planId":%q})`, planId)
	res, err := s.Engine.Execute(ctx, query)
	if err != nil {
		return "", fmt.Errorf("plan lookup: %w", err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return "", nil
	}
	scope := stringField(res.Bundle.Nodes[0], "computerUseScope")
	return scope, nil
}

// WriteInvocation persists the invocation row by routing through
// the same component/worker store the WorkerService uses.
func (s *EngineStore) WriteInvocation(ctx context.Context, row workerservice.InvocationRow) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	args := map[string]any{
		"invocationId":  row.ID,
		"ownerUserId":   row.OwnerUserId,
		"workerId":      row.WorkerId,
		"agentId":       row.AgentId,
		"planId":        row.PlanId,
		"taskId":        row.TaskId,
		"correlationId": row.CorrelationId,
		"tool":          row.Tool,
		"action":        row.Action,
		"argsRedacted":  row.ArgsRedacted,
		"startedAt":     row.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"completedAt":   maybeTime(row.CompletedAt),
		"durationMs":    row.DurationMs,
		"outcome":       row.Outcome,
		"exitCode":      row.ExitCode,
		"signal":        row.Signal,
		"errorCode":     row.ErrorCode,
		"errorMessage":  row.ErrorMessage,
		"bytesIn":       row.BytesIn,
		"bytesOut":      row.BytesOut,
		"outputPreview": row.OutputPreview,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("agent.worker store: marshal invocation: %w", err)
	}
	query := fmt.Sprintf("mutationCreateWorkerInvocation(%s)", string(body))
	_, err = s.Engine.Execute(ctx, query)
	return err
}

// -- helpers ----------------------------------------------------------------

func nestedObject(node *memqlv1.MemoryNode, key string) map[string]any {
	if node == nil || node.Payload == nil {
		return nil
	}
	fields := node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	st := v.GetStructValue()
	if st == nil {
		return nil
	}
	out := make(map[string]any, len(st.Fields))
	for k, val := range st.Fields {
		if val == nil {
			continue
		}
		out[k] = val.AsInterface()
	}
	return out
}

func stringField(node *memqlv1.MemoryNode, key string) string {
	if node == nil || node.Payload == nil {
		return ""
	}
	fields := node.Payload.GetFields()
	if fields == nil {
		return ""
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(v.GetStringValue())
}

func maybeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
