package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// THE ACTOR IS BORROWED, on every registration read and every registration
// write in this file, and it is the load-bearing part of the store rather than
// a tidiness.
//
// v1:worker:registration declares @rowAuthz(owner="ownerUserId", clusterOwner).
// Both halves of that tier are enforced against the ACTOR in context:
//
//   - the read gate (component/memql/rowauthz_enforce.go) has no
//     internal-origin bypass and answers "no identity, no rows" -- so an
//     unstamped WorkerByIdentityId or WorkersForUser returns ZERO rows, not an
//     error. The register handshake would read that as "no existing
//     registration" and insert a duplicate on every reconnect.
//   - the write guard (component/memql/rowauthz_write_guard.go) resolves the
//     target row before the read-merge and refuses when its ownerUserId is not
//     the actor's.
//
// And a worker authenticates as `worker:<identityId>` -- see auth.go, which
// builds a claims map with role "worker" and no user subject at all. So the
// inbound stream context carries no user actor, and every call from this
// package would be refused or empty on its own.
//
// What this package DOES have is the WorkerIdentity the interceptor resolved
// from the presented mql_wkr_ token, which names the owner. So each call runs
// under that user's actor: the engine borrowing the row owner's authority for
// a write on their behalf. This is the same shape createAuthActivity uses
// (component/identity/activity_db.go) and for the same reason -- the service
// knows whose credential it is before the actor envelope does. Nothing here
// can name a user the caller could not: the id comes off an identity row the
// auth path already resolved, never off a request field.
//
// The one thing this package must NOT do is stamp internal origin.
// component/worker is deliberately absent from call_origin_conformance_test.go's
// allowlist and belongs absent: every context in this package descends from a
// worker's own inbound stream, which is precisely the shape that rule forbids.
// None of the constructs below is @serverOnly, so none needs it.

// EngineExecutor is the narrow interface this package needs from the
// MemQL engine. *memql.MemQLEngine satisfies it directly.
type EngineExecutor interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// EngineStore implements Store on top of the memql engine. It runs
// mutations / queries against the engine and parses the returned
// graph bundles into the Row structs in worker.go.
type EngineStore struct {
	Engine EngineExecutor
	Logger *slog.Logger
}

var _ Store = (*EngineStore)(nil)

// ownerActor stamps the registration owner's actor on ctx. See the
// borrowed-authority note above for why every call in this file needs it.
//
// A blank owner is an ERROR rather than a pass-through, and that is
// deliberate: auth.ContextWithUserActor returns the context UNCHANGED for a
// blank id, so a silent fallthrough would hand the engine an actor-less
// context and produce an empty read or a refused write -- the failure would
// surface as "the worker has no registration" somewhere far from the missing
// value. Refusing here names it where it went missing.
func ownerActor(ctx context.Context, ownerUserId string) (context.Context, error) {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return nil, fmt.Errorf("worker.store: ownerUserId required -- v1:worker:registration is owner-tiered and an unstamped call reads nothing and writes nothing")
	}
	return auth.ContextWithUserActor(ctx, owner), nil
}

// CreateRegistration persists a fresh v1:worker:registration row.
//
// ownerUserId is NOT an argument of the mutation: the concept marks it
// @serverSet and createWorkerRegistration stamps it from actor.userId, so the
// owner reaches the row through the context this method builds and through
// nothing else.
func (s *EngineStore) CreateRegistration(ctx context.Context, row RegistrationRow) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("worker.store: engine not configured")
	}
	writeCtx, err := ownerActor(ctx, row.OwnerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId":       row.ID,
		"identityId":           row.IdentityId,
		"name":                 row.Name,
		"capabilities":         row.Capabilities,
		"labels":               row.Labels,
		"capabilityDescriptor": row.CapabilityDescriptor.AsMap(),
		"concurrency":          row.Concurrency,
		"platformInfo":         row.Platform,
		"permissions":          row.Permissions,
		"version":              row.Version,
		"buildTag":             row.BuildTag,
		"apps":                 appsAsMaps(row.Apps),
		"registeredAt":         row.RegisteredAt.UTC().Format(time.RFC3339Nano),
		"lastSeenAt":           row.LastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP":  row.LastConnectedFromIP,
		"connectedNodeId":      row.ConnectedNodeId,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal create args: %w", err)
	}
	query := fmt.Sprintf("createWorkerRegistration(%s)", string(body))
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: create registration: %w", err)
	}
	return nil
}

// RefreshRegistration re-stamps the registration-authoritative
// fields on an existing v1:worker:registration row (memql#1332).
// Called on every reconnect so the persisted row tracks the latest
// Register message instead of going stale across cockpit upgrades.
// row.CapabilityDescriptor == nil serializes to JSON null, which the
// mutation coalesces to {} -- i.e. an omitted descriptor CLEARS the
// persisted one.
//
// operatorLabels and displayName ARE DELIBERATELY ABSENT from the argument
// map, and their absence is the whole of design D3 (memql#4350). `labels` just
// above is overwritten from the Register message on every reconnect, so an
// operator tag living in that map would be erased by the machine carrying it,
// silently, roughly whenever a laptop lid closed; displayName would likewise
// be reverted to the cockpit's hostname. update{} is a read-merge
// (memql#1628), so a field this call does not name survives untouched -- which
// means the prohibition is enforced by the ABSENCE of two lines, and a
// well-meaning "complete the field list" edit is exactly what would break it.
// TestRefreshRegistration_PreservesOperatorLabelsAndDisplayName is the guard.
func (s *EngineStore) RefreshRegistration(ctx context.Context, row RegistrationRow) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("worker.store: engine not configured")
	}
	writeCtx, err := ownerActor(ctx, row.OwnerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId":       row.ID,
		"name":                 row.Name,
		"capabilities":         row.Capabilities,
		"capabilityDescriptor": row.CapabilityDescriptor.AsMap(),
		"labels":               row.Labels,
		"concurrency":          row.Concurrency,
		"platformInfo":         row.Platform,
		"permissions":          row.Permissions,
		"version":              row.Version,
		"buildTag":             row.BuildTag,
		"apps":                 appsAsMaps(row.Apps),
		"lastSeenAt":           row.LastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP":  row.LastConnectedFromIP,
		"connectedNodeId":      row.ConnectedNodeId,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal refresh args: %w", err)
	}
	query := fmt.Sprintf("refreshWorkerRegistration(%s)", string(body))
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: refresh registration: %w", err)
	}
	return nil
}

// UpdateLastSeen flushes the batched heartbeat. Beyond lastSeenAt it carries
// the two fields that are only true while a stream is live: connectedNodeId
// (the replica holding it) and activeCount (calls in flight on it). Both are
// re-asserted on every flush rather than written once at register, because
// both can change without a reconnect -- a rebalanced replica, a call
// finishing.
func (s *EngineStore) UpdateLastSeen(ctx context.Context, registrationId, ownerUserId string, lastSeenAt time.Time, sourceIP, connectedNodeId string, activeCount int) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId":      registrationId,
		"lastSeenAt":          lastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP": sourceIP,
		"connectedNodeId":     connectedNodeId,
		"activeCount":         activeCount,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal lastSeen args: %w", err)
	}
	query := fmt.Sprintf("updateWorkerLastSeen(%s)", string(body))
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: update lastSeen: %w", err)
	}
	return nil
}

// ClearConnectedNode blanks connectedNodeId (and activeCount) when the
// worker's stream closes. Called from the disconnect path, where the session
// context is ALREADY CANCELLED -- so the caller must derive a fresh one; see
// streamSession.close.
//
// lastSeenAt is not touched. It records when the machine was last heard from,
// and advancing it on the way out would make a disconnected worker read as
// online for one whole OnlineWindow.
func (s *EngineStore) ClearConnectedNode(ctx context.Context, registrationId, ownerUserId string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId": registrationId,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal clearConnectedNode args: %w", err)
	}
	query := fmt.Sprintf("clearWorkerConnectedNode(%s)", string(body))
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: clear connected node: %w", err)
	}
	return nil
}

// RevokeRegistration stamps revokedAt on a registration. revokedBy is who
// performed the revocation and is NOT the actor: an admin may revoke somebody
// else's machine, and the write still runs under the row's OWNER because that
// is whose tier the guard checks.
func (s *EngineStore) RevokeRegistration(ctx context.Context, registrationId, ownerUserId, revokedBy, reason string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId": registrationId,
		"revokedAt":      at.UTC().Format(time.RFC3339Nano),
		"revokedBy":      revokedBy,
		"revokeReason":   reason,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal revoke args: %w", err)
	}
	query := fmt.Sprintf("revokeWorker(%s)", string(body))
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: revoke registration: %w", err)
	}
	return nil
}

// CreateInvocation persists a v1:worker:invocation row.
func (s *EngineStore) CreateInvocation(ctx context.Context, row InvocationRow) error {
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
		"startedAt":     row.StartedAt.UTC().Format(time.RFC3339Nano),
		"completedAt":   timeToRFC(row.CompletedAt),
		"durationMs":    row.DurationMs,
		"outcome":       row.Outcome,
		"exitCode":      row.ExitCode,
		"signal":        row.Signal,
		"errorCode":     row.ErrorCode,
		"errorMessage":  row.ErrorMessage,
		"bytesIn":       row.BytesIn,
		"bytesOut":      row.BytesOut,
		"outputPreview": row.OutputPreview,
		// A nil map marshals to JSON null; createWorkerInvocation's
		// `routing: args.routing ?? {}` turns that into {}. Asserted rather
		// than assumed by TestEngineStoreCreateInvocation_RoutingWireShape --
		// ?? is BLANK-coalescing, and its exact behaviour on null is easier
		// to check than to reason about.
		"routing": row.Routing,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal invocation args: %w", err)
	}
	query := fmt.Sprintf("createWorkerInvocation(%s)", string(body))
	if _, err := s.Engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("worker.store: create invocation: %w", err)
	}
	return nil
}

// WorkerByIdentityId resolves a worker registration via the auth
// path. Returns nil when no matching active registration exists.
//
// The owner is required even though the filter keys on identityId: without an
// actor the owned tier admits no rows at all, and this call's caller reads an
// empty result as "no registration yet" and inserts a second one. A read that
// fails by returning nothing is the reason this argument exists.
func (s *EngineStore) WorkerByIdentityId(ctx context.Context, identityId, ownerUserId string) (*RegistrationRow, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	readCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`query workerByIdentityId(identityId:%s)`, langparser.QuoteString(identityId))
	nodes, err := s.executeAndExtract(readCtx, query)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		row := decodeRegistration(n)
		if row != nil && row.IdentityId == identityId {
			return row, nil
		}
	}
	return nil, nil
}

// WorkersForUser returns every registration owned by ownerUserId.
//
// workersForUser carries @public, which scopes the QUERY and not the rows: row
// admission is resolved from the row's own concept, so the owned tier still
// decides and this read is as actor-dependent as the others.
func (s *EngineStore) WorkersForUser(ctx context.Context, ownerUserId string) ([]RegistrationRow, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	readCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`query workersForUser(ownerUserId:%s)`, langparser.QuoteString(ownerUserId))
	nodes, err := s.executeAndExtract(readCtx, query)
	if err != nil {
		return nil, err
	}
	out := make([]RegistrationRow, 0, len(nodes))
	for _, n := range nodes {
		row := decodeRegistration(n)
		if row == nil {
			continue
		}
		out = append(out, *row)
	}
	return out, nil
}

// IdentityByTokenHash resolves a worker token's hash to the
// underlying v1:identity:identity row, returning the worker-specific
// projection. Implementation reads the identity row and unpacks the
// worker_token credential variant.
//
// MVP: looks the identity up by api_key/keyHash semantics --
// extending the identity store with a worker-specific lookup is
// a small follow-up; for now the auth interceptor must provide an
// identityId via the request context (see auth.go) and we simply
// confirm the row is alive here.
func (s *EngineStore) IdentityByTokenHash(ctx context.Context, tokenHash string) (*WorkerIdentity, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	// Phase 1 stops here -- the interceptor consults the identity
	// service directly (Phase 7 wires this through). Returning nil
	// keeps callers honest: they must supply the identity via
	// ContextWithWorkerIdentity instead of trusting the store.
	return nil, nil
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func (s *EngineStore) executeAndExtract(ctx context.Context, query string) ([]*memqlv1.MemoryNode, error) {
	res, err := s.Engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("worker.store: execute: %w", err)
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}
	return res.Bundle.Nodes, nil
}

func decodeRegistration(node *memqlv1.MemoryNode) *RegistrationRow {
	if node == nil {
		return nil
	}
	g := newWorkerFieldGetter(node)
	row := &RegistrationRow{
		ID:                   firstNonEmpty(g.str("id"), node.GetId()),
		OwnerUserId:          g.str("ownerUserId"),
		IdentityId:           g.str("identityId"),
		Name:                 g.str("name"),
		Capabilities:         g.stringList("capabilities"),
		CapabilityDescriptor: capabilityDescriptorFromMap(g.anyMap("capabilityDescriptor")),
		Labels:               g.stringMap("labels"),
		OperatorLabels:       g.stringMap("operatorLabels"),
		DisplayName:          g.str("displayName"),
		ConnectedNodeId:      g.str("connectedNodeId"),
		LastSelectedAt:       g.time("lastSelectedAt"),
		ActiveCount:          g.intVal("activeCount"),
		Concurrency:          g.uint32Map("concurrency"),
		Platform:             g.anyMap("platformInfo"),
		Permissions:          g.anyMap("permissions"),
		Version:              g.str("version"),
		BuildTag:             g.str("buildTag"),
		Apps:                 g.apps("apps"),
		RegisteredAt:         g.time("registeredAt"),
		LastSeenAt:           g.time("lastSeenAt"),
		LastConnectedFromIP:  g.str("lastConnectedFromIP"),
		RevokedAt:            g.time("revokedAt"),
		RevokedBy:            g.str("revokedBy"),
		RevokeReason:         g.str("revokeReason"),
	}
	return row
}

func timeToRFC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// workerFieldGetter wraps a MemoryNode with typed accessors.
type workerFieldGetter struct {
	node *memqlv1.MemoryNode
}

func newWorkerFieldGetter(n *memqlv1.MemoryNode) *workerFieldGetter {
	return &workerFieldGetter{node: n}
}

func (g *workerFieldGetter) str(key string) string {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return ""
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return ""
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(v.GetStringValue())
}

func (g *workerFieldGetter) stringList(key string) []string {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return nil
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	list := v.GetListValue()
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list.Values))
	for _, item := range list.Values {
		if item == nil {
			continue
		}
		out = append(out, item.GetStringValue())
	}
	return out
}

func (g *workerFieldGetter) stringMap(key string) map[string]string {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return nil
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	stru := v.GetStructValue()
	if stru == nil {
		return nil
	}
	out := make(map[string]string, len(stru.Fields))
	for k, val := range stru.Fields {
		if val == nil {
			continue
		}
		out[k] = val.GetStringValue()
	}
	return out
}

func (g *workerFieldGetter) uint32Map(key string) map[string]uint32 {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return nil
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	stru := v.GetStructValue()
	if stru == nil {
		return nil
	}
	out := make(map[string]uint32, len(stru.Fields))
	for k, val := range stru.Fields {
		if val == nil {
			continue
		}
		out[k] = uint32(val.GetNumberValue())
	}
	return out
}

func (g *workerFieldGetter) anyMap(key string) map[string]any {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return nil
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	stru := v.GetStructValue()
	if stru == nil {
		return nil
	}
	out := make(map[string]any, len(stru.Fields))
	for k, val := range stru.Fields {
		if val == nil {
			continue
		}
		out[k] = val.AsInterface()
	}
	return out
}

// intVal reads a numeric payload field. Absent or non-numeric reads as 0,
// which is the right answer for activeCount: a row written before the field
// existed had nothing in flight that this replica knows about.
func (g *workerFieldGetter) intVal(key string) int {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return 0
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return 0
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return 0
	}
	return int(v.GetNumberValue())
}

func (g *workerFieldGetter) time(key string) time.Time {
	s := g.str(key)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

// UpdateApps re-stamps the reported app inventory and the labels derived
// from it (memql#4359). Separate from UpdateLastSeen because an inventory
// change is a ROUTING change: it must land on the row even when the
// heartbeat's lastSeenAt flush is inside its throttle window, or the
// router reads stale `app:` labels for up to a minute -- and a planner
// node, which has no registry at all, reads nothing else.
//
// Owner-scoped like every other write here: v1:worker:registration
// declares an owned tier, so the write needs a context carrying an actor.
func (s *EngineStore) UpdateApps(ctx context.Context, registrationId, ownerUserId string, apps []AppInfo, labels map[string]string, at time.Time, sourceIP string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId":      registrationId,
		"apps":                appsAsMaps(apps),
		"labels":              labels,
		"lastSeenAt":          at.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP": sourceIP,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal apps args: %w", err)
	}
	query := fmt.Sprintf("updateWorkerApps(%s)", string(body))
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: update apps: %w", err)
	}
	return nil
}

// appsAsMaps renders the app inventory for a DSL mutation argument. A nil
// inventory serializes as an empty list, which is how a machine says "I
// report no apps" -- distinct from not reporting at all.
func appsAsMaps(apps []AppInfo) []map[string]any {
	out := make([]map[string]any, 0, len(apps))
	for _, a := range apps {
		out = append(out, map[string]any{
			"id":           a.Id,
			"version":      a.Version,
			"signedIn":     a.SignedIn,
			"subscription": a.Subscription,
			"allowed":      a.Allowed,
		})
	}
	return out
}

// auditAppDetail renders the inventory for an audit event: the id and
// whether the engine can actually drive it, which is the pair a security
// reader needs and the whole struct is not.
func auditAppDetail(apps []AppInfo) []map[string]any {
	out := make([]map[string]any, 0, len(apps))
	for _, a := range apps {
		out = append(out, map[string]any{
			"id":           a.Id,
			"version":      a.Version,
			"runnable":     a.Runnable(),
			"subscription": a.Subscription,
		})
	}
	return out
}

// apps decodes the reported local-app inventory. Malformed entries are
// DROPPED rather than defaulted: an app with no id cannot be routed to,
// and an entry claiming to be runnable without the fields to prove it is
// exactly what must not be trusted.
func (g *workerFieldGetter) apps(key string) []AppInfo {
	if g == nil || g.node == nil || g.node.Payload == nil {
		return nil
	}
	fields := g.node.Payload.GetFields()
	if fields == nil {
		return nil
	}
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	list := v.GetListValue()
	if list == nil {
		return nil
	}
	out := make([]AppInfo, 0, len(list.GetValues()))
	for _, item := range list.GetValues() {
		stru := item.GetStructValue()
		if stru == nil {
			continue
		}
		m := stru.AsMap()
		raw, _ := m["id"].(string)
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		version, _ := m["version"].(string)
		subscription, _ := m["subscription"].(string)
		signedIn, _ := m["signedIn"].(bool)
		allowed, _ := m["allowed"].(bool)
		out = append(out, AppInfo{
			Id:           id,
			Version:      version,
			SignedIn:     signedIn,
			Subscription: NormalizeSubscription(subscription),
			Allowed:      allowed,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}
