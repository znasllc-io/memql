// Package approval is the DSL-backed implementation of
// safety.ApprovalSink (memql#232). The sink translates the Gate's
// Ask verdicts into v1:safety:approvalRequest rows via the engine's
// query + mutation path; the Gate's Evaluate consults this layer
// before returning so an approved bypass token flips Decision to
// Allow without the surface adapters knowing about approvals.
//
// Architectural seam: this package imports `component/safety` (for
// types) but NOT `component/memql` -- the engine is reached via the
// narrow `Engine` interface, which app boot adapts from
// memql.MemQLEngine.Execute. Mirrors the recorder/ package's
// MutationRunner pattern (#234). Keeps the package testable against
// an in-memory stub + avoids a cycle with `component/memql/tool_execution.go`,
// which imports `component/safety`.
package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/safety"
)

// EnvTTLHours is the env var that tunes how long an approved
// approvalRequest acts as a bypass token. Default is 24 hours;
// shorter windows are safer (a typo'd approval doesn't haunt forever)
// but force humans to re-approve more often. The TTL is advisory in
// v1 -- the bypass-check filters approved rows by expiresAt at read
// time; the retention sweep that flips status to "expired" lands as
// a follow-up.
const EnvTTLHours = "MEMQL_SAFETY_APPROVAL_TTL_HOURS"

// DefaultTTLHours is used when EnvTTLHours is unset or unparseable.
// 24h is the sweet spot: long enough that an operator who approves
// in the morning can run the same workflow that afternoon without
// re-approving, short enough that a forgotten approval doesn't
// outlast the operator's awareness of why they approved it.
const DefaultTTLHours = 24

// Engine is the narrow surface the DSL-backed sink needs from the
// memql engine: one method for the bypass-check query, one method
// for the create mutation. Errors from either are recovered +
// logged + folded into ApprovalStateUnconfigured -- a dead approval
// system must not block live traffic (per the ApprovalSink contract).
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Sink is the DSL-backed safety.ApprovalSink. Construct via New;
// install in the Gate via safety.GateOptions.ApprovalSink.
//
// Concurrency: safe for concurrent use. The engine calls are
// synchronous on the dispatch path; if approval-check latency
// becomes a problem, future work can layer a short-TTL cache on top
// (the correlationKey is the natural cache key and approvalRequest
// resolutions are inherently low-frequency).
type Sink struct {
	engine Engine
	logger *slog.Logger
	now    func() time.Time // injectable for tests; defaults to time.Now
	ttl    time.Duration
}

// Options configure New.
type Options struct {
	Engine Engine
	Logger *slog.Logger
	// Now is an injectable clock; defaults to time.Now when nil. Tests
	// inject a deterministic clock to assert TTL behavior.
	Now func() time.Time
	// TTL overrides the env-var-driven default. Zero -> read
	// MEMQL_SAFETY_APPROVAL_TTL_HOURS (default 24h).
	TTL time.Duration
}

// New returns a DSL-backed Sink. A nil engine is a configuration
// error; the returned Sink's Check method returns
// ApprovalStateUnconfigured in that case so the Gate falls back to
// the pre-approval Ask refusal.
func New(opts Options) *Sink {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = ttlFromEnv()
	}
	return &Sink{engine: opts.Engine, logger: logger, now: now, ttl: ttl}
}

// Check is the safety.ApprovalSink implementation. Looks up active
// rows for the descriptor's correlation key; if an approved + non-
// expired one exists, returns Approved. Otherwise creates a pending
// row (idempotently -- if a pending row already exists, returns it
// rather than creating a duplicate).
func (s *Sink) Check(ctx context.Context, desc safety.ActionDescriptor, cls safety.Classification) safety.ApprovalVerdict {
	if s == nil || s.engine == nil {
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	key := safety.ApprovalCorrelationKey(desc)
	// Step 1: look up active rows (pending OR approved -- the query
	// excludes denied + expired).
	active, err := s.queryActive(ctx, key)
	if err != nil {
		s.logger.Warn("safety.approval: lookup failed; falling back to unconfigured",
			"component", "safety.approval_sink",
			"correlation_key", key,
			"error", err)
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	if approved, id, denied := s.classifyActive(active); approved != "" {
		return safety.ApprovalVerdict{State: safety.ApprovalStateApproved, ApprovalRequestID: approved}
	} else if id != "" {
		// pending exists -- reuse its id, don't create a duplicate.
		return safety.ApprovalVerdict{State: safety.ApprovalStatePending, ApprovalRequestID: id}
	} else if denied != "" {
		// denied rows are excluded from the active query so this
		// branch shouldn't fire today; kept for parity with the
		// Verdict enum in case the query shape changes.
		return safety.ApprovalVerdict{State: safety.ApprovalStateDenied, ApprovalRequestID: denied}
	}
	// Step 2: no row yet -- create a pending one.
	id, err := s.createPending(ctx, key, desc, cls)
	if err != nil {
		s.logger.Warn("safety.approval: create failed; falling back to unconfigured",
			"component", "safety.approval_sink",
			"correlation_key", key,
			"error", err)
		return safety.ApprovalVerdict{State: safety.ApprovalStateUnconfigured}
	}
	return safety.ApprovalVerdict{State: safety.ApprovalStatePending, ApprovalRequestID: id}
}

// classifyActive walks the rows returned by queryActiveApprovalsByCorrelationKey
// and returns (approvedId, pendingId, deniedId). Walks all rows so
// the cockpit-side history that someone approved earlier wins over a
// later pending row -- the bypass posture is "any approval suffices."
//
// An approved row is treated as live ONLY if it has a parseable
// expiresAt in the future. Empty / unparseable expiresAt -> treated
// as expired (the safest posture: a forever-live bypass token is
// the failure mode worth avoiding, since the retention sweep that
// would flip stale rows to status=expired is a follow-up). The Gate
// then creates a fresh pending row so the human gets the chance to
// extend or re-deny.
func (s *Sink) classifyActive(rows []map[string]any) (approvedId, pendingId, deniedId string) {
	now := s.now()
	for _, r := range rows {
		status := str(r, "status")
		id := str(r, "id")
		switch status {
		case "approved":
			expires := str(r, "expiresAt")
			if expires == "" {
				// No TTL stamped -- defensive policy: treat as
				// expired. Until the resolve mutation gains an
				// expiresAt arg (follow-up), CLI-created approvals
				// without a TTL fall through to a fresh pending row.
				continue
			}
			t, err := time.Parse(time.RFC3339, expires)
			if err != nil {
				// Unparseable expiresAt -- treat as expired.
				continue
			}
			if t.Before(now) {
				// Past TTL -- treat as expired.
				continue
			}
			if approvedId == "" {
				approvedId = id
			}
		case "pending":
			if pendingId == "" {
				pendingId = id
			}
		case "denied":
			if deniedId == "" {
				deniedId = id
			}
		}
	}
	return
}

// queryActive runs queryActiveApprovalsByCorrelationKey and coerces
// the result into a []map[string]any list of payload rows. The
// engine's Execute returns []any of maps with a "payload" sub-map +
// "id" top-level; we flatten into one map per row carrying both.
func (s *Sink) queryActive(ctx context.Context, key string) ([]map[string]any, error) {
	query := fmt.Sprintf(`queryActiveApprovalsByCorrelationKey({correlationKey: %q})`, key)
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	return rowsFromResult(res), nil
}

// createPending runs mutationCreateApprovalRequest with TTL-derived
// expiresAt + the descriptor's redacted payload. Returns the
// inserted row's id.
func (s *Sink) createPending(ctx context.Context, key string, desc safety.ActionDescriptor, cls safety.Classification) (string, error) {
	redacted := safety.RedactedPayload(desc.Payload)
	if hasCategory(cls.Categories, safety.CategoryCredentialAccess) {
		redacted.Command = "[REDACTED:credential_access]"
		redacted.URL = "[REDACTED:credential_access]"
		redacted.Paths = nil
	}
	redactedJSON, _ := json.Marshal(redacted)
	cats := make([]string, 0, len(cls.Categories))
	for _, c := range cls.Categories {
		cats = append(cats, string(c))
	}
	expiresAt := s.now().Add(s.ttl).UTC().Format(time.RFC3339)
	args := map[string]any{
		"correlationKey": key,
		"surface":        string(desc.Surface),
		"action":         string(desc.Action),
		"argsRedacted":   string(redactedJSON),
		"tier":           cls.Tier.String(),
		"categories":     strings.Join(cats, ","),
		"reason":         cls.Reason,
		"expiresAt":      expiresAt,
		"agentId":        desc.Caller.AgentID,
		"ownerUserId":    desc.Caller.OwnerUserID,
		"planId":         desc.Caller.PlanID,
	}
	query := buildMutationCall("mutationCreateApprovalRequest", args)
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return "", err
	}
	return idFromResult(res), nil
}

// hasCategory mirrors the recorder package's helper. Kept local to
// avoid an import dependency on the recorder package (recorder
// imports safety; recorder is the consumer of approval downstream,
// not the other way around).
func hasCategory(cats []safety.Category, target safety.Category) bool {
	for _, c := range cats {
		if c == target {
			return true
		}
	}
	return false
}

// ttlFromEnv reads MEMQL_SAFETY_APPROVAL_TTL_HOURS; falls back to
// DefaultTTLHours on missing / unparseable / negative. Zero is a
// valid operator-intent ("approvals expire immediately on creation")
// -- a degenerate-but-legitimate posture for nodes that don't want
// any approval to confer a bypass; the classifyActive check treats
// an in-the-past expiresAt as expired, so zero-TTL approvals can
// never satisfy the bypass check.
func ttlFromEnv() time.Duration {
	raw := strings.TrimSpace(envLookup(EnvTTLHours))
	if raw == "" {
		return time.Duration(DefaultTTLHours) * time.Hour
	}
	var hours int
	if _, err := fmt.Sscanf(raw, "%d", &hours); err != nil || hours < 0 {
		return time.Duration(DefaultTTLHours) * time.Hour
	}
	return time.Duration(hours) * time.Hour
}

// envLookup is split out so tests can stub. Not exported; the
// concrete impl just calls os.Getenv.
var envLookup = func(k string) string {
	return getenv(k)
}

// buildMutationCall composes a memql mutation call string with bare-
// identifier keys + JSON-marshalled values, keys sorted for stable
// log lines. Same pattern as component/safety/recorder/persisting.go's
// buildMutationQuery -- duplicated rather than exported because the
// two have different lifetimes (the approval sink's args set may
// drift independently of the classification recorder's).
func buildMutationCall(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteString("({")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		v, _ := json.Marshal(args[k])
		b.Write(v)
	}
	b.WriteString("})")
	return b.String()
}

// rowsFromResult coerces an engine.Execute return into a list of
// rows, each represented as a map carrying the payload fields + the
// top-level "id" field.
//
// The expected shape is what ConvertExecuteResultToMap (in app/adapters.go)
// produces -- a map with two axes:
//
//   - response["data"]: shaped rows from a query that used `shape()`.
//     The shape can be []any of maps, []map[string]any, or a single map.
//     For our queryActiveApprovalsByCorrelationKey (which uses
//     `shape approvalRequestFull`), this is where the rows land.
//   - response["bundle"]["nodes"]: []any of {id, concept, type, payload}
//     for the raw graph nodes. Used as a fallback when "data" is empty --
//     covers queries that don't use shape().
//
// Bare []any / "rows" maps are also tolerated for test stubs that
// don't go through the engine.
//
// Unrecognised shapes return an empty list; the caller treats that as
// "no active rows" and creates a fresh pending row -- safe fallback.
func rowsFromResult(res any) []map[string]any {
	var raws []any
	switch r := res.(type) {
	case []any:
		raws = r
	case []map[string]any:
		raws = make([]any, len(r))
		for i, m := range r {
			raws[i] = m
		}
	case map[string]any:
		// Try the "data" axis first (shape() queries).
		switch d := r["data"].(type) {
		case []any:
			raws = d
		case []map[string]any:
			raws = make([]any, len(d))
			for i, m := range d {
				raws[i] = m
			}
		case map[string]any:
			raws = []any{d}
		}
		// Fall back to bundle.nodes for non-shape queries.
		if len(raws) == 0 {
			if bundle, ok := r["bundle"].(map[string]any); ok {
				if nodes, ok := bundle["nodes"].([]any); ok {
					raws = nodes
				}
			}
		}
		// Test-stub convenience: top-level "rows" key.
		if len(raws) == 0 && r["rows"] != nil {
			if list, ok := r["rows"].([]any); ok {
				raws = list
			}
		}
	}
	out := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		flat := map[string]any{}
		// Lift the top-level id (and createdAt) into the flat map.
		for _, k := range []string{"id", "createdAt"} {
			if v, ok := m[k]; ok {
				flat[k] = v
			}
		}
		// Lift the nested payload up.
		if pl, ok := m["payload"].(map[string]any); ok {
			for k, v := range pl {
				if _, dup := flat[k]; dup {
					continue
				}
				flat[k] = v
			}
		}
		// If the shape projection already flattened payload into the
		// top-level map (some shape implementations do this), copy
		// the leftover fields too. Skip well-known nesting keys.
		for k, v := range m {
			if k == "id" || k == "createdAt" || k == "payload" ||
				k == "concept" || k == "type" {
				continue
			}
			if _, dup := flat[k]; dup {
				continue
			}
			flat[k] = v
		}
		out = append(out, flat)
	}
	return out
}

// idFromResult coerces an engine.Execute return for a mutation into
// the inserted row's id. memql.MemQLEngine.Execute returns
// *ExecuteResult; ConvertExecuteResultToMap (in app/adapters.go)
// flattens it into a map with the inserted node at
// response["bundle"]["nodes"][0]["id"]. Mutations don't go through
// shape(), so they don't populate the "data" axis.
//
// Test-stub shorthand (map with top-level "id") + raw-string return
// are also tolerated for unit tests that don't go through the engine.
// Falls back to the empty string (which the Sink turns into
// ApprovalStateUnconfigured) if the shape is unfamiliar -- a safe
// fail-open posture matching the rest of the sink.
func idFromResult(res any) string {
	switch r := res.(type) {
	case map[string]any:
		// Production path: bundle.nodes[0].id from ConvertExecuteResultToMap.
		if bundle, ok := r["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok && len(nodes) > 0 {
				if first, ok := nodes[0].(map[string]any); ok {
					if id, ok := first["id"].(string); ok {
						return id
					}
				}
			}
		}
		// Test-stub shorthand: top-level "id".
		if id, ok := r["id"].(string); ok {
			return id
		}
	case string:
		return r
	}
	return ""
}

// str is a tiny safe-coercion helper for the row-walker. Missing key
// or non-string value returns "".
func str(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
