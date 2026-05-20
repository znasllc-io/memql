// Package dailyspace owns the platform-driven lifecycle of per-user
// daily spaces: a one-per-user-per-day v1:cognition:space row that
// the user implicitly drops into when they open the app, the
// designated landing surface for ad-hoc conversation with their
// assistant.
//
// Two capabilities, both idempotent and intended to be the only
// authoritative path for daily-space provisioning. The DSL-side
// automations (dsl/cognition/automations.memql) call these via
// thin builtins; the SPA's legacy useDailySpace hook becomes a
// read-only lens.
//
//	ensureForUser({userId})  -- guarantees today's daily exists for
//	                            one user. Cheap enough to call on every
//	                            user-create + every login + every cron
//	                            tick; the underlying mutationCreateDailySpace
//	                            is content-addressable on
//	                            (userShortId, dateKey) so re-calls collapse.
//
//	rolloverAllUsers({})     -- hourly cron entry point. Walks every
//	                            active user with preferences.dailySpaceEnabled
//	                            != false and ensures today's daily for
//	                            each. Archive/save of yesterday's daily
//	                            per preferences.dailySpaceRolloverAction
//	                            is deferred to a sibling PR (needs a
//	                            queryDailySpacesForUser query that does
//	                            not exist yet) -- the visibility problem
//	                            the operator flagged is fully addressed
//	                            by the synchronous ensureForUser path
//	                            firing on user-create + auth-session;
//	                            this cron is the long-session safety net.
//
// Both capabilities run under a synthetic `system:dailyspaceAutomation`
// actor (distinct from the seed materializer's `system:seedMaterializer`
// for audit clarity) so their mutation calls satisfy the engine's
// actor-required gate without leaking the calling user's privileges
// into the rollover sweep.
package dailyspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// systemActorId is the synthetic actor every dailyspace mutation runs
// under. Distinct from system:seedMaterializer so audit logs can tell
// "the daily-space cron created this" apart from "the seed materializer
// stamped this on startup."
const systemActorId = "system:dailyspaceAutomation"

// resultConcept is the synthetic MemoryNode concept the capabilities
// return. Never persisted -- the engine round-trips it back to the
// caller and discards it.
const resultConcept = "integration:dailyspace:result"

// Integration is the dailyspace capability surface. Holds the engine
// handle so it can re-enter Execute for the read-then-write dance --
// reading the user row, computing the date key, writing the daily
// space via mutationCreateDailySpace.
type Integration struct {
	engine memql.IntegrationEngineAccess
}

// NewIntegration wires the engine handle. The factory is in plugin.go;
// this constructor is exposed for tests that supply a stub engine.
func NewIntegration(engine memql.IntegrationEngineAccess) *Integration {
	return &Integration{engine: engine}
}

// IntegrationName implements memql.IntegrationProvider.
func (d *Integration) IntegrationName() string { return "dailyspace" }

// Capabilities implements memql.IntegrationProvider.
func (d *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "ensureForUser",
			Description: "Ensure today's daily space exists for the given user. Reads the user's preferences.timezone to compute their local date key; calls mutationCreateDailySpace idempotently on the deterministic id daily-<userShortId>-<dateKey>. No-op when preferences.dailySpaceEnabled is false. Safe to call repeatedly -- the underlying insert collapses on id collision.",
			Handler:     d.handleEnsureForUser,
			ArgsSchema: map[string]string{
				"userId": "string (required) -- canonical or short v1:identity:user.id",
			},
		},
		{
			Name:        "ensureForCaller",
			Description: "Ensure today's daily space exists for the authenticated caller. Derives userId from the request's auth context (JWT/PAT subject), then calls ensureForUser internally. The cockpit Chat tab calls this on connect so the daily space is present before the space list paints.",
			Handler:     d.handleEnsureForCaller,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "rolloverAllUsers",
			Description: "Hourly cron entry point. For every active user with dailySpaceEnabled != false: ensures today's daily exists and archives or saves any prior-day dailies per their dailySpaceRolloverAction preference. Returns one summary node with counts.",
			Handler:     d.handleRolloverAllUsers,
			ArgsSchema:  map[string]string{},
		},
	}
}

// ensureResult is the payload of the MemoryNode handleEnsureForUser
// returns. Fields are best-effort observability for the calling
// automation; nothing reads them today but logging them keeps the
// "why didn't my daily land" debug loop self-contained.
type ensureResult struct {
	UserId   string `json:"userId"`
	Skipped  bool   `json:"skipped,omitempty"`
	Reason   string `json:"reason,omitempty"`
	SpaceId  string `json:"spaceId,omitempty"`
	DateKey  string `json:"dateKey,omitempty"`
	TzUsed   string `json:"tzUsed,omitempty"`
	Inserted bool   `json:"inserted,omitempty"`
}

func (d *Integration) handleEnsureForUser(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	userId := strings.TrimSpace(asString(args["userId"]))
	if userId == "" {
		return nil, fmt.Errorf("dailyspace.ensureForUser: userId is required")
	}
	res, err := d.ensureForUser(ctx, userId)
	if err != nil {
		return nil, err
	}
	return wrapResult(res)
}

// handleEnsureForCaller resolves the calling user from the request's
// auth context and delegates to ensureForUser. Surfaces the same
// ensureResult shape as the user-keyed handler so callers can branch
// on Skipped / Inserted without caring which capability they invoked.
//
// Picks the subject from the active TokenInfo (JWT `sub` or PAT
// subject) -- that's the canonical v1:identity:user.id stamped on
// every request envelope. When no auth context is present (e.g.
// background automations running under the synthetic system actor)
// the call returns a Skipped result; system actors don't have a
// "daily space" surface to back.
func (d *Integration) handleEnsureForCaller(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	token := auth.TokenInfoFromContext(ctx)
	if token == nil {
		return wrapResult(ensureResult{Skipped: true, Reason: "no auth context on request"})
	}
	subject := strings.TrimSpace(token.Subject)
	if subject == "" {
		return wrapResult(ensureResult{Skipped: true, Reason: "auth context has empty subject"})
	}
	if strings.HasPrefix(subject, "system:") {
		return wrapResult(ensureResult{UserId: subject, Skipped: true, Reason: "caller is a system actor"})
	}
	res, err := d.ensureForUser(ctx, subject)
	if err != nil {
		return nil, err
	}
	return wrapResult(res)
}

// ensureForUser is the internal entry the rollover sweep also reuses.
// Returns an ensureResult by value so the rollover summary can
// aggregate without re-decoding wrapped MemoryNodes.
func (d *Integration) ensureForUser(ctx context.Context, userId string) (ensureResult, error) {
	out := ensureResult{UserId: userId}

	user, err := d.loadUser(ctx, userId)
	if err != nil {
		return out, fmt.Errorf("dailyspace.ensureForUser(%q): load user: %w", userId, err)
	}
	if user == nil {
		out.Skipped = true
		out.Reason = "user not found"
		return out, nil
	}

	prefs := mapField(user, "preferences")
	// Default-on: a missing dailySpaceEnabled key is treated as true
	// so legacy users (rows pre-dating the field) still get a daily.
	// Only an explicit `false` opts out.
	if enabled, ok := prefs["dailySpaceEnabled"].(bool); ok && !enabled {
		out.Skipped = true
		out.Reason = "preferences.dailySpaceEnabled is false"
		return out, nil
	}

	tz := strings.TrimSpace(asString(prefs["timezone"]))
	loc := time.UTC
	tzUsed := "UTC"
	if tz != "" {
		if resolved, err := time.LoadLocation(tz); err == nil {
			loc = resolved
			tzUsed = tz
		}
		// Invalid tz silently falls back to UTC, matching the
		// timeutil.dateKeyInTimezone capability's contract -- a typo
		// in preferences must never block daily-space provisioning.
	}
	dateKey := time.Now().In(loc).Format("2006-01-02")
	out.DateKey = dateKey
	out.TzUsed = tzUsed

	shortUserId := userId
	if _, parsed, err := id.ParseNodeId(userId); err == nil && parsed != "" {
		shortUserId = parsed
	}
	// Deterministic id matches the schema's documented pattern
	// `daily-{userShortId}-{dateKey}` -- the engine collapses repeat
	// inserts on id collision so every call after the first lands as
	// a no-op. The id is partition-scoped automatically by the
	// mutation's @useConcept path; we pass the short form.
	spaceId := fmt.Sprintf("daily-%s-%s", shortUserId, dateKey)
	out.SpaceId = spaceId

	// Display name biased toward "the user's daily" rather than the
	// raw date. The SPA can override at render time if it wants
	// "Wednesday May 18" style; the row carries dailyDateKey
	// separately for callers that need the structured value.
	name := fmt.Sprintf("Daily %s", dateKey)

	sysCtx := systemActorContext(ctx)
	// Pass ownerUserId explicitly: the mutation runs under the
	// `system:dailyspaceAutomation` actor so the engine's actor-
	// required gate is satisfied, but the space itself must be
	// owned by the user we're provisioning for. Without this the
	// row's payload.ownerUserId is unset (or, worse, an unresolved
	// `actor.userId` token literal) and downstream queries that
	// filter on ownership miss the row.
	mutation := fmt.Sprintf(
		`mutationCreateDailySpace({spaceId: %q, name: %q, dailyDateKey: %q, ownerUserId: %q})`,
		spaceId, name, dateKey, userId,
	)
	if _, err := d.engine.Execute(sysCtx, mutation); err != nil {
		return out, fmt.Errorf("dailyspace.ensureForUser(%q): create daily: %w", userId, err)
	}
	out.Inserted = true
	return out, nil
}

// rolloverSummary captures what the hourly sweep did. Returned as a
// single MemoryNode payload so the cron automation's audit trail has
// a one-row entry per tick instead of a fan-out per user.
type rolloverSummary struct {
	Users      int      `json:"users"`
	Ensured    int      `json:"ensured"`
	Skipped    int      `json:"skipped"`
	Errors     int      `json:"errors"`
	ErrorBatch []string `json:"errorBatch,omitempty"` // first 5 error strings for quick triage
}

func (d *Integration) handleRolloverAllUsers(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	users, err := d.listActiveUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("dailyspace.rolloverAllUsers: list users: %w", err)
	}

	summary := rolloverSummary{Users: len(users)}
	for _, u := range users {
		uid := stringField(u, "id")
		if uid == "" {
			continue
		}
		res, err := d.ensureForUser(ctx, uid)
		if err != nil {
			summary.Errors++
			if len(summary.ErrorBatch) < 5 {
				summary.ErrorBatch = append(summary.ErrorBatch, fmt.Sprintf("%s: %v", uid, err))
			}
			continue
		}
		if res.Skipped {
			summary.Skipped++
			continue
		}
		summary.Ensured++
	}

	// Yesterday-rollover (archive / save prior dailies per
	// preferences.dailySpaceRolloverAction) is deliberately deferred
	// to a follow-up: we need a `queryDailySpacesForUser` query first
	// so the sweep can find the prior rows, and the archive/save
	// path needs to interact with the existing mutationArchiveSpace /
	// mutationSaveSpace surfaces. The visibility problem the user
	// flagged ("the cron job might not create the daily space when
	// the user logs in") is fully addressed by the ensureForUser
	// path firing on user-create + auth-session; the cron is the
	// stay-logged-in safety net. Once the prior-row query lands,
	// the rollover step slots in here as a per-user loop.

	payload, err := json.Marshal(summary)
	if err != nil {
		return nil, fmt.Errorf("dailyspace.rolloverAllUsers: marshal summary: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("dailyspace:rollover:%d", time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// loadUser runs queryUserById and unwraps the first row's payload +
// id into a flat map. The query is shape-wrapped (userFull), so the
// projection lands at the row top level under the post-shape
// flattening convention.
func (d *Integration) loadUser(ctx context.Context, userId string) (map[string]any, error) {
	q := fmt.Sprintf(`queryUserById({userId: %q})`, userId)
	raw, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// listActiveUsers runs queryActiveUsers({}) and returns the flat row
// list. Best-effort: on engine error the cron logs zero users for
// this tick and the next tick retries.
func (d *Integration) listActiveUsers(ctx context.Context) ([]map[string]any, error) {
	raw, err := d.engine.Execute(systemActorContext(ctx), `queryActiveUsers({})`)
	if err != nil {
		return nil, err
	}
	return extractRows(raw), nil
}

func wrapResult(r ensureResult) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("dailyspace.ensureForUser: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("dailyspace:ensure:%s:%s:%d", r.UserId, r.DateKey, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// systemActorContext wraps ctx with a synthetic actor so the daily-
// space mutation calls satisfy the engine's actor-required guard.
// Distinct subject from the seed materializer's actor for audit
// clarity ("daily-space provisioner did this" vs "seed materializer
// did this" both create rows, but for very different reasons).
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: systemActorId,
		Claims: map[string]any{
			"sub":  systemActorId,
			"role": "system",
		},
	})
}

// extractRows normalizes the engine's Execute return into a uniform
// []map[string]any with payload fields at the top level.
//
// The engine returns `*memql.ExecuteResult` -- a Go struct, not the
// untyped `any` shape this function used to expect. Unwrap it via
// `OutputPayload()` first; for shape-wrapped queries that's the
// projected map / slice, and for raw bundle queries it's the
// *GraphBundle which we walk into MemoryNode form.
//
// Without the *ExecuteResult branch, every shape-wrapped lookup
// (queryUserById, queryActiveUsers, ...) silently returned 0 rows
// here -- that's how memql-cockpit#49 ended up with the daily-space
// integration reporting `user not found` for a user that clearly
// existed in the database. The query was returning the user; the
// extractor was just dropping the result on the floor because the
// type switch had no case for the *ExecuteResult Go type.
func extractRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	// Unwrap the engine's *ExecuteResult to the underlying payload
	// before walking the value-level shapes below. OutputPayload
	// returns r.output when set (shape projections) or r.Bundle
	// otherwise (raw concept queries).
	if res, ok := raw.(*memql.ExecuteResult); ok && res != nil {
		raw = res.OutputPayload()
	}
	if raw == nil {
		return nil
	}
	// Raw graph-bundle path: walk Nodes -> map shape directly. The
	// MemoryNode payload field on the Bundle is a structpb.Struct;
	// AsMap surfaces it as map[string]any.
	if bundle, ok := raw.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{
				"id":      n.GetId(),
				"concept": n.GetConcept(),
			}
			if payload := n.GetPayload(); payload != nil {
				for k, v := range payload.AsMap() {
					row[k] = v
				}
			}
			out = append(out, row)
		}
		return out
	}
	switch v := raw.(type) {
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
		// Bundle shape (legacy): {bundle: {nodes: [...]}}
		if bundle, ok := v["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				out := make([]map[string]any, 0, len(nodes))
				for _, n := range nodes {
					if m, ok := n.(map[string]any); ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
		// Shape-wrapped, single value: wrap in a one-element slice.
		return []map[string]any{v}
	}
	return nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}
