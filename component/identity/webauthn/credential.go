package webauthn

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	gowebauthn "github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// zeroAAGUID is the all-zero authenticator model id. Platform
// authenticators that deliberately withhold their model report it, so it
// is normalised to the empty string rather than persisted as a string of
// zeroes that reads like a real value.
const zeroAAGUID = "00000000-0000-0000-0000-000000000000"

// RegisteredCredential is the ceremony's output, in exactly the shape
// the v1:identity:identity passkey variant stores. Binary fields are
// base64url (unpadded) because that is the encoding the WebAuthn client
// API itself uses for these values, so a round-trip through the row and
// back into an allowCredentials list is byte-identical.
type RegisteredCredential struct {
	// CredentialId is base64url of the raw credential id. Unique across
	// the cluster; the registration handler enforces that before writing.
	CredentialId string

	// PublicKey is base64url of the COSE_Key, stored verbatim so the
	// assertion verifier hands the library back what it parsed.
	PublicKey string

	// SignCount is the authenticator's counter at registration. Zero is
	// normal -- most platform authenticators do not count.
	SignCount uint32

	// AAGUID is the authenticator model as a UUID string, or empty when
	// the authenticator withheld it.
	AAGUID string

	// Transports are the modalities the authenticator advertised. A hint
	// for the browser, never a server-side gate.
	Transports []string

	// BackupEligible is the BE flag: may this credential sync. Immutable
	// for the credential's life.
	BackupEligible bool

	// BackupState is the BS flag: is it currently backed up. Changes over
	// the credential's life.
	BackupState bool
}

// newRegisteredCredential projects go-webauthn's verified credential
// into the storable shape.
func newRegisteredCredential(c *gowebauthn.Credential) *RegisteredCredential {
	if c == nil {
		return nil
	}
	out := &RegisteredCredential{
		CredentialId:   base64.RawURLEncoding.EncodeToString(c.ID),
		PublicKey:      base64.RawURLEncoding.EncodeToString(c.PublicKey),
		SignCount:      c.Authenticator.SignCount,
		AAGUID:         formatAAGUID(c.Authenticator.AAGUID),
		BackupEligible: c.Flags.BackupEligible,
		BackupState:    c.Flags.BackupState,
	}
	for _, t := range c.Transport {
		if v := strings.TrimSpace(string(t)); v != "" {
			out.Transports = append(out.Transports, v)
		}
	}
	return out
}

// formatAAGUID renders the 16-byte model id as a UUID string, or "" when
// it is absent or all-zero.
func formatAAGUID(raw []byte) string {
	if len(raw) != 16 {
		return ""
	}
	parsed, err := uuid.FromBytes(raw)
	if err != nil {
		return ""
	}
	s := parsed.String()
	if s == zeroAAGUID {
		return ""
	}
	return s
}

// ToWebAuthnCredential converts a stored passkey row back into the
// library's credential type.
//
// Two callers, and they need different subsets: register/begin needs
// only the id and transports (it builds excludeCredentials), while the
// login ceremony (memql#3407) needs the public key and counter as well.
// One converter covers both rather than two partial ones, so a row that
// round-trips for exclusion is provably the same row that round-trips
// for verification.
func ToWebAuthnCredential(row Row) (gowebauthn.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(row.CredentialId))
	if err != nil {
		return gowebauthn.Credential{}, fmt.Errorf("webauthn: credential id %q is not base64url: %w", row.CredentialId, err)
	}
	out := gowebauthn.Credential{
		ID: id,
		Authenticator: gowebauthn.Authenticator{
			SignCount: row.SignCount,
		},
		Flags: gowebauthn.CredentialFlags{
			BackupEligible: row.BackupEligible,
			BackupState:    row.BackupState,
		},
	}
	if key := strings.TrimSpace(row.PublicKey); key != "" {
		if out.PublicKey, err = base64.RawURLEncoding.DecodeString(key); err != nil {
			return gowebauthn.Credential{}, fmt.Errorf("webauthn: public key for credential %q is not base64url: %w", row.CredentialId, err)
		}
	}
	if aaguid := strings.TrimSpace(row.AAGUID); aaguid != "" {
		if parsed, perr := uuid.Parse(aaguid); perr == nil {
			out.Authenticator.AAGUID = parsed[:]
		}
	}
	for _, t := range row.Transports {
		if v := strings.TrimSpace(t); v != "" {
			out.Transport = append(out.Transport, protocol.AuthenticatorTransport(v))
		}
	}
	return out, nil
}
