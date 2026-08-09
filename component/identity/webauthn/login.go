package webauthn

// The WebAuthn LOGIN ceremony (memql#3407) -- the half that turns a
// signature into "this is user X".
//
// It shares the relying party and the challenge store with registration
// rather than configuring its own: a login verified against a different
// RP id or origin than the one the credential was minted under would
// either fail every assertion or, worse, accept assertions scoped
// somewhere else. The one thing it does NOT share is the ceremony tag,
// so a registration challenge cannot be redeemed as a login assertion
// (see ChallengeStore.Take).
//
// The ceremony is USERNAMELESS: allowCredentials is empty, because no
// email has been typed when the button is pressed. That works only
// because registration mints residentKey=required credentials -- the
// authenticator holds the user handle itself and hands it back with the
// assertion. Which row that assertion belongs to is resolved by
// credential id alone, which is why registration refuses a duplicate one
// cluster-wide.
//
// What this file deliberately does NOT do: mint anything. It verifies a
// signature and reports which credential produced it. The auth code, the
// OAuth callback and the audit trail are the handler's business
// (component/identity/http/webauthn_login.go), the same split
// registration uses.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	gowebauthn "github.com/go-webauthn/webauthn/webauthn"
)

// CeremonyLogin tags a challenge minted for an assertion ceremony.
const CeremonyLogin = "login"

var (
	// ErrCredentialUnknown is returned when the asserted credential id
	// matches no row. Deliberately does not distinguish "never
	// registered" from "registered elsewhere": the credential id is
	// public, so a caller who could tell the two apart would have an
	// oracle for whether an authenticator is enrolled on this cluster.
	ErrCredentialUnknown = errors.New("webauthn: no passkey matches the asserted credential")

	// ErrCredentialRevoked is separated from Unknown because the row
	// exists and the operator action that produced it was deliberate --
	// an audit trail that says "revoked" is worth more than one that
	// says "unknown", and the client-facing message collapses them
	// anyway.
	ErrCredentialRevoked = errors.New("webauthn: this passkey has been revoked")

	// ErrSignCountRegression is the spec's cloned-authenticator signal:
	// the counter in this assertion is not greater than the one stored
	// from the last one. A credential's counter only ever moves forward
	// on a genuine authenticator, so a repeat or a decrease means at
	// least two copies of the private key are in use.
	//
	// A counter that is zero on BOTH sides is NOT this: iCloud Keychain
	// and Windows Hello never implement counters and report 0 forever,
	// so "0 again" is the normal state of a healthy passkey rather than
	// evidence of anything. That distinction lives in go-webauthn's
	// Authenticator.UpdateCounter, which raises CloneWarning only when
	// the new count is <= the stored one AND they are not both zero.
	ErrSignCountRegression = errors.New("webauthn: authenticator signature counter went backwards (possible cloned authenticator)")
)

// OAuthContext is the relying-party context a login challenge carries so
// the assertion can end in the SAME auth code a magic-link click
// produces.
//
// These are exactly the five fields IssueMagicLink stamps onto a
// magic-link row's oauthCtx (component/identity/magiclink/issuer.go).
// Carrying the identical set is the entire point of the design: what
// reaches /oauth/token is an auth code, and no client can tell which
// factor minted it.
type OAuthContext struct {
	// ClientId is the registered OAuth client the code is minted for.
	ClientId string

	// RedirectURI is that client's callback. Validated against the
	// client's registration BEFORE the challenge is minted -- see the
	// handler -- so a challenge in flight can only ever deliver to a
	// place the operator already approved.
	RedirectURI string

	// State is the client's opaque CSRF value, echoed back on the
	// callback untouched.
	State string

	// CodeChallenge / CodeChallengeMethod are the PKCE binding
	// (RFC 7636). Stamped onto the auth code so /oauth/token demands the
	// matching verifier, exactly as it does for a magic-link code.
	CodeChallenge       string
	CodeChallengeMethod string
}

// HasClient reports whether a relying party is in scope for this
// ceremony.
func (o OAuthContext) HasClient() bool {
	return strings.TrimSpace(o.ClientId) != "" && strings.TrimSpace(o.RedirectURI) != ""
}

// LoginChallenge is what /auth/webauthn/login/begin returns: the
// browser-ready request options plus the opaque handle the finish step
// must present.
type LoginChallenge struct {
	// ChallengeId is the handle for the stored challenge.
	ChallengeId string

	// ExpiresAt is when the stored challenge stops being redeemable.
	ExpiresAt time.Time

	// Options is the PublicKeyCredentialRequestOptions payload, ready to
	// be JSON-encoded and handed to navigator.credentials.get().
	Options *protocol.CredentialAssertion
}

// AssertedLogin is the verified outcome of a login ceremony.
type AssertedLogin struct {
	// Row is the passkey identity the assertion resolved to, as stored
	// BEFORE this ceremony. The caller needs the whole row rather than
	// just the ids because re-stamping the credential is a wholesale
	// merge (partial nested updates are unsupported), and every field it
	// does not change has to be re-passed from here rather than
	// re-derived from the assertion.
	Row Row

	// SignCount is the counter to persist: the assertion's value.
	SignCount uint32

	// BackupState is the BS flag as of THIS assertion. It legitimately
	// changes over a credential's life (the user turned iCloud Keychain
	// on), so it is re-stamped. Its sibling BackupEligible is immutable
	// by spec and is deliberately absent here -- the library already
	// refuses an assertion whose BE flag disagrees with the stored row,
	// so there is nothing to re-stamp and offering the field would
	// invite overwriting it.
	BackupState bool

	// OAuth is the relying-party context the challenge was minted with.
	OAuth OAuthContext
}

// CredentialResolver resolves a base64url credential id to its stored
// row, or (nil, nil) when nothing matches.
//
// A function rather than a *Store so the ceremony keeps no storage
// dependency, which is what lets the ceremony tests drive the real
// verifier against an in-memory row set.
type CredentialResolver func(credentialId string) (*Row, error)

// BeginLogin mints a discoverable-credential challenge.
//
// allowCredentials is EMPTY, and that is the acceptance criterion rather
// than an omission: nobody has typed an email, so there is no user whose
// credential ids we could list. The browser asks the platform which
// discoverable credentials it holds for this RP id and lets the user
// pick. userVerification=required is stated at the call site as well as
// in the RP config, because a discoverable assertion with no user
// gesture proves possession of a device and nothing about who is
// holding it.
func (c *Ceremony) BeginLogin(oauth OAuthContext) (*LoginChallenge, error) {
	if c == nil || c.rp == nil {
		return nil, errors.New("webauthn: ceremony not configured")
	}
	assertion, session, err := c.rp.BeginDiscoverableLogin(
		gowebauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("webauthn: begin login: %w", err)
	}
	// The library builds a discoverable session with no allowed
	// credentials; assert it rather than trust it, because an
	// allowCredentials list leaking in here would quietly turn the
	// ceremony back into a username-first one.
	if len(session.AllowedCredentialIDs) != 0 || len(assertion.Response.AllowedCredentials) != 0 {
		return nil, errors.New("webauthn: discoverable login produced a non-empty allowCredentials list")
	}
	challengeId, expiresAt, err := c.challenges.Put("", CeremonyLogin, session, oauth)
	if err != nil {
		return nil, err
	}
	return &LoginChallenge{
		ChallengeId: challengeId,
		ExpiresAt:   expiresAt,
		Options:     assertion,
	}, nil
}

// FinishLogin consumes the challenge and verifies the assertion against
// the credential it names.
//
// The challenge is consumed whatever the outcome (ChallengeStore.Take),
// so a captured assertion has nothing left to replay against.
//
// Order matters here. The credential is resolved from the PARSED
// response before the library is asked to validate, rather than inside
// go-webauthn's DiscoverableUserHandler callback, for one reason: the
// callback's errors come back wrapped in a generic protocol.ErrBadRequest,
// and "this credential was revoked" and "this signature is forged" are
// not the same event to an operator reading the audit trail.
func (c *Ceremony) FinishLogin(challengeId string, body io.Reader, resolve CredentialResolver) (*AssertedLogin, error) {
	if c == nil || c.rp == nil {
		return nil, errors.New("webauthn: ceremony not configured")
	}
	if resolve == nil {
		return nil, errors.New("webauthn: FinishLogin requires a credential resolver")
	}
	entry, err := c.challenges.Take(challengeId, CeremonyLogin)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(body)
	if err != nil {
		return nil, fmt.Errorf("webauthn: assertion response rejected: %w", err)
	}

	credentialId := base64.RawURLEncoding.EncodeToString(parsed.RawID)
	row, err := resolve(credentialId)
	if err != nil {
		return nil, fmt.Errorf("webauthn: credential lookup failed: %w", err)
	}
	if row == nil {
		return nil, ErrCredentialUnknown
	}
	if !row.Active {
		return nil, ErrCredentialRevoked
	}
	stored, err := ToWebAuthnCredential(*row)
	if err != nil {
		return nil, err
	}

	// The user projection carries EXACTLY the one credential the
	// assertion named. The library re-checks that the response's user
	// handle equals this user's WebAuthnID, which is what binds the
	// signature to the account rather than merely to a key: a row whose
	// userId had been re-pointed at another person would fail here
	// instead of logging the attacker in as them.
	user := &User{Id: row.UserId, Credentials: []gowebauthn.Credential{stored}}
	verified, err := c.rp.ValidateDiscoverableLogin(
		func(rawID, userHandle []byte) (gowebauthn.User, error) { return user, nil },
		*entry.Session,
		parsed,
	)
	if err != nil {
		return nil, fmt.Errorf("webauthn: assertion verification failed: %w", err)
	}
	if !verified.Flags.UserVerified {
		return nil, ErrUserVerification
	}
	if verified.Authenticator.CloneWarning {
		return nil, ErrSignCountRegression
	}
	return &AssertedLogin{
		Row:         *row,
		SignCount:   verified.Authenticator.SignCount,
		BackupState: verified.Flags.BackupState,
		OAuth:       entry.OAuth,
	}, nil
}
