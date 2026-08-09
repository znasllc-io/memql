package http

// The enrolment token as an authority for the passkey registration ceremony
// (memql#3408).
//
// These tests run the REAL ceremony -- a software authenticator producing an
// attestation the library verifies -- with `Authorization: Enrolment
// mql_enr_...` in place of a session bearer. What they are pinning is the
// property the whole feature rests on: the token authorizes ONE registration,
// and a second presentation of it does not authorize another.
//
// They reuse webauthn_register_test.go's harness deliberately. Authorizing the
// ceremony a different way must not mean testing a different ceremony.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/identity/enrolment"
)

// enrolmentRow builds a v1:identity:enrolmentToken row as the shape query
// projects it.
func enrolmentRow(hash string, over map[string]any) map[string]any {
	row := map[string]any{
		"id":        "enr-1",
		"userId":    passkeyTestUserId,
		"tokenHash": hash,
		"issuedBy":  "v1:identity:user:admin",
		"expiresAt": time.Now().UTC().Add(enrolment.DefaultTTL).Format(time.RFC3339Nano),
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

// driveWithScheme is drivePasskey with an arbitrary Authorization value, so
// the enrolment scheme can drive the same handlers a Bearer does. Kept here
// rather than by widening drivePasskey, so webauthn_register_test.go's harness
// keeps exactly the shape memql#3406 left it in.
func driveWithScheme(t *testing.T, s *Server, path, authorization string, body any, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	// The transport check is the same one pair.go runs; the escape hatch is
	// how every handler test in this package reaches a handler at all.
	t.Setenv(envAllowInsecurePair, "1")
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://identity.test"+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// registerViaEnrolment runs the whole two-step ceremony under an enrolment
// token and returns the finish recorder.
func registerViaEnrolment(t *testing.T, s *Server, auth *httpSoftwareAuthenticator, code string) *httptest.ResponseRecorder {
	t.Helper()
	beginRec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Enrolment "+code,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusOK, beginRec.Code, "begin body=%s", beginRec.Body.String())
	begin := decodeBegin(t, beginRec)
	require.True(t, begin.Success)

	return driveWithScheme(t, s, "/auth/webauthn/register/finish", "Enrolment "+code,
		WebAuthnRegisterFinishRequest{
			ChallengeId: begin.ChallengeId,
			Credential:  auth.create(begin.CreationOptions.Response.Challenge.String()),
		}, s.handleWebAuthnRegisterFinish)
}

// ---------------------------------------------------------------------
// The happy path: one token, one passkey
// ---------------------------------------------------------------------

func TestEnrolmentTokenAuthorizesOneRegistration(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)

	engine := &passkeyStubEngine{
		user:      map[string]any{"id": passkeyTestUserId, "primaryEmail": "op@example.test", "displayName": "Op"},
		enrolment: enrolmentRow(hash, nil),
	}
	s := newPasskeyTestServer(t, engine)
	auth := newHTTPSoftwareAuthenticator(t)

	rec := registerViaEnrolment(t, s, auth, plain)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeFinish(t, rec)
	require.True(t, resp.Success)

	writes := strings.Join(engine.mutations, "\n")

	// THE SINGLE-USE STAMP LANDED. Without it the token stays live and the
	// same link registers a second passkey.
	require.Contains(t, writes, "mutation consumeEnrolmentToken", "the token was not spent")

	// AND IT LANDED BEFORE THE CREDENTIAL. webauthn.Store has no revoke, so a
	// failure between the two has to leave the token dead rather than the
	// passkey unmade -- which is only true if the consume is written first.
	consumeAt := strings.Index(writes, "mutation consumeEnrolmentToken")
	credentialAt := strings.Index(writes, "createPasskeyIdentity")
	require.GreaterOrEqual(t, credentialAt, 0, "no credential row was written; the ordering check below would be vacuous")
	require.Less(t, consumeAt, credentialAt,
		"the enrolment token must be spent BEFORE the credential row is written")

	// THE PLAINTEXT NEVER REACHED THE ENGINE, on the redeem path either.
	for _, q := range append(append([]string{}, engine.mutations...), engine.queries...) {
		require.NotContains(t, q, plain, "the plaintext enrolment token reached the engine")
	}
	require.Contains(t, strings.Join(engine.queries, "\n"), hash,
		"the lookup is by digest; without the hash present this check proves nothing")
}

// ---------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------

func TestASpentEnrolmentTokenCannotBeReplayed(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)

	// The row as it stands AFTER a successful registration.
	engine := &passkeyStubEngine{
		user: map[string]any{"id": passkeyTestUserId, "primaryEmail": "op@example.test"},
		enrolment: enrolmentRow(hash, map[string]any{
			"consumedAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		}),
	}
	s := newPasskeyTestServer(t, engine)

	rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Enrolment "+plain,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "enrolment_already_used", decodeBegin(t, rec).ErrorCode)
	require.Empty(t, engine.mutations, "a replay must write nothing at all")
}

func TestARevokedOrExpiredEnrolmentTokenIsRefusedWithItsOwnCode(t *testing.T) {
	cases := []struct {
		name     string
		over     map[string]any
		status   int
		wantCode string
	}{
		{
			name:     "expired",
			over:     map[string]any{"expiresAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)},
			status:   http.StatusGone,
			wantCode: "enrolment_expired",
		},
		{
			name:     "revoked",
			over:     map[string]any{"revokedAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)},
			status:   http.StatusForbidden,
			wantCode: "enrolment_revoked",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain, hash, err := enrolment.Mint()
			require.NoError(t, err)
			engine := &passkeyStubEngine{enrolment: enrolmentRow(hash, tc.over)}
			s := newPasskeyTestServer(t, engine)

			rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Enrolment "+plain,
				WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

			require.Equal(t, tc.status, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, tc.wantCode, decodeBegin(t, rec).ErrorCode)
			require.Empty(t, engine.mutations)
		})
	}
}

func TestAnUnknownEnrolmentTokenIsRefusedAsInvalid(t *testing.T) {
	plain, _, err := enrolment.Mint()
	require.NoError(t, err)
	// No enrolment row: nothing matches the digest.
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)

	rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Enrolment "+plain,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "enrolment_invalid", decodeBegin(t, rec).ErrorCode)
}

// The begin step must NOT spend the token. A person who opens the page, gets
// as far as the browser prompt and then reloads has done nothing wrong, and
// burning their only link for it would be a worse failure than any this
// feature prevents.
func TestBeginDoesNotSpendTheEnrolmentToken(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)
	engine := &passkeyStubEngine{
		user:      map[string]any{"id": passkeyTestUserId},
		enrolment: enrolmentRow(hash, nil),
	}
	s := newPasskeyTestServer(t, engine)

	for i := 0; i < 3; i++ {
		rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Enrolment "+plain,
			WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
		require.Equal(t, http.StatusOK, rec.Code, "attempt %d body=%s", i, rec.Body.String())
	}
	require.Empty(t, engine.mutations, "begin must write nothing; the stamp belongs on finish")
}

// The account is read off the ROW, never off a caller argument. A leaked token
// can only ever add a credential to the account it was minted for.
func TestTheEnrolledAccountComesFromTheRow(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)
	engine := &passkeyStubEngine{
		user:      map[string]any{"id": passkeyTestUserId},
		enrolment: enrolmentRow(hash, map[string]any{"userId": "v1:identity:user:someone-else"}),
	}
	s := newPasskeyTestServer(t, engine)
	auth := newHTTPSoftwareAuthenticator(t)

	rec := registerViaEnrolment(t, s, auth, plain)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	writes := strings.Join(engine.mutations, "\n")
	require.Contains(t, writes, "v1:identity:user:someone-else",
		"the credential must land on the account the ROW names")
	require.NotContains(t, writes, passkeyTestUserId,
		"no other account may be reachable from a token minted for someone else")
}

// A token with no owner authorizes nothing. Refused rather than defaulted,
// because every default here is somebody else's account.
func TestAnOwnerlessEnrolmentRowAuthorizesNothing(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)
	engine := &passkeyStubEngine{enrolment: enrolmentRow(hash, map[string]any{"userId": ""})}
	s := newPasskeyTestServer(t, engine)

	rec := driveWithScheme(t, s, "/auth/webauthn/register/begin", "Enrolment "+plain,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "enrolment_malformed", decodeBegin(t, rec).ErrorCode)
	require.Empty(t, engine.mutations)
}

// HTTPS is required on redeem, exactly as it is on /pair/redeem: the token is
// a plaintext bearer.
func TestTheCeremonyRefusesAPlaintextEnrolmentRequest(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)
	engine := &passkeyStubEngine{enrolment: enrolmentRow(hash, nil)}
	s := newPasskeyTestServer(t, engine)

	req := httptest.NewRequest(http.MethodPost, "http://identity.test/auth/webauthn/register/begin",
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Enrolment "+plain)
	rec := httptest.NewRecorder()
	s.handleWebAuthnRegisterBegin(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "insecure_transport")
}
