package http

// POST /auth/webauthn/register/{begin,finish} handler tests (memql#3406).
//
// Drives the handlers directly with httptest against a real JWT issuer
// (throwaway key), a real relying party derived from Cfg.BaseURL, a real
// software authenticator, and a stub engine that serves + captures
// v1:identity:identity rows. Nothing about the ceremony is faked: the
// attestation these tests post is one the library verifies.
//
// Mirrors badge_grant_test.go's harness shape so the two credential
// surfaces stay legible side by side.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

const (
	passkeyTestBaseURL = "https://identity.test"
	passkeyTestRPID    = "identity.test"
	passkeyTestOrigin  = "https://identity.test"
	passkeyTestUserId  = "v1:identity:user:op-1"
)

// ---------------------------------------------------------------------
// software authenticator (see the ceremony package's copy for the full
// narrative -- this one is deliberately standalone so the handler tests
// do not reach into another package's test files)
// ---------------------------------------------------------------------

type httpSoftwareAuthenticator struct {
	t            *testing.T
	key          *ecdsa.PrivateKey
	aaguid       []byte
	credentialID []byte
	flags        byte
	// signCount is the counter written into authenticatorData. Zero on
	// registration (which is what a platform authenticator reports) and
	// bumped by the login tests to drive the regression check.
	signCount uint32
}

func newHTTPSoftwareAuthenticator(t *testing.T) *httpSoftwareAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	credentialID := make([]byte, 32)
	_, err = rand.Read(credentialID)
	require.NoError(t, err)
	return &httpSoftwareAuthenticator{
		t:            t,
		key:          key,
		aaguid:       []byte{0x9c, 0x83, 0x5e, 0x11, 0x40, 0x0a, 0x4a, 0x1b, 0xb2, 0x77, 0x0e, 0x9d, 0x5c, 0x64, 0x37, 0x21},
		credentialID: credentialID,
		// user present | user verified | attested credential data |
		// backup eligible | backup state
		flags: 0x01 | 0x04 | 0x40 | 0x08 | 0x10,
	}
}

func (a *httpSoftwareAuthenticator) credentialIdB64() string {
	return base64.RawURLEncoding.EncodeToString(a.credentialID)
}

// coseKey encodes the public half as a COSE_Key (ES256 over P-256).
func (a *httpSoftwareAuthenticator) coseKey() []byte {
	a.t.Helper()
	enc, err := cbor.CTAP2EncOptions().EncMode()
	require.NoError(a.t, err)
	x := make([]byte, 32)
	y := make([]byte, 32)
	a.key.PublicKey.X.FillBytes(x)
	a.key.PublicKey.Y.FillBytes(y)
	out, err := enc.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	require.NoError(a.t, err)
	return out
}

// publicKeyIdB64 is the base64url COSE key the persisted row carries --
// what register/finish would have written, so a login test can seed a
// stored row without running an enrolment first.
func (a *httpSoftwareAuthenticator) publicKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(a.coseKey())
}

// counterBytes is the big-endian signature counter for authenticatorData.
func (a *httpSoftwareAuthenticator) counterBytes() []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, a.signCount)
	return buf
}

// assert emits the PublicKeyCredential JSON body a browser would POST to
// login/finish: a REAL ES256 signature over
// authenticatorData || sha256(clientDataJSON), with the user handle the
// authenticator stored at registration.
func (a *httpSoftwareAuthenticator) assert(challenge, userHandle string) json.RawMessage {
	a.t.Helper()
	rpIDHash := sha256.Sum256([]byte(passkeyTestRPID))
	authData := append([]byte{}, rpIDHash[:]...)
	// No attested credential data on an assertion.
	authData = append(authData, a.flags & ^byte(0x40))
	authData = append(authData, a.counterBytes()...)

	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.get",
		"challenge":   challenge,
		"origin":      passkeyTestOrigin,
		"crossOrigin": false,
	})
	require.NoError(a.t, err)

	clientDataHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientDataHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	require.NoError(a.t, err)

	body, err := json.Marshal(map[string]any{
		"id":                     a.credentialIdB64(),
		"rawId":                  a.credentialIdB64(),
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(userHandle)),
		},
	})
	require.NoError(a.t, err)
	return json.RawMessage(body)
}

func (a *httpSoftwareAuthenticator) create(challenge string) json.RawMessage {
	a.t.Helper()
	enc, err := cbor.CTAP2EncOptions().EncMode()
	require.NoError(a.t, err)

	coseKey := a.coseKey()

	rpIDHash := sha256.Sum256([]byte(passkeyTestRPID))
	authData := append([]byte{}, rpIDHash[:]...)
	authData = append(authData, a.flags)
	authData = append(authData, a.counterBytes()...)
	authData = append(authData, a.aaguid...)
	credLen := make([]byte, 2)
	binary.BigEndian.PutUint16(credLen, uint16(len(a.credentialID)))
	authData = append(authData, credLen...)
	authData = append(authData, a.credentialID...)
	authData = append(authData, coseKey...)

	attObj, err := enc.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	require.NoError(a.t, err)

	clientData, err := json.Marshal(map[string]any{
		"type":        "webauthn.create",
		"challenge":   challenge,
		"origin":      passkeyTestOrigin,
		"crossOrigin": false,
	})
	require.NoError(a.t, err)

	body, err := json.Marshal(map[string]any{
		"id":                     a.credentialIdB64(),
		"rawId":                  a.credentialIdB64(),
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
			"transports":        []string{"internal", "hybrid"},
		},
	})
	require.NoError(a.t, err)
	return json.RawMessage(body)
}

// ---------------------------------------------------------------------
// stub engine
// ---------------------------------------------------------------------

// passkeyStubEngine answers the three reads the handlers perform
// (userByIdSystem, passkeysForSelf, passkeyByCredentialId) and captures
// every mutation so the persisted row can be asserted on.
type passkeyStubEngine struct {
	// byCredentialId is the cluster-wide row set the duplicate check
	// sees, keyed by base64url credential id.
	byCredentialId map[string]map[string]any
	// forSelf is what passkeysForSelf returns.
	forSelf []map[string]any
	// user is the directory row userByIdSystem returns; nil for none.
	user map[string]any
	// enrolment is the row enrolmentTokenByHash returns; nil for none
	// (memql#3408). Keyed by nothing -- the lookup is by a 256-bit digest, so
	// a single row is all a test ever needs to stand in for "the one that
	// matches".
	enrolment map[string]any

	queries   []string
	mutations []string
}

func (e *passkeyStubEngine) nodes(rows ...map[string]any) (*memqlengine.ExecuteResult, error) {
	bundle := &memqlv1.GraphBundle{}
	for _, fields := range rows {
		st, err := structpb.NewStruct(fields)
		if err != nil {
			return nil, err
		}
		id, _ := fields["id"].(string)
		bundle.Nodes = append(bundle.Nodes, &memqlv1.MemoryNode{Id: id, Payload: st})
	}
	return &memqlengine.ExecuteResult{Bundle: bundle}, nil
}

func (e *passkeyStubEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "mutation") {
		e.mutations = append(e.mutations, query)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	e.queries = append(e.queries, query)
	switch {
	case strings.Contains(query, "userByIdSystem"):
		if e.user == nil {
			return e.nodes()
		}
		return e.nodes(e.user)
	case strings.Contains(query, "passkeysForSelf"):
		return e.nodes(e.forSelf...)
	case strings.Contains(query, "enrolmentTokenByHash"):
		if e.enrolment == nil {
			return e.nodes()
		}
		return e.nodes(e.enrolment)
	case strings.Contains(query, "passkeyByCredentialId"):
		for credId, fields := range e.byCredentialId {
			if strings.Contains(query, credId) {
				return e.nodes(fields)
			}
		}
		return e.nodes()
	}
	return e.nodes()
}

func passkeyRow(id, userId, credentialId string, active bool) map[string]any {
	return map[string]any{
		"id":     id,
		"userId": userId,
		"label":  "Existing passkey",
		"active": active,
		"credentials": map[string]any{
			"credentialId":   credentialId,
			"publicKey":      "cGsx",
			"signCount":      float64(0),
			"aaguid":         "",
			"transports":     []any{"internal"},
			"backupEligible": true,
			"backupState":    true,
			"registeredBy":   userId,
			"lastUsedAt":     "",
		},
	}
}

// ---------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------

func newPasskeyTestServer(t *testing.T, engine *passkeyStubEngine) *Server {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     passkeyTestBaseURL,
		JWTAudience: "memql",
		KeyDir:      dir,
		BrandName:   "memQL Test",
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)
	return &Server{
		Cfg:    cfg,
		Issuer: iss,
		Store:  &identity.Store{Engine: engine},
	}
}

func mintPasskeyBearer(t *testing.T, s *Server, userId, role string) string {
	t.Helper()
	token, _, err := s.Issuer.IssueAccessToken(identity.IssueInput{UserId: userId, Role: role}, time.Now().UTC())
	require.NoError(t, err)
	return token
}

func drivePasskey(t *testing.T, s *Server, path, bearer string, body any, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv(envAllowInsecurePair, "1")
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://identity.test"+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func decodeBegin(t *testing.T, rec *httptest.ResponseRecorder) WebAuthnRegisterBeginResponse {
	t.Helper()
	var resp WebAuthnRegisterBeginResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

func decodeFinish(t *testing.T, rec *httptest.ResponseRecorder) WebAuthnRegisterFinishResponse {
	t.Helper()
	var resp WebAuthnRegisterFinishResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// register runs the full two-step ceremony and returns the finish
// response recorder.
func register(t *testing.T, s *Server, engine *passkeyStubEngine, auth *httpSoftwareAuthenticator, bearer, label string) *httptest.ResponseRecorder {
	t.Helper()
	beginRec := drivePasskey(t, s, "/auth/webauthn/register/begin", bearer,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusOK, beginRec.Code, "body=%s", beginRec.Body.String())
	begin := decodeBegin(t, beginRec)
	require.True(t, begin.Success)

	return drivePasskey(t, s, "/auth/webauthn/register/finish", bearer, WebAuthnRegisterFinishRequest{
		ChallengeId: begin.ChallengeId,
		Label:       label,
		Credential:  auth.create(begin.CreationOptions.Response.Challenge.String()),
	}, s.handleWebAuthnRegisterFinish)
}

// ---------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------

func TestHandleWebAuthnRegisterBegin_IssuesABoundChallenge(t *testing.T) {
	engine := &passkeyStubEngine{
		user: map[string]any{
			"id":           passkeyTestUserId,
			"primaryEmail": "op@example.test",
			"displayName":  "Op One",
		},
	}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")

	rec := drivePasskey(t, s, "/auth/webauthn/register/begin", bearer,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeBegin(t, rec)
	require.True(t, resp.Success)
	require.NotEmpty(t, resp.ChallengeId)
	require.Equal(t, passkeyTestRPID, resp.RelyingPartyId)
	require.NotNil(t, resp.CreationOptions)

	opts := resp.CreationOptions.Response
	// The RP id is the CONFIGURED one, not the request Host.
	require.Equal(t, passkeyTestRPID, opts.RelyingParty.ID)
	require.Equal(t, "required", string(opts.AuthenticatorSelection.ResidentKey))
	require.Equal(t, "required", string(opts.AuthenticatorSelection.UserVerification))
	require.Equal(t, "Op One", opts.User.DisplayName)

	expiresAt, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	require.NoError(t, err)
	require.True(t, expiresAt.After(time.Now()))
}

// The RP id is derived from MEMQL_IDENTITY_BASE_URL, so a request that
// arrives claiming a different Host changes nothing.
func TestHandleWebAuthnRegisterBegin_IgnoresTheRequestHost(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	t.Setenv(envAllowInsecurePair, "1")

	raw, err := json.Marshal(WebAuthnRegisterBeginRequest{})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://evil.example/auth/webauthn/register/begin", bytes.NewReader(raw))
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	s.handleWebAuthnRegisterBegin(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decodeBegin(t, rec)
	require.Equal(t, passkeyTestRPID, resp.RelyingPartyId)
	require.Equal(t, passkeyTestRPID, resp.CreationOptions.Response.RelyingParty.ID)
}

// The caller's already-enrolled credentials become excludeCredentials,
// which is what stops a user silently enrolling one authenticator twice.
func TestHandleWebAuthnRegisterBegin_ExcludesTheCallersExistingPasskeys(t *testing.T) {
	engine := &passkeyStubEngine{
		forSelf: []map[string]any{
			passkeyRow("v1:identity:identity:pk-1", passkeyTestUserId, "Y3JlZC1vbmU", true),
			// A revoked row is NOT excluded: the user is entitled to
			// re-enrol an authenticator whose row they revoked.
			passkeyRow("v1:identity:identity:pk-2", passkeyTestUserId, "Y3JlZC10d28", false),
		},
	}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")

	rec := drivePasskey(t, s, "/auth/webauthn/register/begin", bearer,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	exclude := decodeBegin(t, rec).CreationOptions.Response.CredentialExcludeList
	require.Len(t, exclude, 1)
	require.Equal(t, "Y3JlZC1vbmU", base64.RawURLEncoding.EncodeToString(exclude[0].CredentialID))
}

func TestHandleWebAuthnRegisterBegin_RequiresABearer(t *testing.T) {
	s := newPasskeyTestServer(t, &passkeyStubEngine{})
	rec := drivePasskey(t, s, "/auth/webauthn/register/begin", "",
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleWebAuthnRegister_RequiresHTTPS(t *testing.T) {
	s := newPasskeyTestServer(t, &passkeyStubEngine{})
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")

	// No X-Forwarded-Proto, no TLS, and the dev escape hatch unset.
	raw, err := json.Marshal(WebAuthnRegisterBeginRequest{})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/auth/webauthn/register/begin", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	s.handleWebAuthnRegisterBegin(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "insecure_transport")
}

// A badge-class (or any non-user-class) session cannot enrol a passkey:
// enrolment binds a new way INTO the account.
func TestHandleWebAuthnRegisterBegin_RejectsNonUserClass(t *testing.T) {
	s := newPasskeyTestServer(t, &passkeyStubEngine{})
	token, _, err := s.Issuer.IssueBadgeGrantToken(identity.BadgeGrantIssueInput{
		UserId:          passkeyTestUserId,
		RoleCeiling:     "writer",
		BadgeIdentityId: "v1:identity:identity:bdg-1",
	}, time.Now().UTC())
	require.NoError(t, err)

	rec := drivePasskey(t, s, "/auth/webauthn/register/begin", token,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "caller_class_rejected", decodeBegin(t, rec).ErrorCode)
}

func TestHandleWebAuthnRegisterFinish_GoldenPath(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	rec := register(t, s, engine, auth, bearer, "Work laptop")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeFinish(t, rec)
	require.True(t, resp.Success)
	require.Equal(t, "Work laptop", resp.Label, "the created credential's label comes back")
	require.Equal(t, auth.credentialIdB64(), resp.CredentialId)
	require.True(t, strings.HasPrefix(resp.IdentityId, "v1:identity:identity:"))
	require.Equal(t, "9c835e11-400a-4a1b-b277-0e9d5c643721", resp.AAGUID)
	require.ElementsMatch(t, []string{"internal", "hybrid"}, resp.Transports)
	require.True(t, resp.BackupEligible)
	require.True(t, resp.BackupState)

	require.Len(t, engine.mutations, 1)
	write := engine.mutations[0]
	require.Contains(t, write, "createPasskeyIdentity")
	require.Contains(t, write, auth.credentialIdB64())
	require.Contains(t, write, `label:"Work laptop"`)
	require.Contains(t, write, `userId:"`+passkeyTestUserId+`"`)
	require.Contains(t, write, `registeredBy:"`+passkeyTestUserId+`"`)
	require.Contains(t, write, `backupEligible:true`)
}

// An unlabelled credential still gets a distinguishable name.
func TestHandleWebAuthnRegisterFinish_GeneratesALabel(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	rec := register(t, s, engine, auth, bearer, "")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeFinish(t, rec)
	require.True(t, strings.HasPrefix(resp.Label, "Passkey "))
	require.True(t, strings.HasSuffix(resp.Label, auth.credentialIdB64()[len(auth.credentialIdB64())-6:]))
}

// THE DUPLICATE-CREDENTIAL REJECTION. A credential id already present
// anywhere in the cluster is refused, because the login ceremony
// resolves an assertion through that id alone.
func TestHandleWebAuthnRegisterFinish_RejectsDuplicateCredentialId(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	// Pre-existing row carrying the same credential id, owned by the
	// SAME user.
	engine.byCredentialId = map[string]map[string]any{
		auth.credentialIdB64(): passkeyRow("v1:identity:identity:pk-existing", passkeyTestUserId, auth.credentialIdB64(), true),
	}

	rec := register(t, s, engine, auth, bearer, "Second copy")
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "duplicate_credential", decodeFinish(t, rec).ErrorCode)
	require.Empty(t, engine.mutations, "a duplicate must not write a row")
}

// The same rejection when the existing row belongs to ANOTHER user --
// the case that actually matters, since two rows sharing one credential
// id would make which account an assertion lands in a matter of row
// order.
func TestHandleWebAuthnRegisterFinish_RejectsCredentialIdOwnedByAnotherUser(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	engine.byCredentialId = map[string]map[string]any{
		auth.credentialIdB64(): passkeyRow("v1:identity:identity:pk-victim", "v1:identity:user:victim", auth.credentialIdB64(), true),
	}

	rec := register(t, s, engine, auth, bearer, "Takeover attempt")
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "duplicate_credential", decodeFinish(t, rec).ErrorCode)
	require.Empty(t, engine.mutations)
}

// A REVOKED row still holds the credential id: the authenticator has not
// forgotten the private key, so re-registering it would resurrect a
// credential the user revoked.
func TestHandleWebAuthnRegisterFinish_RejectsRevokedDuplicate(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	engine.byCredentialId = map[string]map[string]any{
		auth.credentialIdB64(): passkeyRow("v1:identity:identity:pk-revoked", passkeyTestUserId, auth.credentialIdB64(), false),
	}

	rec := register(t, s, engine, auth, bearer, "Re-enrol")
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "duplicate_credential", decodeFinish(t, rec).ErrorCode)
	require.Empty(t, engine.mutations)
}

func TestHandleWebAuthnRegisterFinish_RejectsAnUnknownChallenge(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	rec := drivePasskey(t, s, "/auth/webauthn/register/finish", bearer, WebAuthnRegisterFinishRequest{
		ChallengeId: "not-a-real-handle",
		Credential:  auth.create("c29tZS1jaGFsbGVuZ2U"),
	}, s.handleWebAuthnRegisterFinish)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "challenge_not_found", decodeFinish(t, rec).ErrorCode)
	require.Empty(t, engine.mutations)
}

// A challenge minted for one user cannot be finished by another. Drives
// begin as user A, finish with user B's bearer.
func TestHandleWebAuthnRegisterFinish_RejectsAnotherUsersChallenge(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	victimBearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	attackerBearer := mintPasskeyBearer(t, s, "v1:identity:user:attacker", "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	beginRec := drivePasskey(t, s, "/auth/webauthn/register/begin", victimBearer,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusOK, beginRec.Code)
	begin := decodeBegin(t, beginRec)

	rec := drivePasskey(t, s, "/auth/webauthn/register/finish", attackerBearer, WebAuthnRegisterFinishRequest{
		ChallengeId: begin.ChallengeId,
		Credential:  auth.create(begin.CreationOptions.Response.Challenge.String()),
	}, s.handleWebAuthnRegisterFinish)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "challenge_user_mismatch", decodeFinish(t, rec).ErrorCode)
	require.Empty(t, engine.mutations)
}

// Replaying a successful finish fails: the challenge was consumed.
func TestHandleWebAuthnRegisterFinish_ChallengeIsSingleUse(t *testing.T) {
	engine := &passkeyStubEngine{}
	s := newPasskeyTestServer(t, engine)
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")
	auth := newHTTPSoftwareAuthenticator(t)

	beginRec := drivePasskey(t, s, "/auth/webauthn/register/begin", bearer,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusOK, beginRec.Code)
	begin := decodeBegin(t, beginRec)
	finishBody := WebAuthnRegisterFinishRequest{
		ChallengeId: begin.ChallengeId,
		Credential:  auth.create(begin.CreationOptions.Response.Challenge.String()),
	}

	first := drivePasskey(t, s, "/auth/webauthn/register/finish", bearer, finishBody, s.handleWebAuthnRegisterFinish)
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	second := drivePasskey(t, s, "/auth/webauthn/register/finish", bearer, finishBody, s.handleWebAuthnRegisterFinish)
	require.Equal(t, http.StatusBadRequest, second.Code)
	require.Equal(t, "challenge_not_found", decodeFinish(t, second).ErrorCode)
	require.Len(t, engine.mutations, 1, "only the first attempt writes")
}

func TestHandleWebAuthnRegisterFinish_RequiresChallengeAndCredential(t *testing.T) {
	s := newPasskeyTestServer(t, &passkeyStubEngine{})
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")

	rec := drivePasskey(t, s, "/auth/webauthn/register/finish", bearer,
		WebAuthnRegisterFinishRequest{}, s.handleWebAuthnRegisterFinish)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "bad_request", decodeFinish(t, rec).ErrorCode)
}

// A base URL that yields no usable RP id makes the routes refuse rather
// than fall back to the request Host.
func TestHandleWebAuthnRegisterBegin_RefusesWithoutABaseURL(t *testing.T) {
	s := newPasskeyTestServer(t, &passkeyStubEngine{})
	s.Cfg.BaseURL = ""
	bearer := mintPasskeyBearer(t, s, passkeyTestUserId, "writer")

	rec := drivePasskey(t, s, "/auth/webauthn/register/begin", bearer,
		WebAuthnRegisterBeginRequest{}, s.handleWebAuthnRegisterBegin)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "webauthn_unconfigured", decodeBegin(t, rec).ErrorCode)
}

// The routes are mounted.
func TestMount_RegistersWebAuthnRoutes(t *testing.T) {
	s := newPasskeyTestServer(t, &passkeyStubEngine{})
	mux := http.NewServeMux()
	s.Mount(mux)

	for _, path := range []string{"/auth/webauthn/register/begin", "/auth/webauthn/register/finish"} {
		req := httptest.NewRequest(http.MethodPost, "http://identity.test"+path, strings.NewReader("{}"))
		handler, pattern := mux.Handler(req)
		require.NotEmpty(t, pattern, "POST %s is not mounted", path)
		require.NotNil(t, handler)
	}
}
