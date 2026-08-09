package webauthn

// Ceremony tests driven by the software authenticator in
// software_authenticator_test.go (memql#3406).
//
// Every negative case here is one the real ceremony has to refuse, and
// each is produced by making the AUTHENTICATOR misbehave rather than by
// stubbing the verifier -- a wrong origin is a wrong origin in the
// clientDataJSON, a wrong relying party is a different rpIdHash in the
// authenticatorData.

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	gowebauthn "github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"
)

func encodeB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

const (
	testBaseURL = "https://identity.test"
	testRPID    = "identity.test"
	testOrigin  = "https://identity.test"
	testUserId  = "v1:identity:user:u-1"
)

func newTestCeremony(t *testing.T) *Ceremony {
	t.Helper()
	c, err := New(Config{BaseURL: testBaseURL, DisplayName: "memQL Test"})
	require.NoError(t, err)
	return c
}

func TestRelyingParty_DerivesFromBaseURL(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		wantRPID   string
		wantOrigin string
	}{
		{"https", "https://identity.znas.io", "identity.znas.io", "https://identity.znas.io"},
		{"trailing slash", "https://identity.znas.io/", "identity.znas.io", "https://identity.znas.io"},
		// The port belongs to the origin, not to the RP id: the RP id is
		// a domain. That split is what makes a localhost dev deployment
		// work without a special case.
		{"localhost with port", "http://localhost:8080", "localhost", "http://localhost:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpID, origin, err := RelyingParty(tc.baseURL)
			require.NoError(t, err)
			require.Equal(t, tc.wantRPID, rpID)
			require.Equal(t, tc.wantOrigin, origin)
		})
	}
}

func TestRelyingParty_RefusesUnusableBaseURL(t *testing.T) {
	for _, bad := range []string{"", "   ", "identity.znas.io", "ftp://identity.znas.io"} {
		_, _, err := RelyingParty(bad)
		require.Error(t, err, "base url %q must be refused", bad)
	}
}

// The RP id must come from configuration, never from a request. This is
// the property stated as an acceptance criterion, so it gets a test that
// names it: two ceremonies built from two base URLs disagree, and
// neither has any input that a request could supply.
func TestNew_RPIDComesFromConfiguredBaseURLOnly(t *testing.T) {
	a := newTestCeremony(t)
	require.Equal(t, testRPID, a.RPID())
	require.Equal(t, testOrigin, a.Origin())

	b, err := New(Config{BaseURL: "https://elsewhere.example"})
	require.NoError(t, err)
	require.Equal(t, "elsewhere.example", b.RPID())

	_, err = New(Config{BaseURL: ""})
	require.ErrorIs(t, err, ErrNoBaseURL)
}

// residentKey=required + userVerification=required are what make the
// credential discoverable, which is what lets memql#3407 be usernameless.
func TestBeginRegistration_RequiresResidentKeyAndUserVerification(t *testing.T) {
	c := newTestCeremony(t)
	challenge, err := c.BeginRegistration(&User{Id: testUserId, Name: "u@example.test"})
	require.NoError(t, err)

	sel := challenge.Options.Response.AuthenticatorSelection
	require.Equal(t, "required", string(sel.ResidentKey))
	require.NotNil(t, sel.RequireResidentKey)
	require.True(t, *sel.RequireResidentKey)
	require.Equal(t, "required", string(sel.UserVerification))

	require.Equal(t, testRPID, challenge.Options.Response.RelyingParty.ID)
	require.NotEmpty(t, challenge.ChallengeId)
	require.True(t, challenge.ExpiresAt.After(time.Now()))
}

func TestBeginRegistration_ExcludesAlreadyEnrolledCredentials(t *testing.T) {
	c := newTestCeremony(t)
	existing, err := ToWebAuthnCredential(Row{
		CredentialId: "Y3JlZC1vbmU",
		Transports:   []string{"internal"},
	})
	require.NoError(t, err)

	challenge, err := c.BeginRegistration(&User{
		Id:          testUserId,
		Credentials: []gowebauthn.Credential{existing},
	})
	require.NoError(t, err)
	require.Len(t, challenge.Options.Response.CredentialExcludeList, 1)
	require.Equal(t, existing.ID, []byte(challenge.Options.Response.CredentialExcludeList[0].CredentialID))
}

func TestBeginRegistration_RequiresAUser(t *testing.T) {
	c := newTestCeremony(t)
	_, err := c.BeginRegistration(nil)
	require.Error(t, err)
	_, err = c.BeginRegistration(&User{Id: "  "})
	require.Error(t, err)
}

// The golden path: a real attestation from the software authenticator is
// verified and projected into the row shape.
func TestFinishRegistration_GoldenPath(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId, Name: "u@example.test"})
	require.NoError(t, err)

	body := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin)
	cred, err := c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.NoError(t, err)

	require.Equal(t, auth.CredentialIdB64(), cred.CredentialId)
	require.NotEmpty(t, cred.PublicKey, "the COSE public key must be captured")
	require.Equal(t, "9c835e11-400a-4a1b-b277-0e9d5c643721", cred.AAGUID)
	require.ElementsMatch(t, []string{"internal", "hybrid"}, cred.Transports)
	require.True(t, cred.BackupEligible)
	require.True(t, cred.BackupState)

	// And it round-trips back into a library credential, which is what
	// register/begin's exclusion list and memql#3407's verifier both do.
	back, err := ToWebAuthnCredential(Row{
		CredentialId:   cred.CredentialId,
		PublicKey:      cred.PublicKey,
		SignCount:      cred.SignCount,
		AAGUID:         cred.AAGUID,
		Transports:     cred.Transports,
		BackupEligible: cred.BackupEligible,
		BackupState:    cred.BackupState,
	})
	require.NoError(t, err)
	require.Equal(t, auth.CredentialIdB64(), encodeB64(back.ID))
}

// The challenge is single-use. The SECOND presentation fails even though
// the response is byte-identical and would otherwise verify -- which is
// the whole point: a captured response has nothing to replay against.
func TestFinishRegistration_ChallengeIsSingleUse(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin)

	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.NoError(t, err)

	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.ErrorIs(t, err, ErrChallengeNotFound)
}

// A FAILED finish burns the challenge too. Otherwise an attacker with a
// stolen challenge handle gets unlimited attempts against it.
func TestFinishRegistration_FailedAttemptStillConsumesTheChallenge(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)

	hostile := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, withOrigin("https://evil.example"))
	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(hostile))
	require.Error(t, err)

	honest := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin)
	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(honest))
	require.ErrorIs(t, err, ErrChallengeNotFound)
}

func TestFinishRegistration_RejectsWrongOrigin(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, withOrigin("https://evil.example"))

	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.Error(t, err)
}

func TestFinishRegistration_RejectsWrongRelyingPartyId(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, withSignedRPID("evil.example"))

	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.Error(t, err)
}

func TestFinishRegistration_RejectsWrongChallenge(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := auth.Attest("bm90LXRoZS1jaGFsbGVuZ2U", testRPID, testOrigin)

	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.Error(t, err)
}

// userVerification=required has to bite. Without UV a passkey is
// possession-only, which is not what this ceremony promises.
func TestFinishRegistration_RejectsMissingUserVerification(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)
	auth.flags &^= flagUserVerified

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin)

	_, err = c.FinishRegistration(challenge.ChallengeId, testUserId, bytes.NewReader(body))
	require.Error(t, err)
}

// The challenge is BOUND to the user it was minted for: a handle lifted
// from another user's begin response is useless.
func TestFinishRegistration_RejectsADifferentUser(t *testing.T) {
	c := newTestCeremony(t)
	auth := newSoftwareAuthenticator(t)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := auth.Attest(challenge.Options.Response.Challenge.String(), testRPID, testOrigin)

	_, err = c.FinishRegistration(challenge.ChallengeId, "v1:identity:user:someone-else", bytes.NewReader(body))
	require.ErrorIs(t, err, ErrChallengeUserMismatch)
}

func TestFinishRegistration_RejectsUnknownChallengeHandle(t *testing.T) {
	c := newTestCeremony(t)
	_, err := c.FinishRegistration("not-a-handle", testUserId, strings.NewReader("{}"))
	require.ErrorIs(t, err, ErrChallengeNotFound)
}

// TTL. The clock is injected rather than slept through.
func TestChallengeStore_Expires(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c, err := New(Config{BaseURL: testBaseURL, ChallengeTTL: time.Minute, Now: clock})
	require.NoError(t, err)

	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)

	now = now.Add(61 * time.Second)
	_, err = c.Challenges().Take(challenge.ChallengeId, CeremonyRegister)
	require.ErrorIs(t, err, ErrChallengeExpired)
}

// A registration challenge presented to a different ceremony is refused,
// which is what keeps the login ceremony (memql#3407) from being able to
// redeem one.
func TestChallengeStore_CeremonyTagIsChecked(t *testing.T) {
	c := newTestCeremony(t)
	challenge, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)

	_, err = c.Challenges().Take(challenge.ChallengeId, "login")
	require.ErrorIs(t, err, ErrChallengeNotFound)

	// ...and it was consumed anyway.
	_, err = c.Challenges().Take(challenge.ChallengeId, CeremonyRegister)
	require.ErrorIs(t, err, ErrChallengeNotFound)
}

// Abandoned ceremonies -- the common case, a user who closes the browser
// sheet -- must not accumulate.
func TestChallengeStore_SweepsExpiredOnPut(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c, err := New(Config{BaseURL: testBaseURL, ChallengeTTL: time.Minute, Now: clock})
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		_, err := c.BeginRegistration(&User{Id: testUserId})
		require.NoError(t, err)
	}
	require.Equal(t, 5, c.Challenges().Len())

	now = now.Add(2 * time.Minute)
	_, err = c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	require.Equal(t, 1, c.Challenges().Len(), "the five expired entries must be swept")
}

func TestFormatAAGUID_NormalisesAbsentAndZero(t *testing.T) {
	require.Equal(t, "", formatAAGUID(nil))
	require.Equal(t, "", formatAAGUID(make([]byte, 16)), "the all-zero model id is 'withheld', not a value")
	require.Equal(t, "", formatAAGUID([]byte{0x01, 0x02}), "a malformed length is not a model id")
}
