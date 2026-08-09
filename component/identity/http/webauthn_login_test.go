package http

// POST /auth/webauthn/login/{begin,finish} handler tests (memql#3407).
//
// The centre of gravity is TestPasskeyLogin_YieldsAnAuthCodeRedeemableWithPKCE:
// a passkey assertion has to produce THE SAME auth code a magic-link
// click produces, PKCE binding and all, or the claim the epic rests on
// -- that no client learns which factor ran -- is not true. That test
// drives the whole path in one go: begin, a really-signed assertion,
// finish, and then the resulting code straight into handleToken, which
// is the code that /oauth/token runs in production and knows nothing
// about passkeys.
//
// Everything is real except the graph: a real JWT issuer, a real relying
// party derived from Cfg.BaseURL, a real software authenticator, and the
// real PKCE verifier. The engine is a stub that serves rows and captures
// mutations -- including the createAuthCode it captures and then serves
// straight back as authCodeByCodeHash, which is what makes the
// mint-then-redeem round trip an actual round trip.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

const (
	passkeyLoginClientId    = "cockpit"
	passkeyLoginRedirectURI = "http://127.0.0.1:7421/callback"
	passkeyLoginState       = "state-abc"
	// RFC 7636 Appendix B pair, the same one token_sso_pkce_test.go uses.
	passkeyLoginVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	passkeyLoginChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

// ---------------------------------------------------------------------
// stub engine
// ---------------------------------------------------------------------

// passkeyLoginEngine extends the registration stub's job with the auth
// code half: it captures the createAuthCode mutation and serves the row
// back through authCodeByCodeHash, so the code minted by login/finish is
// the same one /oauth/token looks up.
type passkeyLoginEngine struct {
	passkeyStubEngine

	// authCode is what createAuthCode wrote, or nil until it runs.
	authCode map[string]string
	// consumed records that /oauth/token burned the code, so a replay
	// hits the consumedAt branch.
	consumed bool
	// failMutationsMatching makes any mutation containing this substring
	// return an error, so the best-effort paths can be proved best-effort
	// rather than assumed.
	failMutationsMatching string
}

func (e *passkeyLoginEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	trimmed := strings.TrimSpace(query)
	if e.failMutationsMatching != "" && strings.Contains(query, e.failMutationsMatching) {
		e.mutations = append(e.mutations, query)
		return nil, errors.New("stub engine: mutation refused")
	}
	switch {
	case strings.HasPrefix(trimmed, "mutation createAuthCode"):
		e.mutations = append(e.mutations, query)
		e.authCode = map[string]string{}
		for _, k := range []string{
			"codeId", "codeHash", "clientId", "redirectURI", "state",
			"codeChallenge", "codeChallengeMethod", "userId", "identityId",
			"magicLinkRequestId", "expiresAt",
		} {
			e.authCode[k] = extractField(query, k)
		}
		return e.nodes()

	case strings.HasPrefix(trimmed, "mutation consumeAuthCode"):
		e.mutations = append(e.mutations, query)
		e.consumed = true
		return e.nodes()

	case strings.Contains(query, "oAuthClientByClientId"):
		e.queries = append(e.queries, query)
		// Only the ONE registered client resolves. A stub that answered
		// for any id would make the unregistered-client case untestable
		// -- and that case is the one keeping a freshly minted code from
		// being aimed at a redirect URI nobody approved.
		if !strings.Contains(query, `"`+passkeyLoginClientId+`"`) {
			return e.nodes()
		}
		return e.nodes(map[string]any{
			"id":               passkeyLoginClientId,
			"clientId":         passkeyLoginClientId,
			"redirectURIsJSON": `["` + passkeyLoginRedirectURI + `"]`,
		})

	case strings.Contains(query, "authCodeByCodeHash"):
		e.queries = append(e.queries, query)
		if e.authCode == nil || extractField(query, "codeHash") != e.authCode["codeHash"] {
			return e.nodes()
		}
		row := map[string]any{
			"id":                  e.authCode["codeId"],
			"codeHash":            e.authCode["codeHash"],
			"clientId":            e.authCode["clientId"],
			"redirectURI":         e.authCode["redirectURI"],
			"state":               e.authCode["state"],
			"codeChallenge":       e.authCode["codeChallenge"],
			"codeChallengeMethod": e.authCode["codeChallengeMethod"],
			"userId":              e.authCode["userId"],
			"identityId":          e.authCode["identityId"],
			"expiresAt":           e.authCode["expiresAt"],
		}
		if e.consumed {
			row["consumedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return e.nodes(row)
	}
	return e.passkeyStubEngine.Execute(ctx, query)
}

// ---------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------

// loginPasskeyRow is the row register/finish would have persisted for
// this authenticator.
func loginPasskeyRow(a *httpSoftwareAuthenticator, userId string, signCount float64, active bool) map[string]any {
	return map[string]any{
		"id":     "v1:identity:identity:pk-" + a.credentialIdB64()[:6],
		"userId": userId,
		"label":  "Test passkey",
		"active": active,
		"credentials": map[string]any{
			"credentialId":   a.credentialIdB64(),
			"publicKey":      a.publicKeyB64(),
			"signCount":      signCount,
			"aaguid":         "",
			"transports":     []any{"internal"},
			"backupEligible": true,
			"backupState":    true,
			"registeredBy":   userId,
			"lastUsedAt":     "",
		},
	}
}

func newPasskeyLoginServer(t *testing.T, rows ...map[string]any) (*Server, *passkeyLoginEngine) {
	t.Helper()
	engine := &passkeyLoginEngine{}
	engine.byCredentialId = map[string]map[string]any{}
	for _, row := range rows {
		creds, _ := row["credentials"].(map[string]any)
		credId, _ := creds["credentialId"].(string)
		engine.byCredentialId[credId] = row
	}
	engine.user = map[string]any{
		"id":           passkeyTestUserId,
		"primaryEmail": "op@example.test",
		"displayName":  "Op One",
		"role":         "writer",
	}
	s := newPasskeyTestServer(t, &engine.passkeyStubEngine)
	// Re-point the store at the extended engine: newPasskeyTestServer
	// wires the embedded one, which does not know about auth codes.
	s.Store = &identity.Store{Engine: engine}
	return s, engine
}

func beginPasskeyLogin(t *testing.T, s *Server, body WebAuthnLoginBeginRequest) *httptest.ResponseRecorder {
	t.Helper()
	return drivePasskey(t, s, "/auth/webauthn/login/begin", "", body, s.handleWebAuthnLoginBegin)
}

func decodeLoginBegin(t *testing.T, rec *httptest.ResponseRecorder) WebAuthnLoginBeginResponse {
	t.Helper()
	var resp WebAuthnLoginBeginResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

func decodeLoginFinish(t *testing.T, rec *httptest.ResponseRecorder) WebAuthnLoginFinishResponse {
	t.Helper()
	var resp WebAuthnLoginFinishResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// pkceBeginRequest is the in-flight OAuth context the /login page hands
// the ceremony -- the same five fields it puts in the magic-link form's
// hidden inputs.
func pkceBeginRequest() WebAuthnLoginBeginRequest {
	return WebAuthnLoginBeginRequest{
		ClientId:            passkeyLoginClientId,
		RedirectURI:         passkeyLoginRedirectURI,
		State:               passkeyLoginState,
		CodeChallenge:       passkeyLoginChallenge,
		CodeChallengeMethod: "S256",
	}
}

// runPasskeyLogin drives begin + finish and returns the finish recorder.
func runPasskeyLogin(t *testing.T, s *Server, a *httpSoftwareAuthenticator, userHandle string, begin WebAuthnLoginBeginRequest) *httptest.ResponseRecorder {
	t.Helper()
	beginRec := beginPasskeyLogin(t, s, begin)
	require.Equal(t, http.StatusOK, beginRec.Code, "body=%s", beginRec.Body.String())
	beginResp := decodeLoginBegin(t, beginRec)
	require.True(t, beginResp.Success)

	return drivePasskey(t, s, "/auth/webauthn/login/finish", "", WebAuthnLoginFinishRequest{
		ChallengeId: beginResp.ChallengeId,
		Credential:  a.assert(beginResp.RequestOptions.Response.Challenge.String(), userHandle),
	}, s.handleWebAuthnLoginFinish)
}

// ---------------------------------------------------------------------
// THE DECISIVE TEST
// ---------------------------------------------------------------------

// A passkey login yields an auth code redeemable at /oauth/token with
// the PKCE binding intact.
//
// This is what the task is for. handleToken is the production
// authorization_code grant; it has no passkey branch and no way to ask
// which factor ran. If the code a passkey mints redeems there under the
// matching verifier -- and refuses under a wrong one -- then /authorize,
// the Cockpit, the portal, the SDK and the VS Code extension all keep
// working unchanged, which is the whole claim.
func TestPasskeyLogin_YieldsAnAuthCodeRedeemableWithPKCE(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))

	a.signCount = 1
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	finish := decodeLoginFinish(t, rec)
	require.True(t, finish.Success)

	// The callback target is the SAME shape buildClientCallback produces
	// for a magic-link consume: the client's redirect URI carrying `code`
	// and the echoed `state`.
	target, err := url.Parse(finish.RedirectTo)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:7421", target.Host)
	require.Equal(t, "/callback", target.Path)
	require.Equal(t, passkeyLoginState, target.Query().Get("state"))
	code := target.Query().Get("code")
	require.NotEmpty(t, code)

	// The persisted row carries the PKCE binding the ceremony was begun
	// with, and only the digest of the code (issue #3187).
	require.NotNil(t, engine.authCode)
	require.Equal(t, passkeyLoginChallenge, engine.authCode["codeChallenge"])
	require.Equal(t, "S256", engine.authCode["codeChallengeMethod"])
	require.Equal(t, passkeyTestUserId, engine.authCode["userId"])
	require.Equal(t, "", engine.authCode["magicLinkRequestId"], "no magic link was involved")
	require.Equal(t, hashCode(code), engine.authCode["codeHash"])
	require.NotContains(t, engine.mutations[len(engine.mutations)-1], code,
		"the plaintext auth code must never reach the database")

	// REDEEM IT. Production code path, no passkey knowledge anywhere.
	tokenRec := postToken(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {passkeyLoginClientId},
		"redirect_uri":  {passkeyLoginRedirectURI},
		"code_verifier": {passkeyLoginVerifier},
	})
	require.Equal(t, http.StatusOK, tokenRec.Code, "body=%s", tokenRec.Body.String())

	var token tokenResponse
	require.NoError(t, json.NewDecoder(tokenRec.Body).Decode(&token))
	require.NotEmpty(t, token.AccessToken)
	require.Equal(t, "Bearer", token.TokenType)

	// The token is the ordinary user access token, minted for the user
	// the credential resolved to.
	claims, err := s.Issuer.VerifyAccessToken(token.AccessToken, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, passkeyTestUserId, claims.Subject)
}

// The other half of the binding: a WRONG verifier must not redeem a
// passkey-minted code, exactly as it must not redeem a magic-link one.
// Without this the test above would pass on a code that carried no
// challenge at all.
func TestPasskeyLogin_AuthCodeRefusesAWrongPKCEVerifier(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, _ := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))

	a.signCount = 1
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	target, err := url.Parse(decodeLoginFinish(t, rec).RedirectTo)
	require.NoError(t, err)
	code := target.Query().Get("code")

	for name, verifier := range map[string]string{
		"wrong verifier":   "this-is-not-the-right-verifier-value-000000",
		"missing verifier": "",
	} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{
				"grant_type":   {"authorization_code"},
				"code":         {code},
				"client_id":    {passkeyLoginClientId},
				"redirect_uri": {passkeyLoginRedirectURI},
			}
			if verifier != "" {
				form.Set("code_verifier", verifier)
			}
			tokenRec := postToken(t, s, form)
			require.NotEqual(t, http.StatusOK, tokenRec.Code, "body=%s", tokenRec.Body.String())
			require.Contains(t, tokenRec.Body.String(), "invalid_grant")
		})
	}
}

// ---------------------------------------------------------------------
// begin
// ---------------------------------------------------------------------

// The challenge is USERNAMELESS: no email has been typed, so
// allowCredentials is empty and userVerification is required.
func TestHandleWebAuthnLoginBegin_IssuesAUsernamelessChallenge(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)

	rec := beginPasskeyLogin(t, s, pkceBeginRequest())
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeLoginBegin(t, rec)
	require.True(t, resp.Success)
	require.NotEmpty(t, resp.ChallengeId)
	require.Equal(t, passkeyTestRPID, resp.RelyingPartyId)
	require.NotNil(t, resp.RequestOptions)

	opts := resp.RequestOptions.Response
	require.Empty(t, opts.AllowedCredentials, "allowCredentials must be EMPTY -- nobody typed an email")
	require.Equal(t, "required", string(opts.UserVerification))
	require.Equal(t, passkeyTestRPID, opts.RelyingPartyID)

	expiresAt, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	require.NoError(t, err)
	require.True(t, expiresAt.After(time.Now()))
}

// An unregistered client / redirect URI is refused AT BEGIN, so an
// unapproved target can never become the stored one a finished ceremony
// delivers to.
func TestHandleWebAuthnLoginBegin_RejectsAnUnregisteredRelyingParty(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)

	for name, body := range map[string]WebAuthnLoginBeginRequest{
		"unknown client":   {ClientId: "not-a-client", RedirectURI: passkeyLoginRedirectURI},
		"foreign redirect": {ClientId: passkeyLoginClientId, RedirectURI: "https://evil.example/steal"},
		"no relying party": {},
		"redirect only":    {RedirectURI: passkeyLoginRedirectURI},
		"client id only":   {ClientId: passkeyLoginClientId},
	} {
		t.Run(name, func(t *testing.T) {
			rec := beginPasskeyLogin(t, s, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, []string{"invalid_client", "client_required"}, decodeLoginBegin(t, rec).ErrorCode)
		})
	}
}

func TestHandleWebAuthnLogin_RequiresHTTPS(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)

	raw, err := json.Marshal(pkceBeginRequest())
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://identity.test/auth/webauthn/login/begin", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	s.handleWebAuthnLoginBegin(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "insecure_transport")
}

// ---------------------------------------------------------------------
// finish
// ---------------------------------------------------------------------

// THE SIGN-COUNT REGRESSION. The stored counter is 7 and the assertion
// reports 7 again -- on a genuine authenticator the counter only moves
// forward, so this is the spec's cloned-authenticator signal. Refused,
// and no auth code is minted.
func TestHandleWebAuthnLoginFinish_RejectsASignCountRegression(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 7, true))

	a.signCount = 7
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())

	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "sign_count_regression", decodeLoginFinish(t, rec).ErrorCode)
	require.Nil(t, engine.authCode, "a cloned-authenticator signal must not mint an auth code")
}

// THE ZERO-COUNTER CASE. iCloud Keychain and Windows Hello never
// implement a counter and report 0 forever, so a stored 0 meeting an
// asserted 0 is "this authenticator does not count", NOT a regression.
// Reading it as one would lock out most real passkeys on their second
// sign-in.
func TestHandleWebAuthnLoginFinish_ZeroCounterIsNotARegression(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))
	a.signCount = 0

	for i := 0; i < 2; i++ {
		engine.authCode = nil
		rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())
		require.Equal(t, http.StatusOK, rec.Code, "attempt %d: body=%s", i+1, rec.Body.String())
		require.NotNil(t, engine.authCode)
	}
}

// A MULTI-CREDENTIAL USER. Two authenticators, two rows, one account --
// which is also the "passkey enrolled on a different device" case, since
// a second device is exactly a second credential. Either one signs the
// user in, and each resolves to its OWN row.
func TestHandleWebAuthnLoginFinish_WorksForEitherOfAUsersPasskeys(t *testing.T) {
	laptop := newHTTPSoftwareAuthenticator(t)
	phone := newHTTPSoftwareAuthenticator(t)
	require.NotEqual(t, laptop.credentialIdB64(), phone.credentialIdB64())

	for name, a := range map[string]*httpSoftwareAuthenticator{"laptop": laptop, "phone": phone} {
		t.Run(name, func(t *testing.T) {
			s, engine := newPasskeyLoginServer(t,
				loginPasskeyRow(laptop, passkeyTestUserId, 0, true),
				loginPasskeyRow(phone, passkeyTestUserId, 0, true),
			)
			a.signCount = 4
			rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			require.NotNil(t, engine.authCode)
			require.Equal(t, passkeyTestUserId, engine.authCode["userId"])
			require.Equal(t, "v1:identity:identity:pk-"+a.credentialIdB64()[:6], engine.authCode["identityId"],
				"the auth code must name the credential that actually authenticated")
		})
	}
}

// An unknown credential is refused, and the client-facing code does not
// distinguish it from a forged signature: the credential id travels in
// the clear, so an unauthenticated caller must not learn whether one is
// enrolled here.
func TestHandleWebAuthnLoginFinish_RejectsAnUnknownCredential(t *testing.T) {
	enrolled := newHTTPSoftwareAuthenticator(t)
	stranger := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(enrolled, passkeyTestUserId, 0, true))

	stranger.signCount = 1
	rec := runPasskeyLogin(t, s, stranger, passkeyTestUserId, pkceBeginRequest())

	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "assertion_rejected", decodeLoginFinish(t, rec).ErrorCode)
	require.Nil(t, engine.authCode)
}

// A revoked row is refused, and reports the SAME code an unknown one
// does. The audit trail keeps the distinction; the wire does not.
func TestHandleWebAuthnLoginFinish_RejectsARevokedCredential(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, false))

	a.signCount = 1
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())

	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "assertion_rejected", decodeLoginFinish(t, rec).ErrorCode)
	require.Nil(t, engine.authCode)
}

// lastUsedAt (plus the counter and the current backup state) is stamped
// on success -- and backupEligible is echoed back UNCHANGED, because the
// spec fixes it for the credential's lifetime.
func TestHandleWebAuthnLoginFinish_StampsLastUsedAt(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 2, true))

	a.signCount = 9
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var stamp string
	for _, m := range engine.mutations {
		if strings.Contains(m, "recordPasskeyAssertion") {
			stamp = m
		}
	}
	require.NotEmpty(t, stamp, "a successful login must stamp the passkey row")
	require.Contains(t, stamp, "signCount:9")
	require.Contains(t, stamp, "backupEligible:true")
	require.Contains(t, stamp, `credentialId:"`+a.credentialIdB64()+`"`)
	lastUsedAt := quotedArg(t, stamp, "lastUsedAt")
	parsed, err := time.Parse(time.RFC3339Nano, lastUsedAt)
	require.NoError(t, err, "lastUsedAt=%q", lastUsedAt)
	require.WithinDuration(t, time.Now().UTC(), parsed, time.Minute)
}

// The stamp is BEST-EFFORT: a bookkeeping write that fails must not cost
// the user their login, matching the badge variant's precedent.
func TestHandleWebAuthnLoginFinish_SurvivesAFailedLastUsedStamp(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))
	engine.failMutationsMatching = "recordPasskeyAssertion"

	a.signCount = 1
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, pkceBeginRequest())

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.True(t, decodeLoginFinish(t, rec).Success)
	require.NotNil(t, engine.authCode, "the auth code stands even when the stamp fails")
}

// A replayed finish fails: the challenge was consumed by the first
// attempt, so a captured assertion has nothing to replay against.
func TestHandleWebAuthnLoginFinish_ChallengeIsSingleUse(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, _ := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))

	beginRec := beginPasskeyLogin(t, s, pkceBeginRequest())
	require.Equal(t, http.StatusOK, beginRec.Code)
	begin := decodeLoginBegin(t, beginRec)

	a.signCount = 1
	body := WebAuthnLoginFinishRequest{
		ChallengeId: begin.ChallengeId,
		Credential:  a.assert(begin.RequestOptions.Response.Challenge.String(), passkeyTestUserId),
	}

	first := drivePasskey(t, s, "/auth/webauthn/login/finish", "", body, s.handleWebAuthnLoginFinish)
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	second := drivePasskey(t, s, "/auth/webauthn/login/finish", "", body, s.handleWebAuthnLoginFinish)
	require.Equal(t, http.StatusBadRequest, second.Code)
	require.Equal(t, "challenge_not_found", decodeLoginFinish(t, second).ErrorCode)
}

func TestHandleWebAuthnLoginFinish_RequiresChallengeAndCredential(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)
	rec := drivePasskey(t, s, "/auth/webauthn/login/finish", "",
		WebAuthnLoginFinishRequest{}, s.handleWebAuthnLoginFinish)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "bad_request", decodeLoginFinish(t, rec).ErrorCode)
}

// The routes are mounted, alongside the registration pair.
func TestMount_RegistersWebAuthnLoginRoutes(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)
	mux := http.NewServeMux()
	s.Mount(mux)

	for _, path := range []string{"/auth/webauthn/login/begin", "/auth/webauthn/login/finish"} {
		req := httptest.NewRequest(http.MethodPost, "http://identity.test"+path, strings.NewReader("{}"))
		handler, pattern := mux.Handler(req)
		require.NotEmpty(t, pattern, "POST %s is not mounted", path)
		require.NotNil(t, handler)
	}
}

// quotedArg pulls `key:"value"` out of a DSL call. Not extractField:
// that one matches `key: "` (a space) and the webauthn store writes no
// space, so it would silently return "" and the assertion would test
// nothing.
func quotedArg(t *testing.T, call, key string) string {
	t.Helper()
	i := strings.Index(call, key+`:"`)
	require.GreaterOrEqual(t, i, 0, "%q not present in %s", key, call)
	rest := call[i+len(key)+2:]
	end := strings.IndexByte(rest, '"')
	require.GreaterOrEqual(t, end, 0, "unterminated %q value in %s", key, call)
	return rest[:end]
}

// s256 is the PKCE challenge derivation, restated here only so the
// constants above are provably a matching pair rather than two strings
// copied from a spec appendix.
func TestPasskeyLoginPKCEConstantsMatch(t *testing.T) {
	sum := sha256.Sum256([]byte(passkeyLoginVerifier))
	require.Equal(t, passkeyLoginChallenge, base64.RawURLEncoding.EncodeToString(sum[:]))
}
