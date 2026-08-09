package http

// POST /auth/webauthn/register/begin
// POST /auth/webauthn/register/finish
//
// The WebAuthn passkey REGISTRATION ceremony (memql#3406). Login is
// memql#3407 and enrolment authorization is memql#3408; this pair
// assumes an already-authenticated session as the registration
// authority, which is what lets it ship and be tested independently.
//
// HTTP rather than gRPC, and pre-approved as such under the identity
// service's documented exception: WebAuthn is a browser API. The
// ceremony is navigator.credentials.create() running inside the page,
// and the bytes it produces have to reach a server the browser can post
// to. There is no gRPC form of "the user touched their security key".
//
// Follows pair.go's conventions throughout: HTTPS required (the
// ceremony carries an authenticated session bearer), per-IP rate
// limiting, SourceIP on every audit event, and errors that name a code
// the client can branch on.
//
// The two properties this file is really enforcing:
//
//  1. THE RP ID COMES FROM CONFIGURATION. webauthn.RelyingParty derives
//     it from MEMQL_IDENTITY_BASE_URL. Deriving it from r.Host would
//     scope minted credentials to whatever name the request arrived
//     under, which an attacker who can reach the service influences.
//  2. THE CREDENTIAL ID IS UNIQUE. A registration whose credential id
//     already exists is refused rather than shadowing the existing row,
//     because the login ceremony resolves a discoverable assertion
//     through that id alone -- two rows sharing one would make which
//     account you land in a matter of row order.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	gowebauthn "github.com/go-webauthn/webauthn/webauthn"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/enrolment"
	"github.com/znasllc-io/memql/component/identity/webauthn"
)

// AuthSchemeEnrolment is the Authorization scheme a single-use enrolment
// token presents itself under (memql#3408): `Authorization: Enrolment
// mql_enr_<...>`.
//
// Deliberately the same shape as pair.go's `Authorization: Pair <code>` --
// the credential IS the authorization, so it travels where a bearer travels
// rather than being smuggled in a body field or a query parameter that a
// proxy would log. It is admitted on THESE TWO ROUTES ONLY; every other
// surface in the service authenticates with a Bearer JWT and does not know
// this scheme exists.
const AuthSchemeEnrolment = "Enrolment"

// envPasskeyRegisterPerHour tunes the per-IP registration-attempt rate
// limit. Registration is a rare, deliberate act -- a handful per user
// per lifetime -- so the default is tight compared with the badge
// grant's 240/h; what the limiter blunts here is a script hammering
// begin to churn challenges.
const envPasskeyRegisterPerHour = "MEMQL_IDENTITY_PASSKEY_REGISTER_PER_HOUR"

const defaultPasskeyRegisterPerHour = 60

// maxPasskeyRegisterBody bounds the finish payload. An attestation
// object with a large certificate chain is a few KB; 64 KB is generous
// and still refuses a body meant to exhaust the parser.
const maxPasskeyRegisterBody = 64 * 1024

// defaultPasskeyLabelPrefix is what an unlabelled credential is called.
// Suffixed with the tail of its credential id so a user who enrols two
// authenticators without naming either can still tell the rows apart in
// the management list (memql#3409).
const defaultPasskeyLabelPrefix = "Passkey "

// maxPasskeyLabel bounds a caller-supplied label.
const maxPasskeyLabel = 120

// WebAuthnRegisterBeginRequest is the JSON body for
// POST /auth/webauthn/register/begin. Everything in it is optional --
// the ceremony's inputs come from the authenticated session, not from
// the body -- but a client may name the credential up front so the
// label survives a page reload between the two steps.
type WebAuthnRegisterBeginRequest struct {
	Label string `json:"label,omitempty"`
}

// WebAuthnRegisterBeginResponse carries the browser-ready creation
// options plus the opaque challenge handle the finish step must present.
//
// CreationOptions marshals as {"publicKey": {...}}, which is exactly the
// argument navigator.credentials.create() takes, so a client passes it
// through untouched.
type WebAuthnRegisterBeginResponse struct {
	Success         bool                         `json:"success"`
	ChallengeId     string                       `json:"challengeId,omitempty"`
	ExpiresAt       string                       `json:"expiresAt,omitempty"`
	RelyingPartyId  string                       `json:"relyingPartyId,omitempty"`
	CreationOptions *protocol.CredentialCreation `json:"creationOptions,omitempty"`
	Error           string                       `json:"error,omitempty"`
	ErrorCode       string                       `json:"errorCode,omitempty"`
}

// WebAuthnRegisterFinishRequest is the JSON body for
// POST /auth/webauthn/register/finish. Credential is the raw
// PublicKeyCredential the browser produced, forwarded verbatim -- it is
// parsed by the WebAuthn library rather than by this package, so it
// stays a RawMessage the whole way through.
type WebAuthnRegisterFinishRequest struct {
	ChallengeId string          `json:"challengeId"`
	Label       string          `json:"label,omitempty"`
	Credential  json.RawMessage `json:"credential"`
}

// WebAuthnRegisterFinishResponse reports the persisted credential. Label
// is the acceptance criterion's "created credential's label": the value
// the management surface will show, resolved server-side so a client
// that supplied none still learns what the row is called.
type WebAuthnRegisterFinishResponse struct {
	Success        bool     `json:"success"`
	IdentityId     string   `json:"identityId,omitempty"`
	Label          string   `json:"label,omitempty"`
	CredentialId   string   `json:"credentialId,omitempty"`
	AAGUID         string   `json:"aaguid,omitempty"`
	Transports     []string `json:"transports,omitempty"`
	BackupEligible bool     `json:"backupEligible,omitempty"`
	BackupState    bool     `json:"backupState,omitempty"`
	Error          string   `json:"error,omitempty"`
	ErrorCode      string   `json:"errorCode,omitempty"`
}

// passkeyLimiter returns this Server's per-IP registration limiter,
// built once on first use. Server-scoped for the same reason
// grantLimiter is: each Server (and each test) gets its own buckets.
func passkeyLimiter(s *Server) *abuse.IPRateLimiter {
	s.passkeyLimiterOnce.Do(func() {
		perHour := defaultPasskeyRegisterPerHour
		if v := strings.TrimSpace(os.Getenv(envPasskeyRegisterPerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		s.passkeyLimiter = abuse.NewIPRateLimiter(perHour, s.Logger)
	})
	return s.passkeyLimiter
}

// webauthnCeremony returns this Server's relying party, built once from
// s.Cfg.BaseURL.
//
// Built lazily rather than wired in app/ so the RP ID has exactly one
// source -- the same Config the rest of the identity service reads --
// and cannot drift from it via a separately-constructed instance. The
// error is cached alongside: a bad base URL is a boot-time
// misconfiguration, and retrying the derivation per request would only
// change how often it is logged.
func (s *Server) webauthnCeremony() (*webauthn.Ceremony, error) {
	s.webauthnCeremonyOnce.Do(func() {
		s.webauthnCeremonyValue, s.webauthnCeremonyErr = webauthn.New(webauthn.Config{
			BaseURL:     s.Cfg.BaseURL,
			DisplayName: s.Cfg.BrandName,
		})
		if s.webauthnCeremonyErr != nil && s.Logger != nil {
			s.Logger.Warn("webauthn: relying party unavailable; passkey routes will refuse",
				"error", s.webauthnCeremonyErr.Error())
		}
	})
	return s.webauthnCeremonyValue, s.webauthnCeremonyErr
}

// handleWebAuthnRegisterBegin issues a registration challenge bound to
// the authenticated caller.
func (s *Server) handleWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "server", Error: "identity engine not wired"})
		return
	}
	ctx := r.Context()
	sourceIP := clientIP(r)

	if allowed, retryAfter := passkeyLimiter(s).Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, WebAuthnRegisterBeginResponse{
			ErrorCode: "rate_limited", Error: "too many passkey registration attempts from this address"})
		return
	}

	enroller, ok := s.requirePasskeyEnroller(w, r, "passkey_registration_challenge_denied")
	if !ok {
		return
	}
	userId := enroller.UserId

	ceremony, err := s.webauthnCeremony()
	if err != nil {
		s.auditPasskey(r, "passkey_registration_challenge_denied", userId, "", identity.AuditOutcomeFailure, "relying_party_unavailable", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "webauthn_unconfigured", Error: err.Error()})
		return
	}

	var body WebAuthnRegisterBeginRequest
	if r.Body != nil {
		// Optional body; an empty one decodes to io.EOF, which is fine.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPasskeyRegisterBody)).Decode(&body)
	}

	user := &webauthn.User{Id: userId, Name: userId, DisplayName: userId}
	if row, lookupErr := s.Store.LookupUserById(ctx, userId); lookupErr == nil && row != nil {
		if row.PrimaryEmail != "" {
			user.Name = row.PrimaryEmail
		}
		if row.DisplayName != "" {
			user.DisplayName = row.DisplayName
		}
	} else if lookupErr != nil {
		// Non-fatal: the directory fields are cosmetic (they are what the
		// browser shows in its credential sheet), and refusing an
		// enrolment over them would be a worse failure than a sheet that
		// says the user's id.
		s.logErr("webauthn: user lookup failed; falling back to userId", lookupErr)
	}

	// Exclusions. The read is self-scoped (userId==actor.userId), and the
	// identity HTTP surface stamps a SYSTEM actor, so the caller's actor
	// has to be put on the context explicitly or the query resolves
	// actor.userId to "" and returns nothing -- which would read as "no
	// exclusions" and silently let a user enrol the same authenticator
	// twice.
	store := &webauthn.Store{Engine: s.Store.Engine, Logger: s.Logger}
	existing, err := store.ListForSelf(auth.ContextWithUserActor(ctx, userId))
	if err != nil {
		s.auditPasskey(r, "passkey_registration_challenge_denied", userId, "", identity.AuditOutcomeFailure, "exclusion_lookup_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "lookup_failed", Error: err.Error()})
		return
	}
	for _, row := range existing {
		if !row.Active {
			continue
		}
		cred, convErr := webauthn.ToWebAuthnCredential(row)
		if convErr != nil {
			s.logErr("webauthn: skipping unreadable stored credential in exclusion list", convErr)
			continue
		}
		user.Credentials = append(user.Credentials, cred)
	}

	challenge, err := ceremony.BeginRegistration(user)
	if err != nil {
		s.auditPasskey(r, "passkey_registration_challenge_denied", userId, "", identity.AuditOutcomeFailure, "begin_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "begin_failed", Error: err.Error()})
		return
	}

	s.auditPasskey(r, "passkey_registration_challenge_issued", userId, "", identity.AuditOutcomeSuccess, "", map[string]any{
		"challengeExpiresAt": challenge.ExpiresAt.Format(time.RFC3339Nano),
		"excludedCount":      len(user.Credentials),
		"relyingPartyId":     ceremony.RPID(),
	})

	writeJSON(w, http.StatusOK, WebAuthnRegisterBeginResponse{
		Success:         true,
		ChallengeId:     challenge.ChallengeId,
		ExpiresAt:       challenge.ExpiresAt.Format(time.RFC3339Nano),
		RelyingPartyId:  ceremony.RPID(),
		CreationOptions: challenge.Options,
	})
}

// handleWebAuthnRegisterFinish verifies the attestation and persists the
// credential.
func (s *Server) handleWebAuthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterFinishResponse{
			ErrorCode: "server", Error: "identity engine not wired"})
		return
	}
	ctx := r.Context()
	sourceIP := clientIP(r)

	if allowed, retryAfter := passkeyLimiter(s).Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, WebAuthnRegisterFinishResponse{
			ErrorCode: "rate_limited", Error: "too many passkey registration attempts from this address"})
		return
	}

	enroller, ok := s.requirePasskeyEnroller(w, r, "passkey_registration_denied")
	if !ok {
		return
	}
	userId := enroller.UserId

	ceremony, err := s.webauthnCeremony()
	if err != nil {
		s.auditPasskey(r, "passkey_registration_denied", userId, "", identity.AuditOutcomeFailure, "relying_party_unavailable", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterFinishResponse{
			ErrorCode: "webauthn_unconfigured", Error: err.Error()})
		return
	}

	var body WebAuthnRegisterFinishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPasskeyRegisterBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, WebAuthnRegisterFinishResponse{
			ErrorCode: "bad_request", Error: "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.ChallengeId) == "" || len(body.Credential) == 0 {
		writeJSON(w, http.StatusBadRequest, WebAuthnRegisterFinishResponse{
			ErrorCode: "bad_request", Error: "challengeId and credential are both required"})
		return
	}

	cred, err := ceremony.FinishRegistration(body.ChallengeId, userId, bytes.NewReader(body.Credential))
	if err != nil {
		status, code := passkeyCeremonyErrorCode(err)
		s.auditPasskey(r, "passkey_registration_denied", userId, "", identity.AuditOutcomeFailure, code, nil)
		writeJSON(w, status, WebAuthnRegisterFinishResponse{ErrorCode: code, Error: err.Error()})
		return
	}

	store := &webauthn.Store{Engine: s.Store.Engine, Logger: s.Logger}

	// Duplicate rejection. Checked against EVERY row cluster-wide, not
	// just the caller's: a credential id already bound to another account
	// is the case that matters, because the login ceremony would then
	// have two candidate rows for one assertion.
	existing, err := store.LookupByCredentialId(ctx, cred.CredentialId)
	if err != nil {
		s.auditPasskey(r, "passkey_registration_denied", userId, "", identity.AuditOutcomeFailure, "duplicate_check_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterFinishResponse{
			ErrorCode: "lookup_failed", Error: err.Error()})
		return
	}
	if existing != nil {
		s.auditPasskey(r, "passkey_registration_denied", userId, existing.ID, identity.AuditOutcomeBlocked, "duplicate_credential", nil)
		writeJSON(w, http.StatusConflict, WebAuthnRegisterFinishResponse{
			ErrorCode: "duplicate_credential",
			Error:     "this authenticator is already registered"})
		return
	}

	identityId, err := webauthn.NewId()
	if err != nil {
		s.logErr("webauthn: identity id mint failed", err)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterFinishResponse{
			ErrorCode: "internal", Error: "identity id mint failed"})
		return
	}
	label := resolvePasskeyLabel(body.Label, cred.CredentialId)

	// SPEND THE ENROLMENT TOKEN BEFORE THE CREDENTIAL LANDS (memql#3408).
	//
	// The opposite order is what pair.go does, and it can: workertoken.Store
	// has a Revoke, so a failed redeem-stamp is undone by soft-revoking the
	// token it just minted. webauthn.Store has no such undo -- a registered
	// passkey is a row on the user's account with no revoke path in this
	// package -- so "create, then stamp, then repair on failure" has no
	// repair step available, and a stamp that fails after the credential
	// landed leaves a LIVE enrolment token that has already produced a
	// passkey. That is a replay window.
	//
	// Consuming first inverts the failure: a persist error after the stamp
	// burns the link and registers nothing. The holder asks for another link
	// -- recoverable, and recoverable in the direction that does not leave a
	// spent credential usable.
	if enroller.Enrolment != nil {
		if err := (&enrolment.Store{Engine: s.Store.Engine, Logger: s.Logger}).
			Consume(ctx, enroller.Enrolment.ID, sourceIP, time.Now().UTC()); err != nil {
			s.logErr("enrolment: consume stamp failed", err)
			s.auditPasskey(r, "passkey_registration_denied", userId, enrolmentTargetId(enroller.Enrolment),
				identity.AuditOutcomeFailure, "enrolment_consume_failed", nil)
			writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterFinishResponse{
				ErrorCode: "persist_failed", Error: "enrolment consume: " + err.Error()})
			return
		}
	}

	// A passkey row is a credential row, so the memql#2513 credential-actor
	// guard admits only a system actor. This handler runs under the
	// enroller's own bearer, which the guard rejects; the verified
	// attestation above is the authorization and the system credential
	// actor is how it reaches the write. Same shape as pair.go's
	// worker-token create. memql#2549.
	writeCtx := identity.ContextWithSystemCredentialActor(ctx)
	if err := store.Create(writeCtx, identityId, userId, label, cred, userId); err != nil {
		s.auditPasskey(r, "passkey_registration_denied", userId, "", identity.AuditOutcomeFailure, "persist_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterFinishResponse{
			ErrorCode: "persist_failed", Error: err.Error()})
		return
	}

	canonicalId := webauthn.CanonicalId(identityId)
	registrationDetail := map[string]any{
		"label":          label,
		"aaguid":         cred.AAGUID,
		"transports":     cred.Transports,
		"backupEligible": cred.BackupEligible,
		"backupState":    cred.BackupState,
		"relyingPartyId": ceremony.RPID(),
	}
	if enroller.Enrolment != nil {
		// The audit trail has to say which authority produced this credential.
		// "A passkey appeared on this account" reads very differently
		// depending on whether the account's own session asked for it or a
		// link somebody issued did.
		registrationDetail["authorizedBy"] = "enrolmentToken"
		registrationDetail["enrolmentTokenId"] = enrolmentTargetId(enroller.Enrolment)
		registrationDetail["enrolmentIssuedBy"] = enroller.Enrolment.IssuedBy
	}
	s.auditPasskey(r, "passkey_registered", userId, canonicalId, identity.AuditOutcomeSuccess, "", registrationDetail)

	writeJSON(w, http.StatusOK, WebAuthnRegisterFinishResponse{
		Success:        true,
		IdentityId:     canonicalId,
		Label:          label,
		CredentialId:   cred.CredentialId,
		AAGUID:         cred.AAGUID,
		Transports:     cred.Transports,
		BackupEligible: cred.BackupEligible,
		BackupState:    cred.BackupState,
	})
}

// passkeyEnroller is the resolved authority for one registration ceremony:
// who will own the credential, and what authorized it.
type passkeyEnroller struct {
	// UserId is the account the credential lands on.
	UserId string
	// Enrolment is non-nil when a single-use enrolment token authorized the
	// ceremony instead of the caller's own session (memql#3408). The finish
	// step spends it; the begin step does not, so reloading the page mid-way
	// does not burn the link.
	Enrolment *enrolment.Row
}

// requirePasskeyEnroller resolves who will own the credential, or writes the
// refusal and returns ok=false.
//
// TWO AUTHORITIES, AND ONLY TWO.
//
//  1. The caller's own user-class session (memql#3406). User-class only, for
//     the same reason the badge grant is: a passkey enrolment binds a NEW way
//     into the account, so the session authorizing it must be a person's own
//     login and not a derived machine or grant class.
//  2. A single-use enrolment token (memql#3408), presented as
//     `Authorization: Enrolment mql_enr_<...>`. This is the authority that
//     exists for someone who has NO session yet -- which is the entire point:
//     it is how a first credential is obtainable with no mailbox.
//
// The owner is read off the token's row, never off a caller-supplied
// argument, so a leaked token can only ever add a credential to the one
// account it was minted for.
//
// The enrolment arm is checked first because it is unambiguous: a request
// carrying `Authorization: Enrolment ...` is not also carrying a Bearer, and
// falling through to requireBearer would answer it with a misleading
// "Authorization: Bearer <token> required".
func (s *Server) requirePasskeyEnroller(w http.ResponseWriter, r *http.Request, deniedAction string) (passkeyEnroller, bool) {
	if plain := strings.TrimSpace(extractAuthScheme(r, AuthSchemeEnrolment)); plain != "" {
		return s.requireEnrolmentToken(w, r, deniedAction, plain)
	}

	caller, ok := s.requireBearer(w, r)
	if !ok {
		return passkeyEnroller{}, false
	}
	userId := strings.TrimSpace(caller.Subject)
	if userId == "" {
		writeJSON(w, http.StatusUnauthorized, WebAuthnRegisterBeginResponse{
			ErrorCode: "unauthenticated", Error: "subject claim missing"})
		return passkeyEnroller{}, false
	}
	if class := strings.TrimSpace(caller.Class); class != "" && class != identity.ClassUser {
		s.auditPasskey(r, deniedAction, userId, "", identity.AuditOutcomeBlocked, "caller_class_"+class, nil)
		writeJSON(w, http.StatusForbidden, WebAuthnRegisterBeginResponse{
			ErrorCode: "caller_class_rejected",
			Error:     "passkey enrolment requires a user-class session; got class=" + class})
		return passkeyEnroller{}, false
	}
	return passkeyEnroller{UserId: userId}, true
}

// requireEnrolmentToken validates a presented enrolment token.
//
// Every rejection is audited with its own reason and answered with its own
// error code. The codes are what the /enroll page branches on to tell the
// holder which of the four things went wrong, so collapsing them into one
// would delete a user-facing requirement, not just a log line.
func (s *Server) requireEnrolmentToken(w http.ResponseWriter, r *http.Request, deniedAction, plain string) (passkeyEnroller, bool) {
	if s.Store == nil || s.Store.Engine == nil {
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "server", Error: "identity engine not wired"})
		return passkeyEnroller{}, false
	}
	store := &enrolment.Store{Engine: s.Store.Engine, Logger: s.Logger}
	row, state, err := store.Resolve(r.Context(), plain, time.Now().UTC())
	if err != nil {
		s.logErr("enrolment: token lookup failed", err)
		s.auditPasskey(r, deniedAction, "", "", identity.AuditOutcomeFailure, "enrolment_lookup_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "lookup_failed", Error: "enrolment token lookup failed"})
		return passkeyEnroller{}, false
	}

	actorUserId := ""
	if row != nil {
		actorUserId = row.UserId
	}
	if state != enrolment.StateValid {
		status, code := enrolmentRejectionCode(state)
		s.auditPasskey(r, deniedAction, actorUserId, enrolmentTargetId(row), identity.AuditOutcomeBlocked,
			"enrolment_"+strings.ReplaceAll(string(state), "-", "_"), nil)
		writeJSON(w, status, WebAuthnRegisterBeginResponse{
			ErrorCode: code, Error: enrolmentRejectionMessage(state)})
		return passkeyEnroller{}, false
	}
	if strings.TrimSpace(row.UserId) == "" {
		// A row with no owner cannot authorize anything. Refused rather than
		// defaulted, because every default here is somebody else's account.
		s.auditPasskey(r, deniedAction, "", enrolmentTargetId(row), identity.AuditOutcomeFailure, "enrolment_row_has_no_user", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "enrolment_malformed", Error: "enrolment token names no user"})
		return passkeyEnroller{}, false
	}
	return passkeyEnroller{UserId: row.UserId, Enrolment: row}, true
}

// enrolmentRejectionCode maps a rejection state onto an HTTP status and the
// stable code the /enroll page branches on.
func enrolmentRejectionCode(state enrolment.State) (int, string) {
	switch state {
	case enrolment.StateExpired:
		return http.StatusGone, "enrolment_expired"
	case enrolment.StateAlreadyUsed:
		return http.StatusConflict, "enrolment_already_used"
	case enrolment.StateRevoked:
		return http.StatusForbidden, "enrolment_revoked"
	default:
		return http.StatusUnauthorized, "enrolment_invalid"
	}
}

// enrolmentRejectionMessage is the API-side sentence for each state. The
// page's own copy lives in the templ component; this is what a non-browser
// caller (or a fetch that lost its page context) reads.
func enrolmentRejectionMessage(state enrolment.State) string {
	switch state {
	case enrolment.StateExpired:
		return "this enrolment link has expired"
	case enrolment.StateAlreadyUsed:
		return "this enrolment link has already been used"
	case enrolment.StateRevoked:
		return "this enrolment link was revoked"
	default:
		return "this enrolment link is not valid"
	}
}

// enrolmentTargetId returns the row's id for an audit event, or "" when there
// is no row. Never returns anything derived from the plaintext token.
func enrolmentTargetId(row *enrolment.Row) string {
	if row == nil {
		return ""
	}
	return enrolment.CanonicalId(row.ID)
}

// passkeyCeremonyErrorCode maps a ceremony failure onto an HTTP status
// and a stable code. Every unmapped failure collapses to
// attestation_rejected on purpose: the library's error text is useful in
// a log and in the response body, but the CODE a client branches on
// should not enumerate the ways verification can fail.
func passkeyCeremonyErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, webauthn.ErrChallengeNotFound):
		return http.StatusBadRequest, "challenge_not_found"
	case errors.Is(err, webauthn.ErrChallengeExpired):
		return http.StatusBadRequest, "challenge_expired"
	case errors.Is(err, webauthn.ErrChallengeUserMismatch):
		return http.StatusForbidden, "challenge_user_mismatch"
	case errors.Is(err, webauthn.ErrUserVerification):
		return http.StatusBadRequest, "user_verification_required"
	default:
		return http.StatusBadRequest, "attestation_rejected"
	}
}

// resolvePasskeyLabel picks the credential's display label: the
// caller's, trimmed and bounded, or a generated one suffixed with the
// tail of the credential id so two unlabelled rows stay distinguishable.
func resolvePasskeyLabel(supplied, credentialId string) string {
	label := strings.TrimSpace(supplied)
	if label == "" {
		return defaultPasskeyLabelPrefix + suffix(credentialId, 6)
	}
	if len(label) > maxPasskeyLabel {
		label = label[:maxPasskeyLabel]
	}
	return label
}

// auditPasskey emits a passkey-lifecycle audit event. s.audit stamps
// SourceIP + UserAgent off the request, so every event -- issued,
// registered, denied -- carries the origin address.
func (s *Server) auditPasskey(r *http.Request, action, actorUserId, targetId string, outcome identity.AuditOutcome, failureReason string, detail map[string]any) {
	s.audit(r, identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		ActorUserId:   actorUserId,
		TargetType:    "passkeyIdentity",
		TargetId:      targetId,
		Outcome:       outcome,
		FailureReason: failureReason,
		Detail:        detail,
	})
}

// compile-time assertion that the ceremony's user projection still
// satisfies the library's interface. Cheap, and it fails at build time
// rather than at the first enrolment if the interface moves.
var _ gowebauthn.User = (*webauthn.User)(nil)
