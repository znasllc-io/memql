package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
)

func recoveryPage(t *testing.T, server *Server, email string, issue identityweb.IssueMagicLinkFunc) *httptest.ResponseRecorder {
	t.Helper()
	web, err := identityweb.NewServer(server.Cfg, server.Logger, nil)
	require.NoError(t, err)
	web.Store = server.Store
	web.CountUsers = func(context.Context) (int, error) { return 1, nil } // Old installers stamped setup early.
	web.ClusterClaimed = func(context.Context) (bool, error) { return true, nil }
	web.IssueMagicLink = issue
	mux := http.NewServeMux()
	web.Mount(mux)
	get := httptest.NewRequest("GET", "https://identity.test/setup", nil)
	get.Header.Set("Accept", identityweb.NativeMediaType)
	get.Header.Set("Origin", "https://os.test")
	page := httptest.NewRecorder()
	mux.ServeHTTP(page, get)
	if email == "" {
		return page
	}
	require.Equal(t, 200, page.Code, page.Body.String())
	var result struct{ CSRF string }
	require.NoError(t, json.Unmarshal(page.Body.Bytes(), &result))
	post := httptest.NewRequest("POST", "https://identity.test/auth/setup/resume", strings.NewReader(url.Values{"email": {email}, "provider": {"docker-local"}}.Encode()))
	post.Header = get.Header.Clone()
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("X-CSRF-Token", result.CSRF)
	for _, cookie := range page.Result().Cookies() {
		post.AddCookie(cookie)
	}
	saved := httptest.NewRecorder()
	mux.ServeHTTP(saved, post)
	return saved
}

func recoverySettings(e *bootstrapEngine, legacy bool) {
	e.settings = map[string]any{"id": "cluster", "bootstrapEmail": "owner@example.test", "bootstrapFirstName": "Ada", "bootstrapLastName": "Owner", "clusterDomain": "test", "brandName": "Example Organization"}
	if legacy {
		e.user = map[string]any{"id": "named-owner", "primaryEmail": "owner@example.test", "role": "owner", "active": true}
		e.settings["bootstrappedAt"] = time.Now().Format(time.RFC3339)
	}
}

func TestLocalOwnerCanRecoverSkippedPasskeyAcrossReplicas(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "no owner yet", true: "installer named owner"}[legacy], func(t *testing.T) {
			first, second, engine := bootstrapServers(t, "docker-local")
			recoverySettings(engine, legacy)
			issue := func(context.Context, identityweb.IssueMagicLinkInput) (identityweb.IssueMagicLinkResult, error) {
				t.Fatal("local recovery sent email")
				return identityweb.IssueMagicLinkResult{}, nil
			}
			page := recoveryPage(t, first, "", issue)
			require.Contains(t, page.Body.String(), `"page":"setup_resume"`)
			require.NotContains(t, page.Body.String(), "owner@example.test")
			denied := recoveryPage(t, first, "stranger@example.test", issue)
			require.Equal(t, 400, denied.Code, denied.Body.String())
			saved := recoveryPage(t, first, " OWNER@example.test ", issue)
			require.Equal(t, 200, saved.Code, saved.Body.String())
			var token string
			for _, c := range saved.Result().Cookies() {
				if c.Name == identity.BootstrapCookie {
					token = c.Value
				}
			}
			require.NotEmpty(t, token)
			require.Empty(t, engine.mutations)
			// Lost/expired cookie: matching contact email renews enrollment before proof.
			_, err := first.Store.DirectDB().Exec(`UPDATE identity_bootstrap_enrollment SET expires_at=now()-interval '1 hour'`)
			require.NoError(t, err)
			saved = recoveryPage(t, second, "owner@example.test", issue)
			old := token
			for _, c := range saved.Result().Cookies() {
				if c.Name == identity.BootstrapCookie {
					token = c.Value
				}
			}
			require.NotEqual(t, old, token)
			require.NotEqual(t, 200, bootstrapRegister(t, first, old, false, map[string]string{}).Code)
			begin := bootstrapRegister(t, first, token, false, map[string]string{})
			require.Equal(t, 200, begin.Code, begin.Body.String())
			challenge := decodeBegin(t, begin)
			a := newHTTPSoftwareAuthenticator(t)
			done := bootstrapRegister(t, second, token, true, WebAuthnRegisterFinishRequest{ChallengeId: challenge.ChallengeId, Credential: a.create(challenge.CreationOptions.Response.Challenge.String())})
			require.Equal(t, 200, done.Code, done.Body.String())
			require.Len(t, engine.byCredentialId, 1)
			if legacy {
				require.Zero(t, engine.users)
				require.Equal(t, "named-owner", engine.organizationOwner)
			} else {
				require.Equal(t, 1, engine.users)
			}
			pending, err := first.Store.PendingOwnerSetup(context.Background(), first.Cfg)
			require.NoError(t, err)
			require.Nil(t, pending)
			// Even revoking the passkey later cannot reopen this claim.
			for _, credential := range engine.byCredentialId {
				credential["active"] = false
			}
			pending, err = second.Store.PendingOwnerSetup(context.Background(), second.Cfg)
			require.NoError(t, err)
			require.Nil(t, pending)
		})
	}
}

func TestOwnerRecoveryRequiresHostedEmailAndNeverOverwritesCredential(t *testing.T) {
	for _, provider := range []string{"azure", "", "unknown"} {
		t.Run(provider, func(t *testing.T) {
			first, _, engine := bootstrapServers(t, provider)
			recoverySettings(engine, false)
			sent := 0
			response := recoveryPage(t, first, "owner@example.test", func(_ context.Context, in identityweb.IssueMagicLinkInput) (identityweb.IssueMagicLinkResult, error) {
				sent++
				require.True(t, in.Bootstrap)
				require.Equal(t, "owner@example.test", in.Email)
				return identityweb.IssueMagicLinkResult{RequestId: "request", BindingNonce: "nonce", ExpiresAt: time.Now().Add(time.Hour)}, nil
			})
			require.Equal(t, 200, response.Code, response.Body.String())
			require.Equal(t, 1, sent)
			for _, c := range response.Result().Cookies() {
				require.NotEqual(t, identity.BootstrapCookie, c.Name)
			}
			require.Empty(t, engine.byCredentialId)
			_, err := first.Store.ResumeOwnerSetupLocked(context.Background(), first.Cfg, "owner@example.test", nil, false)
			require.Error(t, err)
		})
	}
	first, _, engine := bootstrapServers(t, "docker-local")
	recoverySettings(engine, true)
	for _, active := range []bool{true, false} {
		engine.byCredentialId["prior"] = map[string]any{"id": "prior", "active": active, "identityType": "passkey"}
		pending, err := first.Store.PendingOwnerSetup(context.Background(), first.Cfg)
		require.NoError(t, err)
		require.Nil(t, pending)
	}
	engine.byCredentialId = map[string]map[string]any{}
	engine.fail = "includeHistory: true"
	pending, err := first.Store.PendingOwnerSetup(context.Background(), first.Cfg)
	require.Error(t, err)
	require.Nil(t, pending)
}

func TestHostedLegacyOwnerRecoveryVerifiesEmailBeforePasskey(t *testing.T) {
	first, second, engine := bootstrapServers(t, "azure")
	recoverySettings(engine, true)
	verifier := &magiclink.Verifier{Cfg: first.Cfg, Store: first.Store}
	res, err := verifier.Finish(context.Background(), magiclink.FinishInput{RequestId: "request"})
	require.NoError(t, err)
	require.Empty(t, res.AuthCode)
	require.Empty(t, res.UserId)
	pending, err := second.Store.BootstrapEnrollment(context.Background(), res.EnrollmentToken, second.Cfg)
	require.NoError(t, err)
	require.True(t, pending.EmailVerified)
	require.Equal(t, "named-owner", pending.UserID)
	begin := bootstrapRegister(t, second, res.EnrollmentToken, false, map[string]string{})
	require.Equal(t, 200, begin.Code, begin.Body.String())
	challenge := decodeBegin(t, begin)
	a := newHTTPSoftwareAuthenticator(t)
	done := bootstrapRegister(t, first, res.EnrollmentToken, true, WebAuthnRegisterFinishRequest{ChallengeId: challenge.ChallengeId, Credential: a.create(challenge.CreationOptions.Response.Challenge.String())})
	require.Equal(t, 200, done.Code, done.Body.String())
	require.Zero(t, engine.users)
	require.Len(t, engine.byCredentialId, 1)
}

func TestBrowserSetupEntryFindsLegacyOwnerRecovery(t *testing.T) {
	server, _, engine := bootstrapServers(t, "docker-local")
	recoverySettings(engine, true)
	web, err := identityweb.NewServer(server.Cfg, server.Logger, nil)
	require.NoError(t, err)
	web.Store = server.Store
	web.CountUsers = func(context.Context) (int, error) { return 1, nil }
	web.ClusterClaimed = func(context.Context) (bool, error) { return true, nil }
	mux := http.NewServeMux()
	web.Mount(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "https://identity.test/setup?client_id=vscode&state=flow", nil))
	require.Equal(t, http.StatusSeeOther, response.Code)
	require.Equal(t, "https://os.test/identity/setup#client_id=vscode&state=flow", response.Header().Get("Location"))
}

func TestOwnerRecoveryUsesInstallerConfigurationBeforeSettingsExist(t *testing.T) {
	first, second, engine := bootstrapServers(t, "docker-local")
	first.Cfg.Bootstrap = identity.BootstrapConfig{Domain: "test", OwnerEmail: " Owner@example.test ", OwnerFirstName: "Ada", OwnerLastName: "Owner", RegistrationMode: "invite_only"}
	first.Cfg.BrandName = "Configured organization"
	engine.settings = nil
	pending, err := first.Store.PendingOwnerSetup(context.Background(), first.Cfg)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, "Configured organization", pending.Settings.BrandName)
	response := recoveryPage(t, first, "owner@example.test", nil)
	require.Equal(t, 200, response.Code, response.Body.String())
	var token string
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == identity.BootstrapCookie {
			token = cookie.Value
		}
	}
	require.NotEmpty(t, token)
	// The replica with no bootstrap config resumes from shared storage.
	restored, err := second.Store.BootstrapEnrollment(context.Background(), token, second.Cfg)
	require.NoError(t, err)
	require.Equal(t, "Owner@example.test", restored.Settings.BootstrapEmail)
	require.Equal(t, "Configured organization", restored.Settings.BrandName)
	begin := bootstrapRegister(t, second, token, false, map[string]string{})
	require.Equal(t, 200, begin.Code, begin.Body.String())
}
