package abuse

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// Failure-reason constants — used in audit-event failureReason and in
// the JSON error responses so support / dashboards can correlate.
const (
	FailureRateLimited           = "rate_limited"
	FailureTurnstileFailed       = "turnstile_failed"
	FailureDisposableDomain      = "disposable_domain"
	FailureNoMXRecord            = "no_mx_record"
	FailureRiskThresholdExceeded = "risk_threshold_exceeded"
)

// peekRequestBody is the maximum bytes we'll read up-front to extract
// email + Turnstile token. Larger requests pass through to the
// handler with the body untouched. 16 KiB is well above any
// legitimate JSON payload for this endpoint.
const peekRequestBody = 16 * 1024

// magicLinkPath is the only path the middleware actively gates today.
// Other auth routes pass straight through.
const magicLinkPath = "/auth/magic-link"

// Middleware wraps the magic-link handler with the abuse stack.
// Construct once at startup and call Wrap to chain it onto the
// underlying handler.
type Middleware struct {
	Cfg       identity.Config
	Audit     identity.AuditLogger
	Logger    *slog.Logger
	MX        *MXValidator
	Turnstile *TurnstileVerifier
	IPLimiter *IPRateLimiter
	Threshold int

	// SkipPaths lets callers exempt specific request paths from any
	// processing (e.g. health checks). Optional; nil means "process
	// every gated path."
	SkipPaths map[string]struct{}
}

// magicLinkPeek mirrors the JSON shape POST /auth/magic-link expects.
// We only need email + the Turnstile token here; everything else
// (clientId, redirectURI, etc.) is the downstream handler's concern.
type magicLinkPeek struct {
	Email     string `json:"email"`
	Turnstile string `json:"turnstile,omitempty"`
}

// Wrap returns an http.Handler that runs the abuse stack before
// delegating to next. Only POST /auth/magic-link is actively gated;
// every other path is delegated unchanged.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if m == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.shouldGate(r) {
			next.ServeHTTP(w, r)
			return
		}

		ip := clientIP(r)

		// 1) per-IP rate limit. Cheapest check; bounce immediately
		// when over budget.
		if m.IPLimiter != nil {
			if ok, retryAfter := m.IPLimiter.Allow(ip); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				m.reject(w, r, http.StatusTooManyRequests, FailureRateLimited, ip, "", map[string]any{
					"retryAfterSeconds": retryAfter,
				})
				return
			}
		}

		// Buffer the request body so we can peek at it without
		// consuming the downstream handler's read.
		body, err := io.ReadAll(io.LimitReader(r.Body, peekRequestBody+1))
		if err != nil {
			// Read failures here are weird — log and pass through,
			// the handler will surface a more specific error.
			m.logDebug("magic_link_body_read_failed", slog.String("error", err.Error()))
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
			return
		}
		// Restore the body for the handler regardless of what we do
		// next. If the body exceeded our peek budget, just pass
		// through — abuse-vector payloads at the magic-link handler
		// aren't going to be 16KB JSON, and the handler has its own
		// body-size limits.
		var oversize bool
		if len(body) > peekRequestBody {
			oversize = true
			body = body[:peekRequestBody]
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var peek magicLinkPeek
		if !oversize && len(body) > 0 {
			if err := json.Unmarshal(body, &peek); err != nil {
				// Malformed JSON: pass through and let the handler
				// emit a structured invalid_request error.
				m.logDebug("magic_link_body_unparseable", slog.String("error", err.Error()))
				next.ServeHTTP(w, r)
				return
			}
		}

		email := strings.TrimSpace(strings.ToLower(peek.Email))

		// 2) Turnstile verification. Only when a secret is configured.
		var turnstileBorderline bool
		if m.Turnstile != nil && m.Cfg.TurnstileSecret != "" {
			result, err := m.Turnstile.Verify(r.Context(), peek.Turnstile, ip)
			if err != nil {
				// Transport error talking to Cloudflare — fail open
				// (don't block legitimate users on a Cloudflare
				// outage), but record the failure for observability.
				m.logWarn("turnstile_verify_error",
					slog.String("error", err.Error()),
					slog.String("ip", ip),
				)
			} else if !result.OK {
				m.reject(w, r, http.StatusBadRequest, FailureTurnstileFailed, ip, email, map[string]any{
					"errorCodes": result.ErrorCodes,
				})
				return
			} else {
				turnstileBorderline = result.Borderline
			}
		}

		// 3) Disposable-email blocklist (when enabled).
		disposableHit := m.Cfg.DisposableEmailBlocklistEnabled && email != "" && IsDisposable(email)
		if disposableHit {
			m.reject(w, r, http.StatusBadRequest, FailureDisposableDomain, ip, email, nil)
			return
		}

		// 4) MX-record validation (when enabled). Skip for empty
		// email — the handler already enforces presence.
		mxFailed := false
		if m.Cfg.MXValidationEnabled && email != "" && m.MX != nil {
			if !m.MX.HasMX(r.Context(), email) {
				mxFailed = true
				m.reject(w, r, http.StatusBadRequest, FailureNoMXRecord, ip, email, nil)
				return
			}
		}

		// 5) Risk scoring. Bounce when above threshold.
		ipVelocity := 0
		if m.IPLimiter != nil {
			// We don't expose per-IP read on the limiter today;
			// approximate "velocity" with the count of recent buckets.
			// The middleware's calling pattern (one Allow per request)
			// makes the per-IP token deficit a usable proxy in
			// follow-up phases. For now, leave at 0 — disposable + MX
			// + UA already saturate the score on the typical bot.
			ipVelocity = 0
		}
		hourOfDay := time.Now().UTC().Hour()
		scoreResult := Score(ScoreInput{
			Email:               email,
			SourceIP:            ip,
			UserAgent:           r.Header.Get("User-Agent"),
			DisposableHit:       disposableHit, // already returned above, included for completeness
			MXFailed:            mxFailed,
			TurnstileBorderline: turnstileBorderline,
			IPVelocity:          ipVelocity,
			HourOfDay:           hourOfDay,
		})

		threshold := m.Threshold
		if threshold <= 0 {
			threshold = identity.DefaultRiskThreshold
		}
		if scoreResult.Score >= threshold {
			detail := map[string]any{
				"score":     scoreResult.Score,
				"signals":   scoreResult.Signals,
				"threshold": threshold,
			}
			m.reject(w, r, http.StatusBadRequest, FailureRiskThresholdExceeded, ip, email, detail)
			return
		}

		// All checks passed. Hand off to the underlying handler.
		next.ServeHTTP(w, r)
	})
}

// shouldGate returns true when the request is a POST /auth/magic-link
// request that isn't on the SkipPaths list.
func (m *Middleware) shouldGate(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	if r.URL == nil || r.URL.Path != magicLinkPath {
		return false
	}
	if _, skip := m.SkipPaths[r.URL.Path]; skip {
		return false
	}
	return true
}

// reject writes the standard JSON error envelope, sets a generic 400
// (or whatever status the caller passed), and emits the audit event.
// Email may be empty when we couldn't extract it from the body.
func (m *Middleware) reject(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	reason string,
	ip string,
	email string,
	detail map[string]any,
) {
	errorId := generateErrorId()
	if detail == nil {
		detail = map[string]any{}
	}
	detail["errorId"] = errorId
	detail["reason"] = reason

	if m.Audit != nil {
		ev := identity.AuditEvent{
			Category:      identity.AuditCategoryAuth,
			Action:        "magic_link_blocked",
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: reason,
			TargetEmail:   email,
			SourceIP:      ip,
			UserAgent:     r.Header.Get("User-Agent"),
			Detail:        detail,
		}
		m.Audit.Log(r.Context(), ev)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   "request_blocked",
		"errorId": errorId,
		"message": "Your request couldn't be processed.",
	})
}

func (m *Middleware) logDebug(msg string, attrs ...any) {
	if m == nil || m.Logger == nil {
		return
	}
	m.Logger.Debug(msg, attrs...)
}

func (m *Middleware) logWarn(msg string, attrs ...any) {
	if m == nil || m.Logger == nil {
		return
	}
	m.Logger.Warn(msg, attrs...)
}

// generateErrorId returns a stable short id callers can reference in
// support tickets. Format: ERR-XXXXXX (6 hex chars). Mirrors the
// helper in component/identity/http/server.go so the abuse rejection
// surface looks identical to the rest of identity.
func generateErrorId() string {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "ERR-000000"
	}
	return "ERR-" + hex.EncodeToString(buf)
}

// clientIP pulls the originating IP off the request, honoring common
// proxy headers when set. Best-effort — we don't trust the headers
// for security decisions, only for audit metadata + per-IP bucketing.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// First entry in the comma-separated list is the original
		// client.
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}
