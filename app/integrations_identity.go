//go:build identity

package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/admin"
	"github.com/znasllc-io/memql/component/identity/authactivity"
	"github.com/znasllc-io/memql/component/identity/devicecode"
	"github.com/znasllc-io/memql/component/identity/emailsender"
	"github.com/znasllc-io/memql/component/identity/enrolment"
	httpidentity "github.com/znasllc-io/memql/component/identity/http"
	"github.com/znasllc-io/memql/component/identity/invitation"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/component/identity/recoverykey"
	"github.com/znasllc-io/memql/component/identity/refresh"
	"github.com/znasllc-io/memql/component/identity/registration"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	"github.com/znasllc-io/memql/component/identity/webauthn"
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
// MEMQL_IDENTITY_VERIFIER_BASE_URL is empty (the documented production
// posture for identity-tagged deployments — they own the signing
// keys themselves rather than verifying against a remote JWKS).
func (a *App) integrationsIdentity() {
	a.integrationsCore()

	cfg, err := identity.LoadConfigFromEnv()
	if err != nil {
		a.fatal("failed to load identity configuration", "error", err, "component", identity.ComponentName)
	}

	if !cfg.Enabled {
		a.Logger.Warn("identity service is disabled (MEMQL_IDENTITY_ENABLED=false); identity binary started but will not serve auth")
		return
	}

	if err := cfg.Validate(); err != nil {
		a.fatal("identity configuration validation failed", "error", err, "component", identity.ComponentName)
	}

	// TWO SINKS BEHIND ONE LOGGER (memql#4328). Decisions and security
	// signals land in v1:identity:auditEvent; routine mechanics -- refresh
	// rotations, grace-window accepts, PAT-authenticated requests -- land in
	// v1:identity:authActivity. Both also go to the slog stream, which is
	// unbounded and stays the canonical destination.
	//
	// Wiring the activity sink is not optional in practice: without it every
	// mechanic falls back to the audit log (SlogAuditLogger's deliberate
	// degrade), which is the noise the split exists to remove AND leaves the
	// reuse-detection lookup with nothing to key on.
	auditLogger := &identity.SlogAuditLogger{
		Logger: a.Logger,
		DB: &identity.EngineAuditSink{
			Engine: a.engine,
			Logger: a.Logger,
		},
		Activity: &identity.ActivitySink{
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

	// RFC 8628 device authorization grant (memql#3410). One store backs
	// all three surfaces: POST /device/code + the device_code grant on
	// the HTTP server, and the verification page on the web server.
	deviceCodeStore := &devicecode.Store{Engine: a.engine, Logger: a.Logger}

	httpSrv := &httpidentity.Server{
		Cfg:               cfg,
		Store:             store,
		DeviceCodes:       deviceCodeStore,
		Issuer:            svc.Issuer(),
		MLIssuer:          mlIssuer,
		MLVerifier:        mlVerifier,
		Rotator:           rotator,
		Audit:             auditLogger,
		Logger:            a.Logger,
		Abuse:             abuseMW,
		LiveTokenSettings: liveSettings.TokenSettings,
	}
	// UPSTREAM FEDERATION (memql#4611). Wired unconditionally: both hooks are
	// inert unless MEMQL_IDENTITY_OIDC_ENABLED is set, and the routes 404
	// without a provider. Leaving them nil on a configured cluster would give
	// the one failure mode the whole design refuses -- a sign-in button that
	// reaches "federation_not_wired" per user, which is a boot-time
	// configuration problem reported to everybody except the operator.
	httpSrv.OIDCLookup = httpSrv.DefaultOIDCLookup
	httpSrv.OIDCSignIn = httpSrv.DefaultOIDCSignIn
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
	// ONE abuse stack, BOTH issue paths (memql#4303). The same instance
	// httpSrv wraps around POST /auth/magic-link now also wraps the browser's
	// POST /login, so a rule tightened for one cannot leave the other behind.
	// The rejection renders as the web error page rather than the API's JSON
	// envelope -- a person who trips the rate limit should read a sentence,
	// not a payload.
	abuseMW.RenderReject = webSrv.RenderAbuseRejection
	// The new-sign-in notice (memql#4305). Fired from createSessionRow, the
	// one place an authSession row is created, so the token exchange, the
	// device grant, the browser cookie session and the passkey web login are
	// all covered by one hook rather than four call sites that would drift.
	httpSrv.SignInNotifier = emailSender
	webSrv.Abuse = abuseMW
	webSrv.IssueMagicLink = func(ctx context.Context, in identityweb.IssueMagicLinkInput) (identityweb.IssueMagicLinkResult, error) {
		if in.IsAccessRequest {
			// Waitlist path lands an access-request row instead of a
			// magic-link request. The magic-link issuer rejects
			// waitlist-mode emails, so this branch is the correct
			// destination for that form variant. No row, no binding, no
			// cookie -- there is nothing to complete.
			return identityweb.IssueMagicLinkResult{}, store.CreateAccessRequest(ctx,
				identity.NewRequestId(),
				in.Email,
				in.WaitlistName,
				in.WaitlistContext,
				0, "",
				in.SourceIP, in.UserAgent,
			)
		}
		res, err := mlIssuer.Issue(ctx, magiclink.IssueInput{
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
		return identityweb.IssueMagicLinkResult{
			RequestId:    res.RequestId,
			BindingNonce: res.BindingNonce,
			ExpiresAt:    res.ExpiresAt,
		}, err
	}

	// The device-bound magic-link flow (memql#4302). All four routes mount
	// together or not at all, so this is the one wiring point that decides
	// whether a cluster has the flow. The verifier reads and finishes; the
	// session func is the http package's own mint, reached through a seam
	// because web must not import http; the audit sink records approvals and
	// refusals, and its absence would leave a credential surface unaudited.
	webSrv.SetMagicLinkFlow(mlVerifier, func(w nethttp.ResponseWriter, r *nethttp.Request, in identityweb.BrowserSessionInput) error {
		return httpSrv.StartBrowserSessionFor(w, r, in.UserId, in.Email, in.Action)
	}, auditLogger)
	// Wizard's "404 once bootstrapped" check, signal 1 of 2. We use
	// clusterSettings.bootstrappedAt rather than a user-count so
	// out-of-band user rows can't trip it. The CountUsers shape is
	// preserved (returns 1 when bootstrapped, 0 when not) so the web
	// package's API stays stable.
	webSrv.CountUsers = func(ctx context.Context) (int, error) {
		if store.IsClusterBootstrapped(ctx) {
			return 1, nil
		}
		return 0, nil
	}
	// Signal 2 of 2 (memql#3415). bootstrappedAt is ONE field on ONE
	// append-only row; a stray write blanked it on a live cluster and
	// /setup -- the wizard that mints the cluster owner -- answered 200
	// to anyone while hundreds of users were locked out of /login. The
	// original reasoning above still holds (a stray USER row must not
	// seal setup on a fresh cluster), so the cross-check restored here
	// is deliberately NOT a user-count: HasOwnerUser asks whether an
	// active user holds the cluster-OWNER role, which nothing but a
	// completed claim produces. It is the same signal the auto-bootstrap
	// claim-email guard already treats as definitional proof of a claim
	// (memql#1864). Errors propagate; the web layer seals on "unknown".
	webSrv.ClusterClaimed = store.HasOwnerUser
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
	// product's auth portal to the SPA callback, no re-typing the
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
		// `plain` is not persisted -- only its digest (issue #3187). It is
		// returned below for the redirect to the OAuth client.
		if err := store.CreateAuthCode(
			ctx,
			codeId, hash,
			in.ClientId, in.RedirectURI, in.State,
			// Bind the PKCE challenge when the SSO short-circuit was
			// reached via an OAuth 2.1 /authorize flow (e.g. the claude.ai
			// MCP connector). The client's /oauth/token exchange presents
			// the matching code_verifier, which /oauth/token validates
			// against this stored challenge. Empty on the legacy
			// product-SPA SSO path (no PKCE) -- mints a non-PKCE code as before
			// (#1556; previously always empty, #1570).
			in.CodeChallenge, in.CodeChallengeMethod,
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

	// The RFC 8628 verification page. Its ClientName hook resolves
	// through the same static-plus-DCR path /authorize and /oauth/token
	// use, so the approval screen names a dynamically-registered client
	// as readably as a statically-configured one -- and falls back to
	// the raw client_id, which is what the session binds to anyway.
	webSrv.SetDeviceFlow(&identityweb.DeviceFlow{
		Adapter: deviceCodeStore,
		Issuer:  svc.Issuer(),
		Audit:   auditLogger,
		// Returns the ORIGIN alongside the name (memql#3824): the device
		// approval page marks a self-asserted name so the approver knows
		// which of the things in front of them nobody vouched for.
		ClientDisplay: func(ctx context.Context, clientId string) (string, bool) {
			c, selfRegistered := identity.ResolveClientWithOrigin(ctx, cfg, store, clientId)
			if c == nil {
				return "", false
			}
			return c.Name, selfRegistered
		},
	})
	// GET /enroll (memql#3408). The web package stays engine-free, so the
	// store lives here and the page gets a validator seam -- the same shape as
	// PersistClusterSettings and UserExistsByEmail above.
	//
	// The adapter reports STATE, never token material: the plaintext stays in
	// the caller's URL and the hash never leaves this closure, so nothing that
	// crosses into the web package can be logged into existence.
	enrolStore := &enrolment.Store{Engine: a.engine, Logger: a.Logger}
	webSrv.SetResolveEnrolment(func(ctx context.Context, plainToken string) (identityweb.EnrolmentResolution, error) {
		row, state, err := enrolStore.Resolve(ctx, plainToken, time.Now().UTC())
		if err != nil {
			return identityweb.EnrolmentResolution{}, err
		}
		out := identityweb.EnrolmentResolution{State: identityweb.EnrolmentState(state)}
		if row == nil {
			return out, nil
		}
		out.EnrolmentId = enrolment.CanonicalId(row.ID)
		if state != enrolment.StateValid {
			// A spent / expired / revoked row still names its user for the
			// audit event, but the PAGE gets no account label: telling an
			// anonymous holder of a dead link whose account it belonged to
			// would turn a stale credential into an account-existence oracle.
			out.UserId = row.UserId
			return out, nil
		}
		out.UserId = row.UserId
		out.ExpiresAt = row.ExpiresAt
		out.AccountLabel = row.UserId
		if u, lookupErr := store.LookupUserById(ctx, row.UserId); lookupErr == nil && u != nil && u.PrimaryEmail != "" {
			out.AccountLabel = u.PrimaryEmail
		}
		return out, nil
	}, auditLogger)

	// Owner recovery redeem (memql#3968 + the memql#3967 gate).
	//
	// THE GATE LIVES IN THIS ADAPTER because answering "does this owner still
	// have a usable sign-in route" needs the engine, and component/identity/web
	// stays engine-free. The web package gets a STATE and never a reason to ask.
	//
	// The adapter reports state only -- no plaintext, no hash. And it reports
	// NO ACCOUNT LABEL on any refusal, for the reason the enrolment adapter
	// above records: telling an anonymous holder of a dead credential whose
	// account it belonged to turns a stale key into an account-existence
	// oracle. That matters more here, because the account in question is the
	// cluster owner's.
	recoveryStore := &recoverykey.Store{Engine: a.engine, Logger: a.Logger}
	webSrv.SetResolveRecovery(func(ctx context.Context, plainKey string) (identityweb.RecoveryResolution, error) {
		row, state, err := recoveryStore.Resolve(ctx, plainKey)
		if err != nil {
			return identityweb.RecoveryResolution{}, err
		}
		out := identityweb.RecoveryResolution{State: identityweb.RecoveryState(state)}
		if row == nil {
			return out, nil
		}
		out.RecoveryKeyId = recoverykey.CanonicalId(row.ID)
		owner := strings.TrimSpace(row.BoundOwnerUserId)
		if owner == "" {
			owner = strings.TrimSpace(row.UserId)
		}
		out.UserId = owner
		if state != recoverykey.StateValid {
			return out, nil
		}

		// THE BREAK-GLASS GATE. Refused while the bound owner can still sign
		// in normally -- otherwise the key is a second password rather than a
		// break-glass credential. Fail CLOSED on an error: an unknown answer
		// must refuse, because allowing on "I could not tell" is precisely the
		// state an attacker who can disrupt a read would want.
		hasRoute, routeErr := store.HasSignInRoute(ctx, owner)
		if routeErr != nil {
			return identityweb.RecoveryResolution{}, routeErr
		}
		if hasRoute {
			out.State = identityweb.RecoveryNotNeeded
			return out, nil
		}

		out.AccountLabel = owner
		if u, lookupErr := store.LookupUserById(ctx, owner); lookupErr == nil && u != nil && u.PrimaryEmail != "" {
			out.AccountLabel = u.PrimaryEmail
		}
		return out, nil
	}, auditLogger)

	// Invitation redeem (memql#4601). The web package stays engine-free, so the
	// stores live here and the page gets seams -- the same shape as the
	// enrolment and recovery adapters above.
	//
	// THE ADAPTER REPORTS STATE, NEVER TOKEN MATERIAL. The plaintext stays in
	// the caller's URL and the hash never leaves these closures, so nothing
	// crossing into the web package can be logged into existence. The one
	// digest that does cross is the BINDING hash, which is a value this server
	// minted for one browser and is exactly what magicLinkRequest.bindingHash
	// already persists in the clear.
	webSrv.SetInvitationFlow(
		func(ctx context.Context, plainToken string) (identityweb.InvitationResolution, error) {
			row, err := store.LookupInvitationByTokenHash(ctx, invitation.Hash(plainToken))
			if err != nil {
				return identityweb.InvitationResolution{}, err
			}
			out := identityweb.InvitationResolution{State: identityweb.InvitationInvalid}
			if row == nil {
				return out, nil
			}
			out.InvitationId = row.ID

			// ORDER MATTERS AND IT IS THE SAME ORDER resolveInvitation USES in
			// the magic-link issuer. Kind before status before expiry, so a
			// guest link presented here is called a guest link rather than
			// whatever its status happens to be -- the holder of one is not
			// making a typo, and telling them to re-check the link would send
			// them looking for a problem that is not there.
			switch {
			case !strings.EqualFold(strings.TrimSpace(row.Kind), "user"):
				// UNREACHABLE TODAY, AND KEPT ANYWAY. userInvitationByTokenHash
				// filters kind=="user", so no other kind can arrive here --
				// a guest token presented to this page reads as not-found
				// instead, which is the honest answer for a credential this
				// door does not serve.
				//
				// The branch stays because the guarantee lives in a DSL filter
				// one file away. If that query is ever widened -- and the
				// obvious "fix" for the defect memql#4612 records was exactly
				// to widen its sibling -- this is what stops a guest invitation
				// from being spent as a user one. A check that costs nothing
				// and fails closed is worth more than the line it saves.
				out.State = identityweb.InvitationWrongKind
			case !row.Active:
				out.State = identityweb.InvitationRevoked
			case !strings.EqualFold(strings.TrimSpace(row.Status), "pending"):
				out.State = identityweb.InvitationAlreadyUsed
			case !row.ExpiresAt.IsZero() && !row.ExpiresAt.After(time.Now().UTC()):
				out.State = identityweb.InvitationExpired
			default:
				out.State = identityweb.InvitationValid
			}

			// A DEAD INVITATION NAMES NOBODY. Everything below this line is
			// shown on the page, and telling an anonymous holder of a spent or
			// expired link which address it was for would turn a stale
			// credential into an address-disclosure oracle. The enrolment
			// adapter withholds its account label for the same reason.
			if out.State != identityweb.InvitationValid {
				return out, nil
			}

			out.InviteeEmail = row.Email
			out.InviterName = row.InviterName
			out.Role = row.Role
			out.ExpiresAt = row.ExpiresAt
			out.BindingHash = row.BindingHash
			out.StepUp = invitation.RequiresStepUp(row.Role)
			return out, nil
		},

		func(ctx context.Context, invitationId, bindingHash string) error {
			return store.BindUserInvitation(auth.ContextWithInternalOrigin(ctx), invitationId, bindingHash)
		},

		func(ctx context.Context, plainToken, sourceIP string) (identityweb.InvitationAcceptResult, error) {
			internalCtx := auth.ContextWithInternalOrigin(ctx)

			// RE-RESOLVED HERE, NOT TRUSTED FROM THE PAGE. This closure spends
			// a credential; it does its own lookup so the decision to spend is
			// made against the row as it is now, not as some earlier request
			// reported it.
			row, err := store.LookupInvitationByTokenHash(internalCtx, invitation.Hash(plainToken))
			if err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}
			if row == nil || !strings.EqualFold(strings.TrimSpace(row.Kind), "user") ||
				!row.Active || !strings.EqualFold(strings.TrimSpace(row.Status), "pending") ||
				(!row.ExpiresAt.IsZero() && !row.ExpiresAt.After(time.Now().UTC())) {
				return identityweb.InvitationAcceptResult{}, fmt.Errorf("identity: invitation is not redeemable")
			}

			// THE ORDER OF THE NEXT THREE WRITES IS THE WHOLE FAILURE STORY,
			// and it is chosen the way IssueUserInvitation chooses its own.
			//
			// User first. It is the durable thing the invitee actually needs,
			// and creating it twice is what we must avoid -- so it happens
			// once, before anything that could make us retry.
			//
			// Mark accepted second. This is what makes the invitation
			// single-use, and it must land BEFORE a usable credential exists:
			// if the process died between minting the enrolment token and
			// stamping the row, the invitation would still read as pending and
			// a forwarded copy could be redeemed again for a second account.
			// Marking first can only fail the other way -- a spent invitation
			// and no enrolment link -- which strands the invitee with a clear
			// message and a row an admin can see, instead of quietly leaving a
			// live credential behind.
			//
			// Enrolment token last, because it is the only one of the three the
			// caller can be handed again by simply issuing a fresh invitation.
			userId, err := identity.NewRandomId("")
			if err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}
			internal := cfg.IsInternalEmail(row.Email)
			role := strings.TrimSpace(row.Role)
			if role == "" && internal {
				role = cfg.InternalDefaultRole
			}
			seed := identity.UserProfileSeed{
				// Stamped at creation for the reason memql#4304 gives: the flag
				// should be right from the first sign-in rather than appearing
				// later, and the heuristic never runs again.
				SharedMailbox: registration.LooksLikeSharedMailbox(row.Email),
			}
			if err := store.CreateUserOnFirstLogin(internalCtx, userId, row.Email, row.Email, role, internal, seed); err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}
			if err := store.MarkUserInvitationAccepted(internalCtx, row.ID, userId); err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}

			plain, hash, err := enrolment.Mint()
			if err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}
			enrolmentId, err := enrolment.NewId()
			if err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}
			// issuedBy names the INVITATION rather than a person, because no
			// person issued this one -- the invitee's own click did, and the
			// authority for it was the invitation. An operator reading the
			// enrolment row should be able to see that without guessing.
			issuedBy := "invitation:" + row.ID
			expiresAt := time.Now().UTC().Add(enrolment.DefaultTTL)
			if err := enrolStore.Create(internalCtx, enrolmentId, userId, hash, issuedBy, expiresAt, sourceIP); err != nil {
				return identityweb.InvitationAcceptResult{}, err
			}

			return identityweb.InvitationAcceptResult{EnrolmentCode: plain, Email: row.Email}, nil
		},
		auditLogger,
	)

	// /me/devices passkey management (memql#3409). The SAME
	// *webauthn.Store the registration ceremony builds per-request in
	// component/identity/http -- so the list a user manages here and the
	// exclusion set the ceremony builds there can never disagree about
	// what is enrolled.
	webSrv.SetMePasskeys(&identityweb.MePasskeys{
		Adapter: &webauthn.Store{Engine: a.engine, Logger: a.Logger},
		Issuer:  svc.Issuer(),
		Audit:   auditLogger,
	})

	svc.SetWebMounter(webSrv)

	// What is left of the admin web app under /admin/*: the sign-in pages, and
	// an /admin/ root that says where the console went. Every page it served
	// is in the MemQL portal now -- six moved in memql#3324 (writes and
	// owner/admin gate together, the gate landing in
	// component/identity/adminops and reached over MemqlService.Stream), and
	// Deployments followed in memql#3380 once a deploy call could cross the
	// mesh from a bff-served portal to the node that owns the on-disk overlay
	// checkout.
	adminSrv, err := admin.New(&admin.AdminServer{
		Cfg:       cfg,
		Engine:    a.engine,
		Issuer:    svc.Issuer(),
		Audit:     auditLogger,
		Settings:  liveSettings,
		WebServer: webSrv,
		Logger:    a.Logger,
	})
	if err != nil {
		a.fatal("failed to construct identity admin server", "error", err, "component", identity.ComponentName)
	}
	svc.SetAdminMounter(adminSrv)

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

	// The owner recovery key is an INVARIANT, evaluated here on every start
	// (memql#3965): "if a cluster owner exists and there is no active,
	// unredeemed recovery key, mint one".
	//
	// AFTER attemptAutoBootstrap, deliberately -- that call is what names the
	// owner on an env-bootstrapped cluster (memql#3591), so running before it
	// would find no owner on precisely the install that has one. On a cluster
	// claimed by first sign-in instead, this is a no-op now and mints on the
	// next start, which is the ordinary case and is why the rule is an
	// invariant rather than a one-shot.
	//
	// Warnings, not fatal, for the same reason auto-bootstrap's failures are:
	// a cluster that cannot mint a break-glass key must still serve auth. The
	// warning is loud because the absence is silent otherwise.
	a.ensureOwnerRecoveryKey(context.Background(), store)

	a.startAuthActivityRetention(cfg)
}

// startAuthActivityRetention arms the daily hard-delete over
// v1:identity:authActivity (memql#4330).
//
// WHY A GO JOB AND NOT AN AUTOMATION. The DSL sweep beside it,
// auditEventRetentionSweep, only COUNTS -- MemQL has no delete() mutation, and
// an append-only audit log cannot be soft-deleted via active=false without
// changing what the log means. authActivity is a different kind of record: one
// row per rotation and one per PAT-authenticated request, value that decays in
// weeks, and no compliance story. Leaving it to grow forever is not a posture,
// it is a table nobody pruned. So this deletes from the node table directly,
// on the pattern component/node/delivery_store_pg.go already established.
//
// THE DIRECT (non-pooled) HANDLE, for the reason ensureOwnerRecoveryKey gives:
// a transaction-mode PgBouncer recycles the backend between statements. There
// is no advisory lock here -- deletes are idempotent, so two replicas sweeping
// at once race harmlessly -- but the getter is the same one, and using the
// pooled handle for a multi-statement select-then-delete would be the shape
// that bites next.
//
// Every replica runs it. That is deliberate and is what makes the job survive
// a leader disappearing; the cost is a few extra no-op queries a day.
func (a *App) startAuthActivityRetention(cfg identity.Config) {
	getDB := a.directDBGetter()
	if getDB == nil {
		a.Logger.Warn("identity: no database handle, so authActivity retention will not run; the "+
			"activity log will grow without bound and refresh-token reuse detection will keep "+
			"finding rows it should have forgotten",
			"component", identity.ComponentName)
		return
	}
	pruner := &authactivity.Pruner{
		DB: func() *sql.DB {
			bdb := getDB()
			if bdb == nil {
				return nil
			}
			return bdb.DB
		},
		Retention: cfg.AuthActivityRetention,
		Logger:    a.Logger,
	}
	// Background, not tied to a request: the App has no shutdown context to
	// hand it here, and the loop exits with the process. Run() sweeps once
	// IMMEDIATELY -- a pod that restarts daily would otherwise never prune.
	go pruner.Run(context.Background())
	a.Logger.Info("identity: authActivity retention armed",
		"retention_days", int(cfg.AuthActivityRetention.Hours()/24),
		"component", identity.ComponentName)
}

// ensureOwnerRecoveryKey evaluates the recovery-key invariant (memql#3965).
//
// THE PLAINTEXT IS DISCARDED HERE AND THAT IS THE DESIGN. EnsureForAllOwners
// returns it, and this caller drops it on the floor: a plaintext
// owner-equivalent credential must never reach a pod log, which means never
// reaching a log aggregator or whatever ships those logs off the cluster. The
// operator obtains it exactly once, on demand, through
// `memql recovery-key claim` (memql#3969).
func (a *App) ensureOwnerRecoveryKey(ctx context.Context, store *identity.Store) {
	if store == nil || a.engine == nil {
		return
	}
	// The DIRECT (non-pooled) handle. A transaction-mode PgBouncer recycles the
	// backend between statements and would silently drop the advisory lock
	// (epic memql#1925) -- and a dropped lock here means two replicas each mint
	// a live owner credential, which is the exact failure the lock exists for.
	getDB := a.directDBGetter()
	// THE CREDENTIAL ACTOR, NOT THE SERVICE ACTOR, and the distinction is the
	// difference between this invariant working and doing nothing at all.
	//
	// `recovery_key` is one of the machineCredentialIdentityTypes, so the
	// memql#2513 guard admits the write only from a SYSTEM actor --
	// isSystemActor wants role=="system" or an actor string prefixed
	// "system:". ContextWithSystemActor satisfies neither: it stamps
	// role="owner", and it sets an email, which ActorFromToken PREFERS over
	// the subject -- so the actor resolves to "system@identity.memql.local"
	// and the "system:identity-svc" subject that would have passed the prefix
	// check is never consulted. ContextWithSystemCredentialActor sets
	// role="system" with no email, which is what it exists for; its doc
	// comment names this guard by name.
	//
	// The failure this caused was silent and total. The mint died here on
	// every boot, so no cluster ever had a break-glass key, and the only trace
	// was one WARN per boot. Attribution is unaffected: who minted the key is
	// carried by the payload's `mintedBy` (below), not by the actor.
	res, err := recoverykey.EnsureForAllOwners(identity.ContextWithSystemCredentialActor(ctx), recoverykey.EnsureOptions{
		DB: func() *sql.DB {
			bdb := getDB()
			if bdb == nil {
				return nil
			}
			return bdb.DB
		},
		Store:    &recoverykey.Store{Engine: a.engine, Logger: a.Logger},
		Owners:   store,
		MintedBy: "system:identity-svc",
		Logger:   a.Logger,
	})
	if err != nil {
		a.Logger.Warn("identity: recovery-key invariant did not complete; this cluster may have no break-glass route for its owner",
			"error", err, "component", identity.ComponentName)
		return
	}
	if res.Minted > 0 {
		a.Logger.Info("identity: owner recovery key minted; claim it with `memql recovery-key claim` inside the identity pod",
			"minted", res.Minted, "component", identity.ComponentName)
	}
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

	// Decide what to do from the cluster's CLAIMED state, not from
	// bootstrappedAt alone (memql#1864). EvaluateAutoBootstrap gates the
	// one-time claim email on "was the cluster ever claimed" — an
	// existing owner user is definitional proof it was, even when the
	// bootstrappedAt stamp went missing. Crucially it is FAIL-SAFE: a
	// non-nil error means a boot-time DB read failed (e.g. during the
	// #1858 53300 storm) and we CANNOT determine state, so we must NOT
	// send the email. Bounded retry first; on persistent error, log +
	// return without emailing. Never fall through to send on error.
	action, err := a.evaluateAutoBootstrapWithRetry(ctx, store)
	if err != nil {
		a.Logger.Warn("identity auto-bootstrap skipped: could not determine cluster claim state (DB read error); NOT sending the claim email (fail-safe)",
			"error", err,
			"owner_email", cfg.Bootstrap.OwnerEmail,
			"component", identity.ComponentName)
		return
	}
	switch action {
	case identity.BootstrapActionSkip:
		a.Logger.Info("identity auto-bootstrap skipped: cluster already bootstrapped",
			"component", identity.ComponentName)
		return
	case identity.BootstrapActionSelfHeal:
		// An owner user exists but bootstrappedAt was never stamped (the
		// verifier's stamp write was swallowed on a prior boot, memql#1864).
		// The cluster IS claimed — reconcile the stamp so /setup 404s and
		// IsClusterBootstrapped reports true, and do NOT email. Stamp under
		// the system actor (mutation requires one).
		stampCtx := identity.ContextWithSystemActor(ctx)
		if stampErr := store.StampClusterBootstrapped(stampCtx); stampErr != nil {
			a.Logger.Warn("identity auto-bootstrap: self-heal stamp failed; will retry on next boot; NOT sending the claim email (owner already exists)",
				"error", stampErr,
				"owner_email", cfg.Bootstrap.OwnerEmail,
				"component", identity.ComponentName)
			return
		}
		a.Logger.Info("identity auto-bootstrap: self-healed missing bootstrappedAt stamp (owner already exists); claim email suppressed",
			"owner_email", cfg.Bootstrap.OwnerEmail,
			"component", identity.ComponentName)
		return
	case identity.BootstrapActionSuppress:
		// Idempotency (memql#1829): clusterSettings row already present but
		// no owner yet. The claim email was already sent on the boot that
		// created the row; re-running would spam the owner on every restart.
		// The owner claims via the original email's magic link; a lost email
		// is re-issued explicitly through /setup, NOT by an identity restart.
		a.Logger.Info("identity auto-bootstrap skipped: clusterSettings already present (awaiting owner claim); not re-sending the claim email on restart",
			"owner_email", cfg.Bootstrap.OwnerEmail,
			"component", identity.ComponentName)
		return
	case identity.BootstrapActionSend:
		// Truly fresh cluster (no stamp, no owner, no row, reads succeeded).
		// Fall through to persist the row + send exactly one claim email.
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

	// NAME THE OWNER NOW (memql#3591).
	//
	// Until this existed, an env bootstrap produced a clusterSettings row and a
	// magic link and NO user: the owner came into being inside the magic-link
	// verifier, on the click (Store.CreateUserOnFirstLogin -- the name says when).
	// So the install's last step, which mints a passkey-enrolment link for the
	// owner, was asking for a credential naming somebody who would not exist until
	// after the install had ended.
	//
	// WHAT THIS DOES NOT GRANT. A user row is not a way in. Access still requires
	// one of the two credentials -- clicking the magic link issued below, or
	// enrolling a passkey with a single-use token -- and neither exists yet. That
	// is exactly why the auto-bootstrap guard now asks HasClaimedOwner
	// (credentials) rather than HasOwnerUser (rows): naming an owner must not make
	// the cluster look claimed.
	//
	// Non-fatal, like everything else here: an operator can still reach /setup, and
	// the click path creates the user itself when this did not.
	a.provisionBootstrapOwner(ctx, cfg, store)

	// MEMQL_EMAIL_SUPPRESS_OWNER_BOOTSTRAP suppresses the owner magic-link email
	// (memql#374). It is a manifest-listed variable (scripts/secrets/manifest.yaml),
	// so an operator repeatedly repaving with `make up-refresh` seeds it once and
	// iterative DB wipes stop producing an inbox storm (memql#4405: this used to
	// name a `dev-refresh` make target -- one the Makefile has never had -- and
	// attributed the setting to it rather than to the seed path that carries it).
	// The operator is the same person across refreshes + the clusterSettings row
	// is already persisted above, so the cluster is functionally bootstrapped
	// without the email.
	// Unset in staging / production deploys; behaviour there is unchanged.
	if os.Getenv("MEMQL_EMAIL_SUPPRESS_OWNER_BOOTSTRAP") != "" {
		a.Logger.Info("identity auto-bootstrap: owner email suppressed by MEMQL_EMAIL_SUPPRESS_OWNER_BOOTSTRAP",
			"owner_email", cfg.Bootstrap.OwnerEmail,
			"domain", cfg.Bootstrap.Domain,
			"component", identity.ComponentName)
		return
	}

	// UNBOUND, and it has to be: nobody is holding a browser (memql#4302).
	//
	// Every other issue path answers a request FROM a browser and hands that
	// browser the binding nonce, so the link completes only there. This one
	// runs in a boot-time goroutine and emails the configured owner. A bound
	// link here would be approvable from anywhere and completable nowhere --
	// an env-bootstrapped cluster nobody can claim, with the operator sent to
	// /setup to get a bound link and the same outcome by a longer route.
	//
	// So this link keeps the pre-memql#4302 behaviour: whoever opens it
	// completes it. That is the trust this path always had -- it goes to the
	// address the operator configured, on a cluster with no owner credential
	// yet, and it is the credential that creates the first one.
	if _, err := mlIssuer.Issue(ctx, magiclink.IssueInput{
		Email:        cfg.Bootstrap.OwnerEmail,
		State:        "setup",
		Bootstrap:    true,
		AdminSession: true,
		Unbound:      true,
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

// provisionBootstrapOwner writes the owner user row for an env bootstrap, if it is
// not already there (memql#3591).
//
// IDEMPOTENT BY LOOKUP, not by write. `CreateUserOnFirstLogin` inserts a new
// time-series version under a fresh id, so calling it on every boot would
// accumulate owner rows -- and `HasOwnerUser` counts them. The email is the
// identity, so an existing user with that address IS this owner, whether a
// previous boot wrote it or the operator has since signed in.
//
// The profile fields come from the same clusterSettings row the /setup wizard
// seeds from, so an owner named here and an owner named by a click are the same
// shape. Role owner + internal, matching the bootstrap branch in the magic-link
// verifier: this is the wizard-issued owner mint, and the values are the ones that
// branch would have applied.
func (a *App) provisionBootstrapOwner(
	ctx context.Context,
	cfg identity.Config,
	store *identity.Store,
) {
	email := strings.TrimSpace(cfg.Bootstrap.OwnerEmail)
	if email == "" {
		return
	}
	existing, err := store.LookupUserByEmail(ctx, email)
	if err != nil {
		a.Logger.Warn("identity auto-bootstrap: owner lookup failed; the click path will create the user",
			"error", err, "owner_email", email, "component", identity.ComponentName)
		return
	}
	if existing != nil && existing.ID != "" {
		a.Logger.Info("identity auto-bootstrap: owner already named",
			"owner_email", email, "component", identity.ComponentName)
		return
	}

	userId, err := identity.NewRandomId("")
	if err != nil {
		a.Logger.Warn("identity auto-bootstrap: generate owner id failed; the click path will create the user",
			"error", err, "component", identity.ComponentName)
		return
	}
	displayName := strings.TrimSpace(cfg.Bootstrap.OwnerFirstName + " " + cfg.Bootstrap.OwnerLastName)
	if displayName == "" {
		displayName = email
	}
	seed := identity.UserProfileSeed{
		FirstName:   cfg.Bootstrap.OwnerFirstName,
		LastName:    cfg.Bootstrap.OwnerLastName,
		Phone:       cfg.Bootstrap.OwnerPhone,
		PrimaryRole: cfg.Bootstrap.OwnerPrimaryRole,
		Gender:      cfg.Bootstrap.OwnerGender,
		Birthdate:   cfg.Bootstrap.OwnerBirthdate,
	}
	// The engine requires a system actor for any mutation, exactly as the
	// clusterSettings write above does.
	writeCtx := identity.ContextWithSystemActor(ctx)
	if err := store.CreateUserOnFirstLogin(writeCtx, userId, displayName, email, "owner", true, seed); err != nil {
		a.Logger.Warn("identity auto-bootstrap: create owner user failed; the click path will create the user",
			"error", err, "owner_email", email, "component", identity.ComponentName)
		return
	}
	a.Logger.Info("identity auto-bootstrap: owner named; no credential exists yet, so the cluster is not claimed until the first sign-in",
		"owner_email", email, "user_id", userId, "component", identity.ComponentName)
}

// autoBootstrapReadRetries bounds how many times the claim-email guard
// re-reads cluster state before giving up. The #1858 53300 connection
// storm is transient (seconds), so a couple of short retries usually
// rides through it and lets the guard make a definitive decision
// instead of failing safe (which is correct but leaves the owner
// un-emailed on a genuinely fresh cluster).
const autoBootstrapReadRetries = 3

// autoBootstrapRetryDelay is the gap between guard read retries.
var autoBootstrapRetryDelay = 500 * time.Millisecond

// evaluateAutoBootstrapWithRetry runs EvaluateAutoBootstrap, retrying
// on a read error up to autoBootstrapReadRetries times. A retried error
// is almost always the transient DB storm (#1858); a definitive
// decision (any action, nil error) returns immediately. If every
// attempt errors, the final error propagates so the caller fails safe
// and does NOT send the claim email.
func (a *App) evaluateAutoBootstrapWithRetry(
	ctx context.Context,
	store identity.BootstrapGuardStore,
) (identity.BootstrapAction, error) {
	var action identity.BootstrapAction
	var err error
	for attempt := 0; attempt < autoBootstrapReadRetries; attempt++ {
		action, err = identity.EvaluateAutoBootstrap(ctx, store)
		if err == nil {
			return action, nil
		}
		if attempt < autoBootstrapReadRetries-1 {
			a.Logger.Warn("identity auto-bootstrap: cluster-state read failed; retrying before fail-safe",
				"error", err, "attempt", attempt+1,
				"component", identity.ComponentName)
			select {
			case <-ctx.Done():
				return identity.BootstrapActionSkip, ctx.Err()
			case <-time.After(autoBootstrapRetryDelay):
			}
		}
	}
	return identity.BootstrapActionSkip, err
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
	// #2800: server-side identity lookup.
	res, err := l.store.Engine.Execute(auth.ContextWithInternalOrigin(ctx),
		`query userByIdSystem(userId: "`+escapeMemQLArg(userId)+`")`)
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
