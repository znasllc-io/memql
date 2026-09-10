package worker

import (
	"context"
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
		"appDescriptors":       descriptorsAsMaps(row.AppDescriptors),
		"registeredAt":         row.RegisteredAt.UTC().Format(time.RFC3339Nano),
		"lastSeenAt":           row.LastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP":  row.LastConnectedFromIP,
		"connectedNodeId":      row.ConnectedNodeId,
	}
	// Omitted when absent rather than sent as a null. A cockpit that predates
	// the field has said nothing, and a `hardware: null` on the row is a value
	// a reader has to know to treat as silence -- where a missing key already
	// reads that way to everything.
	if len(row.Hardware) > 0 {
		args["hardware"] = row.Hardware
	}
	query, err := langparser.RenderCall("createWorkerRegistration", args)
	if err != nil {
		return fmt.Errorf("worker.store: render create: %w", err)
	}
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
		"identityId":           row.IdentityId,
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
		"appDescriptors":       descriptorsAsMaps(row.AppDescriptors),
		"lastSeenAt":           row.LastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP":  row.LastConnectedFromIP,
		"connectedNodeId":      row.ConnectedNodeId,
	}
	// `hardware` IS named here, unlike operatorLabels and displayName above,
	// and the distinction is which side of the machine/owner line the field
	// sits on. The inventory is the MACHINE's -- it is re-reported on every
	// Register, exactly as `labels` and `platformInfo` are -- so re-stamping it
	// is right and leaving it out would freeze a machine's hardware at whatever
	// it was the day the field shipped.
	//
	// Omitted when ABSENT rather than sent empty: a downgraded cockpit that
	// stops reporting has gone quiet, and update{} being a read-merge is what
	// keeps the last known inventory on the row instead of blanking it.
	if len(row.Hardware) > 0 {
		args["hardware"] = row.Hardware
	}
	query, err := langparser.RenderCall("refreshWorkerRegistration", args)
	if err != nil {
		return fmt.Errorf("worker.store: render refresh: %w", err)
	}
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
//
// `hardware` rides along and NIL MEANS LEAVE IT ALONE (epic memql#5146). A
// non-material inventory refresh -- free disk moved and nothing else -- is
// exactly as fresh as the heartbeat it arrived on, so it belongs in the
// heartbeat's write rather than in a second one to the same row. An inventory
// change that alters what the machine can RUN does not wait for this window;
// UpdateHardware is that path.
//
// `rttMs` / `rttAt` ride the same write on the same terms (epic memql#5218,
// D11): the latest Ping round trip, and a ZERO rttAt means not measured, so
// both stay out of the call. See the Store interface for why a zero must
// never be sent as a figure.
func (s *EngineStore) UpdateLastSeen(ctx context.Context, registrationId, ownerUserId string, lastSeenAt time.Time, sourceIP, connectedNodeId string, activeCount int, hardware map[string]any, rttMs int, rttAt time.Time) error {
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
	// Omitted rather than sent empty. The mutation body coalesces with `??`,
	// which is blank-coalescing, so an ABSENT key keeps the stored inventory
	// while an empty object would overwrite it with a machine that reports
	// nothing -- turning silence into a statement.
	if len(hardware) > 0 {
		args["hardware"] = hardware
	}
	// The same rule for the round trip: a zero rttAt is "not measured", and
	// sending it would write a 0 ms figure over a real one.
	if !rttAt.IsZero() {
		args["rttMs"] = rttMs
		args["rttAt"] = rttAt.UTC().Format(time.RFC3339Nano)
	}
	query, err := langparser.RenderCall("updateWorkerLastSeen", args)
	if err != nil {
		return fmt.Errorf("worker.store: render lastSeen: %w", err)
	}
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
	query, err := langparser.RenderCall("clearWorkerConnectedNode", args)
	if err != nil {
		return fmt.Errorf("worker.store: render clearConnectedNode: %w", err)
	}
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
	query, err := langparser.RenderCall("revokeWorker", args)
	if err != nil {
		return fmt.Errorf("worker.store: render revoke: %w", err)
	}
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: revoke registration: %w", err)
	}
	return nil
}

// CreateInvocation persists a v1:worker:invocation row.
//
// ownerUserId is NOT an argument of the mutation: the concept marks it
// @serverSet and createWorkerInvocation stamps it from actor.userId
// (memql#4406), so the owner reaches the row through the context this method
// builds and through nothing else -- the same shape CreateRegistration uses,
// for the same reason.
func (s *EngineStore) CreateInvocation(ctx context.Context, row InvocationRow) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, err := ownerActor(ctx, row.OwnerUserId)
	if err != nil {
		return err
	}
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
	query, err := langparser.RenderCall("createWorkerInvocation", args)
	if err != nil {
		return fmt.Errorf("worker.store: render invocation: %w", err)
	}
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
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
		AppDescriptors:       g.appDescriptors("appDescriptors"),
		RegisteredAt:         g.time("registeredAt"),
		LastSeenAt:           g.time("lastSeenAt"),
		LastConnectedFromIP:  g.str("lastConnectedFromIP"),
		RttMs:                g.intVal("rttMs"),
		RttAt:                g.time("rttAt"),
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
	query, err := langparser.RenderCall("updateWorkerApps", args)
	if err != nil {
		return fmt.Errorf("worker.store: render apps: %w", err)
	}
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: update apps: %w", err)
	}
	return nil
}

// UpdateHardware re-stamps the reported inventory and the labels derived from
// it (epic memql#5146, D1). Separate from UpdateLastSeen for exactly the reason
// UpdateApps is separate, one function above: the `runtime:<name>` labels are
// derived from this inventory and live on the ROW as well as in the live
// registry, so an inventory change that moves them must land even inside the
// heartbeat's throttle window. A row whose labels disagree with the registry is
// a split no reader can detect, and a planner node -- which holds no registry
// at all -- reads nothing but the row.
//
// The cheap half of the same question is deliberately NOT here: free disk moves
// on every beat and decides nothing, so it rides UpdateLastSeen's write rather
// than buying one of its own.
func (s *EngineStore) UpdateHardware(ctx context.Context, registrationId, ownerUserId string, hardware map[string]any, labels map[string]string, at time.Time, sourceIP string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	if len(hardware) == 0 {
		// A machine that reported no inventory has said nothing, and blanking
		// the stored one would turn that silence into a statement -- the
		// distinction the whole field rests on.
		return nil
	}
	writeCtx, err := ownerActor(ctx, ownerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"registrationId":      registrationId,
		"hardware":            hardware,
		"labels":              labels,
		"lastSeenAt":          at.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP": sourceIP,
	}
	query, err := langparser.RenderCall("updateWorkerHardware", args)
	if err != nil {
		return fmt.Errorf("worker.store: render hardware: %w", err)
	}
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: update hardware: %w", err)
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

// descriptorsAsMaps renders the harness descriptors for a DSL mutation
// argument. Only VALID entries reach here (AppDescriptorsFromProto already
// dropped the rest), so what lands on the row is exactly what the engine can
// act on -- unlike `apps`, which is stored verbatim so an operator can see an
// app the engine cannot drive.
func descriptorsAsMaps(descriptors []AppDescriptor) []map[string]any {
	out := make([]map[string]any, 0, len(descriptors))
	for _, d := range descriptors {
		out = append(out, map[string]any{
			"id":               d.Id,
			"harness":          d.Harness,
			"structuredResult": d.StructuredResult,
			"followUps":        d.FollowUps,
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

// appDescriptors decodes the harness descriptors. An entry whose app id or
// harness word this build does not know is DROPPED on the way out of the row
// exactly as it was on the way in, so a cluster mid-upgrade cannot read a
// protocol name out of the graph that its own code has no client for.
func (g *workerFieldGetter) appDescriptors(key string) []AppDescriptor {
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
	out := make([]AppDescriptor, 0, len(list.GetValues()))
	for _, item := range list.GetValues() {
		stru := item.GetStructValue()
		if stru == nil {
			continue
		}
		m := stru.AsMap()
		id, _ := m["id"].(string)
		harness, _ := m["harness"].(string)
		structured, _ := m["structuredResult"].(bool)
		followUps, _ := m["followUps"].(bool)
		d := AppDescriptor{
			Id:               strings.TrimSpace(id),
			Harness:          strings.TrimSpace(harness),
			StructuredResult: structured,
			FollowUps:        followUps,
		}
		if !d.Valid() {
			continue
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}
