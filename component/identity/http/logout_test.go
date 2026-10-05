package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// Keep both session mints and their later reads connected. A passkey login
// leaves SSO cookies; redeeming its OAuth code replaces only the refresh cookie.
type logoutEngine struct {
	passkeyLoginEngine
	sessions   map[string]map[string]any
	failRevoke bool
}

func (e *logoutEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case strings.HasPrefix(q, "mutation createAuthSession("):
		id := extractField(q, "sessionId")
		row := map[string]any{"id": "v1:identity:authSession:" + id}
		for _, key := range []string{"userId", "subject", "tokenHash", "source", "expiresAt"} {
			row[key] = extractField(q, key)
		}
		e.sessions[id] = row
		return e.nodes()
	case strings.HasPrefix(q, "mutation rotateAuthSession("):
		row := e.sessions[strings.TrimPrefix(extractField(q, "sessionId"), "v1:identity:authSession:")]
		row["refreshTokenHash"] = extractField(q, "newRefreshTokenHash")
		row["previousRefreshTokenHash"] = extractField(q, "previousRefreshTokenHash")
		return e.nodes()
	case strings.HasPrefix(q, "mutation revokeAuthSession("):
		if e.failRevoke {
			return nil, errors.New("revocation unavailable")
		}
		row := e.sessions[strings.TrimPrefix(extractField(q, "sessionId"), "v1:identity:authSession:")]
		row["revokedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		return e.nodes()
	case strings.HasPrefix(q, "query authSessionBy"):
		for _, key := range []string{"tokenHash", "refreshTokenHash", "previousRefreshTokenHash"} {
			if value := extractField(q, key); value != "" {
				for _, row := range e.sessions {
					if row[key] == value {
						return e.nodes(row)
					}
				}
			}
		}
		return e.nodes()
	}
	return e.passkeyLoginEngine.Execute(ctx, q)
}

func requireLogoutCookies(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	// Result snapshots the headers at WriteHeader, just as the network does.
	// Inspecting rec.Header() instead would miss the late-Set-Cookie defect.
	cookies := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c
	}
	for _, name := range []string{adminCookieName, refreshCookieName, sessionMarkerCookieName} {
		require.Contains(t, cookies, name)
		require.Equal(t, -1, cookies[name].MaxAge)
		require.Empty(t, cookies[name].Value)
		require.Equal(t, "/", cookies[name].Path)
		require.Empty(t, cookies[name].Domain, "match the host-only sign-in cookies")
		require.True(t, cookies[name].Secure)
	}
}

func TestLogoutClearsCookiesEvenWithoutARefreshToken(t *testing.T) {
	s, _ := newPasskeyLoginServer(t)
	rec := httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest("POST", "https://identity.test/auth/logout", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
	requireLogoutCookies(t, rec)
}

func TestLogoutPasskeySessionsCannotSilentlySignBackIn(t *testing.T) {
	a := newHTTPSoftwareAuthenticator(t)
	first, base := newPasskeyLoginServer(t, loginPasskeyRow(a, passkeyTestUserId, 0, true))
	e := &logoutEngine{passkeyLoginEngine: *base, sessions: map[string]map[string]any{}}
	first.Store.Engine = e
	login := runPasskeyLogin(t, first, a, passkeyTestUserId, pkceBeginRequest())
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	sso := adminCookie(t, login)
	require.NotNil(t, sso)
	target, err := url.Parse(decodeLoginFinish(t, login).RedirectTo)
	require.NoError(t, err)
	token := postToken(t, first, url.Values{
		"grant_type": {"authorization_code"}, "code": {target.Query().Get("code")},
		"client_id": {passkeyLoginClientId}, "redirect_uri": {passkeyLoginRedirectURI},
		"code_verifier": {passkeyLoginVerifier},
	})
	require.Equal(t, http.StatusOK, token.Code, token.Body.String())
	require.Len(t, e.sessions, 2)
	e.sessions["other-device"] = map[string]any{"id": "v1:identity:authSession:other-device", "userId": passkeyTestUserId, "tokenHash": "other"}

	// Another replica shares persistence and keys, not the first one's state.
	second, _ := newPasskeyLoginServer(t)
	second.Store = &identity.Store{Engine: e}
	second.Issuer = first.Issuer
	web, err := identityweb.NewServer(second.Cfg, slog.Default(), nil)
	require.NoError(t, err)
	web.Store = second.Store
	web.SetMeTokens(&identityweb.MeTokens{Issuer: second.Issuer})
	web.CountUsers = func(context.Context) (int, error) { return 1, nil }
	web.ClusterClaimed = func(context.Context) (bool, error) { return true, nil }
	mux := http.NewServeMux()
	web.Mount(mux)
	probe := func(cookie *http.Cookie) map[string]any {
		r := httptest.NewRequest("GET", "https://identity.test/login", nil)
		r.Header.Set("Accept", identityweb.NativeMediaType)
		r.Header.Set("Origin", "https://os.test")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var data map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
		return data
	}
	require.Equal(t, "https://os.test/", probe(sso)["redirect"], "the SSO session must be live before logout")
	r := httptest.NewRequest("POST", "https://identity.test/auth/logout", nil)
	r.AddCookie(sso)
	for _, c := range token.Result().Cookies() {
		r.AddCookie(c)
	}
	for i := 0; i < 2; i++ {
		out := httptest.NewRecorder()
		second.handleLogout(out, r)
		require.Equal(t, http.StatusNoContent, out.Code, out.Body.String())
		requireLogoutCookies(t, out)
	}
	for id, row := range e.sessions {
		if id == "other-device" {
			require.Empty(t, row["revokedAt"])
		} else {
			require.NotEmpty(t, row["revokedAt"], id)
		}
	}
	require.Equal(t, "login", probe(nil)["page"], "reload without cookies must require sign-in")
	require.Equal(t, "login", probe(sso)["page"], "even replaying the old SSO cookie must not sign in")
}

func TestLogoutRevokesRotatedRefreshAndSurfacesStorageFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated", true: "storage failure"}[fail], func(t *testing.T) {
			s, base := newPasskeyLoginServer(t)
			row := map[string]any{"id": "v1:identity:authSession:session", "userId": passkeyTestUserId, "previousRefreshTokenHash": refresh.HashRefreshToken("previous")}
			e := &logoutEngine{passkeyLoginEngine: *base, sessions: map[string]map[string]any{"session": row}, failRevoke: fail}
			s.Store.Engine = e
			r := httptest.NewRequest("POST", "https://identity.test/auth/logout", nil)
			r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "previous"})
			r.AddCookie(&http.Cookie{Name: adminCookieName, Value: "forged-cookie"})
			out := httptest.NewRecorder()
			s.handleLogout(out, r)
			requireLogoutCookies(t, out)
			if fail {
				require.Equal(t, http.StatusServiceUnavailable, out.Code)
				require.Empty(t, row["revokedAt"])
			} else {
				require.Equal(t, http.StatusNoContent, out.Code)
				require.NotEmpty(t, row["revokedAt"])
			}
		})
	}
}
