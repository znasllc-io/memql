package magiclink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// Sentinel errors the HTTP layer translates to user-facing messages.
var (
	ErrInvalidToken      = errors.New("magiclink: invalid token")
	ErrTokenExpired      = errors.New("magiclink: token expired")
	ErrTokenAlreadyUsed  = errors.New("magiclink: token already consumed")
	ErrStateMismatch     = errors.New("magiclink: state parameter does not match issuance context")
	ErrOAuthCtxCorrupted = errors.New("magiclink: oauth context unparseable")
)

// Verifier consumes magic-link tokens. Like Issuer, it is constructed
// once at app boot.
type Verifier struct {
	Cfg    identity.Config
	Store  *identity.Store
	Audit  identity.AuditLogger
	Logger *slog.Logger
}

// VerifyInput is the per-request payload from GET /auth/complete.
type VerifyInput struct {
	PlainToken string
	State      string
	SourceIP   string
	UserAgent  string
}

// VerifyResult is what the HTTP handler needs to construct the final
// redirect back to the OAuth client.
type VerifyResult struct {
	UserId      string
	IdentityId  string
	Email       string
	ClientId    string
	RedirectURI string
	State       string
	AuthCode    string // plaintext one-time code, returned to the OAuth client
	AuthCodeId  string
	NewUser     bool

	// Bootstrap=true when the consumed magic-link row was issued by
	// the /setup wizard (server-stamped at issue time, never from a
	// request body). The /auth/complete handler skips the OAuth code
	// flow and signs the user straight into /admin/ when this is set.
	Bootstrap bool

	// AdminSession=true when the consumed magic-link row was issued
	// by an identity-admin /login submission with no relying party
	// in scope (server-stamped at issue time). Same effect as
	// Bootstrap on the /auth/complete handler -- start an admin
	// session, redirect to /admin/ -- but a returning user, not a
	// first-run owner mint.
	AdminSession bool
}

// Verify consumes a magic-link token end-to-end:
//
//  1. Hash plaintext, look up the row.
//  2. Reject if not found / expired / already consumed.
//  3. Stamp consumedAt (single-use guarantee).
//  4. Decode oauthCtx; verify state param if both sides have it.
//  5. Ensure-or-create the v1:identity:user (+ identity row).
//  6. Mint a one-time auth code.
//  7. Audit the consume.
//
// The returned VerifyResult.AuthCode is the plaintext code the HTTP
// handler appends to the redirect URL; the row is keyed by codeHash so
// the /oauth/token redemption handler hashes it again at lookup time.
func (v *Verifier) Verify(ctx context.Context, in VerifyInput) (*VerifyResult, error) {
	if v == nil {
		return nil, errors.New("magiclink: nil verifier")
	}
	if v.Store == nil {
		return nil, errors.New("magiclink: nil store")
	}

	plain := strings.TrimSpace(in.PlainToken)
	if plain == "" {
		return nil, ErrInvalidToken
	}
	tokenHash := HashMagicLinkToken(plain)

	row, err := v.Store.LookupMagicLinkByTokenHash(ctx, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("magiclink: lookup: %w", err)
	}
	if row == nil {
		v.auditFailure(ctx, "magic_link_consume", in, "token_not_found")
		return nil, ErrInvalidToken
	}

	now := time.Now().UTC()
	if !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt) {
		v.auditFailure(ctx, "magic_link_consume", in, "expired")
		return nil, ErrTokenExpired
	}
	if !row.ConsumedAt.IsZero() {
		v.auditFailure(ctx, "magic_link_consume", in, "already_consumed")
		return nil, ErrTokenAlreadyUsed
	}

	// Stamp consumedAt FIRST so a double-click race condition can't
	// double-issue auth codes. If a concurrent verify already consumed
	// the row, the second verify still proceeds here (because we read
	// consumedAt above as zero) but its CreateAuthCode will mint a
	// distinct codeId — both codes work but each consumes once at
	// /oauth/token. The narrow race is acceptable for v1; the proper
	// fix is a CAS-style consume mutation.
	if err := v.Store.ConsumeMagicLinkRequest(ctx, row.ID, in.SourceIP); err != nil {
		v.auditFailure(ctx, "magic_link_consume", in, "consume_mutation_failed")
		return nil, fmt.Errorf("magiclink: stamp consumedAt: %w", err)
	}

	// Decode the OAuth ctx.
	clientId, redirectURI, issuedState, bootstrap, adminSession, err := decodeOAuthCtx(row.OAuthCtxJSON)
	if err != nil {
		v.auditFailure(ctx, "magic_link_consume", in, "oauth_ctx_corrupt")
		return nil, ErrOAuthCtxCorrupted
	}

	// State param check: if BOTH sides have it, they must match.
	// Missing on either side is fine (older clients don't send state).
	if issuedState != "" && in.State != "" && issuedState != in.State {
		v.auditFailure(ctx, "magic_link_consume", in, "state_mismatch")
		return nil, ErrStateMismatch
	}
	state := issuedState
	if state == "" {
		state = in.State
	}

	// Validate the registered redirect URI is still kosher (defensive:
	// catches operators removing a redirectURI between issue and consume).
	// Skipped for admin-session links — they carry no clientId.
	if !adminSession {
		if v.Cfg.FindClient(clientId) == nil || !v.Cfg.AllowsRedirectURI(clientId, redirectURI) {
			v.auditFailure(ctx, "magic_link_consume", in, "client_or_redirect_revoked")
			return nil, ErrOAuthCtxCorrupted
		}
	}

	// Ensure user exists. If this is a brand-new email, provision a
	// v1:identity:user + a magic_link v1:identity:identity row.
	user, err := v.Store.LookupUserByEmail(ctx, row.Email)
	if err != nil {
		return nil, fmt.Errorf("magiclink: user lookup: %w", err)
	}

	newUser := false
	var userId string
	if user != nil && user.ID != "" {
		userId = user.ID
	} else {
		// First login: create the user.
		newUser = true
		uid, err := identity.NewRandomId("")
		if err != nil {
			return nil, fmt.Errorf("magiclink: generate user id: %w", err)
		}
		userId = uid
		internal := v.Cfg.IsInternalEmail(row.Email)
		role := ""
		if internal {
			role = v.Cfg.InternalDefaultRole
		}
		// Bootstrap path: this is the wizard-issued owner-mint. Always
		// promote to owner, mark internal so the user gets the
		// cluster-wide role + skips the personal-partition path that
		// external users get. The bootstrap flag is server-stamped on
		// the magicLinkRequest row; an attacker can't forge it.
		if bootstrap {
			role = "owner"
			internal = true
		}
		// Display name defaults to local part of email; admin UI can
		// override it later.
		displayName := defaultDisplayName(row.Email)
		// Bootstrap path: the /setup wizard captured the owner's first
		// name, last name, phone, role, gender, and birthdate alongside
		// the email. Pull them off the clusterSettings row and stamp
		// them onto the freshly minted user so the operator doesn't have
		// to re-type everything from /admin/users/detail. Failure is
		// non-fatal -- worst case the operator fills the fields in via
		// the admin UI.
		seed := identity.UserProfileSeed{}
		if bootstrap {
			if cs, err := v.Store.ReadClusterSettings(ctx); err == nil && cs != nil {
				seed.FirstName = cs.BootstrapFirstName
				seed.LastName = cs.BootstrapLastName
				seed.Phone = cs.BootstrapPhone
				seed.PrimaryRole = cs.BootstrapPrimaryRole
				seed.Gender = cs.BootstrapGender
				seed.Birthdate = cs.BootstrapBirthdate
				if seed.FirstName != "" || seed.LastName != "" {
					displayName = strings.TrimSpace(seed.FirstName + " " + seed.LastName)
				}
			} else if err != nil && v.Logger != nil {
				v.Logger.Warn("magiclink: read clusterSettings for bootstrap seed failed",
					slog.String("error", err.Error()))
			}
		}
		if err := v.Store.CreateUserOnFirstLogin(ctx, userId, displayName, row.Email, role, internal, seed); err != nil {
			return nil, fmt.Errorf("magiclink: create user: %w", err)
		}
		v.audit(ctx, identity.AuditEvent{
			Category:    identity.AuditCategoryIdentity,
			Action:      "user_registered",
			TargetType:  "user",
			TargetId:    userId,
			TargetEmail: row.Email,
			SourceIP:    in.SourceIP,
			UserAgent:   in.UserAgent,
			Outcome:     identity.AuditOutcomeSuccess,
			Detail: map[string]any{
				"internal": internal,
				"role":     role,
			},
		})

		// Bootstrap path: the wizard didn't stamp bootstrappedAt at
		// submission time precisely so /setup remained accessible
		// until the operator clicked the email and proved control.
		// Now that the click happened and the owner row is in,
		// stamp the cluster as fully bootstrapped. Subsequent
		// /setup hits will 404; subsequent /login hits will route
		// the operator straight to a magic-link instead of waitlist.
		if bootstrap {
			if err := v.Store.StampClusterBootstrapped(ctx); err != nil {
				if v.Logger != nil {
					v.Logger.Warn("magiclink: stamp bootstrapped failed",
						slog.String("error", err.Error()))
				}
				// Non-fatal: the user row exists, the link consume
				// succeeded. The next admin-settings save (or a
				// retry-on-cluster-stamp) will reconcile.
			} else {
				v.audit(ctx, identity.AuditEvent{
					Category:    identity.AuditCategoryConfiguration,
					Action:      "cluster_bootstrapped",
					TargetType:  "clusterSettings",
					TargetId:    "cluster",
					TargetEmail: row.Email,
					ActorUserId: userId,
					SourceIP:    in.SourceIP,
					UserAgent:   in.UserAgent,
					Outcome:     identity.AuditOutcomeSuccess,
				})
			}
		}
	}

	// Always create a magic_link identity row keyed deterministically
	// off (userId, email-hash) so re-using the same email is idempotent
	// (a new time-series version under the same id rather than a new
	// row).
	identityId := composeMagicLinkIdentityId(userId, row.Email)
	if err := v.Store.CreateIdentityMagicLink(ctx, identityId, userId, "Magic-link sign-in"); err != nil {
		// Non-fatal: log + continue. The user row exists; the
		// missing identity row only affects the admin UI's "credentials"
		// list.
		if v.Logger != nil {
			v.Logger.Warn("magiclink: create identity row failed",
				slog.String("error", err.Error()))
		}
	}

	// Mint the auth code.
	plainCode, codeHash, err := newAuthCode()
	if err != nil {
		return nil, fmt.Errorf("magiclink: generate auth code: %w", err)
	}
	codeId, err := identity.NewRandomId("")
	if err != nil {
		return nil, fmt.Errorf("magiclink: generate code id: %w", err)
	}
	codeExpiresAt := now.Add(60 * time.Second).Format(time.RFC3339Nano)

	if err := v.Store.CreateAuthCode(
		ctx,
		codeId,
		plainCode,
		codeHash,
		clientId,
		redirectURI,
		state,
		userId,
		identityId,
		row.ID,
		codeExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("magiclink: persist auth code: %w", err)
	}

	v.audit(ctx, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "magic_link_consumed",
		TargetType:  "magicLinkRequest",
		TargetId:    row.ID,
		TargetEmail: row.Email,
		ActorUserId: userId,
		SourceIP:    in.SourceIP,
		UserAgent:   in.UserAgent,
		Outcome:     identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"clientId": clientId,
			"newUser":  newUser,
		},
	})

	return &VerifyResult{
		UserId:       userId,
		IdentityId:   identityId,
		Email:        row.Email,
		ClientId:     clientId,
		RedirectURI:  redirectURI,
		State:        state,
		AuthCode:     plainCode,
		AuthCodeId:   codeId,
		NewUser:      newUser,
		Bootstrap:    bootstrap,
		AdminSession: adminSession,
	}, nil
}

// audit is a nil-guard around the AuditLogger.
func (v *Verifier) audit(ctx context.Context, ev identity.AuditEvent) {
	if v == nil || v.Audit == nil {
		return
	}
	v.Audit.Log(ctx, ev)
}

// auditFailure emits a blocked/failed event with a fixed shape so the
// admin audit view can group them.
func (v *Verifier) auditFailure(ctx context.Context, action string, in VerifyInput, reason string) {
	v.audit(ctx, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		SourceIP:      in.SourceIP,
		UserAgent:     in.UserAgent,
		Outcome:       identity.AuditOutcomeFailure,
		FailureReason: reason,
	})
}

// decodeOAuthCtx parses the JSON blob the issuer stamped onto the
// magic-link row. Returns (clientId, redirectURI, state, bootstrap,
// adminSession, err).
//
// Both `bootstrap` and `adminSession` are trust markers stamped by
// the issuer (never from a request body). When either is true the
// /auth/complete handler routes the click straight to /admin/
// instead of bouncing through a relying-party OAuth callback.
//   - bootstrap     = wizard owner-mint (first-run setup)
//   - adminSession  = identity-admin sign-in (returning admin user
//     who landed on /login without a client app)
//
// In the adminSession path the row carries no clientId / redirectURI
// because there is no relying party; we surface them as empty
// strings and skip the "missing required fields" check.
func decodeOAuthCtx(blob string) (clientId, redirectURI, state string, bootstrap, adminSession bool, err error) {
	blob = strings.TrimSpace(blob)
	if blob == "" {
		return "", "", "", false, false, errors.New("empty oauth ctx")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		return "", "", "", false, false, err
	}
	asString := func(k string) string {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	asBool := func(k string) bool {
		if v, ok := m[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
		return false
	}
	clientId = asString("clientId")
	redirectURI = asString("redirectURI")
	state = asString("state")
	bootstrap = asBool("bootstrap")
	adminSession = asBool("adminSession")
	if !adminSession && (clientId == "" || redirectURI == "") {
		return "", "", "", false, false, errors.New("oauth ctx missing required fields")
	}
	return clientId, redirectURI, state, bootstrap, adminSession, nil
}

// newAuthCode mints a plaintext URL-safe base64 auth code + its
// SHA-256 hex hash.
func newAuthCode() (plain, hash string, err error) {
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

// HashAuthCode hashes a plaintext auth code with the same algorithm
// the verifier used. Exposed for the /oauth/token handler.
func HashAuthCode(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// defaultDisplayName returns a reasonable default for the user's
// display name when first registering — the local part of their email,
// title-cased.
func defaultDisplayName(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return email
	}
	local := email[:at]
	if local == "" {
		return email
	}
	// Crude title-case on the first character only; admin UI can edit.
	return strings.ToUpper(local[:1]) + local[1:]
}

// composeMagicLinkIdentityId builds the deterministic bare-slug id
// used for the per-user magic_link v1:identity:identity row. The
// engine canonicalizes the bare slug to
// `v1:identity:identity:<slug>` on insert.
//
// Determinism: hash (normalized userId, normalized email). Stable
// across email-case changes (fold to lower) and across multiple
// magic-link sends for the same address. Stable also across the
// userId being passed in either bare-slug or canonical form -- we
// strip the canonical prefix before hashing so both shapes hash the
// same.
func composeMagicLinkIdentityId(userId, email string) string {
	bareUser := userId
	const userCanonicalPrefix = "v1:identity:user:"
	if strings.HasPrefix(bareUser, userCanonicalPrefix) {
		bareUser = strings.TrimPrefix(bareUser, userCanonicalPrefix)
	}
	bareUser = strings.ToLower(strings.TrimSpace(bareUser))
	normEmail := strings.ToLower(strings.TrimSpace(email))
	sum := sha256.Sum256([]byte(bareUser + "|" + normEmail))
	return hex.EncodeToString(sum[:8])
}
