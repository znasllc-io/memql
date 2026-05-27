// Package http hosts the HTTP handlers identity binaries serve. The
// package is named generically so callers import it as
// httpidentity "github.com/znasllc-io/memql/component/identity/http".
//
// Routes mounted by Server.Mount:
//
//	POST /auth/magic-link  -- issue a magic link (or access request)
//	GET  /auth/complete    -- consume a magic link, redirect to client
//	POST /oauth/token      -- redeem auth code for tokens
//	POST /auth/refresh     -- rotate refresh token
//	POST /auth/logout      -- revoke current session
//	POST /pair/codes       -- mint a worker-pairing code (Bearer auth)
//	POST /pair/redeem      -- redeem a code for a worker token (Pair auth)
//
// /.well-known/jwks.json is mounted by component/identity.Service
// directly (Phase 1 wiring); Server only owns the auth flow.
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	"github.com/znasllc-io/memql/component/identity/nodetoken"
	"github.com/znasllc-io/memql/component/identity/refresh"
)

// Server bundles the dependencies the handlers need. Constructed once
// in app/integrations_identity.go and shared across requests; safe for
// concurrent use because every dependency is concurrency-safe.
type Server struct {
	Cfg        identity.Config
	Store      *identity.Store
	Issuer     *identity.JWTIssuer
	MLIssuer   *magiclink.Issuer
	MLVerifier *magiclink.Verifier
	Rotator    *refresh.Rotator
	Audit      identity.AuditLogger
	Logger     *slog.Logger

	// Abuse is the anti-abuse middleware stack (Phase 4). When
	// non-nil, it wraps POST /auth/magic-link and emits audit events
	// for blocked requests. Nil keeps the legacy "no abuse layer"
	// behavior — useful for tests and for code paths that already
	// have a higher-level limiter.
	Abuse *abuse.Middleware

	// LiveTokenSettings is the runtime-tunable knob hook. When
	// non-nil, the handlers consult it on every issuance to pick
	// up admin-edited TTLs and cookie SameSite policy without an
	// identity restart. Nil falls back to s.Cfg + env-derived
	// defaults — the legacy boot-time-only behaviour.
	//
	// Returns the currently effective values by reading the
	// singleton clusterSettings row, with field-level fallbacks
	// (a row with accessTokenTTLSeconds=0 uses the env / built-in
	// default; a row missing the field entirely behaves the same).
	// See identity.LiveTokenSettings for the contract.
	LiveTokenSettings func(ctx context.Context) identity.LiveTokenSettings

	// NodeTokenStore wires the v1:identity:identity row-persistence
	// side of the /node/bootstrap path (memql#343). When non-nil, every
	// bootstrap call looks up the canonical row id, creates on miss /
	// updates on hit, and rejects when the row carries a non-empty
	// revokedAt. When nil, the bootstrap path falls back to the
	// pre-#343 synthetic-IdentityId behaviour -- the verifier still
	// trusts the JWT but operators have no visibility / revocability.
	// Tests and minimal deployments can leave this nil.
	NodeTokenStore *nodetoken.Store
}

// effectiveTokenSettings returns the live TTL + cookie settings for
// this request, merged with config-side fallbacks. Boot-time s.Cfg
// values backstop every field so a nil hook (or a freshly bootstrapped
// cluster with no admin row yet) still produces a usable result.
func (s *Server) effectiveTokenSettings(ctx context.Context) identity.LiveTokenSettings {
	out := identity.LiveTokenSettings{
		AccessTokenTTL:  s.Cfg.AccessTokenTTL,
		RefreshTokenTTL: s.Cfg.RefreshTokenTTL,
		MagicLinkTTL:    s.Cfg.MagicLinkTTL,
		InvitationTTL:   s.Cfg.InvitationTTL,
		// SameSite empty here means "let the cookie helper read the
		// env directly" -- preserves the old behaviour when there's
		// no live row.
		RefreshCookieSameSite: "",
	}
	if s.LiveTokenSettings == nil {
		return out
	}
	live := s.LiveTokenSettings(ctx)
	if live.AccessTokenTTL > 0 {
		out.AccessTokenTTL = live.AccessTokenTTL
	}
	if live.RefreshTokenTTL > 0 {
		out.RefreshTokenTTL = live.RefreshTokenTTL
	}
	if live.MagicLinkTTL > 0 {
		out.MagicLinkTTL = live.MagicLinkTTL
	}
	if live.InvitationTTL > 0 {
		out.InvitationTTL = live.InvitationTTL
	}
	if live.RefreshCookieSameSite != "" {
		out.RefreshCookieSameSite = live.RefreshCookieSameSite
	}
	return out
}

// Mount registers the auth-flow routes on the supplied mux.
//
// Every handler is wrapped in the security-headers middleware so the
// JSON-API surface always carries HSTS / nosniff / DENY-frame /
// strict-origin-when-cross-origin / restrictive Permissions-Policy,
// matching the baseline the web UI sets via web.SecurityHeaders*.
//
// POST /auth/magic-link is additionally wrapped in s.Abuse when one
// is configured: per-IP rate limit, Cloudflare Turnstile verification,
// disposable-email blocklist, MX-record validation, and risk scoring.
// See component/identity/abuse for the per-defense breakdown.
func (s *Server) Mount(mux *http.ServeMux) {
	if s == nil || mux == nil {
		return
	}

	// SystemActor stamps "system:identity-svc" when no upstream
	// auth has attached a TokenInfo. Identity's API surface (magic
	// link issue, magic link consume, token exchange, refresh,
	// logout) is unauthenticated by design -- those endpoints
	// EXIST to bootstrap a session, so the engine's actor-required
	// check needs a system fallback. See identity/middleware.go.
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return identity.SystemActorHandlerFunc(abuse.SecurityHeadersHandlerFunc(h))
	}
	wrapHandler := func(h http.Handler) http.Handler {
		return identity.SystemActorMiddleware(abuse.SecurityHeadersMiddleware(h))
	}

	magicLink := http.HandlerFunc(s.cors(s.handleMagicLink))
	var magicLinkHandler http.Handler = magicLink
	if s.Abuse != nil {
		magicLinkHandler = s.Abuse.Wrap(magicLink)
	}

	mux.Handle("POST /auth/magic-link", wrapHandler(magicLinkHandler))
	mux.HandleFunc("OPTIONS /auth/magic-link", wrap(s.cors(s.handleOptions)))
	mux.HandleFunc("GET /auth/complete", wrap(s.handleComplete))
	mux.HandleFunc("POST /oauth/token", wrap(s.cors(s.handleToken)))
	mux.HandleFunc("OPTIONS /oauth/token", wrap(s.cors(s.handleOptions)))
	mux.HandleFunc("POST /auth/refresh", wrap(s.cors(s.handleRefresh)))
	mux.HandleFunc("OPTIONS /auth/refresh", wrap(s.cors(s.handleOptions)))
	mux.HandleFunc("POST /auth/logout", wrap(s.cors(s.handleLogout)))
	mux.HandleFunc("OPTIONS /auth/logout", wrap(s.cors(s.handleOptions)))

	// Worker pairing -- mint + redeem worker pairing codes.
	// /pair/codes is authenticated (Bearer JWT); /pair/redeem
	// authenticates via the pair code itself (Authorization: Pair
	// <code>). Both return JSON.
	mux.HandleFunc("POST /pair/codes", wrap(s.cors(s.handlePairCreate)))
	mux.HandleFunc("OPTIONS /pair/codes", wrap(s.cors(s.handleOptions)))
	mux.HandleFunc("POST /pair/redeem", wrap(s.cors(s.handlePairRedeem)))
	mux.HandleFunc("OPTIONS /pair/redeem", wrap(s.cors(s.handleOptions)))

	// Node bootstrap -- self-mint a class="node" JWT for cluster
	// binaries (bff / agent / cognition / planner / voice) that
	// boot with MEMQL_NODE_TOKEN empty. Authenticates via
	// Authorization: Bootstrap <secret>. Disabled by default;
	// enabled when MEMQL_NODE_BOOTSTRAP_TOKEN is set on the
	// identity service. memql#338.
	mux.HandleFunc("POST /node/bootstrap", wrap(s.cors(s.handleNodeBootstrap)))
	mux.HandleFunc("OPTIONS /node/bootstrap", wrap(s.cors(s.handleOptions)))

	if s.Logger != nil {
		s.Logger.Info("identity HTTP routes mounted",
			slog.String("base_url", s.Cfg.BaseURL),
			slog.Bool("abuse_layer", s.Abuse != nil),
		)
	}
}

// generateErrorId returns a stable short id callers can reference in
// support tickets. Format: ERR-XXXXXX (6 hex chars).
func generateErrorId() string {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "ERR-000000"
	}
	return "ERR-" + hex.EncodeToString(buf)
}

// audit nil-guards the audit logger so handlers stay readable.
func (s *Server) audit(r *http.Request, ev identity.AuditEvent) {
	if s == nil || s.Audit == nil {
		return
	}
	if r != nil {
		if ev.SourceIP == "" {
			ev.SourceIP = clientIP(r)
		}
		if ev.UserAgent == "" {
			ev.UserAgent = r.Header.Get("User-Agent")
		}
	}
	s.Audit.Log(r.Context(), ev)
}

// clientIP pulls the originating IP off the request, honoring common
// proxy headers when set. Best-effort — we don't trust the headers
// for security decisions, only for audit metadata.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// First entry in the comma-separated list is the original
		// client.
		for i := 0; i < len(v); i++ {
			if v[i] == ',' {
				return trim(v[:i])
			}
		}
		return trim(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return trim(v)
	}
	return r.RemoteAddr
}

// trim is a tiny helper to avoid pulling in strings just for
// TrimSpace in this leaf.
func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
