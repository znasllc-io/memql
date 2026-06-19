// reactive_loop_helpers.go
//
// Small pure helpers for the reactive loop: actor / impersonation
// context construction, the convergence decision parser, time + cron
// occurrence math, and a handful of map/slice coercers. Kept separate
// from reactive_loop.go so the orchestration reads top-to-bottom.

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/znasllc-io/memql/component/auth"
)

// reactiveSystemActorContext stamps the synthetic system subject used
// for the cross-user sweep read. No AccessContext (no single user), so
// actor.userId resolves empty -- which is fine: the sweep query carries
// no owner filter.
func reactiveSystemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: reactiveSystemActor,
		Claims: map[string]any{
			"sub":  reactiveSystemActor,
			"role": "system",
		},
	})
}

// ownerActorContext builds the impersonation context for owned-tier
// per-user writes: a system subject (so the write gate's
// auth.ActorFromContext is non-empty) PLUS an AccessContext whose UserId
// is the responsibility owner (so actor.userId in the mutation bodies
// resolves to the owner). This is what lets the planner node -- which
// has no user session -- write a responsibility's own owner's rows
// without being able to touch anyone else's (the sweep is read-only and
// each write re-stamps ownerUserId from this impersonated userId).
func ownerActorContext(ctx context.Context, ownerUserId string) context.Context {
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: reactiveSystemActor,
		Claims: map[string]any{
			"sub":  reactiveSystemActor,
			"role": "system",
		},
	})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: ownerUserId,
		Role:   auth.RoleWriter,
	})
}

// --- convergence decision parsing -----------------------------------------

// convergenceAction is one proactive action the reactiveConductor
// emits.
type convergenceAction struct {
	Kind             string  `json:"kind"`
	ResponsibilityId string  `json:"responsibilityId"`
	Statement        string  `json:"statement"`
	Rationale        string  `json:"rationale"`
	Confidence       float64 `json:"confidence"`
}

// convergenceDecision is the reactiveConductor structured output.
type convergenceDecision struct {
	Actions      []convergenceAction `json:"actions"`
	SilentReason string              `json:"silentReason"`
}

// parseConvergence normalizes the conductor's response (string / map /
// other) into a convergenceDecision, tolerating markdown code fences the
// model occasionally wraps the JSON in (mirrors parsePlannerDecision).
func parseConvergence(resp any) (convergenceDecision, error) {
	if resp == nil {
		return convergenceDecision{}, fmt.Errorf("reactiveConductor returned nil")
	}
	var raw []byte
	switch v := resp.(type) {
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return convergenceDecision{}, err
		}
		raw = b
	}
	raw = stripJSONFence(raw)
	var d convergenceDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return convergenceDecision{}, fmt.Errorf("parse convergence JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	return d, nil
}

// --- time + cron helpers ---------------------------------------------------

// parseTimeOrZero parses an RFC3339 timestamp, returning the zero time
// on empty / unparseable input.
func parseTimeOrZero(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// latestOccurrenceBefore returns the most recent occurrence of a cron
// schedule at or before `now`. cron.Schedule only exposes Next(), so we
// step forward from a point safely in the past (24h before now, which
// covers every standard cron field granularity down to the minute) and
// keep the last occurrence that is still <= now. Returns the zero time
// when no occurrence falls in the lookback window.
func latestOccurrenceBefore(sched cron.Schedule, now time.Time) time.Time {
	// Start one lookback window before now. A standard 5-field cron fires
	// at most once per minute; a daily/weekly/monthly schedule fires far
	// less often. 35 days back guarantees we catch a monthly occurrence;
	// the loop bound keeps the worst case (every-minute schedule) cheap
	// because we only need the latest <= now, found by overshooting once.
	cursor := now.Add(-35 * 24 * time.Hour)
	var last time.Time
	// Bounded iteration: even an every-minute schedule over 35 days is
	// ~50k steps; cap well above that and below an infinite loop. In
	// practice schedules are daily/weekly so this terminates in a handful
	// of steps via the <= now break.
	for i := 0; i < 60000; i++ {
		nxt := sched.Next(cursor)
		if nxt.IsZero() || nxt.After(now) {
			break
		}
		last = nxt
		cursor = nxt
	}
	return last.UTC()
}

// --- plan status -----------------------------------------------------------

// isTerminalPlanStatus reports whether a Plan status is terminal (no
// more work will happen on it). Mirrors the v1:planner:plan status enum.
func isTerminalPlanStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// --- map / slice coercers --------------------------------------------------

// mapGet reads a key off a possibly-nil map.
func mapGet(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

// toStringList coerces a value (typically a JSON-decoded []any) into a
// []string, dropping non-string elements. Returns an empty slice for nil
// / wrong-typed input.
func toStringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// compactResponsibilities projects the responsibility rows into the
// compact shape the reactiveConductor prompt expects, dropping the
// fields the conductor doesn't reason over (condition payloads, scope
// pins) to keep the prompt token-lean.
func compactResponsibilities(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":              getString(row, "id"),
			"statement":       getString(row, "statement"),
			"trigger":         getString(row, "trigger"),
			"schedule":        getString(row, "schedule"),
			"targetKind":      getString(row, "targetKind"),
			"assignedAgentId": getString(row, "assignedAgentId"),
			"lastEvaluatedAt": getString(row, "lastEvaluatedAt"),
			"lastResult":      getString(row, "lastResult"),
		})
	}
	return out
}
