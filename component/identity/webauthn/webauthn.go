// Package webauthn implements the WebAuthn / passkey ceremonies the
// identity service runs (memql#3406).
//
// A passkey is the tenth variant of v1:identity:identity. It is the only
// credential family in that union whose stored material is PUBLIC: the
// row carries a COSE public key, and possession is proved by a signature
// the authenticator produces over a server-issued challenge. A dump of
// the passkey rows forges nothing, which is why this package never talks
// about hashing plaintext the way pat / workertoken / badge do.
//
// What it DOES have to be careful about is the binding. Registration is
// the moment a public key becomes "this user's second factor / first
// factor", so the discipline here is:
//
//   - The RP ID comes from MEMQL_IDENTITY_BASE_URL, never from the
//     request Host header. A Host-derived RP ID lets an attacker who can
//     reach the service under another name have credentials minted for a
//     domain they control. See RelyingParty.
//   - The challenge is minted server-side, bound to ONE authenticated
//     user, single-use, and TTL'd. See ChallengeStore.
//   - residentKey=required + userVerification=required, so the credential
//     is DISCOVERABLE (the login ceremony in memql#3407 can be
//     usernameless) and the ceremony proves a user gesture, not just
//     device possession.
//
// The package is deliberately split so the login ceremony can reuse it
// without a rewrite: RP() exposes the configured relying party, the
// ChallengeStore is ceremony-tagged rather than registration-specific,
// and Store is a plain row accessor with no ceremony knowledge.
package webauthn

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	gowebauthn "github.com/go-webauthn/webauthn/webauthn"

	"github.com/go-webauthn/webauthn/protocol"
)

// CeremonyRegister tags a challenge minted for a registration ceremony.
// Take() refuses a challenge presented to the wrong ceremony, so a
// registration challenge can never be redeemed as a login assertion (the
// login ceremony, memql#3407, adds its own tag).
const CeremonyRegister = "register"

// DefaultChallengeTTL bounds how long a minted challenge stays
// redeemable. Long enough for a user to find the authenticator, pick it
// out of the browser sheet and touch it; short enough that a challenge
// captured off a screen recording is dead by the time it is replayed.
const DefaultChallengeTTL = 5 * time.Minute

// DefaultRPDisplayName is the human-facing relying-party name the
// browser shows in its credential-creation sheet when the operator has
// not set one.
const DefaultRPDisplayName = "memQL"

var (
	// ErrNoBaseURL is returned when the identity service has no
	// configured base URL, which means there is no trustworthy RP ID to
	// derive. Refusing is the point: the fallback would be the request
	// Host header, which is attacker-influenced.
	ErrNoBaseURL = errors.New("webauthn: MEMQL_IDENTITY_BASE_URL is required to derive the WebAuthn relying party id")

	// ErrUserVerification is returned when a ceremony completes without
	// the UV flag despite userVerification=required. The library already
	// enforces this; the explicit check keeps the requirement stated at
	// the seam that persists the row rather than only in the options.
	ErrUserVerification = errors.New("webauthn: the authenticator did not perform user verification")
)

// Config is what New needs to build a relying party.
type Config struct {
	// BaseURL is the identity service's public origin, i.e. the value of
	// MEMQL_IDENTITY_BASE_URL. The RP ID and the expected ceremony origin
	// are both derived from it.
	BaseURL string

	// DisplayName is the relying-party name the browser shows. Defaults
	// to DefaultRPDisplayName.
	DisplayName string

	// ChallengeTTL overrides DefaultChallengeTTL.
	ChallengeTTL time.Duration

	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// Ceremony is the relying party plus its challenge store. Constructed
// once per identity Server and safe for concurrent use.
type Ceremony struct {
	rp         *gowebauthn.WebAuthn
	rpID       string
	origin     string
	challenges *ChallengeStore
}

// RelyingParty derives the WebAuthn RP ID and the expected ceremony
// origin from the identity service's base URL.
//
// The RP ID is the registrable domain the credential is scoped to; the
// origin is the exact scheme://host[:port] the browser must report in
// clientDataJSON. Deriving BOTH from configuration rather than from the
// request is the whole security property: a credential minted under an
// attacker-supplied Host would be scoped to the attacker's domain, and
// the origin check would then be verifying the request against itself.
//
// A port is stripped from the RP ID (it is a domain, not an origin) and
// kept on the origin (it is an origin, not a domain) -- which is what
// makes a localhost dev deployment on :8080 work without a special case.
func RelyingParty(baseURL string) (rpID string, origin string, err error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return "", "", ErrNoBaseURL
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", "", fmt.Errorf("webauthn: base url %q is not parseable: %w", baseURL, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", "", fmt.Errorf("webauthn: base url %q must be http or https (got scheme %q)", baseURL, parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("webauthn: base url %q carries no host", baseURL)
	}
	return host, scheme + "://" + parsed.Host, nil
}

// New builds the relying party from config. Returns an error rather than
// a degraded instance when the base URL is missing or unusable -- a
// WebAuthn deployment with a guessed RP ID is worse than one that is
// switched off, because the credentials it mints are scoped to the wrong
// place and cannot be re-scoped afterwards.
func New(cfg Config) (*Ceremony, error) {
	rpID, origin, err := RelyingParty(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	displayName := strings.TrimSpace(cfg.DisplayName)
	if displayName == "" {
		displayName = DefaultRPDisplayName
	}
	rp, err := gowebauthn.New(&gowebauthn.Config{
		RPID:          rpID,
		RPDisplayName: displayName,
		RPOrigins:     []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: relying party config rejected: %w", err)
	}
	ttl := cfg.ChallengeTTL
	if ttl <= 0 {
		ttl = DefaultChallengeTTL
	}
	return &Ceremony{
		rp:         rp,
		rpID:       rpID,
		origin:     origin,
		challenges: NewChallengeStore(ttl, cfg.Now),
	}, nil
}

// RPID returns the relying-party id every credential this instance mints
// is scoped to.
func (c *Ceremony) RPID() string { return c.rpID }

// Origin returns the single origin ceremonies are accepted from.
func (c *Ceremony) Origin() string { return c.origin }

// RP exposes the underlying relying party. The login ceremony
// (memql#3407) needs it to begin and finish a discoverable assertion
// against the same RP ID / origin this registration used; exporting it
// is what keeps that from becoming a second, independently-configured
// relying party.
func (c *Ceremony) RP() *gowebauthn.WebAuthn { return c.rp }

// Challenges exposes the single-use challenge store, for the same
// reason RP is exported.
func (c *Ceremony) Challenges() *ChallengeStore { return c.challenges }

// User is the go-webauthn user projection. Id is the canonical
// v1:identity:user id, which is what lands in the credential's user
// handle and therefore what a discoverable assertion hands back at
// login.
type User struct {
	Id          string
	Name        string
	DisplayName string
	Credentials []gowebauthn.Credential
}

// WebAuthnID implements gowebauthn.User.
func (u *User) WebAuthnID() []byte { return []byte(u.Id) }

// WebAuthnName implements gowebauthn.User.
func (u *User) WebAuthnName() string {
	if strings.TrimSpace(u.Name) != "" {
		return u.Name
	}
	return u.Id
}

// WebAuthnDisplayName implements gowebauthn.User.
func (u *User) WebAuthnDisplayName() string {
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.WebAuthnName()
}

// WebAuthnCredentials implements gowebauthn.User.
func (u *User) WebAuthnCredentials() []gowebauthn.Credential { return u.Credentials }

// RegistrationChallenge is what /auth/webauthn/register/begin returns:
// the browser-ready creation options plus the opaque handle the finish
// step must present.
type RegistrationChallenge struct {
	// ChallengeId is the handle for the stored challenge. Opaque to the
	// client and useless without the matching authenticator response.
	ChallengeId string

	// ExpiresAt is when the stored challenge stops being redeemable.
	ExpiresAt time.Time

	// Options is the PublicKeyCredentialCreationOptions payload, ready
	// to be JSON-encoded and handed to navigator.credentials.create().
	Options *protocol.CredentialCreation
}

// BeginRegistration mints a challenge bound to user and returns the
// creation options.
//
// user.Credentials is the user's ALREADY-ENROLLED set; it becomes
// excludeCredentials, so an authenticator the user already registered
// declines rather than minting a second credential the user then has to
// tell apart from the first. Passing an empty set is legal and means
// "the user has none yet", not "do not exclude".
func (c *Ceremony) BeginRegistration(user *User) (*RegistrationChallenge, error) {
	if c == nil || c.rp == nil {
		return nil, errors.New("webauthn: ceremony not configured")
	}
	if user == nil || strings.TrimSpace(user.Id) == "" {
		return nil, errors.New("webauthn: BeginRegistration requires an authenticated user")
	}
	creation, session, err := c.rp.BeginRegistration(user,
		// Stated at the call site as well as in the config: this is the
		// property memql#3407 depends on, so it should be visible where
		// the ceremony is started rather than only where the RP is built.
		gowebauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}),
		gowebauthn.WithExclusions(gowebauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
	)
	if err != nil {
		return nil, fmt.Errorf("webauthn: begin registration: %w", err)
	}
	challengeId, expiresAt, err := c.challenges.Put(user.Id, CeremonyRegister, session)
	if err != nil {
		return nil, err
	}
	return &RegistrationChallenge{
		ChallengeId: challengeId,
		ExpiresAt:   expiresAt,
		Options:     creation,
	}, nil
}

// FinishRegistration consumes the challenge, verifies the attestation
// against the stored challenge / origin / RP ID, and returns the
// credential facts to persist.
//
// The challenge is consumed whatever the outcome (see ChallengeStore.Take),
// so a failed attempt cannot be retried against the same challenge --
// the client starts a new ceremony instead.
//
// userId is the AUTHENTICATED caller, and it is checked against the id
// the challenge was minted for. That check is what makes the challenge a
// binding rather than a bearer token: a challenge lifted from another
// user's begin response is useless here.
func (c *Ceremony) FinishRegistration(challengeId, userId string, body io.Reader) (*RegisteredCredential, error) {
	if c == nil || c.rp == nil {
		return nil, errors.New("webauthn: ceremony not configured")
	}
	entry, err := c.challenges.Take(challengeId, CeremonyRegister)
	if err != nil {
		return nil, err
	}
	if entry.UserId != strings.TrimSpace(userId) {
		return nil, ErrChallengeUserMismatch
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(body)
	if err != nil {
		return nil, fmt.Errorf("webauthn: attestation response rejected: %w", err)
	}
	user := &User{Id: entry.UserId}
	credential, err := c.rp.CreateCredential(user, *entry.Session, parsed)
	if err != nil {
		return nil, fmt.Errorf("webauthn: attestation verification failed: %w", err)
	}
	if !credential.Flags.UserVerified {
		return nil, ErrUserVerification
	}
	return newRegisteredCredential(credential), nil
}
