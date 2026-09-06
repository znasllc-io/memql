package http

// Enrolment ends holding a session, not an instruction (memql#4610).
//
// The thing under test is a SEAM, not a handler. Registration and login were
// each already covered -- enrolment_ceremony_test.go proves a token authorizes
// exactly one registration, webauthn_login_test.go proves an assertion yields
// a redeemable auth code -- and both passed for the whole time a person who
// finished enrolling was left with a credential, no session, and a sentence
// telling them to go and sign in. Nothing failed, because nothing ran the two
// halves as one act.
//
// So this file runs them as one act, the way static/enroll.js now does:
// register under `Authorization: Enrolment mql_enr_...`, then immediately
// assert with the credential that registration just created, and check that
// what comes back is a session for the enrolled user. What register/finish
// WRITES is what login/finish READS -- see enrolThenSignInEngine -- because a
// test that hand-seeded the credential row between the halves would prove the
// two ceremonies work and say nothing about whether the second can consume the
// first's output.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	memqlengine "github.com/znasllc-io/memql/component/memql"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/enrolment"
)

// ---------------------------------------------------------------------
// the engine that joins the two halves
// ---------------------------------------------------------------------

// enrolThenSignInEngine is passkeyStubEngine plus the one wire the flow needs:
// the row createPasskeyIdentity writes becomes the row passkeyByCredentialId
// serves. In production that wire is the graph; here it is six lines, and
// without it the login half has nothing to resolve the assertion against.
//
// It reads the mutation rather than the handler's response on purpose. The
// response carries no public key, so a row built from it would be a row this
// test invented -- and the field most likely to break a chained sign-in is
// exactly the one the response does not carry.
type enrolThenSignInEngine struct {
	passkeyStubEngine
}

func (e *enrolThenSignInEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "mutation createPasskeyIdentity") {
		row := passkeyRowFromCreate(query)
		if e.byCredentialId == nil {
			e.byCredentialId = map[string]map[string]any{}
		}
		e.byCredentialId[dslStringArg(query, "credentialId")] = row
	}
	return e.passkeyStubEngine.Execute(ctx, query)
}

// passkeyRowFromCreate projects a createPasskeyIdentity mutation back into the
// node shape passkeyByCredentialId returns, field for field as the DSL's
// `insert` block writes it (dsl/identity/mutations.memql). `active: true` is
// literal there, which is why it is literal here.
func passkeyRowFromCreate(q string) map[string]any {
	signCount, _ := strconv.ParseFloat(dslBareArg(q, "signCount"), 64)
	var transports []any
	for _, t := range dslListArg(q, "transports") {
		transports = append(transports, t)
	}
	return map[string]any{
		"id":     dslStringArg(q, "identityId"),
		"userId": dslStringArg(q, "userId"),
		"label":  dslStringArg(q, "label"),
		"active": true,
		"credentials": map[string]any{
			"credentialId":   dslStringArg(q, "credentialId"),
			"publicKey":      dslStringArg(q, "publicKey"),
			"signCount":      signCount,
			"aaguid":         dslStringArg(q, "aaguid"),
			"transports":     transports,
			"backupEligible": dslBareArg(q, "backupEligible") == "true",
			"backupState":    dslBareArg(q, "backupState") == "true",
			"registeredBy":   dslStringArg(q, "registeredBy"),
			"lastUsedAt":     "",
		},
	}
}

// dslStringArg reads a `key:"value"` argument out of a rendered mutation.
//
// Not register_test.go's extractField, which matches `"key":"` and `key: "` --
// neither of which is the shape webauthn.Store.Create renders. Kept local
// rather than widening that helper, so the register/token tests keep the
// parser they were written against.
func dslStringArg(q, key string) string {
	i := strings.Index(q, key+`:"`)
	if i < 0 {
		return ""
	}
	rest := q[i+len(key)+2:]
	var b strings.Builder
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' && j+1 < len(rest) {
			b.WriteByte(rest[j+1])
			j++
			continue
		}
		if rest[j] == '"' {
			break
		}
		b.WriteByte(rest[j])
	}
	return b.String()
}

// dslBareArg reads an unquoted `key:value` argument -- the ints and bools.
func dslBareArg(q, key string) string {
	i := strings.Index(q, key+":")
	if i < 0 {
		return ""
	}
	rest := q[i+len(key)+1:]
	end := strings.IndexAny(rest, ",)")
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// dslListArg reads a `key:[...]` argument, which Create renders as JSON.
func dslListArg(q, key string) []string {
	i := strings.Index(q, key+":[")
	if i < 0 {
		return nil
	}
	rest := q[i+len(key)+1:]
	end := strings.Index(rest, "]")
	if end < 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(rest[:end+1]), &out); err != nil {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------

func newEnrolThenSignInServer(t *testing.T, tokenHash string) (*Server, *enrolThenSignInEngine) {
	t.Helper()
	engine := &enrolThenSignInEngine{}
	engine.byCredentialId = map[string]map[string]any{}
	engine.user = map[string]any{
		"id":           passkeyTestUserId,
		"primaryEmail": "invitee@example.test",
		"displayName":  "Invitee One",
		"role":         "writer",
	}
	engine.enrolment = enrolmentRow(tokenHash, nil)

	s := newPasskeyTestServer(t, &engine.passkeyStubEngine)
	// Re-point the store at the wrapper, exactly as newPasskeyLoginServer
	// does: newPasskeyTestServer wires the embedded engine, which does not
	// carry the create-to-lookup wire.
	s.Store = &identity.Store{Engine: engine}
	return s, engine
}

func adminCookie(t *testing.T, rec interface{ Result() *http.Response }) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// THE DECISIVE TEST
// ---------------------------------------------------------------------

// One enrolment link in, one authenticated browser out.
//
// This is the acceptance criterion of memql#4610 stated as code: after a
// successful enrolment the person holds a session, having navigated nowhere
// and retyped no address. Every assertion below is about the SEAM -- that the
// second ceremony consumed the first one's output -- rather than about either
// ceremony, which their own files already cover.
func TestEnrolmentEndsHoldingASession(t *testing.T) {
	plain, hash, err := enrolment.Mint()
	require.NoError(t, err)

	s, engine := newEnrolThenSignInServer(t, hash)
	auth := newHTTPSoftwareAuthenticator(t)

	// 1. ENROL, under the enrolment token and nothing else. No session
	//    exists at this point -- that is the whole reason the token is an
	//    authority (requirePasskeyEnroller, arm 2).
	enrolRec := registerViaEnrolment(t, s, auth, plain)
	require.Equal(t, http.StatusOK, enrolRec.Code, "enrol body=%s", enrolRec.Body.String())
	require.True(t, decodeFinish(t, enrolRec).Success)
	require.Contains(t, strings.Join(engine.mutations, "\n"), "mutation consumeEnrolmentToken",
		"the link must be spent by now; the failure path this feature exists for depends on it")

	// 2. SIGN IN, immediately, with the credential step 1 just created. This
	//    is the call static/enroll.js makes on registration success, argument
	//    for argument: firstParty, because an enrolment page opened out of an
	//    email has no relying party to name.
	//
	//    The authenticator's counter is left at 0 on both sides on purpose --
	//    that is what a platform authenticator reports, and a chained sign-in
	//    is the first assertion a credential ever produces, so "0 again" has
	//    to be admitted or this flow never works at all.
	loginRec := runPasskeyLogin(t, s, auth, passkeyTestUserId, WebAuthnLoginBeginRequest{FirstParty: true})
	require.Equal(t, http.StatusOK, loginRec.Code, "login body=%s", loginRec.Body.String())
	finish := decodeLoginFinish(t, loginRec)
	require.True(t, finish.Success)

	// 3. AUTHENTICATED. The cookie is the session -- it is what /authorize's
	//    SSO fast path and every first-party surface read -- and it names the
	//    user the enrolment token named, not merely some user.
	cookie := adminCookie(t, loginRec)
	require.NotNil(t, cookie, "no %s cookie: the browser finished enrolment holding nothing", adminCookieName)
	require.True(t, cookie.HttpOnly)
	claims, err := s.Issuer.VerifyAccessToken(cookie.Value, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, passkeyTestUserId, claims.Subject)

	// The session is LISTABLE AND REVOCABLE (memql#4303): a cookie with no
	// row is a session nobody can see or cut.
	writes := strings.Join(engine.mutations, "\n")
	require.Contains(t, writes, "mutation createAuthSession")

	// 4. AND NOTHING MORE THAN A SESSION. The first-party arm mints no auth
	//    code, which is the entire reason it needs no relying party: there is
	//    no credential in flight that could be delivered to the wrong place.
	require.NotContains(t, writes, "mutation createAuthCode",
		"the first-party arm minted an auth code; it has no client to give one to")

	// The destination came from the server, derived from its own base URL.
	// The browser does not choose where a first-party sign-in lands. It was
	// portal.test until epic memql#4984 retired the portal; the DERIVATION is
	// unchanged (identity.<d> rewritten to the shell's label), only the label
	// moved.
	require.Equal(t, "https://os.test/", finish.RedirectTo)
}

// ---------------------------------------------------------------------
// the arm is opt-in, and it is one arm or the other
// ---------------------------------------------------------------------

// A request that merely FORGOT its client is still refused. The first-party
// arm is asked for by name or not at all: "no client named" is far more often
// a caller's mistake than a deliberate first-party sign-in, and a silent
// fallback would turn every one of those mistakes into a session nobody asked
// for.
//
// (The same refusal is pinned from the other side, among the unregistered
// relying parties in webauthn_login_test.go. Restated here because THIS is the
// change that could have deleted it.)
func TestPasskeyLoginBegin_FirstPartyArmIsOptIn(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)

	rec := beginPasskeyLogin(t, s, WebAuthnLoginBeginRequest{})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "client_required", decodeLoginBegin(t, rec).ErrorCode)
}

// One ceremony, one product. A request asking for the first-party arm AND
// naming a client has not said what it wants, and what would have to be
// guessed is where a credential goes -- so it is refused rather than resolved
// by precedence.
func TestPasskeyLoginBegin_RefusesFirstPartyAlongsideAClient(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)

	for name, body := range map[string]WebAuthnLoginBeginRequest{
		"registered client": {FirstParty: true, ClientId: passkeyLoginClientId, RedirectURI: passkeyLoginRedirectURI},
		"client id alone":   {FirstParty: true, ClientId: passkeyLoginClientId},
		"redirect alone":    {FirstParty: true, RedirectURI: passkeyLoginRedirectURI},
	} {
		t.Run(name, func(t *testing.T) {
			rec := beginPasskeyLogin(t, s, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, "ambiguous_ceremony", decodeLoginBegin(t, rec).ErrorCode)
		})
	}
}

// A first-party ceremony refuses a revoked credential exactly as the
// relying-party one does. Worth stating rather than assuming: the arm skips
// the client validation, and the reason that is safe is that it skips NOTHING
// about the proof -- same FinishLogin, same revocation and sign-count checks.
func TestPasskeyLoginFinish_FirstPartyArmStillRefusesARevokedCredential(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	s, engine := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, false))

	a.signCount = 1
	rec := runPasskeyLogin(t, s, a, passkeyTestUserId, WebAuthnLoginBeginRequest{FirstParty: true})
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "assertion_rejected", decodeLoginFinish(t, rec).ErrorCode)
	require.Nil(t, adminCookie(t, rec), "a refused assertion stamped a session cookie")
	require.NotContains(t, strings.Join(engine.mutations, "\n"), "mutation createAuthSession")
}
