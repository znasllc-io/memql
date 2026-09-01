package datasync

import (
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/num"
)

// fields.go -- reading a materialized engine row.
//
// The engine hands back `map[string]any` whose values arrive as whatever
// JSON produced: a number may be a float64 or a json.Number, a timestamp
// is a string. These readers absorb that rather than each call site
// type-switching, and every one of them returns the ZERO VALUE for a
// field that is absent or the wrong shape.
//
// Zero-on-absent is the right default here specifically because the
// callers are counters and timestamps on an operational row: a missing
// attempts count means zero attempts, and a missing nextAttemptAt means
// "no wait has been scheduled", which OutboxEntry.Due reads as due. A
// reader that errored instead would turn a partially-written health row
// into a stalled drain.

func stringField(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	if s, isString := v.(string); isString {
		return s
	}
	return ""
}

// intField reads a sync-health row's numeric field.
//
// SATURATES out of range (memql#4779). These are health counters -- attempts,
// lagSeconds, driftCount, outboxDepth, deadLetterCount -- displayed rather
// than allocated against, and zero is the dangerous answer here: it reads as
// "healthy" for a connector that is anything but.
func intField(row map[string]any, key string) int {
	if row == nil {
		return 0
	}
	switch v := row[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case float64:
		return num.ClampFloat64(v)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func boolField(row map[string]any, key string) bool {
	if row == nil {
		return false
	}
	switch v := row[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

// timeField parses a timestamp field, accepting the two shapes the
// engine emits: an RFC3339 string, and a time.Time when a caller has
// already decoded it (the in-process test seam).
func timeField(row map[string]any, key string) time.Time {
	if row == nil {
		return time.Time{}
	}
	switch v := row[key].(type) {
	case time.Time:
		return v.UTC()
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
		return time.Time{}
	default:
		return time.Time{}
	}
}
