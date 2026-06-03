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
package memql

import (
	"bytes"
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

const (
	defaultLoopGuardMaxRepeat      = 8
	defaultLoopGuardWindowSeconds  = 60
	defaultLoopGuardCooldownSecond = 30
)

// llmGuard is the process-global circuit breaker shared by every
// provider HTTP client. Safe for concurrent use.
type llmGuard struct {
	enabled bool

	maxRepeat int
	window    time.Duration
	cooldown  time.Duration

	mu      sync.Mutex
	hits    map[string][]time.Time // fingerprint -> recent call times (within window)
	tripped map[string]time.Time   // fingerprint -> breaker-open-until

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
		hits:      map[string][]time.Time{},
		tripped:   map[string]time.Time{},
		now:       time.Now,
		logger:    slog.Default().With("component", "llmGuard"),
	}
	if g.maxRepeat < 1 {
		g.maxRepeat = defaultLoopGuardMaxRepeat
	}
	if g.window <= 0 {
		g.window = defaultLoopGuardWindowSeconds * time.Second
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
	if g == nil || !g.enabled {
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
		if open, repeats := g.admit(fp); open {
			return loopBreakerResponse(req, repeats), nil
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
