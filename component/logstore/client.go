package logstore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/logger"
)

// client.go -- the OS write, logsRecordClient (design E, L9).
//
// The MemQL OS front end batches its window errors, unhandled rejections and
// console warnings and hands them to the bff that holds its stream. Nothing
// in the payload is trusted for identity: userId is stamped from the actor,
// nodeType is os, node is blank. `session` is a client-minted per-tab id, a
// CORRELATION KEY and never an authority -- it groups lines, it grants
// nothing.

// The caps (design L9). A call past any of them is refused WHOLE, with a
// sentence naming the line and the cap, so the shell can see what it did
// rather than lose half a batch silently.
const (
	MaxClientLines = 50

	// ClientClockSkew is how far a client `at` may sit from the node clock
	// and still be kept; one outside it is replaced by the node's now, which
	// keeps a browser with a wrong clock from writing into next week.
	ClientClockSkew = 5 * time.Minute

	// The per-(user, session) bucket: 120 lines, refilled two per second.
	ClientBucketCapacity = 120
	ClientBucketRefill   = 2.0

	// ReasonRateLimited is the reply's reason when the bucket refuses.
	ReasonRateLimited = "rate_limited"

	clientLimiterMaxBuckets = 20000
	clientLimiterIdleSweep  = 30 * time.Minute
)

var (
	sessionPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)
	appPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	componentPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

var clientLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// ClientReply is what logsRecordClient answers.
type ClientReply struct {
	Accepted int    `json:"accepted"`
	Dropped  int    `json:"dropped"`
	Reason   string `json:"reason,omitempty"`
}

// ValidateSession checks the tab session id.
func ValidateSession(session string) error {
	if !sessionPattern.MatchString(session) {
		return fmt.Errorf("session %q is not a valid tab session id: 4 to 64 characters of A-Z, a-z, 0-9, _ or -", session)
	}
	return nil
}

// ParseClientLines validates and converts the raw `lines` argument -- the
// []object the builtin declares, arriving as []any of map[string]any -- into
// logger.Lines. Any violation refuses the WHOLE call with a sentence naming
// the line index and the cap.
func ParseClientLines(raw []any, now time.Time) ([]logger.Line, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("lines is empty: nothing to record")
	}
	if len(raw) > MaxClientLines {
		return nil, fmt.Errorf("%d lines is past the cap of %d per call; the shell batches at most %d", len(raw), MaxClientLines, MaxClientLines)
	}
	out := make([]logger.Line, 0, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("line %d is not an object", i)
		}
		l, err := parseClientLine(obj, now)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i, err)
		}
		out = append(out, l)
	}
	return out, nil
}

func parseClientLine(obj map[string]any, now time.Time) (logger.Line, error) {
	var l logger.Line

	levelRaw := strings.ToLower(strings.TrimSpace(stringField(obj, "level")))
	level, ok := clientLevels[levelRaw]
	if !ok {
		return l, fmt.Errorf("level %q is not one of debug, info, warn, error", stringField(obj, "level"))
	}
	l.Level = level

	l.Message = stringField(obj, "message")
	if strings.TrimSpace(l.Message) == "" {
		return l, fmt.Errorf("message is required")
	}
	if len(l.Message) > MaxMessageBytes {
		return l, fmt.Errorf("message is %d bytes; the cap is %d", len(l.Message), MaxMessageBytes)
	}

	l.App = strings.TrimSpace(stringField(obj, "app"))
	if l.App != "" && !appPattern.MatchString(l.App) {
		return l, fmt.Errorf("app %q is not an app id: a lowercase letter then up to 39 of a-z, 0-9 or -", l.App)
	}

	l.Component = strings.TrimSpace(stringField(obj, "component"))
	if l.Component == "" {
		if l.App != "" {
			l.Component = "os." + l.App
		} else {
			l.Component = "os.shell"
		}
	}
	if !componentPattern.MatchString(l.Component) {
		return l, fmt.Errorf("component %q is not a component name: a lowercase letter then up to 63 of a-z, 0-9, ., _ or -", l.Component)
	}

	if attrs, present := obj["attributes"]; present && attrs != nil {
		m, ok := attrs.(map[string]any)
		if !ok {
			return l, fmt.Errorf("attributes is not an object")
		}
		if len(m) > 0 {
			b, err := json.Marshal(m)
			if err != nil {
				return l, fmt.Errorf("attributes cannot be serialized: %v", err)
			}
			if len(b) > MaxAttributesBytes {
				return l, fmt.Errorf("attributes serialize to %d bytes; the cap is %d", len(b), MaxAttributesBytes)
			}
			l.Attributes = m
		}
	}

	l.Subject = strings.TrimSpace(stringField(obj, "subject"))
	l.SubjectConcept = strings.TrimSpace(stringField(obj, "subjectConcept"))

	l.At = now
	if atRaw := strings.TrimSpace(stringField(obj, "at")); atRaw != "" {
		if at, err := time.Parse(time.RFC3339Nano, atRaw); err == nil {
			if d := at.Sub(now); d >= -ClientClockSkew && d <= ClientClockSkew {
				l.At = at
			}
		}
	}
	return l, nil
}

func stringField(obj map[string]any, key string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// ClientLimiter is the per-(user, session) bucket. In memory and per
// replica, like abuse.IPRateLimiter, and for the same reason: a browser
// reconnecting to a sibling starts a fresh bucket, and the cap is a courtesy
// against a storm rather than an authority (design I).
type ClientLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*clientBucket
	capacity float64
	refill   float64
}

type clientBucket struct {
	bucket   *tokenBucket
	lastSeen time.Time
}

// NewClientLimiter builds the limiter at the L9 bounds.
func NewClientLimiter() *ClientLimiter {
	return &ClientLimiter{
		buckets:  make(map[string]*clientBucket, 64),
		capacity: ClientBucketCapacity,
		refill:   ClientBucketRefill,
	}
}

// Allow asks for n lines' worth of tokens for one (user, session). A refusal
// consumes nothing: the whole call is refused and the shell drops it.
func (l *ClientLimiter) Allow(userId, session string, n int, now time.Time) bool {
	if l == nil || n <= 0 {
		return true
	}
	key := userId + "\x00" + session
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= clientLimiterMaxBuckets {
			cutoff := now.Add(-clientLimiterIdleSweep)
			for k, v := range l.buckets {
				if v.lastSeen.Before(cutoff) {
					delete(l.buckets, k)
				}
			}
		}
		b = &clientBucket{bucket: newTokenBucket(l.capacity, l.refill, now)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	l.mu.Unlock()
	return b.bucket.take(now, float64(n))
}

// Snapshot reports the bucket count, for tests and telemetry.
func (l *ClientLimiter) Snapshot() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
