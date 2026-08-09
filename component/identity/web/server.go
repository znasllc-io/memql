// Package web hosts the public-facing identity web app.
//
// The package owns the rendered HTML pages — login form, magic-link
// landing, logout splash, error page, first-run wizard, legal docs,
// and the per-user /me/* dashboard. The API endpoints live in the
// sibling component/identity/http package; this package's handlers
// post to those endpoints (or to themselves) over the same origin.
//
// Pages are rendered through templ-generated components in
// component/identity/web/templ. The handler builds a typed Data
// struct and calls Component.Render(ctx, w). All assets are
// embedded via embed.go's FS variable. CSP is enforced by csp.go.
// SetupSeedFunc / IssueMagicLinkFunc are dependency-injection seams
// the wiring layer fills in.
package web

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// LiveSettings is the read-side of the runtime-editable cluster
// settings. Implemented by the wiring layer with a memql query;
// abstracted here so the web package doesn't import the engine.
//
// All fields are best-effort — when a row hasn't been seeded yet,
// implementations should fall back to env-derived defaults so the
// page still renders something usable.
type LiveSettings interface {
	Snapshot(ctx context.Context) Settings
}

// Settings is the read-only snapshot rendered into templates.
type Settings struct {
	BrandName         string
	BrandPrimaryColor string
	BrandLogoDataURI  string
	BrandIconDataURI  string
	RegistrationMode  identity.RegistrationMode
	// RegistrationDomains comma-joined for the help-text rendering on
	// the login page (when RegistrationMode=domain_restricted).
	RegistrationDomains string
}

// IssueMagicLinkFunc adapts the web handler's POST /login form
// submission to the magiclink package without importing it directly
// (which would create a dependency cycle in some refactor scenarios).
//
// Implementation is provided by the wiring layer.
type IssueMagicLinkFunc func(ctx context.Context, in IssueMagicLinkInput) error

// IssueMagicLinkInput mirrors the magiclink.IssueInput surface but
// without importing the magiclink package.
type IssueMagicLinkInput struct {
	Email           string
	ClientId        string
	RedirectURI     string
	State           string
	SourceIP        string
	UserAgent       string
	InvitationId    string
	WaitlistName    string
	WaitlistContext string
	IsAccessRequest bool

	// CodeChallenge / CodeChallengeMethod carry the OAuth 2.1 PKCE
	// challenge (RFC 7636) from the /authorize browser flow through to
	// the magic-link issuer, which stamps them onto the magic-link row's
	// OAuth context so /auth/complete mints a PKCE-bound auth code. The
	// /oauth/token exchange then requires the matching verifier. Empty
	// for the non-OAuth admin-session login path.
	CodeChallenge       string
	CodeChallengeMethod string

	// Bootstrap=true marks the call as the /setup wizard's owner-mint
	// path. Routed straight to the magic-link issuer with the same flag,
	// which bypasses the bootstrap-state gate AND the registration-mode
	// gate. Only the wizard's POST handler sets this; nothing else in
	// the web package should propagate it from a request body.
	Bootstrap bool

	// ExistingUser=true tells the issuer that routeLoginEmail's
	// existing-user check already passed, so the registration-mode
	// gate (waitlist / invite_only / domain_restricted) should be
	// skipped — those modes apply only to new sign-ups, never to
	// returning users.
	ExistingUser bool

	// AdminSession=true means /login was hit without a relying-party
	// in scope (no return_to / no client query). The verifier should
	// treat the click as an identity-admin sign-in: skip the OAuth
	// code flow, mint an access token + memql_admin cookie, redirect
	// to /admin/. Set by handleLoginPost when pickOAuthCtx couldn't
	// match the request to a registered client.
	AdminSession bool
}

// CountUsersFunc returns the number of cluster users. Drives the
// /setup wizard's 404-when-bootstrapped behavior.
type CountUsersFunc func(ctx context.Context) (int, error)

// UserExistsByEmailFunc returns true when a v1:identity:user row
// exists for the given primary email. Drives the /login flow's
// "is this a returning user or a new registration?" branch — the
// page renders the same email-only form for both, then the POST
// handler picks the right path based on this lookup.
//
// Callers MUST treat any error as "unknown" and fall through to
// the registration-mode-default path. The web layer never reveals
// the answer to the user (anti-enumeration).
type UserExistsByEmailFunc func(ctx context.Context, email string) (bool, error)

// PersistClusterSettingsFunc lands the wizard's submitted settings
// into v1:identity:clusterSettings (id="cluster"). The wiring layer
// supplies the implementation; web stays engine-free.
type PersistClusterSettingsFunc func(ctx context.Context, in ClusterSettingsInput) error

// ClusterSettingsInput is the wizard's submitted bundle.
type ClusterSettingsInput struct {
	Domain                    string
	OwnerEmail                string
	OwnerFirstName            string
	OwnerLastName             string
	OwnerPhone                string
	OwnerPrimaryRole          string
	OwnerGender               string
	OwnerBirthdate            string
	BrandName                 string
	RegistrationMode          string
	RegistrationDomains       string
	InternalDomains           string
	InternalDefaultRole       string
	AccessRequestNotifyEmails string
}

// Server bundles the handlers and dependency adapters. Constructed
// once at bootstrap and shared across requests.
type Server struct {
	Cfg                    identity.Config
	Live                   LiveSettings
	Logger                 *slog.Logger
	IssueMagicLink         IssueMagicLinkFunc
	CountUsers             CountUsersFunc
	UserExistsByEmail      UserExistsByEmailFunc
	PersistClusterSettings PersistClusterSettingsFunc

	// Store backs the DB-side OAuth client resolution on /authorize.
	// When non-nil, a client_id not present in the static
	// MEMQL_IDENTITY_REGISTERED_CLIENTS slice falls back to the
	// dynamically-registered (RFC 7591) v1:identity:oauthClient rows.
	// Nil keeps the static-only behaviour (tests / engine-less binaries).
	Store *identity.Store

	// Phase 7: PAT management for the /me/tokens page. Wired by the
	// integration layer once the engine is up. Nil leaves the routes
	// unmounted so binaries without the engine don't 500 on the page.
	meTokens *MeTokens

	// deviceFlow backs GET/POST /device, the RFC 8628 verification page
	// (memql#3410). Wired by the integration layer once the engine is
	// up; nil leaves the routes unmounted, same as meTokens.
	deviceFlow *DeviceFlow

	// deviceVerifyLimiterVal is the per-IP limiter on /device, lazily
	// built on first use. See verifyLimiter in device.go for why the
	// page needs one at all.
	deviceVerifyLimiterVal  *abuse.IPRateLimiter
	deviceVerifyLimiterOnce sync.Once

	// SSO auth-code minter -- when wired, signed-in users hitting
	// /login?return_to=<registered-client> get a fresh auth code
	// minted from their existing session and bounced straight to
	// the relying-party callback. Nil leaves the redirectIf-
	// Authenticated middleware in "fall through to login form"
	// mode for return_to flows.
	mintSSOAuthCode MintSSOAuthCodeFunc

	// Asset versioning. Computed once at boot from the embedded FS.
	// The webtempl layout pulls versioned URLs through LayoutData.Asset
	// (a closure over s.assetURL), and handlers run paths through
	// assetURL when they construct URLs in Go (e.g. ExtraScripts).
	assetVersions map[string]string

	tosBytes       []byte
	tosVersion     string
	privacyBytes   []byte
	privacyVersion string
}

// NewServer loads legal docs (with override-path support) and returns
// a ready-to-mount Server. Templates are compiled into the binary by
// the templ generator; nothing is parsed at runtime.
func NewServer(cfg identity.Config, logger *slog.Logger, live LiveSettings) (*Server, error) {
	if logger == nil {
		return nil, fmt.Errorf("identity/web: logger required")
	}
	versions, err := computeAssetVersions(FS, "static")
	if err != nil {
		return nil, fmt.Errorf("identity/web: compute asset versions: %w", err)
	}
	s := &Server{
		Cfg:           cfg,
		Live:          live,
		Logger:        logger.With(slog.String("component", "identity-web")),
		assetVersions: versions,
	}
	tos, tosVer, err := loadLegalDoc(cfg.TosOverridePath, "legal/tos.md")
	if err != nil {
		return nil, fmt.Errorf("identity/web: load TOS: %w", err)
	}
	priv, privVer, err := loadLegalDoc(cfg.PrivacyOverridePath, "legal/privacy.md")
	if err != nil {
		return nil, fmt.Errorf("identity/web: load Privacy: %w", err)
	}
	s.tosBytes = tos
	s.tosVersion = tosVer
	s.privacyBytes = priv
	s.privacyVersion = privVer
	return s, nil
}

// Mount registers all web-UI routes on the supplied mux. Wraps
// every handler in CSP + security-headers middleware.
func (s *Server) Mount(mux *http.ServeMux) {
	if s == nil || mux == nil {
		return
	}

	// Static assets — served straight from the embed FS, with the
	// long-lived immutable Cache-Control header. Safe because the URL
	// templates emit carries a content hash (?v=<hex>); a deploy that
	// changes a file changes its hash and therefore its URL, so the
	// browser is guaranteed to fetch fresh bytes.
	staticFS, err := fs.Sub(FS, "static")
	if err == nil {
		mux.Handle("GET /static/", staticCacheHeaders(http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))))
	}

	// CSRF middleware (memql#111). Double-submit-cookie pattern:
	// every GET / HEAD / OPTIONS gets a memql_csrf cookie minted
	// (or carries an existing one through); every POST / PUT /
	// PATCH / DELETE on a non-exempt path must echo the cookie
	// value back via the `_csrf` form field or `X-CSRF-Token`
	// header. The /login and /setup POSTs are explicitly exempted
	// because they're unauthenticated entry points -- a CSRF on
	// /login mints a magic link to an attacker-chosen email which
	// the attacker can't read; SameSite=Lax on the auth cookie
	// blocks cross-site cookie attachment on those paths too.
	secure := s.cookieSecure()
	csrf := CSRFMiddlewareFunc(CSRFOptions{
		Secure: &secure,
		ExemptPaths: []string{
			"/login",
			"/setup",
			// /auth/landing is the cross-browser magic-link
			// confirmation POST -- unauthenticated; same trust
			// profile as /login. SameSite=Lax on memql_auth blocks
			// cross-site cookie attachment.
			"/auth/landing",
		},
		Logger: s.Logger,
	})

	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		// SystemActorHandlerFunc stamps "system:identity-svc" when
		// no upstream auth has attached a TokenInfo. Identity's
		// HTTP surface is an unauthenticated entry point by design,
		// so without this middleware every mutation downstream
		// fails the engine's actor-required check. See
		// component/identity/middleware.go for the full rationale.
		//
		// CSRF wraps innermost so the cookie minted on a fresh GET
		// is set BEFORE the security-headers / CSP layer writes
		// other headers; ordering is otherwise irrelevant.
		return identity.SystemActorHandlerFunc(SecurityHeadersHandlerFunc(CSPHandlerFunc(csrf(h))))
	}

	// Pre-auth GET surfaces wrap with redirectIfAuthenticated. If
	// the caller already has a valid memql_admin cookie, the
	// browser is bounced to /admin/ instead of being asked to
	// re-authenticate. The middleware no-ops on requests that
	// carry a return_to (relying-party flow) so SPA sign-in
	// callbacks still see the form. Add new pre-auth GETs here
	// rather than open-coding "if signed in, redirect" inside the
	// handler.
	preAuth := func(h http.HandlerFunc) http.HandlerFunc {
		return s.redirectIfAuthenticated("/admin/", h)
	}
	mux.HandleFunc("GET /{$}", wrap(preAuth(s.handleRoot)))
	mux.HandleFunc("GET /login", wrap(preAuth(s.handleLoginGet)))
	mux.HandleFunc("POST /login", wrap(s.handleLoginPost))
	// OAuth 2.1 authorization endpoint (advertised by the RFC 8414
	// metadata document). A browser-based OAuth client opens this; it
	// validates the code-flow params and funnels into the magic-link
	// login carrying the PKCE-bound OAuth context.
	//
	// Wrapped with preAuth (redirectIfAuthenticated) so an ALREADY-
	// signed-in user (e.g. the cluster owner who just claimed ownership)
	// connecting an OAuth client gets the SSO short-circuit -- mint a
	// PKCE-bound auth code and 303 straight to redirect_uri?code=...&
	// state=... -- instead of being shown the email/login form again,
	// which dropped the OAuth context and landed them on /admin/ (#1556).
	// Unauthenticated callers fall through to handleAuthorize (the
	// consent/login page), unchanged.
	mux.HandleFunc("GET /authorize", wrap(preAuth(s.handleAuthorize)))
	mux.HandleFunc("GET /check-email", wrap(s.handleCheckEmail))
	mux.HandleFunc("GET /logout-complete", wrap(s.handleLogoutComplete))
	mux.HandleFunc("GET /error", wrap(s.handleError))
	mux.HandleFunc("GET /setup", wrap(preAuth(s.handleSetupGet)))
	mux.HandleFunc("POST /setup", wrap(s.handleSetupPost))
	mux.HandleFunc("GET /legal/tos", wrap(s.handleLegalTOS))
	mux.HandleFunc("GET /legal/privacy", wrap(s.handleLegalPrivacy))

	// /me/* SPA shells. Authentication is fetched client-side via
	// /auth/refresh — these handlers only render the shell.
	mux.HandleFunc("GET /me/", wrap(s.handleMeDashboard))
	mux.HandleFunc("GET /me/settings", wrap(s.handleMeSettings))
	mux.HandleFunc("GET /me/devices", wrap(s.handleMeDevices))
	mux.HandleFunc("GET /me/export", wrap(s.handleMeExport))
	mux.HandleFunc("GET /me/deletion-pending", wrap(s.handleMeDeletionPending))

	// Phase 7: server-rendered /me/tokens. Auth-gated server-side
	// (Bearer header or memql_admin cookie); see requireUser in
	// me_tokens.go. Routes mount only when the adapter is wired.
	if s.meTokens != nil && s.meTokens.Adapter != nil {
		mux.HandleFunc("GET /me/tokens", wrap(s.handleMeTokensGet))
		mux.HandleFunc("POST /me/tokens", wrap(s.handleMeTokensPost))
		mux.HandleFunc("POST /me/tokens/revoke", wrap(s.handleMeTokensRevoke))
	}

	// RFC 8628 verification page (memql#3410). Auth-gated server-side
	// (requireUserForDevice); a signed-out visitor goes through /login
	// and comes back here with their code. Mounts only when the device
	// flow is wired.
	if s.deviceFlow != nil && s.deviceFlow.Adapter != nil {
		mux.HandleFunc("GET /device", wrap(s.handleDeviceGet))
		mux.HandleFunc("POST /device", wrap(s.handleDevicePost))
	}

	if s.Logger != nil {
		s.Logger.Info("identity web routes mounted")
	}
}

// cookieSecure reports whether cookies the web surface mints
// should carry the Secure flag. Derived from Cfg.BaseURL --
// https:// turns on the flag. Tests pass BaseURL=http://localhost
// and get Secure=false so the cookie still sticks.
func (s *Server) cookieSecure() bool {
	if s == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Cfg.BaseURL)), "https://")
}

// LayoutData builds the brand-aware base layout payload for a request.
// Exported because the sibling admin package needs the same builder
// to render through the shared templ Layout component.
func (s *Server) LayoutData(r *http.Request, title string, dataMe bool, navLinks []webtempl.NavLink, extraScripts []string) webtempl.LayoutData {
	settings := s.snapshotSettings(r)
	path := ""
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	return webtempl.LayoutData{
		Title:             title,
		BrandName:         settings.BrandName,
		BrandPrimaryColor: settings.BrandPrimaryColor,
		BrandLogoDataURI:  settings.BrandLogoDataURI,
		BrandIconDataURI:  settings.BrandIconDataURI,
		Year:              time.Now().UTC().Year(),
		NavLinks:          navLinks,
		ExtraScripts:      extraScripts,
		DataMe:            dataMe,
		Path:              path,
		Asset:             s.assetURL,
		CSRFToken:         CSRFTokenFromRequest(r),
	}
}

// snapshotSettings collapses the immutable Config + the live settings
// snapshot into the Settings shape the layout needs. Used both by
// LayoutData and by the few handlers that need access to mode /
// allowed-domains hints (e.g. the login form renderer).
func (s *Server) snapshotSettings(r *http.Request) Settings {
	settings := Settings{
		BrandName:           "memQL",
		BrandPrimaryColor:   "#0433ff",
		RegistrationMode:    s.Cfg.RegistrationMode,
		RegistrationDomains: strings.Join(s.Cfg.RegistrationDomains, ", "),
	}
	if s.Cfg.BrandName != "" {
		settings.BrandName = s.Cfg.BrandName
	}
	if s.Cfg.BrandPrimaryColor != "" {
		settings.BrandPrimaryColor = s.Cfg.BrandPrimaryColor
	}
	if s.Cfg.BrandLogoDataURI != "" {
		settings.BrandLogoDataURI = s.Cfg.BrandLogoDataURI
	}
	if s.Live != nil {
		live := s.Live.Snapshot(r.Context())
		if live.BrandName != "" {
			settings.BrandName = live.BrandName
		}
		if live.BrandPrimaryColor != "" {
			settings.BrandPrimaryColor = live.BrandPrimaryColor
		}
		if live.BrandLogoDataURI != "" {
			settings.BrandLogoDataURI = live.BrandLogoDataURI
		}
		if live.BrandIconDataURI != "" {
			settings.BrandIconDataURI = live.BrandIconDataURI
		}
		if live.RegistrationMode != "" {
			settings.RegistrationMode = live.RegistrationMode
		}
		if live.RegistrationDomains != "" {
			settings.RegistrationDomains = live.RegistrationDomains
		}
	}
	return settings
}

// render writes the standard Cache-Control + Content-Type headers
// then renders the supplied templ.Component. Logs a single warning
// on render failure but does not retry — partial writes are already
// out the door.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	noStoreHTMLHeaders(w)
	if err := c.Render(r.Context(), w); err != nil {
		s.Logger.Error("identity-web: render failed", "name", name, "error", err)
	}
}

// loadLegalDoc reads a markdown doc from the override path (if set
// and readable) or falls back to the embedded copy. Returns the body
// bytes and a best-effort version string parsed from the front-matter.
func loadLegalDoc(overridePath, embedPath string) ([]byte, string, error) {
	if strings.TrimSpace(overridePath) != "" {
		if body, err := os.ReadFile(overridePath); err == nil {
			return body, parseVersion(body), nil
		}
	}
	body, err := fs.ReadFile(FS, embedPath)
	if err != nil {
		return nil, "", err
	}
	return body, parseVersion(body), nil
}

// parseVersion does a minimal scan of the front-matter for `version:`.
// Returns "unknown" when the doc isn't structured as expected.
func parseVersion(body []byte) string {
	src := string(body)
	if !strings.HasPrefix(src, "---") {
		return "unknown"
	}
	end := strings.Index(src[3:], "---")
	if end < 0 {
		return "unknown"
	}
	front := src[3 : 3+end]
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "version:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "version:"))
		}
	}
	return "unknown"
}
