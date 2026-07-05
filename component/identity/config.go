// Package identity implements the in-house identity service — the
// authentication provider for the cluster.
//
// The identity service is a separate node type compiled with the
// `identity` build tag. It hosts the public auth web pages (magic-link
// login, registration, account management), the admin web app
// (user / session / partition / audit management), the OAuth-style
// token endpoints (POST /oauth/token, POST /auth/refresh), and the
// JWKS endpoint that every other node uses to verify access tokens.
//
// The package is split into focused files:
//
//	config.go     -- Config struct, env-var loading, validation
//	keys.go       -- Ed25519 keypair management (generate, persist,
//	                 encrypt-at-rest, rotate)
//	jwt.go        -- Access-token issue + verify primitives
//	jwks.go       -- JWKS publishing endpoint + per-node verifier cache
//	audit.go      -- Append-only audit logger interface (DB + slog)
//	identity.go   -- Top-level Service struct that ties the pieces
//	                 together and exposes Start / Shutdown
//
// The package surface covers: magic-link issuance + verification,
// access-request / waitlist flows, the web app templates + assets,
// anti-abuse middleware, per-node JWT verifier middleware that other
// binaries use to validate tokens against the JWKS feed, the admin
// web app, and PAT auth for CLI clients.
package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ComponentName is the slog component label for log entries emitted
// by this package.
const ComponentName = "identity"

// Default values for env-driven configuration. Anything that has a
// hardcoded default here must be safe for production. Anything that
// MUST be configured per-deployment is left blank and validated in
// Validate() so misconfigurations fail fast at startup rather than
// silently doing the wrong thing.
const (
	// DefaultKeyDir is where the Ed25519 keypair files live on disk.
	// Relative to the working directory in dev. Override in
	// staging/prod via MEMQL_IDENTITY_KEY_DIR.
	DefaultKeyDir = "var/identity/keys"

	// DefaultAccessTokenTTLSeconds is the lifetime of issued access
	// tokens. 15 minutes balances "short enough to limit XSS blast
	// radius" against "long enough to avoid hammering /auth/refresh".
	DefaultAccessTokenTTLSeconds = 900 // 15 min

	// Bounds enforced by the admin-form validator. Picked to keep
	// the operator from setting a value that's clearly broken (a
	// 1-second access token would burn through provider rate limits
	// on the first page load) while still letting operators tune
	// for their own threat model.
	MinAccessTokenTTLSeconds  = 60
	MaxAccessTokenTTLSeconds  = 24 * 60 * 60 // 24 h
	MinRefreshTokenTTLSeconds = 24 * 60 * 60 // 1 d
	MaxRefreshTokenTTLSeconds = 365 * 24 * 60 * 60
	MinMagicLinkTTLSeconds    = 60
	MaxMagicLinkTTLSeconds    = 60 * 60
	MinInvitationTTLDays      = 1
	MaxInvitationTTLDays      = 90

	// DefaultRefreshTokenTTLSeconds is the absolute lifetime of a
	// refresh token. 30 days is the consumer-SaaS norm; idle and
	// max-age policies are enforced separately.
	DefaultRefreshTokenTTLSeconds = 2_592_000 // 30 days

	// DefaultNodeTokenTTLSeconds is the lifetime of issued node-class
	// access tokens (#105). 30 days balances "long enough that ops
	// doesn't rotate weekly" against "short enough that a leaked
	// node token doesn't outlive its deploy window." There's no
	// refresh path for node tokens -- they're minted out-of-band
	// and copied into MEMQL_NODE_TOKEN on the target binary.
	DefaultNodeTokenTTLSeconds = 30 * 24 * 60 * 60 // 30 days

	// DefaultVoiceAgentTokenTTLSeconds is the lifetime of issued
	// voice-agent service-account tokens (#109). 90 days matches
	// the worker_token rotation cadence -- the voice-agent runs
	// long-lived in production and rotating monthly is excessive.
	// No refresh path; rotate by minting fresh + restarting.
	DefaultVoiceAgentTokenTTLSeconds = 90 * 24 * 60 * 60 // 90 days

	// DefaultServiceAccountTokenTTLSeconds is the lifetime of issued
	// class="service_account" tokens (#691, deployment-v2 Phase 3). Short by
	// design: the machine principal (deploy gate / automation) mints one per
	// run, so a leaked synthetic credential expires quickly. Rotate by minting
	// fresh; no refresh path.
	DefaultServiceAccountTokenTTLSeconds = 60 * 60 // 1 hour

	// DefaultMagicLinkTTLSeconds is how long a magic-link is valid.
	DefaultMagicLinkTTLSeconds = 600 // 10 min

	// DefaultInvitationTTLDays is how long an admin-issued invitation
	// remains usable before expiring.
	DefaultInvitationTTLDays = 7

	// DefaultSessionIdleDays controls the inactivity timeout. If
	// lastRefreshedAt + IdleDays < now, refresh fails.
	DefaultSessionIdleDays = 14

	// DefaultSessionMaxDays controls the absolute session lifetime.
	// firstAuthenticatedAt + MaxDays < now triggers re-auth.
	DefaultSessionMaxDays = 90

	// DefaultKeyRotationDays drives the auto-rotation cadence for
	// the JWT signing keypair.
	DefaultKeyRotationDays = 90

	// DefaultJWKSOverlapHours is how long a retired key remains in
	// JWKS so in-flight access-token verification doesn't fail during
	// the rotation handoff.
	DefaultJWKSOverlapHours = 24

	// DefaultAuditLogRetentionDays is the in-DB retention window for
	// audit events. The structured slog stream is unbounded; operator
	// log retention applies there.
	DefaultAuditLogRetentionDays = 365

	// DefaultRiskThreshold is the score at which registration is
	// blocked. Range 0-100; lower = stricter.
	DefaultRiskThreshold = 50

	// DefaultRateLimitPerIPPerHour caps magic-link / access-request
	// volume from a single source IP.
	DefaultRateLimitPerIPPerHour = 10

	// DefaultAccessRequestNotifyThrottleMinutes batches admin
	// notification emails so a burst of requests doesn't spam the
	// inbox; high-risk requests bypass via immediate-notify.
	DefaultAccessRequestNotifyThrottleMinutes = 5

	// DefaultDataExportRateLimitHours caps how often a single user
	// can export their own data, to prevent compromised-account
	// exfiltration loops.
	DefaultDataExportRateLimitHours = 24

	// DefaultDeletionCooldownDays is the soft-delete grace period
	// before account-deletion becomes irreversible.
	DefaultDeletionCooldownDays = 30
)

// RegistrationMode controls who can self-register on this cluster.
type RegistrationMode string

const (
	// RegistrationModeOpen — anyone with any email can register.
	RegistrationModeOpen RegistrationMode = "open"
	// RegistrationModeDomainRestricted — emails must match an
	// allow-list (Config.RegistrationDomains).
	RegistrationModeDomainRestricted RegistrationMode = "domain_restricted"
	// RegistrationModeInviteOnly — no self-registration; users only
	// enter via admin-issued v1:identity:invitation rows.
	RegistrationModeInviteOnly RegistrationMode = "invite_only"
	// RegistrationModeWaitlist — users submit access requests; admin
	// approves into invitations.
	RegistrationModeWaitlist RegistrationMode = "waitlist"
)

// IsValid reports whether m is one of the four supported modes.
func (m RegistrationMode) IsValid() bool {
	switch m {
	case RegistrationModeOpen,
		RegistrationModeDomainRestricted,
		RegistrationModeInviteOnly,
		RegistrationModeWaitlist:
		return true
	}
	return false
}

// RegisteredClient describes a relying-party application allowed to
// initiate the OAuth-style code flow against this identity service.
// Loaded from the MEMQL_IDENTITY_REGISTERED_CLIENTS env var as a JSON array.
type RegisteredClient struct {
	ClientId     string   `json:"clientId"`
	RedirectURIs []string `json:"redirectURIs"`
	// Name is a human-friendly display name for consent UI (e.g. "Claude").
	// Populated from the RFC 7591 client_name of a dynamically-registered
	// client; empty for static config clients (the consent page falls back
	// to the clientId).
	Name string `json:"name,omitempty"`
}

// Config is the runtime configuration for the identity service.
// Loaded from environment variables via LoadConfigFromEnv. Fields
// without sensible production defaults are validated for non-empty
// presence in Validate().
//
// All fields are documented at the env-var name they're populated
// from to keep the operator-facing surface and the developer-facing
// surface aligned. See docs/public/operate/auth/identity-service.md (added in a
// subsequent phase) for the operator-side narrative.

// BootstrapConfig captures every value the first-run /setup wizard
// asks for. Each field maps 1:1 to an IDENTITY_BOOTSTRAP_<NAME>
// env var. This is the unattended-deploy path: set the env vars
// at deploy time and the cluster auto-bootstraps with no /setup
// visit. Operators who prefer the interactive path leave the
// env vars unset and fill the form on /setup.
type BootstrapConfig struct {
	// Domain is the cluster's deployment domain. Used as the suffix
	// for every public hostname: app.<Domain>, identity.<Domain>,
	// bff.<Domain>, agent.<Domain>. Examples:
	//   local.znas.io        (maintainer's local dev cluster)
	//   staging.acme.com     (a customer's staging deployment)
	//   acme.com             (the customer's production deployment)
	// Env: MEMQL_IDENTITY_BOOTSTRAP_DOMAIN
	Domain string

	// OwnerEmail is the email the cluster owner receives the
	// first sign-in magic link at. Becomes v1:identity:user.
	// primaryEmail on the auto-created owner row.
	// Env: MEMQL_IDENTITY_BOOTSTRAP_OWNER_EMAIL
	OwnerEmail string

	// OwnerFirstName + OwnerLastName seed the owner's profile so
	// the product's "Welcome, Jose" header reads correctly without a
	// follow-up edit. Env stamping pulls double duty: also baked
	// into the JWT given_name / family_name claims.
	// Env: MEMQL_IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME
	//      MEMQL_IDENTITY_BOOTSTRAP_OWNER_LAST_NAME
	OwnerFirstName string
	OwnerLastName  string

	// OwnerPhone, OwnerPrimaryRole, OwnerGender, OwnerBirthdate
	// are optional profile fields the wizard captures alongside
	// the required ones. The product's ProfileCompletenessGuard
	// looks for these; pre-filling them at bootstrap means the
	// owner skips straight into the app.
	// Env: IDENTITY_BOOTSTRAP_OWNER_{PHONE,PRIMARY_ROLE,GENDER,BIRTHDATE}
	OwnerPhone       string
	OwnerPrimaryRole string
	OwnerGender      string
	OwnerBirthdate   string

	// OrgName is the organization label captured by the wizard.
	// Reserved for future surfaces (admin UI, email templates);
	// no functional use today.
	// Env: MEMQL_IDENTITY_BOOTSTRAP_ORG_NAME
	OrgName string

	// RegistrationMode overrides the runtime MEMQL_IDENTITY_REGISTRATION_MODE
	// default at bootstrap time. When set here, the wizard's
	// registration-mode radio is preselected; when set without a
	// wizard visit, the cluster bootstraps with this mode in effect.
	// Env: MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_MODE
	RegistrationMode string

	// RegistrationDomains preselects the comma-separated allowlist
	// shown when RegistrationMode is "domain_restricted".
	// Env: MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_DOMAINS (comma-separated)
	RegistrationDomains []string

	// InternalDomains preselects the comma-separated allowlist that
	// flips users to internal=true at first login.
	// Env: MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS (comma-separated)
	InternalDomains []string

	// InternalDefaultRole is the cluster-wide role for internal users.
	// Env: MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DEFAULT_ROLE (owner|admin|writer|reader)
	InternalDefaultRole string

	// NotifyEmails preselects the waitlist-notification recipient
	// list (comma-separated). Used only when RegistrationMode is
	// "waitlist".
	// Env: MEMQL_IDENTITY_BOOTSTRAP_NOTIFY_EMAILS (comma-separated)
	NotifyEmails []string
}

// HasAllRequired reports whether the operator supplied every
// IDENTITY_BOOTSTRAP_* field needed for an unattended bootstrap.
// "Optional" fields (phone, gender, birthdate, etc.) don't gate;
// they prefill the wizard but their absence doesn't stop auto-
// bootstrap.
func (b BootstrapConfig) HasAllRequired() bool {
	return strings.TrimSpace(b.Domain) != "" &&
		strings.TrimSpace(b.OwnerEmail) != "" &&
		strings.TrimSpace(b.OwnerFirstName) != "" &&
		strings.TrimSpace(b.OwnerLastName) != "" &&
		strings.TrimSpace(b.RegistrationMode) != ""
}

type Config struct {
	// Enabled gates the whole service. When false, the identity
	// build-tag binary still compiles but the HTTP/gRPC handlers
	// aren't wired in. Useful in tests and during partial rollout.
	// Env: MEMQL_IDENTITY_ENABLED (bool, default false)
	Enabled bool

	// BaseURL is the public origin the identity service is reachable
	// at. Used as the JWT `iss` claim, in redirect targets, and in
	// links emitted into magic-link / invitation emails.
	// Env: MEMQL_IDENTITY_BASE_URL (e.g. "https://auth.znasllc.io")
	BaseURL string

	// JWTAudience is the value placed in the JWT `aud` claim.
	// Other nodes verify this matches their configured audience.
	// Env: MEMQL_IDENTITY_JWT_AUDIENCE (default "memql")
	JWTAudience string

	// KeyDir is the filesystem directory holding jwt-current.ed25519
	// and (during rotation overlap) jwt-previous.ed25519.
	// Env: MEMQL_IDENTITY_KEY_DIR (default "var/identity/keys")
	KeyDir string

	// KeyEncryptionKey is the master secret used to derive the
	// AES-256-GCM key that wraps the on-disk private keypair. When
	// empty, keys are stored in plaintext (dev mode). Required in
	// staging/prod — Validate() returns an error if BaseURL is set
	// to a non-localhost origin and this is empty.
	// Env: MEMQL_IDENTITY_KEY_ENCRYPTION_KEY (any string >= 16 bytes)
	KeyEncryptionKey string

	// SigningKeyB64 is a base64 (std) 32-byte Ed25519 seed that, when
	// set, becomes THE signing key for this process -- loaded from the
	// environment instead of generated onto a ReadWriteOnce PVC. Every
	// identity replica handed the same seed derives the same keypair +
	// kid + JWKS, so identity can run >=2 replicas on RollingUpdate (no
	// single-writer key volume -> no strategy:Recreate downtime). Rides
	// the sealed genesis envelope like the other secrets. Automatic
	// rotation is disabled in this mode (re-seal + roll to rotate).
	// Empty = legacy on-disk KeyDir behaviour (dev).
	// Env: MEMQL_IDENTITY_SIGNING_KEY_B64 (#550)
	SigningKeyB64 string

	// AllowEphemeralKey opts a deployment INTO the per-pod ephemeral
	// signing-key mode (no MEMQL_IDENTITY_SIGNING_KEY_B64). Without this, a
	// non-localhost deployment that leaves MEMQL_IDENTITY_SIGNING_KEY_B64
	// unset is REFUSED at startup: each replica would mint its own
	// file-backed key on its own ephemeral container filesystem, so
	// /.well-known/jwks.json diverges across replicas and ~half of all
	// token verifications (browser sessions AND mesh node tokens) fail
	// with `unknown kid`. That misconfiguration caused the 2026-06-16
	// staging auth outage (#1515): staging had no seed, so the 2
	// identity replicas published different JWKS and login flapped.
	//
	// Set this to true ONLY for single-node / dev deployments where one
	// identity replica owns the only key (or where a shared key volume
	// makes the on-disk key coherent). Multi-replica HA deployments MUST
	// instead set MEMQL_IDENTITY_SIGNING_KEY_B64 so every replica derives the
	// same key. localhost-shaped MEMQL_IDENTITY_BASE_URL origins are exempt
	// from the guard (local dev), so this knob is only needed when
	// running ephemeral keys against a real (non-localhost) origin.
	// Env: MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY (bool, default false)
	AllowEphemeralKey bool

	// AccessTokenTTL is the lifetime of issued access tokens.
	// Env: MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS (default 900)
	AccessTokenTTL time.Duration

	// RefreshTokenTTL is the absolute lifetime of refresh tokens.
	// Idle / max-age policies enforce earlier expiration in practice.
	// Env: MEMQL_IDENTITY_REFRESH_TOKEN_TTL_SECONDS (default 2592000)
	RefreshTokenTTL time.Duration

	// MagicLinkTTL is how long a freshly issued magic link works.
	// Env: MEMQL_IDENTITY_MAGIC_LINK_TTL_SECONDS (default 600)
	MagicLinkTTL time.Duration

	// InvitationTTL is how long an admin-issued user invitation
	// remains valid before expiring (waitlist and invite-only modes).
	// Env: MEMQL_IDENTITY_INVITATION_TTL_DAYS (default 7)
	InvitationTTL time.Duration

	// SessionIdleTimeout — refresh fails if lastRefreshedAt plus
	// this is in the past.
	// Env: MEMQL_IDENTITY_SESSION_IDLE_DAYS (default 14)
	SessionIdleTimeout time.Duration

	// SessionMaxAge — refresh fails if firstAuthenticatedAt plus
	// this is in the past, regardless of activity.
	// Env: MEMQL_IDENTITY_SESSION_MAX_DAYS (default 90)
	SessionMaxAge time.Duration

	// KeyRotationInterval drives the auto-rotation cron.
	// Env: MEMQL_IDENTITY_KEY_ROTATION_DAYS (default 90)
	KeyRotationInterval time.Duration

	// JWKSOverlapWindow is how long a retired key stays in JWKS so
	// in-flight tokens still verify.
	// Env: MEMQL_IDENTITY_JWKS_OVERLAP_HOURS (default 24)
	JWKSOverlapWindow time.Duration

	// AuditLogRetention is how long audit-event rows are retained
	// in the database. Slog output is unbounded.
	// Env: MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS (default 365)
	AuditLogRetention time.Duration

	// RegistrationMode — see RegistrationMode constants. Captured by
	// first-run wizard if env unset.
	// Env: MEMQL_IDENTITY_REGISTRATION_MODE (default "open")
	RegistrationMode RegistrationMode

	// RegistrationDomains is the allowlist consulted when
	// RegistrationMode == domain_restricted. Comma-separated.
	// Env: MEMQL_IDENTITY_REGISTRATION_DOMAINS
	RegistrationDomains []string

	// InternalDomains is the cluster's "internal users" allowlist.
	// Email-domain match flags v1:identity:user.internal=true.
	// Env: MEMQL_IDENTITY_INTERNAL_DOMAINS
	InternalDomains []string

	// InternalDefaultRole is the cluster-wide role assigned to
	// internal users on registration. External users get owner of
	// their personal partition only and have no cluster-wide role.
	// Env: MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE (default "writer")
	InternalDefaultRole string

	// Bootstrap captures the values needed at first-run cluster
	// setup. Each field has a matching IDENTITY_BOOTSTRAP_<NAME>
	// env var. When ALL the required fields are set (Domain,
	// OwnerEmail, OwnerFirstName, OwnerLastName, RegistrationMode),
	// the identity service auto-bootstraps on first start --
	// no operator visit to /setup is needed. When any required
	// field is missing, /setup waits for the operator and prefills
	// each form field with whatever env vars ARE set.
	//
	// After bootstrap, the wizard is closed (clusterSettings.
	// bootstrappedAt is non-empty); the env vars become inert.
	// Runtime config (registration mode, brand, registered clients,
	// etc.) is editable via /admin/settings -- the post-bootstrap
	// values live on the clusterSettings row, NOT in env.
	Bootstrap BootstrapConfig

	// BrandName — display name used in email subject prefixes and
	// templates. Captured by first-run wizard, editable in admin UI.
	// Env: MEMQL_IDENTITY_BRAND_NAME
	BrandName string

	// BrandPrimaryColor is the hex accent color in HTML emails and
	// the web UI primary button. Defaults to a deep blue (#0433ff)
	// matching the in-CSS :root --brand-primary fallback so a
	// freshly bootstrapped cluster renders consistently from
	// either side of the boundary.
	// Env: MEMQL_IDENTITY_BRAND_PRIMARY_COLOR (default "#0433ff")
	BrandPrimaryColor string

	// BrandLogoDataURI is the embedded horizontal logo (wordmark)
	// as base64 data URI. Up to ~200KB. Empty = no logo block.
	// Env: MEMQL_IDENTITY_BRAND_LOGO_DATA_URI
	BrandLogoDataURI string

	// BrandIconDataURI is the embedded square icon (icon-only mark)
	// as base64 data URI. Used in compact contexts like favicon and
	// share previews. Up to ~150KB. Empty = no icon.
	// Env: MEMQL_IDENTITY_BRAND_ICON_DATA_URI
	BrandIconDataURI string

	// RegisteredClients lists the relying-party applications allowed
	// to initiate auth flows against this identity service. Each
	// has a clientId and an explicit redirect-URI list (no
	// wildcards). Loaded as a JSON array from the env var.
	// Env: MEMQL_IDENTITY_REGISTERED_CLIENTS
	// Example:
	//   [{"clientId":"app","redirectURIs":["https://app.example.com/auth/callback"]}]
	RegisteredClients []RegisteredClient

	// OAuthDCREnabled gates the RFC 7591 dynamic client registration
	// endpoint (POST /register). When true (the default), unknown OAuth
	// clients -- e.g. claude.ai / Claude Desktop's "add custom connector"
	// -- self-register a public client_id they then use at /authorize +
	// /oauth/token. When false, POST /register returns 403
	// registration_disabled and persists nothing.
	// Env: MEMQL_IDENTITY_OAUTH_DCR_ENABLED (default true)
	OAuthDCREnabled bool

	// CORSAllowedOrigins is the set of Origin values identity will
	// allow on credentialed cross-origin requests (the refresh-token
	// cookie path). Comma-separated.
	// Env: MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS
	CORSAllowedOrigins []string

	// AccessRequestNotifyEmails — admin recipients for waitlist
	// notification emails. Empty = fall back to all owner+admin
	// users on the cluster.
	// Env: MEMQL_IDENTITY_ACCESS_REQUEST_NOTIFY_EMAILS (comma-separated)
	AccessRequestNotifyEmails []string

	// AccessRequestNotifyThrottle batches notifications.
	// Env: MEMQL_IDENTITY_ACCESS_REQUEST_NOTIFY_THROTTLE_MINUTES (default 5)
	AccessRequestNotifyThrottle time.Duration

	// RiskThreshold blocks registrations whose computed score is
	// at or above this number. 0-100; lower = stricter.
	// Env: MEMQL_IDENTITY_RISK_THRESHOLD (default 50)
	RiskThreshold int

	// RateLimitPerIPPerHour caps magic-link and access-request
	// submissions from a single source IP.
	// Env: MEMQL_IDENTITY_RATE_LIMIT_PER_IP_PER_HOUR (default 10)
	RateLimitPerIPPerHour int

	// DisposableEmailBlocklistEnabled toggles the embedded blocklist.
	// Env: MEMQL_IDENTITY_DISPOSABLE_EMAIL_BLOCKLIST_ENABLED (default true)
	DisposableEmailBlocklistEnabled bool

	// MXValidationEnabled toggles per-domain MX-record DNS check.
	// Env: MEMQL_IDENTITY_MX_VALIDATION_ENABLED (default true)
	MXValidationEnabled bool

	// EmailValidationProvider is the env-var stub for the future
	// third-party email-deliverability provider plug-in. Defaults
	// to "none"; reserved values include "kickbox", "verifalia",
	// "zerobounce". Full plug-in lands in a later iteration.
	// Env: MEMQL_IDENTITY_EMAIL_VALIDATION_PROVIDER (default "none")
	EmailValidationProvider string

	// TurnstileSiteKey + TurnstileSecret enable Cloudflare Turnstile
	// on registration and login forms.
	// Env: MEMQL_IDENTITY_TURNSTILE_SITE_KEY, MEMQL_IDENTITY_TURNSTILE_SECRET
	TurnstileSiteKey string
	TurnstileSecret  string

	// DataExportRateLimit caps per-user data-export frequency.
	// Env: MEMQL_IDENTITY_DATA_EXPORT_RATE_LIMIT_HOURS (default 24)
	DataExportRateLimit time.Duration

	// DeletionCooldown is the soft-delete grace window before a
	// scheduled account deletion becomes irreversible.
	// Env: MEMQL_IDENTITY_DELETION_COOLDOWN_DAYS (default 30)
	DeletionCooldown time.Duration

	// TosOverridePath / PrivacyOverridePath let operators replace
	// the embedded ToS / Privacy markdown with their own files at
	// runtime (mounted as a Cloud Run secret / config volume).
	// Empty = use the embedded defaults.
	// Env: MEMQL_IDENTITY_TOS_OVERRIDE_PATH, MEMQL_IDENTITY_PRIVACY_OVERRIDE_PATH
	TosOverridePath     string
	PrivacyOverridePath string

	// JWKSURL is the URL OTHER nodes use to fetch the public keys
	// for JWT verification. Defaults to BaseURL +
	// "/.well-known/jwks.json"; override only if the public-facing
	// URL differs from the internal-mesh URL.
	// Env: MEMQL_IDENTITY_JWKS_URL
	JWKSURL string

	// NodeBootstrapToken is the shared secret that gates the
	// `/node/bootstrap` HTTP endpoint. When set, every cluster
	// node (bff / agent / cognition / planner / voice) that starts
	// without `MEMQL_NODE_TOKEN` set can present this secret to mint
	// itself a fresh class="node" JWT instead of failing every
	// peer-to-peer call with "authorization header missing"
	// (memql#338). Distinct from the cluster-bootstrap wizard's
	// `Bootstrap` field above -- that's for first-run owner
	// onboarding, this is per-binary-restart node auth.
	//
	// When empty, the `/node/bootstrap` endpoint is disabled (returns
	// 503 -- "bootstrap not configured"). This is the safe default
	// for production deploys that mint per-node tokens out-of-band
	// via the operator CLI; the bootstrap endpoint is only enabled
	// when the operator opts in by setting the secret on both the
	// identity service and every node that should be allowed to
	// self-mint.
	//
	// Operational guidance: rotate this secret if exposed; every
	// node that needs to self-bootstrap will need the new value
	// pushed before its next restart. Token TTL is independent
	// (defaults to DefaultNodeTokenTTLSeconds = 30 days), so a
	// rotated bootstrap secret doesn't invalidate live node tokens.
	//
	// Env: MEMQL_NODE_BOOTSTRAP_TOKEN
	NodeBootstrapToken string
}

// LiveTokenSettings is the runtime-tunable knob set the http handlers
// consult on every issuance to pick up admin-edited values without
// an identity restart. Returned by Server.LiveTokenSettings (when
// configured) and threaded into the same code paths that previously
// only read s.Cfg.
//
// Each field carries an "effective" value: the runtime override
// when non-zero/non-empty, falling back to s.Cfg (which itself fell
// back to env / built-in default at boot). So a freshly bootstrapped
// cluster with no admin edits still gets sensible behaviour from the
// 15m / 30d / 10m / 7d defaults.
//
// RefreshCookieSameSite is "lax" or "none" (or "" to inherit env);
// the http layer maps "" to the env-derived default. The cookie is
// always Secure when BaseURL is HTTPS regardless of this setting.
type LiveTokenSettings struct {
	AccessTokenTTL        time.Duration
	RefreshTokenTTL       time.Duration
	MagicLinkTTL          time.Duration
	InvitationTTL         time.Duration
	RefreshCookieSameSite string
}

// LoadConfigFromEnv reads the IDENTITY_* environment variables and
// assembles a Config struct. Returns an error only for malformed
// values (e.g., a non-int where an int is required, an invalid JSON
// payload). Missing-but-required values are caught later by Validate.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Enabled:           envBool("MEMQL_IDENTITY_ENABLED", false),
		BaseURL:           strings.TrimRight(os.Getenv("MEMQL_IDENTITY_BASE_URL"), "/"),
		JWTAudience:       envString("MEMQL_IDENTITY_JWT_AUDIENCE", "memql"),
		KeyDir:            envString("MEMQL_IDENTITY_KEY_DIR", DefaultKeyDir),
		KeyEncryptionKey:  os.Getenv("MEMQL_IDENTITY_KEY_ENCRYPTION_KEY"),
		SigningKeyB64:     strings.TrimSpace(os.Getenv("MEMQL_IDENTITY_SIGNING_KEY_B64")),
		AllowEphemeralKey: envBool("MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY", false),
		Bootstrap: BootstrapConfig{
			Domain:              strings.TrimRight(os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN"), "/"),
			OwnerEmail:          os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_EMAIL"),
			OwnerFirstName:      os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME"),
			OwnerLastName:       os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_LAST_NAME"),
			OwnerPhone:          os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_PHONE"),
			OwnerPrimaryRole:    os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_PRIMARY_ROLE"),
			OwnerGender:         os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_GENDER"),
			OwnerBirthdate:      os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_OWNER_BIRTHDATE"),
			OrgName:             os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_ORG_NAME"),
			RegistrationMode:    os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_MODE"),
			RegistrationDomains: envStringList("MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_DOMAINS"),
			InternalDomains:     envStringList("MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS"),
			InternalDefaultRole: os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DEFAULT_ROLE"),
			NotifyEmails:        envStringList("MEMQL_IDENTITY_BOOTSTRAP_NOTIFY_EMAILS"),
		},
		BrandName:                       os.Getenv("MEMQL_IDENTITY_BRAND_NAME"),
		BrandPrimaryColor:               envString("MEMQL_IDENTITY_BRAND_PRIMARY_COLOR", "#0433ff"),
		BrandLogoDataURI:                os.Getenv("MEMQL_IDENTITY_BRAND_LOGO_DATA_URI"),
		BrandIconDataURI:                os.Getenv("MEMQL_IDENTITY_BRAND_ICON_DATA_URI"),
		InternalDefaultRole:             envString("MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE", "writer"),
		EmailValidationProvider:         envString("MEMQL_IDENTITY_EMAIL_VALIDATION_PROVIDER", "none"),
		TurnstileSiteKey:                os.Getenv("MEMQL_IDENTITY_TURNSTILE_SITE_KEY"),
		TurnstileSecret:                 os.Getenv("MEMQL_IDENTITY_TURNSTILE_SECRET"),
		TosOverridePath:                 os.Getenv("MEMQL_IDENTITY_TOS_OVERRIDE_PATH"),
		PrivacyOverridePath:             os.Getenv("MEMQL_IDENTITY_PRIVACY_OVERRIDE_PATH"),
		JWKSURL:                         os.Getenv("MEMQL_IDENTITY_JWKS_URL"),
		DisposableEmailBlocklistEnabled: envBool("MEMQL_IDENTITY_DISPOSABLE_EMAIL_BLOCKLIST_ENABLED", true),
		MXValidationEnabled:             envBool("MEMQL_IDENTITY_MX_VALIDATION_ENABLED", true),
		NodeBootstrapToken:              strings.TrimSpace(os.Getenv("MEMQL_NODE_BOOTSTRAP_TOKEN")),
		OAuthDCREnabled:                 envBool("MEMQL_IDENTITY_OAUTH_DCR_ENABLED", true),
	}

	cfg.AccessTokenTTL = envDurationSeconds("MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS", DefaultAccessTokenTTLSeconds)
	cfg.RefreshTokenTTL = envDurationSeconds("MEMQL_IDENTITY_REFRESH_TOKEN_TTL_SECONDS", DefaultRefreshTokenTTLSeconds)
	cfg.MagicLinkTTL = envDurationSeconds("MEMQL_IDENTITY_MAGIC_LINK_TTL_SECONDS", DefaultMagicLinkTTLSeconds)
	cfg.InvitationTTL = envDurationDays("MEMQL_IDENTITY_INVITATION_TTL_DAYS", DefaultInvitationTTLDays)
	cfg.SessionIdleTimeout = envDurationDays("MEMQL_IDENTITY_SESSION_IDLE_DAYS", DefaultSessionIdleDays)
	cfg.SessionMaxAge = envDurationDays("MEMQL_IDENTITY_SESSION_MAX_DAYS", DefaultSessionMaxDays)
	cfg.KeyRotationInterval = envDurationDays("MEMQL_IDENTITY_KEY_ROTATION_DAYS", DefaultKeyRotationDays)
	cfg.JWKSOverlapWindow = envDurationHours("MEMQL_IDENTITY_JWKS_OVERLAP_HOURS", DefaultJWKSOverlapHours)
	cfg.AuditLogRetention = envDurationDays("MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", DefaultAuditLogRetentionDays)
	cfg.AccessRequestNotifyThrottle = envDurationMinutes("MEMQL_IDENTITY_ACCESS_REQUEST_NOTIFY_THROTTLE_MINUTES", DefaultAccessRequestNotifyThrottleMinutes)
	cfg.DataExportRateLimit = envDurationHours("MEMQL_IDENTITY_DATA_EXPORT_RATE_LIMIT_HOURS", DefaultDataExportRateLimitHours)
	cfg.DeletionCooldown = envDurationDays("MEMQL_IDENTITY_DELETION_COOLDOWN_DAYS", DefaultDeletionCooldownDays)

	cfg.RegistrationMode = RegistrationMode(envString("MEMQL_IDENTITY_REGISTRATION_MODE", string(RegistrationModeOpen)))
	cfg.RegistrationDomains = envStringList("MEMQL_IDENTITY_REGISTRATION_DOMAINS")
	cfg.InternalDomains = envStringList("MEMQL_IDENTITY_INTERNAL_DOMAINS")
	cfg.CORSAllowedOrigins = envStringList("MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS")
	cfg.AccessRequestNotifyEmails = envStringList("MEMQL_IDENTITY_ACCESS_REQUEST_NOTIFY_EMAILS")

	cfg.RiskThreshold = envInt("MEMQL_IDENTITY_RISK_THRESHOLD", DefaultRiskThreshold)
	cfg.RateLimitPerIPPerHour = envInt("MEMQL_IDENTITY_RATE_LIMIT_PER_IP_PER_HOUR", DefaultRateLimitPerIPPerHour)

	clients, err := envRegisteredClients("MEMQL_IDENTITY_REGISTERED_CLIENTS")
	if err != nil {
		return cfg, fmt.Errorf("MEMQL_IDENTITY_REGISTERED_CLIENTS: %w", err)
	}
	cfg.RegisteredClients = clients

	if cfg.JWKSURL == "" && cfg.BaseURL != "" {
		cfg.JWKSURL = cfg.BaseURL + "/.well-known/jwks.json"
	}

	return cfg, nil
}

// Validate checks that required fields are populated when Enabled is
// true. Should be called after LoadConfigFromEnv at service startup.
// Returns the first error encountered; the operator fixes one thing
// at a time.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	if c.BaseURL == "" {
		return errors.New("MEMQL_IDENTITY_BASE_URL is required when MEMQL_IDENTITY_ENABLED=true")
	}

	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("MEMQL_IDENTITY_BASE_URL must be an absolute URL (got %q)", c.BaseURL)
	}

	if !c.RegistrationMode.IsValid() {
		return fmt.Errorf("MEMQL_IDENTITY_REGISTRATION_MODE %q is not one of: open, domain_restricted, invite_only, waitlist", c.RegistrationMode)
	}

	if c.RegistrationMode == RegistrationModeDomainRestricted && len(c.RegistrationDomains) == 0 {
		return errors.New("MEMQL_IDENTITY_REGISTRATION_DOMAINS must be set when MEMQL_IDENTITY_REGISTRATION_MODE=domain_restricted")
	}

	switch c.InternalDefaultRole {
	case "owner", "admin", "developer", "writer", "reader":
	default:
		return fmt.Errorf("MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE %q must be one of: owner, admin, developer, writer, reader", c.InternalDefaultRole)
	}

	// An env-provided signing key (#550) lives in the sealed genesis
	// envelope, not on disk, so the at-rest MEMQL_IDENTITY_KEY_ENCRYPTION_KEY
	// requirement does not apply -- but it MUST be a valid 32-byte seed.
	if c.SigningKeyB64 != "" {
		seed, err := base64.StdEncoding.DecodeString(c.SigningKeyB64)
		if err != nil {
			return fmt.Errorf("MEMQL_IDENTITY_SIGNING_KEY_B64 is not valid base64: %w", err)
		}
		if len(seed) != ed25519.SeedSize {
			return fmt.Errorf("MEMQL_IDENTITY_SIGNING_KEY_B64 must decode to %d bytes (an Ed25519 seed), got %d", ed25519.SeedSize, len(seed))
		}
	} else {
		// FAIL-FAST GUARD (#1515): no MEMQL_IDENTITY_SIGNING_KEY_B64 means the
		// signing key is FILE-backed on the pod's OWN ephemeral container
		// filesystem (MEMQL_IDENTITY_KEY_DIR, no shared volume in the cluster
		// manifest). With >=2 identity replicas each replica mints its
		// OWN key, so /.well-known/jwks.json diverges across replicas and
		// ~half of all token verifications (browser sessions AND mesh node
		// tokens) fail with `unknown kid`. That is exactly the 2026-06-16
		// staging auth outage. Refuse to start a non-localhost deployment
		// in per-pod ephemeral-key mode unless the operator explicitly
		// opts in via MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true (single-node / dev,
		// or a deployment with a shared key volume). localhost origins are
		// exempt -- that's the local-dev path.
		if !c.AllowEphemeralKey && !isLocalHost(parsed.Host) {
			return errors.New("identity: MEMQL_IDENTITY_SIGNING_KEY_B64 is not set, so each replica would mint its OWN per-pod ephemeral signing key and /.well-known/jwks.json would diverge across replicas -- ~50% of token verifications (browser sessions AND mesh node tokens) fail with 'unknown kid' (this caused the 2026-06-16 auth outage). Fix: set MEMQL_IDENTITY_SIGNING_KEY_B64 to a shared base64-std-encoded 32-byte Ed25519 seed on every identity replica (generate with: `head -c 32 /dev/urandom | base64`), so all replicas derive the same key. For a single-node / dev deployment with no HA, set MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true to opt into per-pod ephemeral keys")
		}

		// Production posture requires the master key. We approximate
		// "production" as "BaseURL is not localhost / 127.0.0.1 /
		// memql-internal." Operators running staging on a real domain
		// will hit this and configure the secret. Local dev hitting
		// localhost passes through with plaintext keys.
		if c.KeyEncryptionKey == "" && !isLocalHost(parsed.Host) {
			return errors.New("MEMQL_IDENTITY_KEY_ENCRYPTION_KEY is required when MEMQL_IDENTITY_BASE_URL is not a localhost origin (production deployments must encrypt the signing key at rest, or set MEMQL_IDENTITY_SIGNING_KEY_B64)")
		}

		if c.KeyEncryptionKey != "" && len(c.KeyEncryptionKey) < 16 {
			return errors.New("MEMQL_IDENTITY_KEY_ENCRYPTION_KEY must be at least 16 bytes")
		}
	}

	if c.RiskThreshold < 0 || c.RiskThreshold > 100 {
		return fmt.Errorf("MEMQL_IDENTITY_RISK_THRESHOLD must be between 0 and 100, got %d", c.RiskThreshold)
	}

	for _, client := range c.RegisteredClients {
		if client.ClientId == "" {
			return errors.New("MEMQL_IDENTITY_REGISTERED_CLIENTS: every entry must have a non-empty clientId")
		}
		if len(client.RedirectURIs) == 0 {
			return fmt.Errorf("MEMQL_IDENTITY_REGISTERED_CLIENTS: client %q has no redirectURIs", client.ClientId)
		}
		for _, uri := range client.RedirectURIs {
			if strings.Contains(uri, "*") {
				return fmt.Errorf("MEMQL_IDENTITY_REGISTERED_CLIENTS: client %q has wildcard in redirect URI %q (wildcards are forbidden — list every URI explicitly)", client.ClientId, uri)
			}
			if _, err := url.Parse(uri); err != nil {
				return fmt.Errorf("MEMQL_IDENTITY_REGISTERED_CLIENTS: client %q has malformed redirect URI %q: %w", client.ClientId, uri, err)
			}
		}
	}

	return nil
}

// FindClient returns the RegisteredClient with the given clientId, or
// nil if none is registered. The handler chain uses this to validate
// incoming OAuth code-flow requests.
func (c Config) FindClient(clientId string) *RegisteredClient {
	for i := range c.RegisteredClients {
		if c.RegisteredClients[i].ClientId == clientId {
			return &c.RegisteredClients[i]
		}
	}
	return nil
}

// AllowsRedirectURI returns true if the given uri is registered under
// the given clientId. Exact-match by default; one structured exception
// per RFC 8252 §7.3:
//
//	If a registered URI is a loopback redirect (host = 127.0.0.1 /
//	::1 / localhost) AND it has no explicit port, an incoming URI
//	matches when its scheme + host + path agree -- the port is
//	allowed to vary. This is what lets a native client (the cockpit)
//	listen on an ephemeral loopback port and still get its callback
//	through the OAuth code-flow validator.
//
// No wildcards, no prefix matching, no path-only matching. Production
// (non-loopback) URIs always go through exact-match.
func (c Config) AllowsRedirectURI(clientId, uri string) bool {
	return clientAllowsRedirectURI(c.FindClient(clientId), uri)
}

// clientAllowsRedirectURI applies the redirect-URI matcher to a single
// RegisteredClient: exact-match plus the RFC 8252 §7.3 loopback-any-port
// exception. Factored out of AllowsRedirectURI so the same rule applies
// to DB-backed (dynamically-registered) clients via the unified resolver
// (see client_resolver.go). nil client never matches.
func clientAllowsRedirectURI(client *RegisteredClient, uri string) bool {
	if client == nil {
		return false
	}
	candidate, err := url.Parse(uri)
	if err != nil {
		return false
	}
	for _, registered := range client.RedirectURIs {
		if registered == uri {
			return true
		}
		if matchesLoopbackAnyPort(registered, candidate) {
			return true
		}
	}
	return false
}

// matchesLoopbackAnyPort implements RFC 8252's loopback-redirect
// flexibility. The registered URI must be a loopback URL with no
// explicit port; the candidate must be a loopback URL with the same
// scheme + path. Any port is accepted on the candidate.
func matchesLoopbackAnyPort(registered string, candidate *url.URL) bool {
	if candidate == nil {
		return false
	}
	r, err := url.Parse(registered)
	if err != nil {
		return false
	}
	if r.Port() != "" {
		return false
	}
	if !isLoopbackHost(r.Hostname()) {
		return false
	}
	if !isLoopbackHost(candidate.Hostname()) {
		return false
	}
	return r.Scheme == candidate.Scheme && r.Path == candidate.Path
}

// IsInternalEmail reports whether email's domain matches the
// configured MEMQL_IDENTITY_INTERNAL_DOMAINS list (case-insensitive).
func (c Config) IsInternalEmail(email string) bool {
	domain := emailDomain(email)
	if domain == "" {
		return false
	}
	for _, d := range c.InternalDomains {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

// IsAllowedRegistrationDomain reports whether email is permitted to
// self-register under the current registration mode.
func (c Config) IsAllowedRegistrationDomain(email string) bool {
	if c.RegistrationMode != RegistrationModeDomainRestricted {
		return true
	}
	domain := emailDomain(email)
	if domain == "" {
		return false
	}
	for _, d := range c.RegistrationDomains {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

// emailDomain returns the lowercased domain portion of an email
// address, or "" if no `@` is present.
func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// isLocalHost returns true for hosts that should be treated as local
// development (skipping the encryption-at-rest requirement). Three
// shapes count as "local":
//
//  1. Literal loopback: localhost, 127.0.0.1, ::1, 0.0.0.0.
//  2. RFC 6761 .localhost TLD: anything.localhost.
//  3. The dev wildcard convention this repo locks in: any
//     *.local.<domain> shape (e.g. identity.local.znas.io,
//     bff.local.acme.test). The second-level label `local` is the
//     dev marker -- it pairs with the wildcard DNS record
//     `*.local.<domain>` -> 127.0.0.1 and the matching
//     `*.local.<domain>` mkcert SAN. Production deployments use
//     `<service>.<domain>` (no `local.` middle) and DO require
//     the encryption key.
func isLocalHost(host string) bool {
	// Strip the port. url.URL.Host comes through here in two
	// shapes: "host:port" (IPv4 / DNS name) and "[ipv6]:port"
	// or raw "ipv6" (IPv6). Only the first form has exactly one
	// colon -- IPv6 has multiple, so multi-colon means we're
	// looking at a bare IPv6 address with no port.
	if strings.Count(host, ":") == 1 {
		host = host[:strings.Index(host, ":")]
	} else if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 0 {
			host = host[1:end]
		}
	}
	host = strings.ToLower(host)
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	if strings.HasSuffix(host, ".localhost") {
		return true
	}
	// Dev wildcard shape: *.local.<domain>. The second label from
	// the left must be exactly "local" to mark the host as a dev
	// alias (not a real production hostname that happens to live
	// under a domain whose name contains "local").
	labels := strings.Split(host, ".")
	if len(labels) >= 3 && labels[1] == "local" {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Env-var helpers — kept private to this package since they encode
// the IDENTITY_* parsing conventions specifically.
// ---------------------------------------------------------------------------

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// Tolerate the "yes"/"no" / "on"/"off" convention some env
		// systems use; default to the fallback otherwise.
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "yes", "y", "on", "1":
			return true
		case "no", "n", "off", "0":
			return false
		}
		return fallback
	}
	return b
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDurationSeconds(key string, fallbackSeconds int) time.Duration {
	return time.Duration(envInt(key, fallbackSeconds)) * time.Second
}

func envDurationMinutes(key string, fallbackMinutes int) time.Duration {
	return time.Duration(envInt(key, fallbackMinutes)) * time.Minute
}

func envDurationHours(key string, fallbackHours int) time.Duration {
	return time.Duration(envInt(key, fallbackHours)) * time.Hour
}

func envDurationDays(key string, fallbackDays int) time.Duration {
	return time.Duration(envInt(key, fallbackDays)) * 24 * time.Hour
}

func envStringList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// envRegisteredClients parses the JSON array env-var format.
func envRegisteredClients(key string) ([]RegisteredClient, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	clients, err := parseRegisteredClientsJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return clients, nil
}
