//go:build identity

package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/admin"
	"github.com/znasllc-io/memql/component/identity/emailsender"
	httpidentity "github.com/znasllc-io/memql/component/identity/http"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/component/identity/refresh"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
)

// newSSOAuthCode mints a plaintext URL-safe base64 code + its
// SHA-256 hex hash, mirroring magiclink.newAuthCode (private to
// that package). The shape MUST match what /oauth/token expects --
// hashCode in component/identity/http/token.go is the consumer.
func newSSOAuthCode() (plain, hash string, err error) {
	const codeBytes = 32
	buf := make([]byte, codeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(sum[:])
	return plain, hash, nil
}

// integrationsIdentity registers integration providers for an
// identity node. Identity nodes host the in-house authentication
// provider — public auth web pages, the admin web app, OAuth-style
// token endpoints, and the JWKS feed every other node uses to
// verify access tokens.
//
// Identity binaries do not run the per-node verifier middleware in
// app/config.go. configAndAuth's verifier setup short-circuits when
// IDENTITY_VERIFIER_BASE_URL is empty (the documented production
// posture for identity-tagged deployments — they own the signing
// keys themselves rather than verifying against a remote JWKS).
func (a *App) integrationsIdentity() {
	a.integrationsCore()

	cfg, err := identity.LoadConfigFromEnv()
	if err != nil {
		a.fatal("failed to load identity configuration", "error", err, "component", identity.ComponentName)
	}

	if !cfg.Enabled {
		a.Logger.Warn("identity service is disabled (IDENTITY_ENABLED=false); identity binary started but will not serve auth")
		return
	}

	if err := cfg.Validate(); err != nil {
		a.fatal("identity configuration validation failed", "error", err, "component", identity.ComponentName)
	}

	// Wire the EngineAuditSink so audit events land in
	// v1:identity:auditEvent in addition to the slog stream. The
	// engine satisfies identity.EngineExecutor directly.
	auditLogger := &identity.SlogAuditLogger{
		Logger: a.Logger,
		DB: &identity.EngineAuditSink{
			Engine: a.engine,
			Logger: a.Logger,
		},
	}

	svc, err := identity.NewService(cfg, a.Logger, auditLogger)
	if err != nil {
		a.fatal("failed to construct identity service", "error", err, "component", identity.ComponentName)
	}

	// Phase 2: build the Store, magic-link issuer/verifier, refresh
	// rotator, email sender, and HTTP server. The Service's HTTP
	// mounter is set to the constructed *Server so RegisterRoutes
	// picks up the auth flow when transportIdentity runs.
	store := &identity.Store{Engine: a.engine, Logger: a.Logger}
	emailSender := emailsender.New(a.engine, a.Logger, cfg)

	// Bootstrap-state callback shared by the magic-link issuer + the
	// web layer's /setup wizard + the /login bootstrap gate. The
	// cluster is "bootstrapped" once the wizard at /setup has
	// completed (clusterSettings.bootstrappedAt is non-empty).
	// Pre-bootstrap, the only sanctioned path to mint the first
	// real user is the wizard, which sends a magic link to the
	// operator-supplied owner email. /login + the auth-API magic-
	// link endpoint are gated until then, so a stranger can't claim
	// cluster ownership before the operator runs setup.
	//
	// "Any user exists" is NOT the signal here -- user rows can be
	// created out-of-band (direct mutation, prior dev-mode artifacts)
	// and shouldn't unilaterally claim the cluster is bootstrapped.
	// The wizard's stamp on clusterSettings.bootstrappedAt is the
	// authoritative gate.
	bootstrapCheck := func(ctx context.Context) bool {
		return store.IsClusterBootstrapped(ctx)
	}

	mlIssuer := &magiclink.Issuer{
		Cfg:            cfg,
		Store:          store,
		Audit:          auditLogger,
		Sender:         emailSender,
		Logger:         a.Logger,
		IsBootstrapped: bootstrapCheck,
	}
	mlVerifier := &magiclink.Verifier{
		Cfg:    cfg,
		Store:  store,
		Audit:  auditLogger,
		Logger: a.Logger,
	}
	// Live cluster-settings reader. Constructed before the rotator
	// + http server so they can both share the same TokenSettings
	// hook -- admin-edited TTLs apply on the next access-token mint
	// (token endpoint) and the next refresh-rotation (rotator)
	// without an identity restart.
	liveSettings := &admin.LiveSettingsReader{
		Engine:   a.engine,
		Fallback: cfg,
		Logger:   a.Logger,
	}
	rotator := &refresh.Rotator{
		Cfg:               cfg,
		Store:             store,
		Issuer:            svc.Issuer(),
		Audit:             auditLogger,
		Logger:            a.Logger,
		LiveTokenSettings: liveSettings.TokenSettings,
	}
	// Phase 4: anti-abuse middleware in front of POST /auth/magic-link.
	// Per-IP rate limit + Cloudflare Turnstile + disposable-email
	// blocklist + MX-record validation + risk scoring. Each rejection
	// emits an audit event with action=magic_link_blocked and a
	// failureReason matching the specific defense.
	rateLimit := cfg.RateLimitPerIPPerHour
	if rateLimit <= 0 {
		rateLimit = identity.DefaultRateLimitPerIPPerHour
	}
	threshold := cfg.RiskThreshold
	if threshold <= 0 {
		threshold = identity.DefaultRiskThreshold
	}
	abuseMW := &abuse.Middleware{
		Cfg:       cfg,
		Audit:     auditLogger,
		Logger:    a.Logger,
		MX:        abuse.NewMXValidator(a.Logger),
		Turnstile: &abuse.TurnstileVerifier{Secret: cfg.TurnstileSecret, Logger: a.Logger},
		IPLimiter: abuse.NewIPRateLimiter(rateLimit, a.Logger),
		Threshold: threshold,
	}

	httpSrv := &httpidentity.Server{
		Cfg:               cfg,
		Store:             store,
		Issuer:            svc.Issuer(),
		MLIssuer:          mlIssuer,
		MLVerifier:        mlVerifier,
		Rotator:           rotator,
		Audit:             auditLogger,
		Logger:            a.Logger,
		Abuse:             abuseMW,
		LiveTokenSettings: liveSettings.TokenSettings,
	}
	svc.SetHTTPMounter(httpSrv)

	// Phase 3 + Phase 6: web UI. Phase 6 swaps the static
	// identityWebSettings adapter for LiveSettingsReader so admin-UI
	// edits to v1:identity:clusterSettings take effect on the public
	// pages without restarting the binary. The reader carries the env
	// snapshot as Fallback so a fresh cluster (no cluster-settings row
	// yet) still renders something sensible. liveSettings is
	// constructed earlier (alongside the Rotator) so the same
	// instance backstops both the public web pages AND the runtime-
	// tunable TTL hook.
	webSrv, err := identityweb.NewServer(cfg, a.Logger, liveSettings)
	if err != nil {
		a.fatal("failed to construct identity web server", "error", err, "component", identity.ComponentName)
	}
	// Back the DB-side OAuth client resolution on /authorize so
	// dynamically-registered (RFC 7591) clients resolve like static ones.
	webSrv.Store = store
	webSrv.IssueMagicLink = func(ctx context.Context, in identityweb.IssueMagicLinkInput) error {
		if in.IsAccessRequest {
			// Waitlist path lands an access-request row instead of a
			// magic-link request. The magic-link issuer rejects
			// waitlist-mode emails, so this branch is the correct
			// destination for that form variant.
			return store.CreateAccessRequest(ctx,
				identity.NewRequestId(),
				in.Email,
				in.WaitlistName,
				in.WaitlistContext,
				0, "",
				in.SourceIP, in.UserAgent,
			)
		}
		return mlIssuer.Issue(ctx, magiclink.IssueInput{
			Email:               in.Email,
			ClientId:            in.ClientId,
			RedirectURI:         in.RedirectURI,
			State:               in.State,
			CodeChallenge:       in.CodeChallenge,
			CodeChallengeMethod: in.CodeChallengeMethod,
			SourceIP:            in.SourceIP,
			UserAgent:           in.UserAgent,
			InvitationId:        in.InvitationId,
			Bootstrap:           in.Bootstrap,
			ExistingUser:        in.ExistingUser,
			AdminSession:        in.AdminSession,
		})
	}
	// Wizard's "404 once bootstrapped" check. Same signal as the
	// login gate — we use clusterSettings.bootstrappedAt rather than
	// a user-count so out-of-band user rows can't trip it. The
	// CountUsers shape is preserved (returns 1 when bootstrapped,
	// 0 when not) so the web package's API stays stable.
	webSrv.CountUsers = func(ctx context.Context) (int, error) {
		if store.IsClusterBootstrapped(ctx) {
			return 1, nil
		}
		return 0, nil
	}
	// "Does this email already belong to a cluster user?" — drives the
	// /login flow's existing-user-vs-registration branch. Bound to the
	// Store's LookupUserByEmail; absent rows are returned as
	// (false, nil), and the web layer treats any error as "unknown"
	// so transient lookup failures don't leak through.
	webSrv.UserExistsByEmail = func(ctx context.Context, email string) (bool, error) {
		row, err := store.LookupUserByEmail(ctx, email)
		if err != nil {
			return false, err
		}
		return row != nil, nil
	}
	webSrv.PersistClusterSettings = func(ctx context.Context, in identityweb.ClusterSettingsInput) error {
		return store.PersistClusterSettings(ctx, identity.ClusterSettingsRow{
			ClusterDomain:             in.Domain,
			BrandName:                 in.BrandName,
			RegistrationMode:          in.RegistrationMode,
			RegistrationDomains:       in.RegistrationDomains,
			InternalDomains:           in.InternalDomains,
			InternalDefaultRole:       in.InternalDefaultRole,
			AccessRequestNotifyEmails: in.AccessRequestNotifyEmails,
			BootstrapEmail:            in.OwnerEmail,
			BootstrapFirstName:        in.OwnerFirstName,
			BootstrapLastName:         in.OwnerLastName,
			BootstrapPhone:            in.OwnerPhone,
			BootstrapPrimaryRole:      in.OwnerPrimaryRole,
			BootstrapGender:           in.OwnerGender,
			BootstrapBirthdate:        in.OwnerBirthdate,
		})
	}
	// SSO mint adapter. Lets redirectIfAuthenticated short-circuit
	// `/login?return_to=<registered-client>` for users who already
	// have a memql_admin cookie -- one click straight from the
	// CoPresent AuthPortal to the SPA callback, no re-typing the
	// email. Lives here because the web package can't reach the
	// store directly.
	webSrv.SetMintSSOAuthCode(func(ctx context.Context, in identityweb.MintSSOAuthCodeInput) (identityweb.MintSSOAuthCodeResult, error) {
		plain, hash, err := newSSOAuthCode()
		if err != nil {
			return identityweb.MintSSOAuthCodeResult{}, fmt.Errorf("sso mint: gen code: %w", err)
		}
		codeId, err := identity.NewRandomId("")
		if err != nil {
			return identityweb.MintSSOAuthCodeResult{}, fmt.Errorf("sso mint: gen code id: %w", err)
		}
		expiresAt := time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
		if err := store.CreateAuthCode(
			ctx,
			codeId, plain, hash,
			in.ClientId, in.RedirectURI, in.State,
			in.UserId,
			"", // identityId -- no specific credential row tracked for SSO mint
			"", // magicLinkRequestId -- the original sign-in's magic link, not surfaced
			expiresAt,
		); err != nil {
			return identityweb.MintSSOAuthCodeResult{}, fmt.Errorf("sso mint: persist: %w", err)
		}
		auditLogger.Log(ctx, identity.AuditEvent{
			Category:    identity.AuditCategoryAuth,
			Action:      "sso_auth_code_minted",
			ActorUserId: in.UserId,
			TargetType:  "authCode",
			TargetId:    codeId,
			SourceIP:    in.SourceIP,
			UserAgent:   in.UserAgent,
			Outcome:     identity.AuditOutcomeSuccess,
			Detail: map[string]any{
				"clientId": in.ClientId,
			},
		})
		return identityweb.MintSSOAuthCodeResult{Code: plain}, nil
	})
	// PAT (Personal Access Token) layer. Lives under the existing
	// v1:identity:identity concept (api_key variant); the pat
	// package wraps the engine with typed Create/Revoke/Lookup plus
	// the Verifier the identity binary's own auth path uses
	// (component/identity/verifier on bff/voice/etc. delegates here
	// only when the identity service is the verifying node — every
	// other binary fetches JWKS instead).
	patStore := &pat.Store{Engine: a.engine, Logger: a.Logger}
	patVerifier := &pat.Verifier{
		Store:  patStore,
		Issuer: svc.Issuer(),
		Users:  &patUserLookup{store: store},
		Audit:  auditLogger,
		Logger: a.Logger,
	}
	svc.SetPATStore(patStore)
	svc.SetPATVerifier(patVerifier)

	// /me/tokens — wired into the public web server. Browsers reach
	// it after sign-in; CLI clients use the underlying mql_pat_<...>
	// tokens, not this page.
	webSrv.SetMeTokens(&identityweb.MeTokens{
		Adapter: patStore,
		Issuer:  svc.Issuer(),
		Audit:   auditLogger,
	})

	svc.SetWebMounter(webSrv)

	// Phase 6: admin web app under /admin/*. Mounted on the same mux
	// as the public web pages; auth-gated by the admin package's own
	// requireAdmin middleware (role=owner|admin).
	adminSrv, err := admin.New(&admin.AdminServer{
		Cfg:       cfg,
		Engine:    a.engine,
		Issuer:    svc.Issuer(),
		Keys:      svc.Keys(),
		Audit:     auditLogger,
		Settings:  liveSettings,
		WebServer: webSrv,
		Logger:    a.Logger,
		RotateNow: func(r *http.Request) error {
			actor := identity.AuditActor{}
			if r != nil {
				return svc.RotateNow(r.Context(), actor)
			}
			return svc.RotateNow(context.Background(), actor)
		},
	})
	if err != nil {
		a.fatal("failed to construct identity admin server", "error", err, "component", identity.ComponentName)
	}
	adminSrv.SetPATAdapter(patStore)
	// memql#350: the /admin/tokens Node-tokens section + revoke
	// route is backed by the identity Store's NodeTokenIdentity
	// methods (ListNodeTokenIdentities / LookupNodeTokenIdentityById
	// / RevokeNodeTokenIdentity). The narrow adapter shape lives in
	// the admin package; an inline struct satisfying it is the
	// thinnest wiring.
	adminSrv.SetNodeTokenAdapter(&storeNodeTokenAdapter{store: store})
	svc.SetAdminMounter(adminSrv)

	// Stash the admin server so setupDeployControlService (which runs
	// after the deploy-control service is constructed) can wire the
	// in-process deployment-status reader onto it (memql#726).
	a.adminServer = adminSrv

	a.identityService = svc
	a.Logger.Info("identity service initialized",
		"base_url", cfg.BaseURL,
		"registration_mode", string(cfg.RegistrationMode),
		"registered_clients", len(cfg.RegisteredClients),
		"key_dir", cfg.KeyDir,
		"key_encryption_at_rest", cfg.KeyEncryptionKey != "",
	)

	// The HTTP routes get mounted by transportIdentity() once the
	// mux exists; the rotation goroutine starts here so it begins
	// timekeeping before transport is up. context.Background is
	// fine — Service.Shutdown stops it cleanly on server shutdown.
	svc.Start(context.Background())

	// Unattended-deploy path. When every required IDENTITY_BOOTSTRAP_*
	// env var is set AND the cluster hasn't been bootstrapped yet,
	// stamp clusterSettings + issue the owner magic link without
	// requiring a /setup visit. Operators who skipped some required
	// vars still go through the interactive wizard; partial env values
	// just prefill the form. Errors here are warnings, not fatal --
	// the operator can always fall back to /setup.
	a.attemptAutoBootstrap(context.Background(), cfg, store, mlIssuer)
}

// attemptAutoBootstrap drives the same path as the /setup wizard but
// from env values when all required IDENTITY_BOOTSTRAP_* vars are
// present and the cluster hasn't been bootstrapped yet. The actual
// bootstrappedAt stamp lands when the operator clicks the emailed
// magic link (verifier-side); this only writes the clusterSettings
// row + sends the owner email. Idempotent: re-run on every restart
// is a no-op once the cluster is bootstrapped.
func (a *App) attemptAutoBootstrap(
	ctx context.Context,
	cfg identity.Config,
	store *identity.Store,
	mlIssuer *magiclink.Issuer,
) {
	if !cfg.Bootstrap.HasAllRequired() {
		return
	}
	if store.IsClusterBootstrapped(ctx) {
		a.Logger.Info("identity auto-bootstrap skipped: cluster already bootstrapped",
			"component", identity.ComponentName)
		return
	}
	// System actor is required by the engine for any mutation. The
	// identity service owns this row, so 'system:identity-svc' is
	// the correct attribution -- same actor SystemActorMiddleware
	// stamps on /setup wizard requests.
	ctx = identity.ContextWithSystemActor(ctx)

	row := identity.ClusterSettingsRow{
		ClusterDomain:             cfg.Bootstrap.Domain,
		RegistrationMode:          cfg.Bootstrap.RegistrationMode,
		RegistrationDomains:       strings.Join(cfg.Bootstrap.RegistrationDomains, ","),
		InternalDomains:           strings.Join(cfg.Bootstrap.InternalDomains, ","),
		InternalDefaultRole:       cfg.Bootstrap.InternalDefaultRole,
		AccessRequestNotifyEmails: strings.Join(cfg.Bootstrap.NotifyEmails, ","),
		BootstrapEmail:            cfg.Bootstrap.OwnerEmail,
		BootstrapFirstName:        cfg.Bootstrap.OwnerFirstName,
		BootstrapLastName:         cfg.Bootstrap.OwnerLastName,
		BootstrapPhone:            cfg.Bootstrap.OwnerPhone,
		BootstrapPrimaryRole:      cfg.Bootstrap.OwnerPrimaryRole,
		BootstrapGender:           cfg.Bootstrap.OwnerGender,
		BootstrapBirthdate:        cfg.Bootstrap.OwnerBirthdate,
	}
	if err := store.PersistClusterSettings(ctx, row); err != nil {
		a.Logger.Warn("identity auto-bootstrap: persist clusterSettings failed; falling back to interactive /setup",
			"error", err, "component", identity.ComponentName)
		return
	}

	// MAIL_SUPPRESS_OWNER_BOOTSTRAP suppresses the owner magic-link email
	// (memql#374). Set by `make dev-refresh` so iterative DB wipes don't
	// produce an inbox storm; the operator is the same person across
	// refreshes + the clusterSettings row is already persisted above, so
	// the cluster is functionally bootstrapped without the email.
	// Unset in staging / production deploys; behaviour there is unchanged.
	if os.Getenv("MAIL_SUPPRESS_OWNER_BOOTSTRAP") != "" {
		a.Logger.Info("identity auto-bootstrap: owner email suppressed by MAIL_SUPPRESS_OWNER_BOOTSTRAP",
			"owner_email", cfg.Bootstrap.OwnerEmail,
			"domain", cfg.Bootstrap.Domain,
			"component", identity.ComponentName)
		return
	}

	if err := mlIssuer.Issue(ctx, magiclink.IssueInput{
		Email:        cfg.Bootstrap.OwnerEmail,
		State:        "setup",
		Bootstrap:    true,
		AdminSession: true,
	}); err != nil {
		a.Logger.Warn("identity auto-bootstrap: issue owner magic link failed; rerun setup or check email config",
			"error", err, "owner_email", cfg.Bootstrap.OwnerEmail,
			"component", identity.ComponentName)
		return
	}
	a.Logger.Info("identity auto-bootstrap: owner magic link issued; click the link in the owner inbox to claim ownership",
		"owner_email", cfg.Bootstrap.OwnerEmail,
		"domain", cfg.Bootstrap.Domain,
		"component", identity.ComponentName)
}

// transportIdentity mounts the identity service's HTTP routes on
// the App's mux. Called from build_identity.go after configAndAuth
// has constructed a.mux.
//
// Phase 1 mounted only /.well-known/jwks.json. Phase 2 also mounts
// /auth/magic-link, /auth/complete, /oauth/token, /auth/refresh,
// /auth/logout via the configured HTTPMounter on the Service.
func (a *App) transportIdentity() {
	if a.identityService == nil {
		return
	}
	svc, ok := a.identityService.(*identity.Service)
	if !ok {
		a.fatal("identityService field has unexpected type", "component", identity.ComponentName)
	}
	if a.mux == nil {
		a.fatal("transport mux not constructed before transportIdentity", "component", identity.ComponentName)
	}
	svc.RegisterRoutes(a.mux)
	a.Logger.Info("identity HTTP routes mounted", "component", identity.ComponentName)
}

// patUserLookup satisfies pat.UserLookup with a thin wrapper around
// the existing identity.Store. Lives here (rather than in the pat
// package) because Store's user lookups today are email-based; we
// reuse the listing helpers and find by id Go-side. When the Phase-3
// userById query lands a "by canonical id" code path, this collapses
// to a single Execute call.
type patUserLookup struct {
	store *identity.Store
}

func (l *patUserLookup) UserById(ctx context.Context, userId string) (*pat.UserSummary, error) {
	if l == nil || l.store == nil {
		return nil, nil
	}
	res, err := l.store.Engine.Execute(ctx,
		`queryUserById({userId: "`+escapeMemQLArg(userId)+`"})`)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return nil, nil
	}
	n := res.Bundle.Nodes[0]
	if n == nil || n.Payload == nil {
		return nil, nil
	}
	fields := n.Payload.GetFields()
	str := func(k string) string {
		if v, ok := fields[k]; ok && v != nil {
			return v.GetStringValue()
		}
		return ""
	}
	boolean := func(k string, def bool) bool {
		v, ok := fields[k]
		if !ok || v == nil {
			return def
		}
		return v.GetBoolValue()
	}
	return &pat.UserSummary{
		ID:           n.GetId(),
		DisplayName:  str("displayName"),
		PrimaryEmail: str("primaryEmail"),
		Role:         str("role"),
		Active:       boolean("active", true),
		Internal:     boolean("internal", false),
	}, nil
}

// escapeMemQLArg is the minimal escape needed to embed a string
// safely inside a quoted MemQL argument. Mirrors the pattern used
// across other store wrappers.
func escapeMemQLArg(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '"', '\\':
			out = append(out, '\\', r)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// shutdownIdentityService gracefully stops the identity background
// goroutines (key rotation). Called from build_identity.go's
// teardown sequence — though for now there's no central shutdown
// hook, so this is queued for the broader shutdown refactor.
func (a *App) shutdownIdentityService(ctx context.Context) error {
	if a.identityService == nil {
		return nil
	}
	svc, ok := a.identityService.(*identity.Service)
	if !ok {
		return nil
	}
	return svc.Shutdown(ctx)
}

// storeNodeTokenAdapter satisfies admin.NodeTokenAdapter via the
// identity.Store's NodeTokenIdentity surface (memql#350). Lives
// here rather than in the admin package so the admin layer stays
// decoupled from the concrete store type -- the package depends on
// the narrow port shape, the wiring layer satisfies it.
type storeNodeTokenAdapter struct {
	store *identity.Store
}

func (a *storeNodeTokenAdapter) ListAll(ctx context.Context) ([]identity.NodeTokenRow, error) {
	if a == nil || a.store == nil {
		return nil, nil
	}
	return a.store.ListNodeTokenIdentities(ctx)
}

func (a *storeNodeTokenAdapter) LookupById(ctx context.Context, identityId string) (*identity.NodeTokenRow, error) {
	if a == nil || a.store == nil {
		return nil, nil
	}
	return a.store.LookupNodeTokenIdentityById(ctx, identityId)
}

func (a *storeNodeTokenAdapter) Revoke(ctx context.Context, identityId string) error {
	if a == nil || a.store == nil {
		return fmt.Errorf("storeNodeTokenAdapter: nil store")
	}
	return a.store.RevokeNodeTokenIdentity(ctx, identityId)
}
