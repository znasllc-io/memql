package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/webauthn"
)

func TestOSPasskeyCrossReplicaKeepsRPAndRejectsUntrustedOrigin(t *testing.T) {
	for _, origin := range []string{"https://os.test", "https://evil.test"} {
		t.Run(origin, func(t *testing.T) {
			a := newHTTPSoftwareAuthenticator(t)
			a.origin = origin
			a.signCount = 1
			first, _ := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))
			second, _ := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))
			shared := webauthn.NewMemoryChallengeBackend()
			first.ChallengeBackend = shared
			second.ChallengeBackend = shared
			begin := decodeLoginBegin(t, beginPasskeyLogin(t, first, pkceBeginRequest()))
			require.Equal(t, passkeyTestRPID, begin.RequestOptions.Response.RelyingPartyID)
			rec := drivePasskey(t, second, "/auth/webauthn/login/finish", "", WebAuthnLoginFinishRequest{
				ChallengeId: begin.ChallengeId, Credential: a.assert(begin.RequestOptions.Response.Challenge.String(), passkeyTestUserId),
			}, second.handleWebAuthnLoginFinish)
			if origin == "https://os.test" {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			} else {
				require.NotEqual(t, http.StatusOK, rec.Code)
			}
			replay := drivePasskey(t, first, "/auth/webauthn/login/finish", "", WebAuthnLoginFinishRequest{
				ChallengeId: begin.ChallengeId, Credential: a.assert(begin.RequestOptions.Response.Challenge.String(), passkeyTestUserId),
			}, first.handleWebAuthnLoginFinish)
			require.NotEqual(t, http.StatusOK, replay.Code)
		})
	}
}

func TestRelatedOriginsComeOnlyFromInstallationConfiguration(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "https://evil.test/.well-known/webauthn", nil)
	r.Header.Set("Origin", "https://evil.test")
	s.handleWebAuthnOrigins(w, r)
	require.Equal(t, `{"origins":["https://os.test"]}`, strings.TrimSpace(w.Body.String()))
}
