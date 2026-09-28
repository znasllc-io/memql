//go:build agent || planner

package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	fleetcatalog "github.com/znasllc-io/memql/component/worker/fleetcatalog"
	"github.com/znasllc-io/memql/core/num"
)

// EngineStore is the production Store implementation. Reads + writes
// are routed through the MemQL engine; the queries already exist
// (User by id, agent authorization, plan by id) so we just shape the
// projections.
type EngineStore struct {
	Engine *memqlengine.MemQLEngine
}

// UserPreferences resolves the user's computer-use kill switch. Any error or
// missing row leaves it NOT engaged (Q13: opt in to disable) -- see
// killSwitchEngaged for the one value that engages it.
func (s *EngineStore) UserPreferences(ctx context.Context, userId string) (Preferences, error) {
	if s == nil || s.Engine == nil {
		return Preferences{}, nil
	}
	if strings.TrimSpace(userId) == "" {
		return Preferences{}, nil
	}
	// #2800: reads the owning user's preferences server-side (worker
	// kill-switch), not the caller's.
	query := fmt.Sprintf(`query userByIdSystem(userId:%s)`, langparser.QuoteString(userId))
	res, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return Preferences{}, fmt.Errorf("user lookup: %w", err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return Preferences{}, nil
	}
	return Preferences{KillSwitchEngaged: killSwitchEngaged(nestedObject(res.Bundle.Nodes[0], "preferences"))}, nil
}

// killSwitchEngaged reads the switch off a user's preferences object. It is
// engaged ONLY by an explicit boolean false: a missing object, a missing key
// and a value of any other type all read as not engaged, because the switch
// is something a person throws and none of those is a person throwing it.
func killSwitchEngaged(prefs map[string]any) bool {
	enabled, ok := prefs["computerUseEnabled"].(bool)
	return ok && !enabled
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

// outputPayloadRows is shared by the fleet store and the agent integration.

// WriteInvocation persists the invocation row by routing through
// the same component/worker store the WorkerService uses.
//
// ownerUserId is NOT an argument: v1:worker:invocation declares the composite
// owner tier and marks the field @serverSet (memql#4406), so the owner is
// stamped from actor.userId. A blank owner is refused rather than passed
// through -- auth.ContextWithUserActor returns ctx UNCHANGED for a blank id,
// and the write would then land on a row nobody can read, which surfaces much
// later as "the fleet page shows no activity".
func (s *EngineStore) WriteInvocation(ctx context.Context, row workerservice.InvocationRow) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, query, err := invocationWriteCall(ctx, row)
	if err != nil {
		return err
	}
	_, err = s.Engine.Execute(writeCtx, query)
	return err
}

// invocationWriteCall builds the borrowed-authority context and the mutation
// text for one invocation write.
//
// Split out of WriteInvocation so the two properties memql#4406 depends on are
// checkable without an engine: that the call runs as the OWNER, and that
// ownerUserId does not also travel as an argument. EngineStore holds a concrete
// *MemQLEngine rather than an interface, so there is no fake to record a
// context off -- and both properties fail SILENTLY. An unstamped write lands a
// row with an empty owner that nobody can read, and an undeclared argument is
// discarded without an error (memql#3626), so a reviewer sees a plausible
// ownerUserId in the map and concludes the owner reached the row.
func invocationWriteCall(ctx context.Context, row workerservice.InvocationRow) (context.Context, string, error) {
	owner := strings.TrimSpace(row.OwnerUserId)
	if owner == "" {
		return nil, "", fmt.Errorf("agent.worker store: ownerUserId required -- v1:worker:invocation is owner-tiered and an unstamped write is refused")
	}
	writeCtx := auth.ContextWithUserActor(ctx, owner)
	args := map[string]any{
		"invocationId":  row.ID,
		"workerId":      row.WorkerId,
		"agentId":       row.AgentId,
		"runId":         row.RunId,
		"stepId":        row.StepId,
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
	call, err := langparser.RenderCall("createWorkerInvocation", args)
	if err != nil {
		return nil, "", fmt.Errorf("agent.worker store: render invocation: %w", err)
	}
	return writeCtx, call, nil
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
	return (&fleetcatalog.EngineStore{Engine: s.Engine}).WorkersForOwner(ctx, ownerUserId)
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
			Id:              rowString(row, "id"),
			Strategy:        rowString(row, "strategy"),
			RequireLabels:   rowStringMap(row, "requireLabels"),
			PreferLabels:    rowStringMap(row, "preferLabels"),
			Fallback:        rowString(row, "fallback"),
			ModelPreference: rowStringList(row, "modelPreference"),
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

// rowInt reads a worker row's numeric field.
//
// SATURATES out of range (memql#4779). `activeCount` is compared as a uint32
// against the machine's concurrency (`uint32(ActiveCount) >= MaxConcurrent`),
// so a negative becomes roughly four billion and takes a healthy machine out
// of the fleet -- silently, and only under load.
func rowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case float64:
		return num.ClampFloat64(v)
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
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
			// narrowing: GUARDED -- num.WholeInt64 IS the guard (memql#4779).
			if whole, ok := num.WholeInt64(t); ok {
				out[k] = fmt.Sprintf("%d", whole)
			} else {
				out[k] = fmt.Sprintf("%g", t)
			}
		}
	}
	return out
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

// SharedInferenceWorkers returns every machine in the cluster, with the
// owner's shared-inference opt-in resolved (epic memql#4676, task memql#4678).
//
// THE ONE CROSS-OWNER READ IN THIS PACKAGE, and it runs under the ENGINE'S OWN
// operator identity -- the campaigns precedent (component/campaigns/worker.go
// systemActorContext), for the reason stated there: v1:worker:registration
// declares the composite clusterOwner tier, the read gate resolves that tier
// through auth.IsClusterOwner() and has no other way in, and an escape hatch
// in the enforcement layer would be available to every caller that can reach
// it where an identity is only as powerful as the queries it is used for.
//
// It exists to serve calls with NO ACTING USER. Every user-scoped call routes
// through WorkersForOwner, which cannot see another user's machines at all,
// and that is the property this method must not undermine: it is deliberately
// on a second interface (SharedFleetStore) so the user-scoped code paths do
// not have it in reach.
func (s *EngineStore) SharedInferenceWorkers(ctx context.Context) ([]Candidate, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	return (&fleetcatalog.EngineStore{Engine: s.Engine}).SharedInferenceWorkers(ctx)
}

// outputPayloadRows normalises a shape() query's OutputPayload into
// a []map[string]any slice. shape() can land as a slice of maps, a
// slice of `any` whose elements are maps, or a bare map (single-row
// projections). Returns nil when the payload doesn't carry rows.
// Mirrors app/computer_use_status_agent.go's helper of the same name
// -- duplicated here so the worker integration stays self-contained
// (no agent-build cross-import on the BFF side).
func outputPayloadRows(payload any) []map[string]any {
	if payload == nil {
		return nil
	}
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}
