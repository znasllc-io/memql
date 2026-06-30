package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

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

// CreateRegistration persists a fresh v1:worker:registration row.
func (s *EngineStore) CreateRegistration(ctx context.Context, row RegistrationRow) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("worker.store: engine not configured")
	}
	args := map[string]any{
		"registrationId":       row.ID,
		"ownerUserId":          row.OwnerUserId,
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
		"registeredAt":         row.RegisteredAt.UTC().Format(time.RFC3339Nano),
		"lastSeenAt":           row.LastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP":  row.LastConnectedFromIP,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal create args: %w", err)
	}
	query := fmt.Sprintf("createWorkerRegistration(%s)", string(body))
	if _, err := s.Engine.Execute(ctx, query); err != nil {
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
func (s *EngineStore) RefreshRegistration(ctx context.Context, row RegistrationRow) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("worker.store: engine not configured")
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
		"lastSeenAt":           row.LastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP":  row.LastConnectedFromIP,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal refresh args: %w", err)
	}
	query := fmt.Sprintf("refreshWorkerRegistration(%s)", string(body))
	if _, err := s.Engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("worker.store: refresh registration: %w", err)
	}
	return nil
}

// UpdateLastSeen bumps lastSeenAt on a registration.
func (s *EngineStore) UpdateLastSeen(ctx context.Context, registrationId string, lastSeenAt time.Time, sourceIP string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	args := map[string]any{
		"registrationId":      registrationId,
		"lastSeenAt":          lastSeenAt.UTC().Format(time.RFC3339Nano),
		"lastConnectedFromIP": sourceIP,
	}
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal lastSeen args: %w", err)
	}
	query := fmt.Sprintf("updateWorkerLastSeen(%s)", string(body))
	if _, err := s.Engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("worker.store: update lastSeen: %w", err)
	}
	return nil
}

// RevokeRegistration stamps revokedAt on a registration.
func (s *EngineStore) RevokeRegistration(ctx context.Context, registrationId, revokedBy, reason string, at time.Time) error {
	if s == nil || s.Engine == nil {
		return nil
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
	if _, err := s.Engine.Execute(ctx, query); err != nil {
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
func (s *EngineStore) WorkerByIdentityId(ctx context.Context, identityId string) (*RegistrationRow, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	query := fmt.Sprintf(`query workerByIdentityId(identityId:%q)`, identityId)
	nodes, err := s.executeAndExtract(ctx, query)
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
func (s *EngineStore) WorkersForUser(ctx context.Context, ownerUserId string) ([]RegistrationRow, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	query := fmt.Sprintf(`query workersForUser(ownerUserId:%q)`, ownerUserId)
	nodes, err := s.executeAndExtract(ctx, query)
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
		Concurrency:          g.uint32Map("concurrency"),
		Platform:             g.anyMap("platformInfo"),
		Permissions:          g.anyMap("permissions"),
		Version:              g.str("version"),
		BuildTag:             g.str("buildTag"),
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
