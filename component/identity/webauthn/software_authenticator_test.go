package webauthn

// A SOFTWARE AUTHENTICATOR for the ceremony tests (memql#3406).
//
// The registration ceremony is only worth testing if something actually
// produces an attestation: a test that stubs out the verifier proves the
// plumbing and nothing about whether a real authenticator's bytes are
// accepted. So this file is a minimal authenticator -- it holds a P-256
// key, assembles authenticatorData exactly as the spec lays it out, and
// emits the PublicKeyCredential JSON a browser would POST.
//
// It uses the "none" attestation format, which is what a platform
// authenticator (Touch ID, Windows Hello, an Android keystore) produces
// for a passkey. That is deliberate rather than a shortcut: "none" is
// the format this registration path is built to accept, so the test
// drives the same code the product does.
//
// The knobs exist so the negative cases are reachable: withSignedRPID and
// withOrigin let a test sign for the WRONG relying party or report the
// WRONG origin, which is how the origin and RP-ID checks get proved
// rather than assumed.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// authenticator data flag bits (WebAuthn §6.1).
const (
	flagUserPresent    byte = 0x01
	flagUserVerified   byte = 0x04
	flagBackupEligible byte = 0x08
	flagBackupState    byte = 0x10
	flagAttestedData   byte = 0x40
)

// softwareAuthenticator is a test-only WebAuthn authenticator.
type softwareAuthenticator struct {
	t            *testing.T
	key          *ecdsa.PrivateKey
	aaguid       []byte
	credentialID []byte
	signCount    uint32
	transports   []string

	// flags is the byte written into authenticatorData. Tests mutate it
	// to drop UV and prove the user-verification requirement bites.
	flags byte
}

// newSoftwareAuthenticator builds an authenticator with a fresh key, a
// fresh credential id, and the flags a passkey-capable platform
// authenticator sets: user present, user verified, attested credential
// data included, backup eligible + backed up.
func newSoftwareAuthenticator(t *testing.T) *softwareAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate authenticator key: %v", err)
	}
	credentialID := make([]byte, 32)
	if _, err := rand.Read(credentialID); err != nil {
		t.Fatalf("generate credential id: %v", err)
	}
	// A stable, non-zero AAGUID so the model round-trip is observable.
	aaguid := []byte{
		0x9c, 0x83, 0x5e, 0x11, 0x40, 0x0a, 0x4a, 0x1b,
		0xb2, 0x77, 0x0e, 0x9d, 0x5c, 0x64, 0x37, 0x21,
	}
	return &softwareAuthenticator{
		t:            t,
		key:          key,
		aaguid:       aaguid,
		credentialID: credentialID,
		transports:   []string{"internal", "hybrid"},
		flags:        flagUserPresent | flagUserVerified | flagAttestedData | flagBackupEligible | flagBackupState,
	}
}

// coseKey encodes the public half as a COSE_Key (ES256 over P-256) in
// CTAP2 canonical form, which is what an authenticator embeds in
// attested credential data.
func (a *softwareAuthenticator) coseKey() []byte {
	a.t.Helper()
	x := make([]byte, 32)
	y := make([]byte, 32)
	a.key.PublicKey.X.FillBytes(x)
	a.key.PublicKey.Y.FillBytes(y)
	enc, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		a.t.Fatalf("cbor enc mode: %v", err)
	}
	out, err := enc.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: x,
		-3: y,
	})
	if err != nil {
		a.t.Fatalf("encode COSE key: %v", err)
	}
	return out
}

// authenticatorData assembles rpIdHash || flags || signCount ||
// attestedCredentialData.
func (a *softwareAuthenticator) authenticatorData(rpID string) []byte {
	a.t.Helper()
	rpIDHash := sha256.Sum256([]byte(rpID))
	out := make([]byte, 0, 128)
	out = append(out, rpIDHash[:]...)
	out = append(out, a.flags)

	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, a.signCount)
	out = append(out, counter...)

	if a.flags&flagAttestedData != 0 {
		out = append(out, a.aaguid...)
		credLen := make([]byte, 2)
		binary.BigEndian.PutUint16(credLen, uint16(len(a.credentialID)))
		out = append(out, credLen...)
		out = append(out, a.credentialID...)
		out = append(out, a.coseKey()...)
	}
	return out
}

// attestationObject wraps authenticatorData in a "none"-format
// attestation object.
func (a *softwareAuthenticator) attestationObject(rpID string) []byte {
	a.t.Helper()
	enc, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		a.t.Fatalf("cbor enc mode: %v", err)
	}
	out, err := enc.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": a.authenticatorData(rpID),
	})
	if err != nil {
		a.t.Fatalf("encode attestation object: %v", err)
	}
	return out
}

// createOptions are the per-ceremony knobs. Zero values mean "use the
// relying party's real rpID / origin", i.e. the honest case.
type createOptions struct {
	rpID   string
	origin string
}

type createOption func(*createOptions)

// withSignedRPID makes the authenticator hash a DIFFERENT relying-party
// id into authenticatorData, which is the shape of a credential minted
// for another site being replayed at this one.
func withSignedRPID(rpID string) createOption {
	return func(o *createOptions) { o.rpID = rpID }
}

// withOrigin makes the client report a different origin in
// clientDataJSON.
func withOrigin(origin string) createOption {
	return func(o *createOptions) { o.origin = origin }
}

// Attest emits the PublicKeyCredential JSON body a browser would POST to
// the finish endpoint.
//
// challenge is the base64url challenge from the creation options; rpID
// and origin are the relying party's real values, which the options may
// override to produce a hostile response.
func (a *softwareAuthenticator) Attest(challenge, rpID, origin string, opts ...createOption) []byte {
	a.t.Helper()
	cfg := createOptions{rpID: rpID, origin: origin}
	for _, opt := range opts {
		opt(&cfg)
	}

	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.create",
		"challenge":   challenge,
		"origin":      cfg.origin,
		"crossOrigin": false,
	})
	if err != nil {
		a.t.Fatalf("encode clientDataJSON: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"id":                     base64.RawURLEncoding.EncodeToString(a.credentialID),
		"rawId":                  base64.RawURLEncoding.EncodeToString(a.credentialID),
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"attestationObject": base64.RawURLEncoding.EncodeToString(a.attestationObject(cfg.rpID)),
			"transports":        a.transports,
		},
	})
	if err != nil {
		a.t.Fatalf("encode credential response: %v", err)
	}
	return body
}

// CredentialIdB64 is the base64url credential id the row will carry.
func (a *softwareAuthenticator) CredentialIdB64() string {
	return base64.RawURLEncoding.EncodeToString(a.credentialID)
}

// PublicKeyB64 is the base64url COSE key the row will carry -- the same
// bytes FinishRegistration would have persisted, so a login test can
// build a stored Row without running a registration first.
func (a *softwareAuthenticator) PublicKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(a.coseKey())
}

// assertionAuthenticatorData assembles rpIdHash || flags || signCount for
// an ASSERTION. No attested credential data: that is registration-only,
// so the AT flag is cleared regardless of what the registration flags
// said.
func (a *softwareAuthenticator) assertionAuthenticatorData(rpID string) []byte {
	a.t.Helper()
	rpIDHash := sha256.Sum256([]byte(rpID))
	out := make([]byte, 0, 37)
	out = append(out, rpIDHash[:]...)
	out = append(out, a.flags & ^flagAttestedData)
	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, a.signCount)
	return append(out, counter...)
}

// Assert emits the PublicKeyCredential JSON body a browser would POST to
// the login/finish endpoint.
//
// The signature is the real thing -- ES256 over
// authenticatorData || sha256(clientDataJSON), exactly as WebAuthn §7.2
// step 20 specifies -- so these tests exercise the library's verifier
// rather than a stub of it. That is the whole reason this file exists.
//
// userHandle is what makes a DISCOVERABLE assertion discoverable: the
// authenticator stored the user id at registration and hands it back
// here, which is how a usernameless ceremony learns whose account it is
// looking at. The library refuses a blank one.
func (a *softwareAuthenticator) Assert(challenge, rpID, origin, userHandle string, opts ...createOption) []byte {
	a.t.Helper()
	cfg := createOptions{rpID: rpID, origin: origin}
	for _, opt := range opts {
		opt(&cfg)
	}

	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.get",
		"challenge":   challenge,
		"origin":      cfg.origin,
		"crossOrigin": false,
	})
	if err != nil {
		a.t.Fatalf("encode clientDataJSON: %v", err)
	}
	authData := a.assertionAuthenticatorData(cfg.rpID)
	clientDataHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientDataHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		a.t.Fatalf("sign assertion: %v", err)
	}

	body, err := json.Marshal(map[string]any{
		"id":                     a.CredentialIdB64(),
		"rawId":                  a.CredentialIdB64(),
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(userHandle)),
		},
	})
	if err != nil {
		a.t.Fatalf("encode assertion response: %v", err)
	}
	return body
}
