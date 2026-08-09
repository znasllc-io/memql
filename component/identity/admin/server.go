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

// AdminServer hosts what is left of the /admin/* operator surface: the
// sign-in pages and /admin/deployments. Mounted onto the identity binary's mux
// via the SetAdminMounter setter on component/identity.Service.
//
// It hosted seven screens. Six moved into the memQL portal in memql#3324 --
// writes and owner/admin gate together, the gate landing in
// component/identity/adminops and reached over MemqlService.Stream rather than
// over an HTTP route. Deployments did not, for a topology reason:
// DeployControlService runs shell scripts against an on-disk overlay checkout,
// so it exists only on this node, while the portal is served by the bff and
// dials the origin that served it. Retiring this page would have deleted a
// working capability rather than moved it.
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
//   - Audit   -- audit logger; Phase 6 emits admin_* events for every
//     operator action.
//   - Settings-- LiveSettings reader so brand fields render the same
//     values the public web pages use. Optional; falls back
//     to the immutable Config.
//   - WebServer-- handle to the public web server; used to share the
//     LayoutData builder so admin pages render through the
//     same templ Layout component as the public pages.
//   - Logger  -- slog logger; component label "identity-admin" added
//     during Mount.
type AdminServer struct {
	Cfg       identity.Config
	Engine    identity.EngineExecutor
	Issuer    *identity.JWTIssuer
	Audit     identity.AuditLogger
	Settings  identityweb.LiveSettings
	WebServer *identityweb.Server
	Logger    *slog.Logger

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

// The one page this app still serves.
//
// It served seven. Six of them -- the dashboard, users, tokens, audit, JWKS
// and settings -- moved into the memQL portal in memql#3324, along with every
// write they carried and the owner/admin gate that protected it
// (component/identity/adminops). Deployments did not, and the reason is
// topology rather than effort: DeployControlService runs shell scripts against
// an on-disk overlay checkout, so it exists ONLY on the identity node, while
// the portal is served by the bff and dials the origin that served it. A
// bff-served portal reaching for the deploy surface gets UNIMPLEMENTED. Until
// that RPC can cross the mesh, retiring this page would delete a working
// capability instead of moving it.
//
// So this is what remains: /admin/login to establish a session, and
// /admin/deployments. Everything else under /admin is gone -- the routes with
// the pages, in the same commit, per the repo's no-stale-code convention.
var adminNav = []webtempl.NavLink{
	{Href: "/admin/deployments", Label: "Deployments"},
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
	mux.HandleFunc("GET /admin/{$}", gated(s.handleRoot))

	mux.HandleFunc("GET /admin/deployments", gated(s.handleDeploymentsGet))
	mux.HandleFunc("POST /admin/deployments/deploy-staging", gated(s.handleDeployStagingPost))
	mux.HandleFunc("POST /admin/deployments/promote", gated(s.handleDeployPromotePost))
	mux.HandleFunc("POST /admin/deployments/rollback", gated(s.handleDeployRollbackPost))
	mux.HandleFunc("POST /admin/deployments/rollout", gated(s.handleDeployRolloutPost))

	// 6 gated + 3 session-establishment. Hand-maintained, and it had already
	// drifted once (it read 19 while Mount registered 27), so
	// route_gate_test.go pins it against the routes actually registered.
	s.Logger.Info("admin web routes mounted",
		slog.String("base_url", s.Cfg.BaseURL),
		slog.Int("routes", 9),
	)
}

// layoutData builds the standard webtempl.LayoutData for an admin
// page, with the admin nav and brand snapshot. `noNav` suppresses the
// nav (used on /admin/login before the operator authenticates).
//
// extraScripts forwards page-specific Stimulus controller files. No
// surviving page uses one -- the settings colour picker / logo cropper went
// with the settings page in memql#3324 -- but the parameter stays because it
// is the layout's contract, not this console's.
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
