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

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/registration"
)

// Sentinel errors the HTTP layer translates to user-facing messages.
var (
	ErrInvalidToken      = errors.New("magiclink: invalid token")
	ErrTokenExpired      = errors.New("magiclink: token expired")
	ErrTokenAlreadyUsed  = errors.New("magiclink: token already consumed")
	ErrOAuthCtxCorrupted = errors.New("magiclink: oauth context unparseable")
)

// RoleFloorError is returned by Finish when the link was valid, the person is
// who they say they are, and the CLIENT that started the sign-in declares a
// role floor this user does not meet (identity.CheckClientRoleFloor).
//
// It is a type rather than a sentinel because the caller has to do more than
// pick a message: the refusal has to reach the relying party as a standard
// OAuth error redirect, so the handler needs the redirect URI and state that
// were already validated on this row. A refused sign-in is an ANSWER, not a
// breakage, and the extension that asked deserves to be told which role it
// needs rather than left to time out.
type RoleFloorError struct {
	Refusal identity.RoleFloorRefusal
	// RedirectURI + State come off the consumed row and were validated
	// against the client earlier in Finish, so building the error redirect
	// from them cannot become an open redirect.
	RedirectURI string
	State       string
}

func (e *RoleFloorError) Error() string { return e.Refusal.Description() }

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

// Inspect looks a magic-link token up and reports whether it is usable.
// IT WRITES NOTHING (memql#4302, design D3).
//
// This is the read half that GET /auth/complete needs. A GET used to
// consume the link before any human had interacted with it, which is what
// let Outlook SafeLinks, Gmail's proxy and every mail-security appliance
// burn a link by scanning it -- and what let the recipient of a shared
// mailbox spend a link somebody else had asked for. The click now renders a
// page; the state change is a POST from a page a human is looking at.
//
// Returns the row on success and one of ErrInvalidToken / ErrTokenExpired /
// ErrTokenAlreadyUsed otherwise. Note there is no audit row on the happy
// path: rendering a page is not an event, and auditing every prefetch would
// bury the clicks that matter.
func (v *Verifier) Inspect(ctx context.Context, plainToken, sourceIP, userAgent string) (*identity.MagicLinkRow, error) {
	if v == nil {
		return nil, errors.New("magiclink: nil verifier")
	}
	if v.Store == nil {
		return nil, errors.New("magiclink: nil store")
	}
	plain := strings.TrimSpace(plainToken)
	if plain == "" {
		return nil, ErrInvalidToken
	}
	in := VerifyInput{PlainToken: plain, SourceIP: sourceIP, UserAgent: userAgent}

	row, err := v.Store.LookupMagicLinkByTokenHash(ctx, HashMagicLinkToken(plain))
	if err != nil {
		return nil, fmt.Errorf("magiclink: lookup: %w", err)
	}
	if row == nil {
		v.auditFailure(ctx, "magic_link_inspect", in, "token_not_found")
		return nil, ErrInvalidToken
	}
	now := time.Now().UTC()
	if !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt) {
		v.auditFailure(ctx, "magic_link_inspect", in, "expired")
		return nil, ErrTokenExpired
	}
	if !row.ConsumedAt.IsZero() {
		v.auditFailure(ctx, "magic_link_inspect", in, "already_consumed")
		return nil, ErrTokenAlreadyUsed
	}
	return row, nil
}

// FinishInput names the request that completes a sign-in.
//
// It carries a request ID rather than a token because the finisher is the
// REQUESTING browser, which never saw the token -- it holds the binding
// cookie and the request id rendered on its own /check-email page. The
// caller has already checked the cookie against the row's bindingHash; this
// function's job is the exactly-once consume and everything downstream of
// it.
type FinishInput struct {
	RequestId string
	SourceIP  string
	UserAgent string
	// CrossDevice records whether this completion followed an approval from
	// a different device. Audit only -- it changes no behaviour, and exists
	// so an operator reading the trail can tell "I clicked my own link" from
	// "somebody else clicked and I finished".
	CrossDevice bool
}

// Finish consumes a magic-link request end-to-end:
//
//  1. Re-read the row by id.
//  2. Reject if not found / expired / already consumed.
//  3. Consume EXACTLY ONCE (Store.ConsumeMagicLinkRequest is a
//     compare-and-swap under an advisory lock -- memql#4301).
//  4. Decode oauthCtx.
//  5. Ensure-or-create the v1:identity:user (+ identity row).
//  6. Mint a one-time auth code.
//  7. Audit the consume and the completion.
//
// The returned VerifyResult.AuthCode is the plaintext code the handler
// appends to the redirect URL; the row is keyed by codeHash so the
// /oauth/token redemption handler hashes it again at lookup time.
//
// THE `state` COMPARISON IS GONE, and its absence is the design rather than
// an omission. `state` used to ride in the emailed URL and be compared
// against the row, which proved only that the clicker had the link. It no
// longer travels in the email at all: the row keeps it, and the finisher
// proves itself with the binding cookie -- possession of a value only the
// requesting browser holds, which is strictly stronger than echoing back a
// value that was printed in the email. `state` is still echoed to the
// relying party, unchanged.
func (v *Verifier) Finish(ctx context.Context, fin FinishInput) (*VerifyResult, error) {
	if v == nil {
		return nil, errors.New("magiclink: nil verifier")
	}
	if v.Store == nil {
		return nil, errors.New("magiclink: nil store")
	}
	in := VerifyInput{SourceIP: fin.SourceIP, UserAgent: fin.UserAgent}

	row, err := v.Store.LookupMagicLinkById(ctx, strings.TrimSpace(fin.RequestId))
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

	// EXACTLY ONCE. The store re-reads the row inside an advisory lock and
	// writes only if consumedAt is still empty, so the loser of a race
	// between the poller and a same-device click is TOLD it lost rather than
	// quietly minting a second auth code from one link (memql#4301).
	if err := v.Store.ConsumeMagicLinkRequest(ctx, row.ID, fin.SourceIP); err != nil {
		if errors.Is(err, identity.ErrMagicLinkAlreadyConsumed) {
			v.auditFailure(ctx, "magic_link_consume", in, "already_consumed")
			return nil, ErrTokenAlreadyUsed
		}
		v.auditFailure(ctx, "magic_link_consume", in, "consume_mutation_failed")
		return nil, fmt.Errorf("magiclink: stamp consumedAt: %w", err)
	}

	// Decode the OAuth ctx.
	clientId, redirectURI, state, codeChallenge, codeChallengeMethod, bootstrap, adminSession, err := decodeOAuthCtx(row.OAuthCtxJSON)
	if err != nil {
		v.auditFailure(ctx, "magic_link_consume", in, "oauth_ctx_corrupt")
		return nil, ErrOAuthCtxCorrupted
	}

	// Validate the registered redirect URI is still kosher (defensive:
	// catches operators removing a redirectURI between issue and consume).
	// Skipped for admin-session links — they carry no clientId.
	if !adminSession {
		if identity.ResolveClient(ctx, v.Cfg, v.Store, clientId) == nil ||
			!identity.ClientAllowsRedirectURI(ctx, v.Cfg, v.Store, clientId, redirectURI) {
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
	// effectiveRole is the cluster-wide role this sign-in carries -- read off
	// an existing row, or the one about to be written for a first login. The
	// role floor below reads it, which is why it is hoisted out of the
	// create-user branch rather than declared inside it.
	effectiveRole := ""
	if user != nil && user.ID != "" {
		userId = user.ID
		effectiveRole = user.Role
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
		// THE SHARED-MAILBOX HINT, STAMPED AT CREATION (memql#4304). The
		// heuristic runs exactly once, here, at the moment the account comes
		// into existence -- so the flag is right from the first sign-in
		// rather than appearing later and looking like an accusation. It
		// blocks nothing; the user or an admin can clear it in one click.
		seed := identity.UserProfileSeed{
			SharedMailbox: registration.LooksLikeSharedMailbox(row.Email),
		}
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
		effectiveRole = role
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
	}

	// STAMPED WHETHER OR NOT THE USER WAS NEW (memql#3591). This block used to sit
	// inside the "first login, create the user" branch, so a bootstrap link
	// consumed by an owner who ALREADY had a row stamped nothing. That was
	// unreachable while the only way an owner row appeared was this very branch;
	// the env bootstrap now names the owner up front, which makes it the ordinary
	// path -- and an unstamped claim leaves /setup reachable on a claimed cluster
	// until the next boot's self-heal notices.
	if bootstrap {
		// Retry the stamp once before giving up. A swallowed failure
		// here is the root cause of memql#1864: the owner row lands but
		// bootstrappedAt stays empty, so the cluster looks "unclaimed"
		// forever and the auto-bootstrap path re-emails on every deploy.
		// Keeping it non-fatal (the login must still succeed) but adding
		// a retry, backed by the boot-time self-heal in attemptAutoBootstrap
		// (EvaluateAutoBootstrap -> BootstrapActionSelfHeal), makes the
		// stamp durable: a transient miss reconciles on the next boot
		// instead of recurring.
		stampErr := v.Store.StampClusterBootstrapped(ctx)
		if stampErr != nil {
			if v.Logger != nil {
				v.Logger.Warn("magiclink: stamp bootstrapped failed; retrying once",
					slog.String("error", stampErr.Error()))
			}
			stampErr = v.Store.StampClusterBootstrapped(ctx)
		}
		if stampErr != nil {
			if v.Logger != nil {
				v.Logger.Warn("magiclink: stamp bootstrapped failed after retry; owner row exists, next identity boot self-heals the stamp (memql#1864)",
					slog.String("error", stampErr.Error()))
			}
			// Non-fatal: the user row exists, the link consume
			// succeeded. The boot-time self-heal in attemptAutoBootstrap
			// reconciles bootstrappedAt on the next identity start.
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

	// THE ROLE FLOOR (memql#4516). One of FOUR places a signed-in person is
	// known at the moment a credential would be minted, all calling the one
	// rule, identity.CheckClientRoleFloor:
	//
	//   this file                                    magic-link sign-in
	//   http/webauthn_login.go                       a passkey assertion
	//   web/redirect_authenticated.go                the /authorize SSO fast path
	//   web/device.go                                the RFC 8628 approval
	//
	// The first three are all the CODE flow -- it can reach an auth code three
	// different ways, and a floor on one factor is not a floor. If you are
	// adding a fifth way to mint, it belongs on this list.
	//
	// It runs AFTER the user row exists (being refused the editor is not a
	// reason to refuse someone an account -- they may sign into the portal a
	// moment later) and BEFORE the auth code, so nothing redeemable is ever
	// created. The link is already consumed at this point, deliberately: a
	// refused attempt must not leave a live link behind to retry.
	if !adminSession {
		if refusal := identity.CheckClientRoleFloor(clientId, auth.Role(effectiveRole)); refusal != nil {
			v.audit(ctx, identity.AuditEvent{
				Category:      identity.AuditCategoryIdentity,
				Action:        identity.AuditActionRoleFloorRefused,
				TargetType:    "magicLinkRequest",
				TargetId:      row.ID,
				TargetEmail:   row.Email,
				ActorUserId:   userId,
				ActorEmail:    row.Email,
				ActorRole:     effectiveRole,
				SourceIP:      in.SourceIP,
				UserAgent:     in.UserAgent,
				Outcome:       identity.AuditOutcomeBlocked,
				FailureReason: "role_below_client_floor",
				Detail:        refusal.AuditDetail(),
			})
			return nil, &RoleFloorError{
				Refusal:     *refusal,
				RedirectURI: redirectURI,
				State:       state,
			}
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

	// plainCode is NOT passed: only the digest is persisted (issue #3187).
	// It stays in memory here and travels back to the caller in
	// VerifyResult.AuthCode for the redirect to the OAuth client.
	if err := v.Store.CreateAuthCode(
		ctx,
		codeId,
		codeHash,
		clientId,
		redirectURI,
		state,
		codeChallenge,
		codeChallengeMethod,
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

	// magic_link_completed is the SECOND row, and it is not redundant with
	// magic_link_consumed. Consumed says the credential was spent; completed
	// says which shape of flow spent it. `cross_device` means somebody else
	// clicked and the requesting browser finished -- the alias case the
	// design exists for. An operator reading a trail wants to see that
	// without joining two tables.
	completionMode := "same_device"
	if fin.CrossDevice {
		completionMode = "cross_device"
	}
	v.audit(ctx, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      "magic_link_completed",
		TargetType:  "magicLinkRequest",
		TargetId:    row.ID,
		TargetEmail: row.Email,
		ActorUserId: userId,
		SourceIP:    in.SourceIP,
		UserAgent:   in.UserAgent,
		Outcome:     identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"mode": completionMode,
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
func decodeOAuthCtx(blob string) (clientId, redirectURI, state, codeChallenge, codeChallengeMethod string, bootstrap, adminSession bool, err error) {
	blob = strings.TrimSpace(blob)
	if blob == "" {
		return "", "", "", "", "", false, false, errors.New("empty oauth ctx")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		return "", "", "", "", "", false, false, err
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
	codeChallenge = asString("codeChallenge")
	codeChallengeMethod = asString("codeChallengeMethod")
	bootstrap = asBool("bootstrap")
	adminSession = asBool("adminSession")
	if !adminSession && (clientId == "" || redirectURI == "") {
		return "", "", "", "", "", false, false, errors.New("oauth ctx missing required fields")
	}
	return clientId, redirectURI, state, codeChallenge, codeChallengeMethod, bootstrap, adminSession, nil
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
