//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
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
	// #2800: reads the owning user's preferences server-side (worker
	// kill-switch), not the caller's.
	query := fmt.Sprintf(`query userByIdSystem(userId:%s)`, langparser.QuoteString(userId))
	res, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
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
// Reads via agentAuthorizationsForSelf() -- a shape()
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
	// #3177: `agentAuthorizationsForSelf` is self-scoped on actor.userId and
	// takes no userId argument, because v1:agents:agentAuthorization declares
	// `@rowAuthz(owner="userId")` and a caller-supplied-id read of a declared
	// concept is what #3172's land gate refuses.
	//
	// The dispatcher's ctx is NOT reliably the grant owner's -- ownerUserId is
	// resolved from the AGENT row (replier.go), and an agent answers in spaces
	// its owner does not have to be the caller in. So the owner's actor
	// envelope is supplied for this ONE Execute, built inline as the argument
	// (the memql#3072 shape epic decision C blesses); it is never stamped onto
	// the request's own context, which memql#2989 refuted.
	res, err := s.Engine.Execute(
		auth.ContextWithUserActor(ctx, ownerUserId),
		`query agentAuthorizationsForSelf()`)
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
	query := fmt.Sprintf(`query planById(planId:%s)`, langparser.QuoteString(planId))
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
		"routing":       row.Routing,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("agent.worker store: marshal invocation: %w", err)
	}
	query := fmt.Sprintf("createWorkerInvocation(%s)", string(body))
	_, err = s.Engine.Execute(ctx, query)
	return err
}

// -- the fleet reads (memql#4351) --------------------------------------------
//
// ONE STAMP, and why it is the actor rather than internal origin.
//
// v1:worker:registration and v1:worker:routingPolicy declare the composite
// owner tier, and rowAuthzAdmits has NO internal-origin escape on the read
// path: a context with no actor resolves the owner comparison against an empty
// caller and denies every row -- silently, with no error, which is the failure
// that reads as "this user has no machines". So the ACTOR is what these reads
// need, and it is the owner whose fleet this turn is routing.
//
// The queries themselves are caller-scoped (`ownerUserId==actor.userId`)
// rather than argument-scoped, which is why none of them is @serverOnly and
// why none needs an internal-origin stamp. The earlier shape took an
// ownerUserId argument and carried @serverOnly to excuse it; taking the owner
// from the actor instead removes the argument, so there is no id to supply and
// nothing to enumerate.
//
// The actor context is built inline as the argument to one Execute and never
// stamped onto the request's own context -- the memql#3072 shape, not the
// memql#2989 one.
//
// The ownerUserId is not caller-supplied in any meaningful sense: it is
// resolved from the AGENT row (replier.go) before the tool loop runs, the same
// value AgentAuthorization above scopes on.

func (s *EngineStore) fleetContext(ctx context.Context, ownerUserId string) context.Context {
	return auth.ContextWithUserActor(ctx, ownerUserId)
}

// WorkersForOwner returns the owner's machines in registration order.
func (s *EngineStore) WorkersForOwner(ctx context.Context, ownerUserId string) ([]Candidate, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, fmt.Errorf("agent.worker store: ownerUserId is required")
	}
	res, err := s.Engine.Execute(s.fleetContext(ctx, ownerUserId), `query myWorkersWithStatus()`)
	if err != nil {
		return nil, fmt.Errorf("fleet read: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	rows := outputPayloadRows(res.OutputPayload())
	out := make([]Candidate, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		id := rowString(row, "id")
		if id == "" {
			continue
		}
		out = append(out, Candidate{
			RegistrationId: id,
			Name:           rowString(row, "name"),
			DisplayName:    rowString(row, "displayName"),
			Capabilities:   rowStringList(row, "capabilities"),
			// The merge happens HERE, once, on the way out of the store --
			// so no caller can accidentally match on the cockpit's map alone
			// and quietly ignore the labels the owner set.
			Labels:          MergeLabels(rowStringMap(row, "labels"), rowStringMap(row, "operatorLabels")),
			Concurrency:     rowUint32Map(row, "concurrency"),
			ActiveCount:     rowInt(row, "activeCount"),
			ConnectedNodeId: rowString(row, "connectedNodeId"),
			LastSelectedAt:  rowTime(row, "lastSelectedAt"),
			LastSeenAt:      rowTime(row, "lastSeenAt"),
			RevokedAt:       rowTime(row, "revokedAt"),
		})
	}
	return out, nil
}

// RoutingPolicyForOwner returns the owner's active policy, or nil when they
// have none. Nil is the COMMON case and not an error: a user who never opened
// the Fleet page routes on DefaultPolicy.
func (s *EngineStore) RoutingPolicyForOwner(ctx context.Context, ownerUserId string) (*Policy, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, nil
	}
	res, err := s.Engine.Execute(s.fleetContext(ctx, ownerUserId), `query routingPolicyForOwner()`)
	if err != nil {
		return nil, fmt.Errorf("routing policy read: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	// The query sorts newest first and the FIRST row wins. One active policy
	// per user is the model, but the DSL cannot enforce it (@unique is
	// declared metadata, memql#2960), so taking the first of a deterministic
	// order is what makes a second active row harmless instead of making two
	// replicas route differently.
	for _, row := range outputPayloadRows(res.OutputPayload()) {
		if row == nil {
			continue
		}
		return &Policy{
			Id:            rowString(row, "id"),
			Strategy:      rowString(row, "strategy"),
			RequireLabels: rowStringMap(row, "requireLabels"),
			PreferLabels:  rowStringMap(row, "preferLabels"),
			Fallback:      rowString(row, "fallback"),
		}, nil
	}
	return nil, nil
}

// TouchWorkerSelected stamps lastSelectedAt on the machine the router picked.
func (s *EngineStore) TouchWorkerSelected(ctx context.Context, registrationId, ownerUserId string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	if strings.TrimSpace(registrationId) == "" {
		return nil
	}
	query := fmt.Sprintf(`touchWorkerSelected(registrationId:%s)`, langparser.QuoteString(registrationId))
	_, err := s.Engine.Execute(s.fleetContext(ctx, ownerUserId), query)
	return err
}

// -- row readers -------------------------------------------------------------
//
// These read the map[string]any rows outputPayloadRows produces, which is the
// shape a shape() query lands on (the Data axis), not res.Bundle.Nodes.

func rowString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func rowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func rowStringList(row map[string]any, key string) []string {
	raw, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func rowStringMap(row map[string]any, key string) map[string]string {
	raw, ok := row[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = fmt.Sprintf("%t", t)
		case float64:
			if t == float64(int64(t)) {
				out[k] = fmt.Sprintf("%d", int64(t))
			} else {
				out[k] = fmt.Sprintf("%g", t)
			}
		}
	}
	return out
}

func rowUint32Map(row map[string]any, key string) map[string]uint32 {
	raw, ok := row[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]uint32, len(raw))
	for k, v := range raw {
		if f, ok := v.(float64); ok && f >= 0 {
			out[k] = uint32(f)
		}
	}
	return out
}

// rowTime parses an RFC3339 timestamp. An empty or unparseable value yields
// the zero time, which every caller reads as "never" -- and for lastSeenAt
// that means offline, which is the safe direction: a machine whose timestamp
// cannot be read is not one to send work to.
func rowTime(row map[string]any, key string) time.Time {
	raw := rowString(row, key)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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

// DelegationPolicy resolves the user's app-delegation preference
// (memql#4362).
//
// The read runs under the OWNER's actor rather than a system one.
// `delegationPolicyForUser` is caller-scoped by design -- it is a
// user's own preference, readable in the portal -- so the engine
// BORROWS the owner's authority the same way the campaign sender
// does, instead of out-ranking them with a system read that would
// also work for anyone else's row.
//
// An absent row means "never delegate": PreferSubscriptionApps stays
// false, so a user who has not opted in is never surprised by an
// agent running on their laptop.
func (s *EngineStore) DelegationPolicy(ctx context.Context, ownerUserId string) (DelegationPolicy, error) {
	if s == nil || s.Engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return DelegationPolicy{}, nil
	}
	// No ownerUserId argument: the query scopes on actor.userId. The engine
	// BORROWS the owner's actor for the read -- the same borrowed-authority
	// pattern the campaign sender uses -- rather than out-ranking them with
	// a system read that would work for anyone else's row too.
	res, err := s.Engine.Execute(
		auth.ContextWithUserActor(ctx, ownerUserId), "query delegationPolicyForUser()")
	if err != nil {
		return DelegationPolicy{}, fmt.Errorf("delegation policy lookup: %w", err)
	}
	// A shape() query lands on the Data axis, not on Bundle.Nodes --
	// reading the wrong one is how a query that works in psql returns
	// nothing here.
	rows := outputPayloadRows(res.OutputPayload())
	if len(rows) == 0 {
		return DelegationPolicy{}, nil
	}
	row := rows[0]
	policy := DelegationPolicy{
		Found:                  true,
		PreferSubscriptionApps: boolFrom(row["preferSubscriptionApps"]),
		EligibleKinds:          stringsFrom(row["eligibleKinds"]),
		AppOrder:               stringsFrom(row["appOrder"]),
		MaxConcurrentSessions:  intFrom(row["maxConcurrentSessions"]),
		WorkspaceRoot:          stringFrom(row["workspaceRoot"]),
	}
	if secs := intFrom(row["credentialLifetimeSeconds"]); secs > 0 {
		policy.CredentialLifetime = time.Duration(secs) * time.Second
	}
	// Zero reads as the DEFAULT rather than as "none": a zero here
	// would silently disable a feature the user turned on, which is
	// the opposite of what writing 0 into an unset field means.
	if policy.MaxConcurrentSessions <= 0 {
		policy.MaxConcurrentSessions = 1
	}
	return policy, nil
}

func boolFrom(v any) bool {
	b, _ := v.(bool)
	return b
}

func stringFrom(v any) string {
	s, _ := v.(string)
	return s
}

func intFrom(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func stringsFrom(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
