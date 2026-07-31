package admin

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/znasllc-io/memql/component/identity"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// AdminServer hosts the /admin/* operator surface. Mounted onto the
// identity binary's mux via the SetAdminMounter setter on
// component/identity.Service.
//
// All dependencies are required:
//
//   - Cfg     -- the immutable identity Config snapshot. Drives brand
//     fallbacks and CORS rules. Live overrides come through
//     Settings.
//   - Engine  -- memql engine for queries / mutations against the
//     v1:identity:* concept tree.
//   - Issuer  -- JWT issuer used to validate the admin's access token
//     on every request.
//   - Keys    -- key manager, exposed for the JWKS rotation page.
//   - Audit   -- audit logger; Phase 6 emits admin_* events for every
//     operator action.
//   - Settings-- LiveSettings reader so brand fields render the same
//     values the public web pages use. Optional; falls back
//     to the immutable Config.
//   - RotateNow- callback wired by the integration layer that triggers
//     an immediate key rotation. Kept as a callback so the
//     admin package doesn't depend on the higher-level
//     Service struct.
//   - WebServer-- handle to the public web server; used to share the
//     LayoutData builder so admin pages render through the
//     same templ Layout component as the public pages.
//   - Logger  -- slog logger; component label "identity-admin" added
//     during Mount.
type AdminServer struct {
	Cfg       identity.Config
	Engine    identity.EngineExecutor
	Issuer    *identity.JWTIssuer
	Keys      *identity.KeyManager
	Audit     identity.AuditLogger
	Settings  identityweb.LiveSettings
	WebServer *identityweb.Server
	RotateNow func(r *http.Request) error
	Logger    *slog.Logger

	// Phase 7: PAT management. Wired by SetPATAdapter from
	// app/integrations_identity.go after the engine is up.
	patAdapter PATAdapter

	// memql#350: node-token management on the same /admin/tokens
	// page. Wired by SetNodeTokenAdapter from
	// app/integrations_identity.go after the engine is up. Nil-
	// safe: the page degrades to "PATs only" when this isn't wired
	// (e.g. an identity binary built without the node-token surface).
	nodeTokenAdapter NodeTokenAdapter

	// memql#726: deployment-status reader backing the read-only
	// /admin/deployments view. Wired by SetDeployControlReader from
	// app/integrations_deploy_control.go after the deploy-control
	// service is up. Nil-safe: the page renders an error flash when
	// unwired (e.g. an identity binary without the deploy-control
	// surface).
	deployReader DeployControlReader

	// memql#727: deployment write-action port backing the
	// POST /admin/deployments/* handlers (deploy-staging, promote,
	// rollback, rollout). Wired by SetDeployControlActions from
	// app/integrations_deploy_control.go after the deploy-control
	// service is up. Nil-safe: the handlers reject with an error flash
	// when unwired.
	deployActions DeployControlActions
}

// Built-in nav links rendered in the layout header on every admin
// page. Source-of-truth for both the dashboard pages and the
// placeholder pages.
var adminNav = []webtempl.NavLink{
	{Href: "/admin/", Label: "Overview"},
	{Href: "/admin/users", Label: "Users"},
	{Href: "/admin/tokens", Label: "Tokens"},
	{Href: "/admin/audit", Label: "Audit"},
	{Href: "/admin/deployments", Label: "Deployments"},
	{Href: "/admin/jwks", Label: "JWKS"},
	{Href: "/admin/settings", Label: "Settings"},
}

// New validates dependencies and returns a ready-to-mount AdminServer.
// Returns a typed error when a required dependency is missing so the
// wiring layer can fail fast at boot.
func New(s *AdminServer) (*AdminServer, error) {
	if s == nil {
		return nil, fmt.Errorf("admin: nil AdminServer")
	}
	if s.Logger == nil {
		return nil, fmt.Errorf("admin: Logger required")
	}
	if s.Issuer == nil {
		return nil, fmt.Errorf("admin: Issuer required for JWT validation")
	}
	if s.Engine == nil {
		return nil, fmt.Errorf("admin: Engine required")
	}
	if s.Keys == nil {
		return nil, fmt.Errorf("admin: Keys required for JWKS page")
	}
	if s.WebServer == nil {
		return nil, fmt.Errorf("admin: WebServer required for shared layout")
	}
	s.Logger = s.Logger.With(slog.String("component", "identity-admin"))
	return s, nil
}

// Mount registers the /admin/* routes on the supplied mux. Safe for
// nil receiver (no-op) so the wiring layer can defer construction.
func (s *AdminServer) Mount(mux *http.ServeMux) {
	if s == nil || mux == nil {
		return
	}

	// CSRF middleware (memql#111). See the matching block in
	// component/identity/web/server.go for the design notes; the
	// admin chain shares the cookie name + same exemptions plus
	// /admin/establish, which is the unauthenticated POST that
	// completes the magic-link admin-session bootstrap (no prior
	// session, no opportunity to embed the form token until AFTER
	// the cookie lands).
	secure := strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Cfg.BaseURL)), "https://")
	csrf := identityweb.CSRFMiddlewareFunc(identityweb.CSRFOptions{
		Secure: &secure,
		ExemptPaths: []string{
			"/admin/establish",
		},
		Logger: s.Logger,
	})
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return identityweb.SecurityHeadersHandlerFunc(identityweb.CSPHandlerFunc(csrf(h)))
	}
	gated := func(h http.HandlerFunc) http.HandlerFunc {
		return wrap(s.requireAdmin(h))
	}

	// Auth-establishment routes (no admin role required to render).
	mux.HandleFunc("GET /admin/login", wrap(s.handleLoginGet))
	mux.HandleFunc("POST /admin/establish", wrap(s.handleEstablish))
	mux.HandleFunc("POST /admin/logout", wrap(s.handleLogout))

	// Gated pages.
	mux.HandleFunc("GET /admin/{$}", gated(s.handleDashboard))
	mux.HandleFunc("GET /admin/users", gated(s.handleUsersList))
	mux.HandleFunc("GET /admin/users/detail", gated(s.handleUsersDetail))
	mux.HandleFunc("POST /admin/users/profile", gated(s.handleEditUserProfile))
	mux.HandleFunc("POST /admin/users/role", gated(s.handleChangeUserRole))
	mux.HandleFunc("POST /admin/users/suspend", gated(s.handleSuspendUser))
	mux.HandleFunc("POST /admin/users/unsuspend", gated(s.handleUnsuspendUser))

	mux.HandleFunc("GET /admin/audit", gated(s.handleAuditList))

	mux.HandleFunc("GET /admin/deployments", gated(s.handleDeploymentsGet))
	mux.HandleFunc("POST /admin/deployments/deploy-staging", gated(s.handleDeployStagingPost))
	mux.HandleFunc("POST /admin/deployments/promote", gated(s.handleDeployPromotePost))
	mux.HandleFunc("POST /admin/deployments/rollback", gated(s.handleDeployRollbackPost))
	mux.HandleFunc("POST /admin/deployments/rollout", gated(s.handleDeployRolloutPost))

	mux.HandleFunc("GET /admin/tokens", gated(s.handleTokensList))
	mux.HandleFunc("POST /admin/tokens/revoke", gated(s.handleTokensRevoke))
	mux.HandleFunc("POST /admin/tokens/node/revoke", gated(s.handleNodeTokensRevoke))

	mux.HandleFunc("GET /admin/jwks", gated(s.handleJWKS))
	mux.HandleFunc("POST /admin/jwks/rotate", gated(s.handleJWKSRotate))

	mux.HandleFunc("GET /admin/settings", gated(s.handleSettingsGet))
	mux.HandleFunc("POST /admin/settings", gated(s.handleSettingsPost))

	// Deferred surfaces. Placeholder page so navigation doesn't 404
	// during the in-between window before a per-surface implementation
	// lands. Each route shares the same handler.
	mux.HandleFunc("GET /admin/sessions", gated(s.handlePlaceholder("Sessions",
		"List active sessions across users with per-row revoke. Tracked for a follow-up commit.")))
	mux.HandleFunc("GET /admin/invitations", gated(s.handlePlaceholder("Invitations",
		"Issue + manage admin invitations for invite-only mode. Tracked for a follow-up commit.")))
	mux.HandleFunc("GET /admin/partition-grants", gated(s.handlePlaceholder("Partition grants",
		"View + edit per-(user, partition) access grants. Tracked for a follow-up commit.")))
	mux.HandleFunc("GET /admin/access-requests", gated(s.handlePlaceholder("Access requests",
		"Review the waitlist queue: approve, reject, or defer. Tracked for a follow-up commit.")))

	// 24 gated + 3 session-establishment. Hand-maintained and it had already
	// drifted -- this read 19 while Mount registered 27, so anyone using the
	// log line to confirm a route landed was misled. route_gate_test.go now
	// pins it against the routes actually registered.
	s.Logger.Info("admin web routes mounted",
		slog.String("base_url", s.Cfg.BaseURL),
		slog.Int("routes", 27),
	)
}

// layoutData builds the standard webtempl.LayoutData for an admin
// page, with the admin nav and brand snapshot. `noNav` suppresses the
// nav (used on /admin/login before the operator authenticates).
//
// extraScripts forwards page-specific Stimulus controller files (e.g.
// /static/admin-settings-branding.js for the Settings page color
// picker + logo cropper). Callers that don't need scripts pass nil.
func (s *AdminServer) layoutData(r *http.Request, title string, noNav bool, extraScripts ...string) webtempl.LayoutData {
	nav := adminNav
	if noNav {
		nav = nil
	}
	var scripts []string
	if len(extraScripts) > 0 {
		scripts = extraScripts
	}
	return s.WebServer.LayoutData(r, title, false, nav, scripts)
}

// readFlash pulls a one-shot flash message off the query string. Uses
// the same convention as the public web layer so existing redirects
// can route through `?flash=...&flash_kind=...`.
func readFlash(r *http.Request) *webtempl.Flash {
	if r == nil {
		return nil
	}
	msg := strings.TrimSpace(r.URL.Query().Get("flash"))
	if msg == "" {
		return nil
	}
	kind := strings.TrimSpace(r.URL.Query().Get("flash_kind"))
	if kind == "" {
		kind = "info"
	}
	return &webtempl.Flash{Kind: kind, Message: msg}
}

// modeSnapshot returns the live registration mode for an admin page.
// Used by the dashboard to display "Registration mode" without an
// extra DB call.
func (s *AdminServer) modeSnapshot(r *http.Request) string {
	mode := string(s.Cfg.RegistrationMode)
	if s.Settings != nil {
		live := s.Settings.Snapshot(r.Context())
		if live.RegistrationMode != "" {
			mode = string(live.RegistrationMode)
		}
	}
	return mode
}

// render writes the cache-control + content-type headers and renders
// the supplied templ.Component. Mirrors identityweb.Server.render but
// scoped to the admin logger.
func (s *AdminServer) render(w http.ResponseWriter, r *http.Request, name string, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	if err := c.Render(r.Context(), w); err != nil {
		s.Logger.Error("admin: render failed", "name", name, "error", err)
	}
}
