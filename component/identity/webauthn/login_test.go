package webauthn

// Login-ceremony tests (memql#3407), driven by the same software
// authenticator the registration tests use. The assertions here are
// REALLY SIGNED -- ES256 over authenticatorData || sha256(clientData) --
// so what these prove is that go-webauthn's verifier accepts and refuses
// the right bytes, not that our plumbing calls it.

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// storedRow builds the passkey row registration would have persisted for
// this authenticator, so a login test can start from a credential
// without running an enrolment first.
func storedRow(a *softwareAuthenticator, userId string, signCount uint32) Row {
	return Row{
		ID:             "v1:identity:identity:pk-" + a.CredentialIdB64()[:6],
		UserId:         userId,
		Label:          "Test passkey",
		Active:         true,
		CredentialId:   a.CredentialIdB64(),
		PublicKey:      a.PublicKeyB64(),
		SignCount:      signCount,
		Transports:     a.transports,
		BackupEligible: true,
		BackupState:    true,
		RegisteredBy:   userId,
	}
}

// resolverFor returns a CredentialResolver over a fixed row set, keyed
// the way the real one is: by credential id alone.
func resolverFor(rows ...Row) CredentialResolver {
	return func(credentialId string) (*Row, error) {
		for i := range rows {
			if rows[i].CredentialId == credentialId {
				row := rows[i]
				return &row, nil
			}
		}
		return nil, nil
	}
}

// THE USERNAMELESS PROPERTY. No email was typed, so there is no user
// whose credentials could be listed -- allowCredentials must be empty,
// and userVerification must be required.
func TestBeginLogin_IsUsernamelessAndRequiresUserVerification(t *testing.T) {
	c := newTestCeremony(t)

	challenge, err := c.BeginLogin(OAuthContext{ClientId: "cockpit", RedirectURI: "http://127.0.0.1:9-1/cb"})
	require.NoError(t, err)
	require.NotEmpty(t, challenge.ChallengeId)
	require.NotNil(t, challenge.Options)

	opts := challenge.Options.Response
	require.Empty(t, opts.AllowedCredentials, "a usernameless ceremony must not name any credential")
	require.Equal(t, "required", string(opts.UserVerification))
	require.Equal(t, testRPID, opts.RelyingPartyID)
	require.True(t, challenge.ExpiresAt.After(time.Now()))
}

// The OAuth context is held SERVER-SIDE on the challenge and comes back
// out of the ceremony untouched -- the client never restates it.
func TestFinishLogin_ReturnsTheChallengesOAuthContext(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)

	oauth := OAuthContext{
		ClientId:            "cockpit",
		RedirectURI:         "http://127.0.0.1:7421/callback",
		State:               "st-1",
		CodeChallenge:       "chal-1",
		CodeChallengeMethod: "S256",
	}
	challenge, err := c.BeginLogin(oauth)
	require.NoError(t, err)

	a.signCount = 1
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)
	asserted, err := c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.NoError(t, err)

	require.Equal(t, oauth, asserted.OAuth)
	require.Equal(t, testUserId, asserted.Row.UserId)
	require.Equal(t, row.ID, asserted.Row.ID)
	require.Equal(t, uint32(1), asserted.SignCount)
	require.True(t, asserted.BackupState)
}

// A USER WITH MULTIPLE PASSKEYS. Each of the user's authenticators
// resolves to its own row through its own credential id, and either one
// logs them in -- which is also the "enrolled on a different device"
// case, since a second device is exactly a second credential.
func TestFinishLogin_ResolvesAnyOfAUsersPasskeys(t *testing.T) {
	laptop := newSoftwareAuthenticator(t)
	phone := newSoftwareAuthenticator(t)
	rows := []Row{storedRow(laptop, testUserId, 0), storedRow(phone, testUserId, 0)}
	require.NotEqual(t, rows[0].CredentialId, rows[1].CredentialId)

	for name, a := range map[string]*softwareAuthenticator{"laptop": laptop, "phone": phone} {
		t.Run(name, func(t *testing.T) {
			c := newTestCeremony(t)
			challenge, err := c.BeginLogin(OAuthContext{})
			require.NoError(t, err)
			a.signCount = 3
			body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

			asserted, err := c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(rows...))
			require.NoError(t, err)
			require.Equal(t, a.CredentialIdB64(), asserted.Row.CredentialId)
			require.Equal(t, testUserId, asserted.Row.UserId)
		})
	}
}

// THE CLONED-AUTHENTICATOR SIGNAL. The stored counter is 7 and the
// assertion reports 7 again: on a real authenticator the counter only
// moves forward, so a repeat means two copies of the private key.
func TestFinishLogin_RejectsASignCountRegression(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stored, asserted_ uint32
	}{
		{"counter repeats", 7, 7},
		{"counter goes backwards", 7, 3},
		{"counter drops to zero after a non-zero", 7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCeremony(t)
			a := newSoftwareAuthenticator(t)
			row := storedRow(a, testUserId, tc.stored)

			challenge, err := c.BeginLogin(OAuthContext{})
			require.NoError(t, err)
			a.signCount = tc.asserted_
			body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

			_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrSignCountRegression), "got %v", err)
		})
	}
}

// THE ZERO-COUNTER CASE, and the reason the test above is not simply
// "new <= stored". iCloud Keychain and Windows Hello never implement a
// counter and report 0 on every assertion forever. Treating that as a
// regression would lock out the majority of real passkeys on their
// SECOND sign-in, which is the failure this case exists to prevent.
func TestFinishLogin_ZeroCounterAuthenticatorIsNotARegression(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)
	a.signCount = 0

	// Two consecutive logins, both reporting zero.
	for i := 0; i < 2; i++ {
		challenge, err := c.BeginLogin(OAuthContext{})
		require.NoError(t, err)
		body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

		asserted, err := c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
		require.NoError(t, err, "attempt %d: a zero counter on both sides is 'this authenticator does not count', not a clone", i+1)
		require.Equal(t, uint32(0), asserted.SignCount)
	}
}

// A credential id nothing matches is refused. The discoverable ceremony
// resolves through that id alone, so this is the "unknown authenticator"
// path.
func TestFinishLogin_RejectsAnUnknownCredential(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

	// Resolver holds a DIFFERENT authenticator's row.
	other := storedRow(newSoftwareAuthenticator(t), testUserId, 0)
	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(other))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrCredentialUnknown), "got %v", err)
}

func TestFinishLogin_RejectsARevokedCredential(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)
	row.Active = false

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	a.signCount = 1
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrCredentialRevoked), "got %v", err)
}

// The user handle binds the signature to an ACCOUNT, not just to a key.
// A row whose userId had been re-pointed at someone else fails here
// rather than logging the attacker in as them.
func TestFinishLogin_RejectsAUserHandleThatDoesNotMatchTheRow(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	a.signCount = 1
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, "v1:identity:user:someone-else")

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification failed")
}

// A user gesture is required: possession of the device is not identity.
func TestFinishLogin_RejectsAnAssertionWithoutUserVerification(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	a.signCount = 1
	a.flags &^= flagUserVerified
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.Error(t, err)
}

func TestFinishLogin_RejectsAWrongOrigin(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	a.signCount = 1
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId,
		withOrigin("https://evil.example"))

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.Error(t, err)
}

func TestFinishLogin_RejectsAWrongRelyingParty(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	a.signCount = 1
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId,
		withSignedRPID("evil.example"))

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.Error(t, err)
}

// The challenge is single-use, and a FAILED attempt burns it too --
// otherwise the window between look and consume is a replay window.
func TestFinishLogin_ChallengeIsSingleUse(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)
	row := storedRow(a, testUserId, 0)

	challenge, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	a.signCount = 1
	body := a.Assert(challenge.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.NoError(t, err)

	_, err = c.FinishLogin(challenge.ChallengeId, bytes.NewReader(body), resolverFor(row))
	require.True(t, errors.Is(err, ErrChallengeNotFound), "got %v", err)
}

// CEREMONY CONFUSION. A registration challenge presented to the login
// ceremony -- and the reverse -- is refused by the ceremony tag, so a
// challenge minted under an authenticated session can never be redeemed
// on the unauthenticated path.
func TestChallengeTagsSeparateRegistrationFromLogin(t *testing.T) {
	c := newTestCeremony(t)
	a := newSoftwareAuthenticator(t)

	reg, err := c.BeginRegistration(&User{Id: testUserId})
	require.NoError(t, err)
	body := a.Assert(reg.Options.Response.Challenge.String(), testRPID, testOrigin, testUserId)
	_, err = c.FinishLogin(reg.ChallengeId, bytes.NewReader(body), resolverFor(storedRow(a, testUserId, 0)))
	require.True(t, errors.Is(err, ErrChallengeNotFound), "got %v", err)

	login, err := c.BeginLogin(OAuthContext{})
	require.NoError(t, err)
	attestation := a.Attest(login.Options.Response.Challenge.String(), testRPID, testOrigin)
	_, err = c.FinishRegistration(login.ChallengeId, testUserId, bytes.NewReader(attestation))
	require.True(t, errors.Is(err, ErrChallengeNotFound), "got %v", err)
}
