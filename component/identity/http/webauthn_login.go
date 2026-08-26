package http

// POST /auth/webauthn/login/begin
// POST /auth/webauthn/login/finish
//
// The WebAuthn passkey LOGIN ceremony (memql#3407), and the load-bearing
// claim of the epic: a passkey assertion produces THE SAME AUTH CODE a
// magic-link click produces. /authorize, /oauth/token, the Cockpit, the
// portal, the SDK and the VS Code extension keep working with no
// knowledge that a second factor exists, because there is nothing for
// them to know -- what reaches them is an auth code.
//
// Two existing facts make that small rather than large:
//
//   - IssueMagicLink stamps clientId / redirectURI / state /
//     codeChallenge / codeChallengeMethod onto a magic-link row's
//     oauthCtx, and handleComplete decodes them and redirects with
//     code + state.
//   - Store.CreateAuthCode already mints a one-time OAuth code
//     independently of magic links.
//
// So this path stamps the same five fields onto its CHALLENGE instead of
// a magic-link row, and calls CreateAuthCode directly. The magic-link
// verifier is untouched; that separation is why it could be.
//
// UNAUTHENTICATED, unlike the registration pair. That is the point --
// this IS the authentication -- and it is what the rest of the file's
// caution is about:
//
//   - The relying party is validated at BEGIN, against the same
//     registered-client resolver /oauth/token uses, and then held
//     server-side on the challenge. A client that could restate the
//     redirect URI at finish time could aim someone else's freshly
//     minted code at itself.
//   - A ceremony with NO relying party in scope is refused rather than
//     silently succeeding into nowhere. The magic-link form remains the
//     path for the admin-session case (/login reached with no client),
//     which is exactly what makes the button a progressive enhancement
//     rather than a second front door. memql#4610 added ONE named
//     exception -- an explicit `firstParty: true` ceremony that mints no
//     auth code and produces only the browser session -- so that /enroll,
//     which has no relying party and never will, can end holding a
//     session instead of an instruction. Silence is still refused; only
//     the request that asks for that arm by name gets it, and the full
//     argument sits at the branch in handleWebAuthnLoginBegin.
//   - A sign-count regression is refused and audited as the spec's
//     cloned-authenticator signal. A zero counter from an authenticator
//     that does not implement one is not that case; see
//     webauthn.ErrSignCountRegression.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/webauthn"
)

// envPasskeyLoginPerHour tunes the per-IP login-attempt rate limit.
// Looser than registration's: signing in is the routine act and a user
// who fumbles the platform sheet retries a few times, whereas enrolling
// happens a handful of times per account per lifetime.
const envPasskeyLoginPerHour = "MEMQL_IDENTITY_PASSKEY_LOGIN_PER_HOUR"

const defaultPasskeyLoginPerHour = 240

// maxPasskeyLoginBody bounds the finish payload. An assertion is
// signature + authenticator data + client data -- under a kilobyte in
// practice; 64 KB matches the registration bound and still refuses a
// body meant to exhaust the parser.
const maxPasskeyLoginBody = 64 * 1024

// authCodeTTL is how long a minted auth code stays redeemable. Same 60
// seconds the magic-link verifier stamps: the code is handed straight to
// the client's callback, so its life is a redirect, not a user's
// attention span.
const authCodeTTL = 60 * time.Second

// WebAuthnLoginBeginRequest is the JSON body for
// POST /auth/webauthn/login/begin.
//
// It carries the in-flight OAuth context -- the same five fields the
// /login form threads through its hidden inputs into IssueMagicLink.
// They are VALIDATED here and then held server-side; nothing the client
// says at finish time can change where the code is delivered.
type WebAuthnLoginBeginRequest struct {
	ClientId            string `json:"clientId"`
	RedirectURI         string `json:"redirectUri"`
	State               string `json:"state,omitempty"`
	CodeChallenge       string `json:"codeChallenge,omitempty"`
	CodeChallengeMethod string `json:"codeChallengeMethod,omitempty"`

	// FirstParty asks for the arm that mints no auth code (memql#4610).
	// Mutually exclusive with the five fields above; the full argument for
	// why it is opt-in, and for why it is not a widening of what a passkey
	// assertion authorizes, is at the branch in handleWebAuthnLoginBegin.
	FirstParty bool `json:"firstParty,omitempty"`
}

// WebAuthnLoginBeginResponse carries the browser-ready request options
// plus the opaque challenge handle the finish step must present.
//
// RequestOptions marshals as {"publicKey": {...}}, exactly the argument
// navigator.credentials.get() takes, so a client passes it through
// untouched. Its allowCredentials is empty by construction.
type WebAuthnLoginBeginResponse struct {
	Success        bool                          `json:"success"`
	ChallengeId    string                        `json:"challengeId,omitempty"`
	ExpiresAt      string                        `json:"expiresAt,omitempty"`
	RelyingPartyId string                        `json:"relyingPartyId,omitempty"`
	RequestOptions *protocol.CredentialAssertion `json:"requestOptions,omitempty"`
	Error          string                        `json:"error,omitempty"`
	ErrorCode      string                        `json:"errorCode,omitempty"`
}

// WebAuthnLoginFinishRequest is the JSON body for
// POST /auth/webauthn/login/finish. Credential is the raw
// PublicKeyCredential the browser produced, forwarded verbatim.
type WebAuthnLoginFinishRequest struct {
	ChallengeId string          `json:"challengeId"`
	Credential  json.RawMessage `json:"credential"`
}

// WebAuthnLoginFinishResponse reports where the browser should go next.
//
// RedirectTo is the client callback target -- the same URL
// buildClientCallback produces for a magic-link consume, carrying `code`
// and the echoed `state`. The auth code is NOT surfaced as its own
// field: one delivery channel means a client cannot accidentally use the
// code for anything but the redirect it was minted for.
type WebAuthnLoginFinishResponse struct {
	Success    bool   `json:"success"`
	RedirectTo string `json:"redirectTo,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty"`
}

// passkeyLoginLimiter returns this Server's per-IP login limiter, built
// once on first use. Server-scoped for the same reason the registration
// limiter is.
func passkeyLoginLimiter(s *Server) *abuse.IPRateLimiter {
	s.passkeyLoginLimiterOnce.Do(func() {
		perHour := defaultPasskeyLoginPerHour
		if v := strings.TrimSpace(os.Getenv(envPasskeyLoginPerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		s.passkeyLoginLimiter = abuse.NewIPRateLimiter(perHour, s.Logger)
	})
	return s.passkeyLoginLimiter
}

// handleWebAuthnLoginBegin issues a usernameless assertion challenge
// bound to a validated relying party.
func (s *Server) handleWebAuthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		writeJSON(w, http.StatusInternalServerError, WebAuthnLoginBeginResponse{
			ErrorCode: "server", Error: "identity engine not wired"})
		return
	}
	ctx := r.Context()

	if allowed, retryAfter := passkeyLoginLimiter(s).Allow(clientIP(r)); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, WebAuthnLoginBeginResponse{
			ErrorCode: "rate_limited", Error: "too many passkey sign-in attempts from this address"})
		return
	}

	ceremony, err := s.webauthnCeremony()
	if err != nil {
		s.auditPasskey(r, "passkey_login_challenge_denied", "", "", identity.AuditOutcomeFailure, "relying_party_unavailable", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnLoginBeginResponse{
			ErrorCode: "webauthn_unconfigured", Error: err.Error()})
		return
	}

	var body WebAuthnLoginBeginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPasskeyLoginBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, WebAuthnLoginBeginResponse{
			ErrorCode: "bad_request", Error: "invalid JSON body: " + err.Error()})
		return
	}
	oauth := webauthn.OAuthContext{
		ClientId:            strings.TrimSpace(body.ClientId),
		RedirectURI:         strings.TrimSpace(body.RedirectURI),
		State:               strings.TrimSpace(body.State),
		CodeChallenge:       strings.TrimSpace(body.CodeChallenge),
		CodeChallengeMethod: strings.TrimSpace(body.CodeChallengeMethod),
	}

	// TWO ARMS, AND ONLY TWO.
	//
	//  1. A RELYING-PARTY ceremony (memql#3407), the original one. A client
	//     is named, validated here, and held on the challenge; the assertion
	//     ends in an auth code delivered to that client's registered
	//     callback. This is what the /login button drives.
	//  2. A FIRST-PARTY ceremony (memql#4610), asked for explicitly with
	//     `firstParty: true`. It mints NO auth code. Its whole product is
	//     the memql_admin cookie startBrowserSession already stamps on arm 1
	//     (memql#3920), plus a destination this server computes for itself.
	//     It exists because /enroll has no relying party and never will --
	//     an enrolment link is opened out of an email, not by an application
	//     -- so without it a person who has just made a passkey there is
	//     told to go and sign in with the credential they produced ten
	//     seconds earlier (memql#4610, and the friction memql#4601 exists to
	//     remove).
	//
	// ARM 2 GRANTS STRICTLY LESS THAN ARM 1, which is why it is not a new
	// boundary. The proof it demands is identical: a user-verified assertion
	// from a discoverable credential enrolled on this cluster, verified by
	// the same FinishLogin, with the same sign-count and revocation checks.
	// What it hands back is a SUBSET of arm 1's output -- the same browser
	// session, minus the auth code -- so nothing is reachable through it
	// that a client-bearing ceremony could not already reach. The refusal
	// below was always about DELIVERY rather than about authority: an auth
	// code with no client is a credential aimed nowhere, and a caller that
	// could name the target later could aim someone else's freshly minted
	// code at itself. Arm 2 mints no code, so it has nothing to misdeliver,
	// and its redirect target is read off this server's own configuration
	// rather than off the request.
	//
	// IT IS OPT-IN, NOT A FALLBACK. A request that merely forgot its client
	// is still refused with client_required, exactly as before: "no client
	// named" is far more often a caller's mistake than a deliberate
	// first-party sign-in, and a silent fallback would quietly convert every
	// one of those mistakes into a session nobody asked for. It also leaves
	// the /login page's posture untouched -- the button still renders only
	// with a relying party in scope, so it remains a progressive enhancement
	// beside the magic-link form rather than a second front door.
	switch {
	case body.FirstParty:
		if oauth.ClientId != "" || oauth.RedirectURI != "" {
			// One ceremony, one product. A request asking for the first-party
			// arm AND naming a client has not said what it wants, and the
			// thing that would have to be guessed is where a credential goes.
			s.auditPasskey(r, "passkey_login_challenge_denied", "", "", identity.AuditOutcomeBlocked, "first_party_names_a_client", map[string]any{
				"clientId": oauth.ClientId,
			})
			writeJSON(w, http.StatusBadRequest, WebAuthnLoginBeginResponse{
				ErrorCode: "ambiguous_ceremony",
				Error:     "firstParty cannot be combined with clientId or redirectUri"})
			return
		}
		// Zeroed rather than merely unvalidated: the challenge is what the
		// finish step reads its arm off, and an OAuth context carrying a
		// stray state or PKCE value would make "no client" a matter of
		// inspecting five fields instead of one.
		oauth = webauthn.OAuthContext{}

	case !oauth.HasClient():
		s.auditPasskey(r, "passkey_login_challenge_denied", "", "", identity.AuditOutcomeBlocked, "no_relying_party", nil)
		writeJSON(w, http.StatusBadRequest, WebAuthnLoginBeginResponse{
			ErrorCode: "client_required",
			Error:     "clientId and redirectUri are required for passkey sign-in"})
		return

	default:
		// Validated HERE, not at finish: the challenge is what carries the
		// target from this point on, so an unregistered pair must never
		// become a stored one.
		if identity.ResolveClient(ctx, s.Cfg, s.Store, oauth.ClientId) == nil ||
			!identity.ClientAllowsRedirectURI(ctx, s.Cfg, s.Store, oauth.ClientId, oauth.RedirectURI) {
			s.auditPasskey(r, "passkey_login_challenge_denied", "", "", identity.AuditOutcomeBlocked, "unregistered_client", map[string]any{
				"clientId": oauth.ClientId,
			})
			writeJSON(w, http.StatusBadRequest, WebAuthnLoginBeginResponse{
				ErrorCode: "invalid_client",
				Error:     "client_id or redirect_uri is not registered"})
			return
		}
	}

	challenge, err := ceremony.BeginLogin(oauth)
	if err != nil {
		s.auditPasskey(r, "passkey_login_challenge_denied", "", "", identity.AuditOutcomeFailure, "begin_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnLoginBeginResponse{
			ErrorCode: "begin_failed", Error: err.Error()})
		return
	}

	s.auditPasskey(r, "passkey_login_challenge_issued", "", "", identity.AuditOutcomeSuccess, "", map[string]any{
		"challengeExpiresAt": challenge.ExpiresAt.Format(time.RFC3339Nano),
		"clientId":           oauth.ClientId,
		"firstParty":         body.FirstParty,
		"relyingPartyId":     ceremony.RPID(),
	})

	writeJSON(w, http.StatusOK, WebAuthnLoginBeginResponse{
		Success:        true,
		ChallengeId:    challenge.ChallengeId,
		ExpiresAt:      challenge.ExpiresAt.Format(time.RFC3339Nano),
		RelyingPartyId: ceremony.RPID(),
		RequestOptions: challenge.Options,
	})
}

// handleWebAuthnLoginFinish verifies the assertion, mints the auth code
// and returns the client callback target.
func (s *Server) handleWebAuthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
			ErrorCode: "server", Error: "identity engine not wired"})
		return
	}
	ctx := r.Context()

	if allowed, retryAfter := passkeyLoginLimiter(s).Allow(clientIP(r)); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, WebAuthnLoginFinishResponse{
			ErrorCode: "rate_limited", Error: "too many passkey sign-in attempts from this address"})
		return
	}

	ceremony, err := s.webauthnCeremony()
	if err != nil {
		s.auditPasskey(r, "passkey_login_denied", "", "", identity.AuditOutcomeFailure, "relying_party_unavailable", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
			ErrorCode: "webauthn_unconfigured", Error: err.Error()})
		return
	}

	var body WebAuthnLoginFinishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPasskeyLoginBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, WebAuthnLoginFinishResponse{
			ErrorCode: "bad_request", Error: "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.ChallengeId) == "" || len(body.Credential) == 0 {
		writeJSON(w, http.StatusBadRequest, WebAuthnLoginFinishResponse{
			ErrorCode: "bad_request", Error: "challengeId and credential are both required"})
		return
	}

	store := &webauthn.Store{Engine: s.Store.Engine, Logger: s.Logger}
	asserted, err := ceremony.FinishLogin(body.ChallengeId, bytes.NewReader(body.Credential),
		func(credentialId string) (*webauthn.Row, error) {
			return store.LookupByCredentialId(ctx, credentialId)
		})
	if err != nil {
		status, code := passkeyLoginErrorCode(err)
		outcome := identity.AuditOutcomeFailure
		if errors.Is(err, webauthn.ErrSignCountRegression) || errors.Is(err, webauthn.ErrCredentialRevoked) {
			outcome = identity.AuditOutcomeBlocked
		}
		action := "passkey_login_denied"
		if errors.Is(err, webauthn.ErrSignCountRegression) {
			// Its own action, not a flavour of "denied": this is the
			// cloned-authenticator signal, and an operator scanning the
			// audit trail for it should not have to read failure reasons
			// to find it.
			action = "passkey_sign_count_regression"
		}
		s.auditPasskey(r, action, "", "", outcome, code, nil)
		writeJSON(w, status, WebAuthnLoginFinishResponse{ErrorCode: code, Error: err.Error()})
		return
	}

	userId := strings.TrimSpace(asserted.Row.UserId)
	if userId == "" {
		s.auditPasskey(r, "passkey_login_denied", "", asserted.Row.ID, identity.AuditOutcomeFailure, "credential_unowned", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
			ErrorCode: "credential_unowned", Error: "passkey row carries no owner"})
		return
	}

	now := time.Now().UTC()

	// WHICH ARM THIS CEREMONY WAS BEGUN AS is read off the CHALLENGE, never
	// off this request (memql#4610). asserted.OAuth is what the begin handler
	// validated and then stored server-side, so a challenge with no client in
	// it can only have come from the first-party arm, and no field a caller
	// sends here can turn one arm into the other. Same reasoning as the
	// redirect URI: the ceremony's target is fixed when the challenge is
	// minted and is not restatable at finish time.
	firstParty := !asserted.OAuth.HasClient()

	// target is where the browser goes next; codeId names the auth code for
	// the audit trail, and stays empty on the arm that mints none.
	target := ""
	codeId := ""

	if firstParty {
		// NO AUTH CODE, and no role floor either -- both are properties of a
		// relying party, and this ceremony has none. identity.CheckClientRoleFloor
		// would admit an empty client id anyway (only the built-in editor
		// declares a floor), but skipping it says why rather than relying on
		// a default two packages away.
		//
		// The destination is computed from this server's own configuration.
		// It is the one thing the first-party arm must not take from the
		// caller: a redirect target a request could name is exactly the
		// misdelivery the relying-party arm validates against.
		target = s.postLoginLanding(ctx)
	} else {
		// THE ROLE FLOOR (memql#4516). The third and last place the code flow can
		// mint a credential, so it consults the same identity.CheckClientRoleFloor
		// the magic-link verifier and the SSO fast path do. Without it, a reader
		// refused through the emailed link would simply present a passkey instead
		// and be admitted -- the floor has to sit on the MINT, not on one factor.
		//
		// The role is read from the user row rather than from a session, because
		// authenticating is what this handler just did: there is no session yet.
		if refusal := s.passkeyRoleFloorRefusal(ctx, asserted.OAuth.ClientId, userId); refusal != nil {
			s.auditPasskey(r, identity.AuditActionRoleFloorRefused, userId, asserted.Row.ID,
				identity.AuditOutcomeBlocked, "role_below_client_floor", refusal.AuditDetail())
			// Hand the browser the client's own callback carrying the OAuth error
			// envelope, so the refusal reaches the application that asked and the
			// editor can print error_description verbatim. passkey-login.js
			// navigates on redirectTo and does not read `success`, so a refused
			// ceremony ends where an approved one would -- at the client -- rather
			// than as an inline message the extension never sees.
			if target, err := buildClientErrorCallback(asserted.OAuth.RedirectURI, "access_denied",
				refusal.Description(), asserted.OAuth.State); err == nil {
				writeJSON(w, http.StatusOK, WebAuthnLoginFinishResponse{
					ErrorCode:  "role_floor",
					Error:      refusal.Description(),
					RedirectTo: target,
				})
				return
			}
			writeJSON(w, http.StatusForbidden, WebAuthnLoginFinishResponse{
				ErrorCode: "role_floor", Error: refusal.Description()})
			return
		}

		// The auth code. Identical shape to the one the magic-link verifier
		// mints -- same five OAuth fields, same 60-second life, same
		// digest-only persistence (the plaintext travels back on the
		// redirect and never reaches the database). identityId names the
		// PASSKEY row, so the session created at /oauth/token attributes to
		// the credential that actually authenticated. magicLinkRequestId is
		// empty because no magic link was involved, which is also how an
		// operator tells the two apart after the fact.
		plainCode, codeHash, err := newPasskeyAuthCode()
		if err != nil {
			s.auditPasskey(r, "passkey_login_denied", userId, asserted.Row.ID, identity.AuditOutcomeFailure, "code_mint_failed", nil)
			writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
				ErrorCode: "internal", Error: "auth code mint failed"})
			return
		}
		codeId, err = identity.NewRandomId("")
		if err != nil {
			s.auditPasskey(r, "passkey_login_denied", userId, asserted.Row.ID, identity.AuditOutcomeFailure, "code_id_mint_failed", nil)
			writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
				ErrorCode: "internal", Error: "auth code id mint failed"})
			return
		}
		if err := s.Store.CreateAuthCode(
			ctx,
			codeId,
			codeHash,
			asserted.OAuth.ClientId,
			asserted.OAuth.RedirectURI,
			asserted.OAuth.State,
			asserted.OAuth.CodeChallenge,
			asserted.OAuth.CodeChallengeMethod,
			userId,
			asserted.Row.ID,
			"",
			now.Add(authCodeTTL).Format(time.RFC3339Nano),
		); err != nil {
			s.auditPasskey(r, "passkey_login_denied", userId, asserted.Row.ID, identity.AuditOutcomeFailure, "auth_code_persist_failed", nil)
			writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
				ErrorCode: "persist_failed", Error: err.Error()})
			return
		}

		target, err = buildClientCallback(asserted.OAuth.RedirectURI, plainCode, asserted.OAuth.State)
		if err != nil {
			s.auditPasskey(r, "passkey_login_denied", userId, asserted.Row.ID, identity.AuditOutcomeFailure, "redirect_build_failed", nil)
			writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
				ErrorCode: "internal", Error: "could not build the client callback target"})
			return
		}
	}

	// THE BROWSER GETS A SESSION TOO (memql#3920).
	//
	// Everything above hands the CLIENT an auth code. This hands the
	// BROWSER the same thing a magic-link sign-in leaves it: the
	// memql_admin cookie that /authorize's SSO fast-path reads. Without
	// it a browser that had just proved possession of a passkey held
	// nothing, so the next first-party surface -- the portal -- started
	// its own ceremony and asked for the passkey again. The stronger
	// factor got the worse single sign-on.
	//
	// AFTER the auth code, and best-effort for the same reason the
	// bookkeeping below is: the login has already succeeded, and failing
	// it now over a cookie would cost the operator the credential they
	// just proved. A missing cookie costs one extra ceremony; a failed
	// login costs the whole attempt.
	//
	// ON THE FIRST-PARTY ARM IT IS FATAL INSTEAD (memql#4610), and the same
	// sentence is why: there, the session IS the whole attempt. A cookie
	// that failed to stamp costs nothing extra on the relying-party arm
	// because the client still got its code, but on this one it would leave
	// a caller holding a success response and no session -- and /enroll acts
	// on that response by navigating, so the person would land on a page
	// that asks them to sign in with no idea why.
	if err := s.startBrowserSession(w, r, browserSessionSubject{
		UserId: userId,
		lookup: func(ctx context.Context) (*identity.UserRow, error) {
			return s.Store.LookupUserById(ctx, userId)
		},
	}, "passkey_session_started"); err != nil {
		s.logErr("webauthn: passkey browser session not started", err)
		if firstParty {
			s.auditPasskey(r, "passkey_login_denied", userId, asserted.Row.ID,
				identity.AuditOutcomeFailure, "session_start_failed", nil)
			writeJSON(w, http.StatusInternalServerError, WebAuthnLoginFinishResponse{
				ErrorCode: "session_failed",
				Error:     "the passkey was accepted but the sign-in could not be completed"})
			return
		}
	}

	// Best-effort bookkeeping, AFTER the code is minted so a failure here
	// cannot cost the user their login. The passkey row is a credential
	// row, so the write is gated by the memql#2513 credential-actor guard
	// and needs the system actor: this handler has no authenticated actor
	// at all, because authenticating is what it just did.
	if err := store.RecordAssertion(
		identity.ContextWithSystemCredentialActor(ctx),
		&asserted.Row, asserted.SignCount, asserted.BackupState, now,
	); err != nil {
		s.logErr("webauthn: passkey assertion bookkeeping failed", err)
	}

	s.auditPasskey(r, "passkey_login_succeeded", userId, asserted.Row.ID, identity.AuditOutcomeSuccess, "", map[string]any{
		"clientId":       asserted.OAuth.ClientId,
		"authCodeId":     codeId,
		"firstParty":     firstParty,
		"signCount":      asserted.SignCount,
		"backupState":    asserted.BackupState,
		"relyingPartyId": ceremony.RPID(),
	})

	writeJSON(w, http.StatusOK, WebAuthnLoginFinishResponse{
		Success:    true,
		RedirectTo: target,
	})
}

// passkeyRoleFloorRefusal resolves the user's cluster-wide role and asks the
// one role-floor rule about it. Returns nil to admit.
//
// The lookup is skipped entirely for a client that declares no floor, which is
// every client but the built-in editor -- so the portal and every MCP connector
// pay nothing for this.
func (s *Server) passkeyRoleFloorRefusal(ctx context.Context, clientId, userId string) *identity.RoleFloorRefusal {
	if !identity.ClientDeclaresRoleFloor(clientId) {
		return nil
	}
	role := ""
	if s.Store != nil {
		if row, err := s.Store.LookupUserById(ctx, userId); err == nil && row != nil {
			role = row.Role
		} else if err != nil {
			s.logErr("webauthn: role lookup for the client role floor failed", err)
			// Fail CLOSED. A floor that opens when the database blinks is not
			// a floor; the person retries and the operator sees the log line.
		}
	}
	return identity.CheckClientRoleFloor(clientId, auth.Role(role))
}

// postLoginLanding is where a first-party passkey sign-in ends up: the
// cluster's own portal when its origin can be named, and the same-origin /me
// when it cannot (memql#4610).
//
// COMPUTED, NEVER SUPPLIED. This is the whole reason the first-party arm can
// do without the relying-party validation the other arm runs: there is no
// caller-named target to validate, because the only target is the one this
// server derives from the cluster domain it was configured with.
//
// A three-line copy of component/identity/web's postLoginLanding rather than a
// shared helper, for the reason buildClientCallback in complete.go already
// records: web cannot import this package without closing a cycle. The policy
// itself is not duplicated -- both call identity.DefaultPostLoginLanding, which
// is the one place it lives.
func (s *Server) postLoginLanding(ctx context.Context) string {
	if s == nil {
		return "/me"
	}
	domain := ""
	if s.Store != nil {
		if row, err := s.Store.ReadClusterSettings(ctx); err == nil && row != nil {
			domain = row.ClusterDomain
		}
	}
	return identity.DefaultPostLoginLanding(domain, s.Cfg.BaseURL)
}

// buildClientErrorCallback is buildClientCallback's refusal counterpart: the
// client's registered redirect URI carrying an OAuth error envelope
// (RFC 6749 §4.1.2.1) instead of a code.
func buildClientErrorCallback(redirectURI, code, description, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// newPasskeyAuthCode mints a one-time OAuth auth code: 32 bytes of
// crypto/rand as base64url, plus the digest that is all the database
// ever sees (issue #3187).
//
// Local rather than borrowed from the magic-link package on purpose.
// magiclink.newAuthCode is unexported, and exporting it would mean
// editing the verifier -- which memql#3407 exists precisely to avoid:
// CreateAuthCode was put on its own so a new factor never has to touch
// the magic-link path. What MUST match is the digest algorithm, and it
// does by construction: hashCode is the same function /oauth/token runs
// on the presented code to find the row.
func newPasskeyAuthCode() (plain, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	return plain, hashCode(plain), nil
}

// passkeyLoginErrorCode maps a login-ceremony failure onto an HTTP
// status and a stable code.
//
// The two credential-resolution outcomes collapse to ONE client-facing
// code (`assertion_rejected`) even though they are separate errors
// internally: telling an unauthenticated caller whether a credential id
// is enrolled here, or enrolled-and-revoked, is an oracle over a value
// that travels in the clear. The audit trail keeps the distinction,
// which is where it is useful.
func passkeyLoginErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, webauthn.ErrChallengeNotFound):
		return http.StatusBadRequest, "challenge_not_found"
	case errors.Is(err, webauthn.ErrChallengeExpired):
		return http.StatusBadRequest, "challenge_expired"
	case errors.Is(err, webauthn.ErrUserVerification):
		return http.StatusBadRequest, "user_verification_required"
	case errors.Is(err, webauthn.ErrSignCountRegression):
		return http.StatusForbidden, "sign_count_regression"
	case errors.Is(err, webauthn.ErrCredentialUnknown):
		return http.StatusUnauthorized, "assertion_rejected"
	case errors.Is(err, webauthn.ErrCredentialRevoked):
		return http.StatusUnauthorized, "assertion_rejected"
	default:
		return http.StatusUnauthorized, "assertion_rejected"
	}
}
