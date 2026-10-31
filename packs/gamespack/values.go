package gamespack

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// values.go -- the small vocabulary every handler shares: who is calling,
// how an id is spelled, how a decoded payload value narrows to a Go value,
// and the one node a builtin answers with.

// actorUserID is the caller's user id, or "" when the context carries none.
func actorUserID(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		return strings.TrimSpace(ac.UserId)
	}
	return ""
}

// requireActor refuses a call with no person behind it: every games row is
// somebody's, and a row written with no owner is readable by nobody.
func requireActor(ctx context.Context) (string, error) {
	owner := actorUserID(ctx)
	if owner == "" {
		return "", fmt.Errorf("games_unauthenticated: this call needs a signed-in owner")
	}
	return owner, nil
}

// ownerKey is the owner as a hand-written statement compares it: CANONICAL.
// ownerUserId declares @relationship(type="parent", target=user), so the
// engine canonicalizes it on insert ("v1:identity:user:<id>") while the
// actor carries the bare id; a statement comparing the two spellings matches
// nothing and says nothing.
func ownerKey(owner string) string { return canonical(conceptUser, owner) }

// canonical returns the canonical id of a row of concept, from a bare or a
// canonical spelling. Empty stays empty.
func canonical(concept, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, concept+":") {
		return value
	}
	return id.BuildNodeId(concept, memql.BareShortId(value))
}

// bare returns the bare short id of any id spelling.
func bare(value string) string { return memql.BareShortId(strings.TrimSpace(value)) }

// bareRow puts a read row's id in the bare spelling every answer carries:
// clients never compose, parse or compare canonical ids.
func bareRow(r map[string]any) map[string]any {
	if r != nil {
		r["id"] = bare(trimmed(r["id"]))
	}
	return r
}

// asString narrows a payload value to text.
func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// trimmed is asString plus the trim every caller wants.
func trimmed(v any) string { return strings.TrimSpace(asString(v)) }

// asInt narrows a payload number to an int. Out of range SATURATES, through
// core/num -- the memql#4779 rule that every narrowing names its answer.
func asInt(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return num.ClampInt64(n)
	case float64:
		if math.IsNaN(n) {
			return fallback
		}
		return num.ClampFloat64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return fallback
		}
		return asInt(f, fallback)
	}
	return fallback
}

// asFloat narrows a payload number to a float64. A number that is not finite is
// the fallback, whichever way it arrives: strconv.ParseFloat, which reads a
// json.Number, also reads the words NaN, Inf and Infinity without an error.
func asFloat(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return fallback
		}
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fallback
		}
		return f
	}
	return fallback
}

// clampInt bounds v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// asStrings narrows a payload list of strings.
func asStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := trimmed(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// sameStrings reports two lists equal element by element, in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// asObject narrows a payload object.
func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// timeOf reads a stored datetime, which the engine hands back as an RFC 3339
// string (or, in-process, occasionally as a time.Time). It answers the zero time
// for what it cannot read.
func timeOf(v any) time.Time {
	t, _ := readTime(v)
	return t
}

// readTime is timeOf that also says whether there was a time to read. The zero
// time timeOf answers for what it cannot read is also the first instant of year
// 1, so a caller that must tell the two apart asks here.
func readTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), true
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, strings.TrimSpace(t)); err == nil {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// rfc3339 formats a time the way a datetime argument is written.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// versionAfter is when a read-modify-write's new version is stamped: now, or
// one microsecond after the version it read, whichever is later
// (component/emailrules firedAtAfter, memql#5431). The newest version is the
// one with the newest createdAt, so a replica whose clock lags would
// otherwise file its write BEHIND the version it read; microseconds, because
// that is what the column stores.
func versionAfter(prior, now time.Time) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	if prior.IsZero() {
		return now
	}
	prior = prior.UTC().Truncate(time.Microsecond)
	if now.After(prior) {
		return now
	}
	return prior.Add(time.Microsecond)
}

// answer wraps a capability's result as the one node a builtin returns. The
// node is the expression's value and is never persisted.
func answer(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("games: marshal %s answer: %w", kind, err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("games:%s:%d", kind, time.Now().UTC().UnixNano()),
		Concept:   "integration:games:" + kind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}
