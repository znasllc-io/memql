// si_guard.go
//
// Global LLM circuit breaker (memql#825). The single point every LLM
// HTTP call leaves the process is the provider SDKs' *http.Client. We
// install ONE guarding http.RoundTripper on the clients every provider
// (OpenAI + Anthropic, stream + non-stream, chat / tools / structured /
// vision) is built from, so a runaway loop is caught HERE -- cheaply,
// before a real vendor request -- no matter which caller (planner,
// agent tool loop, cognition, ...) initiated it.
//
// Primary guard: an identical-request loop breaker. Two LLM calls with
// the byte-identical request (method + URL + body) are fingerprinted to
// the same key; if the SAME fingerprint fires more than N times within
// a sliding window, the breaker trips for a cooldown and returns a
// synthetic 429 rate_limit_error response WITHOUT calling the vendor.
// Because distinct requests fingerprint differently, legitimate traffic
// is never throttled -- only a wild loop spamming the exact same call.
//
// Returning a real 429 (not a transport error) means both vendor SDKs
// surface their normal rate-limit error type, so this composes cleanly
// with the planner's provider-rate-limit backoff (memql#821).
//
// Secondary guard (memql#834): a HARD, process-wide rate ceiling on
// chat/messages calls. Independent of request content -- it counts
// EVERY guarded LLM call leaving the process within a rolling window,
// regardless of which caller fired it or what the body says. When the
// count in the window reaches the ceiling, further calls get the same
// synthetic 429 WITHOUT touching the vendor. This is the seatbelt the
// per-fingerprint loop breaker can't be: a runaway that VARIES its
// request body (growing context, timestamps) fingerprints differently
// every call and sails past the loop breaker -- but it cannot outrun
// the global call counter. With a generous default (20 calls / 10s) it
// is invisible to real traffic and lethal to a spend loop.
package memql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// laneCtxKey tags a context as belonging to the background (batch)
// execution lane (memql#897). The background SI executor stamps it via
// ContextWithBackgroundLane before issuing model calls; because the
// vendor SDKs thread the call context all the way to the HTTP request,
// guardedTransport.RoundTrip can read it off req.Context() and count the
// call against the background rate bucket instead of the interactive one.
type laneCtxKey struct{}

// ContextWithBackgroundLane marks ctx as the background execution lane so
// downstream LLM HTTP calls are rate-limited against the background bucket
// rather than the interactive one. No-op-safe on a nil ctx.
func ContextWithBackgroundLane(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, laneCtxKey{}, true)
}

// backgroundLaneFromContext reports whether ctx was tagged as the
// background execution lane. Defaults to false (interactive) for any
// context that wasn't stamped -- so chat, voice, suggest, and every legacy
// path keep counting against the interactive ceiling unchanged.
func backgroundLaneFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(laneCtxKey{}).(bool)
	return v
}

const (
	defaultLoopGuardMaxRepeat      = 8
	defaultLoopGuardWindowSeconds  = 60
	defaultLoopGuardCooldownSecond = 30

	// Global rate-ceiling defaults (memql#834). Generous for real
	// traffic, lethal to a runaway: 20 guarded LLM calls in any rolling
	// 10s window. A legitimate burst (a few concurrent users, a planner
	// fanning out a handful of tasks) stays well under 2 calls/sec; a
	// spend loop blows straight through it and is capped.
	defaultRateMaxCalls      = 20
	defaultRateWindowSeconds = 10

	// Background-lane rate-ceiling defaults (memql#897). The background /
	// batch execution lane (planner-dispatched plan/task turns) gets its
	// OWN rate bucket, independent of the interactive ceiling above, so a
	// burst of task executions counts against THIS budget and can never
	// consume the interactive budget that protects live chat + voice. A
	// touch more generous than interactive since background work is the
	// fan-out path (memql#899) and nobody is waiting on its first token.
	defaultBgRateMaxCalls      = 40
	defaultBgRateWindowSeconds = 10
)

// llmGuard is the process-global circuit breaker shared by every
// provider HTTP client. Safe for concurrent use.
type llmGuard struct {
	enabled bool

	maxRepeat int
	window    time.Duration
	cooldown  time.Duration

	// Global rate ceiling (memql#834). Independent of the per-fingerprint
	// loop breaker above: counts EVERY admitted guarded call across the
	// whole process within rateWindow. rateEnabled gates it separately
	// so an operator can tune the loop breaker and the rate ceiling on
	// their own switches.
	rateEnabled bool
	rateMax     int
	rateWindow  time.Duration

	mu      sync.Mutex
	hits    map[string][]time.Time // fingerprint -> recent call times (within window)
	tripped map[string]time.Time   // fingerprint -> breaker-open-until

	// rateHits is the global sliding window of admitted guarded-call
	// timestamps (memql#834). Blocked calls are NOT recorded, so a
	// hammering loop can't extend its own window -- the window drains
	// naturally and admits real traffic again once old calls age out.
	rateHits []time.Time
	// lastRateAlert throttles the loud ERROR log to once per window so a
	// sustained runaway doesn't drown the logs while still being loud.
	lastRateAlert time.Time

	// Background-lane rate ceiling (memql#897). A SEPARATE bucket from the
	// interactive one above, selected per-call by a context flag the
	// background executor stamps (see ContextWithBackgroundLane). Keeping
	// the two buckets independent is the isolation that matters: a burst of
	// planner-dispatched task executions fills bgRateHits and can trip the
	// background ceiling without ever touching rateHits, so live chat +
	// voice never get a synthetic 429 because batch work was busy.
	bgRateEnabled   bool
	bgRateMax       int
	bgRateWindow    time.Duration
	bgRateHits      []time.Time
	bgLastRateAlert time.Time

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time

	logger *slog.Logger
}

// loggerOrDefault guards against a nil logger in tests that build the
// struct literal directly.
func (g *llmGuard) log() *slog.Logger {
	if g.logger == nil {
		return slog.Default()
	}
	return g.logger
}

// sharedLLMGuard is the one breaker instance every provider references.
var sharedLLMGuard = newLLMGuardFromEnv()

func newLLMGuardFromEnv() *llmGuard {
	g := &llmGuard{
		enabled:   envBoolDefault("MEMQL_LLM_LOOP_GUARD_ENABLED", true),
		maxRepeat: envIntDefault("MEMQL_LLM_LOOP_MAX_REPEAT", defaultLoopGuardMaxRepeat),
		window:    time.Duration(envIntDefault("MEMQL_LLM_LOOP_WINDOW_SECONDS", defaultLoopGuardWindowSeconds)) * time.Second,
		cooldown:  time.Duration(envIntDefault("MEMQL_LLM_LOOP_COOLDOWN_SECONDS", defaultLoopGuardCooldownSecond)) * time.Second,

		rateEnabled: envBoolDefault("MEMQL_LLM_RATE_GUARD_ENABLED", true),
		rateMax:     envIntDefault("MEMQL_LLM_MAX_CALLS_PER_WINDOW", defaultRateMaxCalls),
		rateWindow:  time.Duration(envIntDefault("MEMQL_LLM_RATE_WINDOW_SECONDS", defaultRateWindowSeconds)) * time.Second,

		bgRateEnabled: envBoolDefault("MEMQL_LLM_BG_RATE_GUARD_ENABLED", true),
		bgRateMax:     envIntDefault("MEMQL_LLM_BG_MAX_CALLS_PER_WINDOW", defaultBgRateMaxCalls),
		bgRateWindow:  time.Duration(envIntDefault("MEMQL_LLM_BG_RATE_WINDOW_SECONDS", defaultBgRateWindowSeconds)) * time.Second,

		hits:    map[string][]time.Time{},
		tripped: map[string]time.Time{},
		now:     time.Now,
		logger:  slog.Default().With("component", "llmGuard"),
	}
	if g.maxRepeat < 1 {
		g.maxRepeat = defaultLoopGuardMaxRepeat
	}
	if g.window <= 0 {
		g.window = defaultLoopGuardWindowSeconds * time.Second
	}
	if g.rateMax < 1 {
		g.rateMax = defaultRateMaxCalls
	}
	if g.rateWindow <= 0 {
		g.rateWindow = defaultRateWindowSeconds * time.Second
	}
	if g.bgRateMax < 1 {
		g.bgRateMax = defaultBgRateMaxCalls
	}
	if g.bgRateWindow <= 0 {
		g.bgRateWindow = defaultBgRateWindowSeconds * time.Second
	}
	return g
}

// admit records one call against the fingerprint and reports whether
// the breaker is OPEN for it (i.e. the call must be rejected). When it
// returns true, the caller returns a synthetic 429 and makes no vendor
// request.
func (g *llmGuard) admit(fingerprint string) (open bool, repeatCount int) {
	if g == nil || !g.enabled {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	// Already tripped + still in cooldown -> reject without recording
	// (so a hammering loop doesn't keep extending its own window).
	if until, ok := g.tripped[fingerprint]; ok {
		if now.Before(until) {
			return true, len(g.hits[fingerprint])
		}
		// Cooldown elapsed -- reset this fingerprint and let it through.
		delete(g.tripped, fingerprint)
		delete(g.hits, fingerprint)
	}

	// Prune entries older than the window, then record this call.
	cutoff := now.Add(-g.window)
	kept := g.hits[fingerprint][:0]
	for _, t := range g.hits[fingerprint] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	g.hits[fingerprint] = kept

	if len(kept) > g.maxRepeat {
		// Trip the breaker for this exact request.
		g.tripped[fingerprint] = now.Add(g.cooldown)
		g.log().Error("LLM circuit breaker TRIPPED: identical request repeated past the loop threshold",
			"fingerprint", fingerprint,
			"repeats_in_window", len(kept),
			"max_repeat", g.maxRepeat,
			"window", g.window.String(),
			"cooldown", g.cooldown.String(),
		)
		return true, len(kept)
	}
	return false, len(kept)
}

// admitRate enforces the global, content-independent rate ceiling
// (memql#834). It prunes the sliding window, and if the count of
// admitted guarded calls in the last rateWindow has reached rateMax it
// returns open=true (REJECT) WITHOUT recording this call -- so a
// runaway can't keep its own window full and starve real traffic
// forever; the window drains and admits again once old calls age out.
// Otherwise it records the call and returns open=false (ADMIT).
//
// This is deliberately separate from admit(): admit fingerprints on the
// request body and only catches byte-identical loops; admitRate counts
// ALL guarded calls regardless of content, which is the only thing that
// stops a loop that varies its request body.
//
// background selects which lane's bucket the call counts against
// (memql#897). The interactive and background buckets are fully
// independent -- distinct windows, distinct ceilings, distinct alert
// throttles -- so saturating one cannot trip the other. The background
// executor stamps the lane on the request context; everything else
// (chat, voice, suggest) counts against the interactive bucket.
func (g *llmGuard) admitRate(background bool) (open bool, count int) {
	if g == nil {
		return false, 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	// Select the lane's bucket. Pointers so the single window-prune +
	// record body works against either bucket without duplication.
	enabled, max, window := g.rateEnabled, g.rateMax, g.rateWindow
	hits, lastAlert := &g.rateHits, &g.lastRateAlert
	lane := "interactive"
	if background {
		enabled, max, window = g.bgRateEnabled, g.bgRateMax, g.bgRateWindow
		hits, lastAlert = &g.bgRateHits, &g.bgLastRateAlert
		lane = "background"
	}
	if !enabled {
		return false, 0
	}

	cutoff := now.Add(-window)
	kept := (*hits)[:0]
	for _, t := range *hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	*hits = kept

	if len(kept) >= max {
		// Throttle the alert to once per window so a sustained runaway
		// stays loud without flooding the logs.
		if lastAlert.IsZero() || now.Sub(*lastAlert) >= window {
			*lastAlert = now
			g.log().Error("LLM rate ceiling TRIPPED: per-lane call rate exceeded; blocking to prevent a runaway spend loop",
				"lane", lane,
				"calls_in_window", len(kept),
				"max_calls", max,
				"window", window.String(),
			)
		}
		return true, len(kept)
	}
	*hits = append(*hits, now)
	return false, len(*hits)
}

// fingerprintRequest derives the loop-breaker key from a request:
// method + URL path + a hash of the (buffered) body. Returns ("", body,
// false) when the request is not a chat/messages call we want to guard
// -- callers then skip the guard entirely.
func fingerprintRequest(method, urlPath string, body []byte) (fingerprint string, guarded bool) {
	if !isGuardedLLMPath(urlPath) {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(urlPath))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), true
}

// isGuardedLLMPath limits the breaker to chat-completion / messages
// endpoints. Embeddings, TTS, audio, moderation, etc. legitimately
// repeat identical inputs and must NOT be loop-guarded.
func isGuardedLLMPath(urlPath string) bool {
	p := strings.ToLower(urlPath)
	return strings.Contains(p, "/chat/completions") || // OpenAI-wire vendors
		strings.HasSuffix(p, "/messages") || // Anthropic
		strings.Contains(p, "/messages?") ||
		strings.Contains(p, "/v1/messages")
}

// guardedTransport is the http.RoundTripper that fronts every provider
// HTTP client. It fingerprints guarded requests and short-circuits with
// a synthetic 429 when the breaker is open.
type guardedTransport struct {
	base  http.RoundTripper
	guard *llmGuard
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	g := t.guard
	if g == nil || (!g.enabled && !g.rateEnabled) {
		return base.RoundTrip(req)
	}

	// Read + restore the body so we can fingerprint it. Chat request
	// bodies are small JSON payloads (the request we send, not the SSE
	// response), so buffering once is cheap.
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			// Can't read the body -> can't fingerprint; fail open so a
			// transient read error never blocks legitimate work.
			return base.RoundTrip(req)
		}
		bodyBytes = b
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bodyBytes)), nil
		}
		req.ContentLength = int64(len(bodyBytes))
	}

	fp, guarded := fingerprintRequest(req.Method, req.URL.Path, bodyBytes)
	if guarded {
		// Per-fingerprint loop breaker first (catches byte-identical
		// loops cheaply). admit() self-gates on g.enabled.
		if open, repeats := g.admit(fp); open {
			return loopBreakerResponse(req, repeats), nil
		}
		// Then the per-lane, content-independent rate ceiling -- the
		// backstop that catches loops varying their request body. The lane
		// is read off the request context (memql#897): background-executor
		// calls count against the background bucket, everything else against
		// the interactive bucket. admitRate self-gates on the lane's enable
		// flag.
		background := backgroundLaneFromContext(req.Context())
		if open, calls := g.admitRate(background); open {
			return rateCeilingResponse(req, calls), nil
		}
	}
	return base.RoundTrip(req)
}

// loopBreakerResponse builds a synthetic HTTP 429 the vendor SDKs parse
// as a rate-limit error. Shaped like an Anthropic error envelope; the
// OpenAI SDK keys on the 429 status code, so the body shape is
// tolerated by both.
func loopBreakerResponse(req *http.Request, repeats int) *http.Response {
	msg := fmt.Sprintf(
		"memql LLM circuit breaker: the identical request was repeated %d times within the loop window and was blocked to prevent a runaway spend loop. This is a local guard, not a provider limit; the request will be admitted again after the cooldown.",
		repeats,
	)
	body := fmt.Sprintf(`{"type":"error","error":{"type":"rate_limit_error","message":%q}}`, msg)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"30"},
		},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// rateCeilingResponse builds the synthetic HTTP 429 returned when the
// process-wide rate ceiling (memql#834) is hit. Same envelope shape as
// loopBreakerResponse so both vendor SDKs surface their normal
// rate-limit error type and the planner's 429 backoff (memql#821)
// composes cleanly.
func rateCeilingResponse(req *http.Request, calls int) *http.Response {
	msg := fmt.Sprintf(
		"memql LLM rate ceiling: %d chat/messages calls left the process within the rolling rate window, at or above the configured ceiling, so this call was blocked to prevent a runaway spend loop. This is a local process-wide guard, not a provider limit; calls are admitted again as the window drains.",
		calls,
	)
	body := fmt.Sprintf(`{"type":"error","error":{"type":"rate_limit_error","message":%q}}`, msg)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"5"},
		},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// guardedHTTPClient returns an *http.Client whose transport is fronted
// by the shared circuit breaker. base may be nil (uses
// http.DefaultTransport). Both vendor SDKs accept an *http.Client, so
// this one helper covers every provider construction.
func guardedHTTPClient(base *http.Client) *http.Client {
	var baseTransport http.RoundTripper
	c := &http.Client{}
	if base != nil {
		*c = *base
		baseTransport = base.Transport
	}
	c.Transport = &guardedTransport{base: baseTransport, guard: sharedLLMGuard}
	return c
}

// --- small env helpers (local to keep the guard self-contained) ------------

func envBoolDefault(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envIntDefault(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}
