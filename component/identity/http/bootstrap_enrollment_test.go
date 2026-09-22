package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// A private schema contains only authentication substrate tables. All graph
// writes use the fixture: these tests never create an installation owner.
func bootstrapTestDB(t *testing.T) *sql.DB {
	t.Helper()
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		dbtest.Unreachable(t, "bootstrap enrollment", dbtest.DSN(), err)
		return nil
	}
	schema := fmt.Sprintf("bootstrap_test_%d", time.Now().UnixNano())
	_, err := admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
	t.Cleanup(func() { db.Close(); _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	for _, name := range []string{"20260909000000_webauthn_challenges", "20260922000000_bootstrap_enrollment"} {
		migration, err := os.ReadFile("../../database/memory-nodes/migrations/" + name + ".up.sql")
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	return db
}

type bootstrapEngine struct {
	mu sync.Mutex
	passkeyStubEngine
	settings          map[string]any
	users             int
	consumed          bool
	organization      string
	organizationOwner string
	fail              string
}

func (e *bootstrapEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail != "" && strings.Contains(q, e.fail) {
		return nil, errors.New("injected persistence failure")
	}
	switch {
	case strings.HasPrefix(q, "builtin configureSelfAccount("):
		if !auth.OriginFromContext(ctx).IsInternal() {
			return nil, errors.New("organization setup must be internal")
		}
		e.organization = extractField(q, "name")
		e.organizationOwner = extractField(q, "ownerUserId")
	case strings.Contains(q, "clusterSettingsCurrent("):
		if e.settings == nil {
			return e.nodes()
		}
		return e.nodes(e.settings)
	case strings.Contains(q, "activeUsers(") || strings.Contains(q, "userByEmail("):
		if e.user == nil {
			return e.nodes()
		}
		return e.nodes(e.user)
	case strings.HasPrefix(q, "mutation createUserOnFirstLogin("):
		e.users++
		e.user = map[string]any{"id": extractField(q, "userId"), "primaryEmail": extractField(q, "primaryEmail"), "role": "owner", "internal": true, "active": true}
	case strings.HasPrefix(q, "mutation createPasskeyIdentity("):
		cred := extractField(q, "credentialId")
		e.byCredentialId[cred] = map[string]any{"id": extractField(q, "identityId"), "userId": extractField(q, "userId"), "active": true, "credentials": map[string]any{"credentialId": cred, "publicKey": extractField(q, "publicKey"), "signCount": float64(0), "backupEligible": true, "backupState": true}}
	case strings.HasPrefix(q, "mutation createClusterSettings(") || strings.HasPrefix(q, "mutation updateClusterSettings("):
		e.settings = map[string]any{"id": "cluster"}
		for _, key := range []string{"brandName", "clusterDomain", "bootstrapEmail", "bootstrapFirstName", "bootstrapLastName", "bootstrappedAt"} {
			e.settings[key] = extractField(q, key)
		}
	case strings.Contains(q, "magicLinkRequestById("):
		row := map[string]any{"id": "request", "email": "owner@example.test", "oauthCtxJSON": `{"bootstrap":true,"adminSession":true}`, "expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339)}
		if e.consumed {
			row["consumedAt"] = time.Now().Format(time.RFC3339)
		}
		return e.nodes(row)
	case strings.HasPrefix(q, "mutation consumeMagicLinkRequest("):
		e.consumed = true
	}
	return e.passkeyStubEngine.Execute(ctx, q)
}

func bootstrapServers(t *testing.T, provider string) (*Server, *Server, *bootstrapEngine) {
	t.Helper()
	db := bootstrapTestDB(t)
	e := &bootstrapEngine{passkeyStubEngine: passkeyStubEngine{byCredentialId: map[string]map[string]any{}}}
	first := newPasskeyTestServer(t, &e.passkeyStubEngine)
	second := newPasskeyTestServer(t, &e.passkeyStubEngine)
	for _, s := range []*Server{first, second} {
		s.Logger = slog.Default()
		s.Cfg.DeployProvider = provider
		s.Store = &identity.Store{Engine: e, DirectDB: func() *sql.DB { return db }}
		s.ChallengeBackend = nil
	}
	return first, second, e
}
func beginBootstrap(t *testing.T, s *Server, verified bool) string {
	t.Helper()
	release, err := s.Store.AcquireBootstrapGate(context.Background())
	require.NoError(t, err)
	defer release()
	token, err := s.Store.BeginBootstrapEnrollmentLocked(context.Background(), s.Cfg, identity.ClusterSettingsRow{ClusterDomain: "test", BrandName: "Example Organization", BootstrapEmail: "owner@example.test", BootstrapFirstName: "Ada", BootstrapLastName: "Owner"}, nil, verified)
	require.NoError(t, err)
	return token
}
func bootstrapRegister(t *testing.T, s *Server, token string, finish bool, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	path := "/auth/webauthn/register/begin"
	handler := s.handleWebAuthnRegisterBegin
	if finish {
		path = "/auth/webauthn/register/finish"
		handler = s.handleWebAuthnRegisterFinish
	}
	r := httptest.NewRequest("POST", "https://identity.test"+path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bootstrap "+token)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestBootstrapLocalWizardSkipsEmailAndRequiresRealPasskeyAcrossReplicas(t *testing.T) {
	first, second, e := bootstrapServers(t, "docker-local")
	web, err := identityweb.NewServer(first.Cfg, slog.Default(), nil)
	require.NoError(t, err)
	web.Store = first.Store
	web.CountUsers = func(context.Context) (int, error) { return 0, nil }
	web.ClusterClaimed = func(context.Context) (bool, error) { return false, nil }
	web.IssueMagicLink = func(context.Context, identityweb.IssueMagicLinkInput) (identityweb.IssueMagicLinkResult, error) {
		t.Fatal("local setup emailed")
		return identityweb.IssueMagicLinkResult{}, nil
	}
	mux := http.NewServeMux()
	web.Mount(mux)
	get := httptest.NewRequest("GET", "https://identity.test/setup", nil)
	get.Header.Set("Accept", identityweb.NativeMediaType)
	get.Header.Set("Origin", "https://os.test")
	page := httptest.NewRecorder()
	mux.ServeHTTP(page, get)
	require.Equal(t, 200, page.Code, page.Body.String())
	var data struct {
		CSRF string
		Data struct{ Local bool }
	}
	require.NoError(t, json.Unmarshal(page.Body.Bytes(), &data))
	require.True(t, data.Data.Local)
	form := url.Values{"owner_first_name": {"Ada"}, "owner_last_name": {"Owner"}, "owner_email": {"owner@example.test"}, "domain": {"test"}, "brand_name": {"Example Organization"}}
	post := httptest.NewRequest("POST", "https://identity.test/setup", strings.NewReader(form.Encode()))
	post.Header = get.Header.Clone()
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("X-CSRF-Token", data.CSRF)
	for _, cookie := range page.Result().Cookies() {
		post.AddCookie(cookie)
	}
	saved := httptest.NewRecorder()
	mux.ServeHTTP(saved, post)
	require.Equal(t, 200, saved.Code, saved.Body.String())
	token := ""
	for _, cookie := range saved.Result().Cookies() {
		if cookie.Name == identity.BootstrapCookie {
			token = cookie.Value
		}
	}
	require.NotEmpty(t, token)
	require.Zero(t, e.users)
	require.Empty(t, e.mutations)
	var redirect struct{ Redirect string }
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &redirect))
	require.Equal(t, "/auth/setup/passkey", redirect.Redirect)
	follow := httptest.NewRequest("GET", "https://identity.test"+redirect.Redirect, nil)
	follow.Header = get.Header.Clone()
	for _, cookie := range saved.Result().Cookies() {
		follow.AddCookie(cookie)
	}
	passkeyPage := httptest.NewRecorder()
	mux.ServeHTTP(passkeyPage, follow)
	require.Equal(t, http.StatusOK, passkeyPage.Code, passkeyPage.Body.String())
	var enrollmentPage struct {
		Page string
		Data struct {
			Local           bool
			EnrollmentToken string
		}
	}
	require.NoError(t, json.Unmarshal(passkeyPage.Body.Bytes(), &enrollmentPage))
	require.Equal(t, "setup_passkey", enrollmentPage.Page)
	require.True(t, enrollmentPage.Data.Local)
	require.Equal(t, token, enrollmentPage.Data.EnrollmentToken)
	// Losing the cookie must expose an error, never silently restart the
	// wizard and make a successful submission look like a flickering button.
	missingCookie := httptest.NewRequest("GET", "https://identity.test"+redirect.Redirect, nil)
	missingCookie.Header = get.Header.Clone()
	missingPage := httptest.NewRecorder()
	mux.ServeHTTP(missingPage, missingCookie)
	require.Equal(t, http.StatusUnauthorized, missingPage.Code, missingPage.Body.String())
	var missingResult struct{ Page, Redirect string }
	require.NoError(t, json.Unmarshal(missingPage.Body.Bytes(), &missingResult))
	require.Equal(t, "error", missingResult.Page)
	require.Empty(t, missingResult.Redirect)
	begin := bootstrapRegister(t, first, token, false, map[string]string{})
	require.Equal(t, 200, begin.Code, begin.Body.String())
	invalid := bootstrapRegister(t, second, token, true, WebAuthnRegisterFinishRequest{ChallengeId: decodeBegin(t, begin).ChallengeId, Credential: json.RawMessage(`{}`)})
	require.NotEqual(t, 200, invalid.Code)
	require.Zero(t, e.users)
	require.Empty(t, invalid.Result().Cookies())
	begin = bootstrapRegister(t, first, token, false, map[string]string{})
	require.Equal(t, 200, begin.Code, begin.Body.String())
	challenge := decodeBegin(t, begin)
	a := newHTTPSoftwareAuthenticator(t)
	a.origin = "https://os.test"
	done := bootstrapRegister(t, second, token, true, WebAuthnRegisterFinishRequest{ChallengeId: challenge.ChallengeId, Credential: a.create(challenge.CreationOptions.Response.Challenge.String())})
	require.Equal(t, 200, done.Code, done.Body.String())
	require.Equal(t, 1, e.users)
	require.Equal(t, "Example Organization", e.organization)
	require.Equal(t, e.user["id"], e.organizationOwner)
	require.NotEmpty(t, e.settings["bootstrappedAt"])
	require.Contains(t, strings.Join(e.mutations, "\n"), `policy: "passkey_only"`)
	require.NotContains(t, strings.Join(e.mutations, "\n"), "mutation createIdentityMagicLink")
	require.NotEmpty(t, done.Result().Cookies())
	replay := bootstrapRegister(t, first, token, true, map[string]string{})
	require.Equal(t, 200, replay.Code)
	require.Empty(t, replay.Result().Cookies())
	require.Equal(t, 1, e.users)
}

func TestBootstrapHostedEmailOnlyNeverCreatesOwnerOrSession(t *testing.T) {
	first, second, e := bootstrapServers(t, "azure")
	_, err := first.Store.BeginBootstrapEnrollmentLocked(context.Background(), first.Cfg, identity.ClusterSettingsRow{ClusterDomain: "test", BrandName: "Example Organization", BootstrapEmail: "owner@example.test", BootstrapFirstName: "Ada", BootstrapLastName: "Owner"}, nil, false)
	require.Error(t, err)
	e.settings = map[string]any{"id": "cluster", "bootstrapEmail": "owner@example.test", "bootstrapFirstName": "Ada", "bootstrapLastName": "Owner", "clusterDomain": "test", "brandName": "Example Organization", "bootstrappedAt": ""}
	verifier := &magiclink.Verifier{Cfg: first.Cfg, Store: first.Store}
	res, err := verifier.Finish(context.Background(), magiclink.FinishInput{RequestId: "request"})
	require.NoError(t, err)
	require.NotEmpty(t, res.EnrollmentToken)
	require.Empty(t, res.AuthCode)
	require.Empty(t, res.UserId)
	require.Zero(t, e.users)
	require.Empty(t, e.settings["bootstrappedAt"])
	pending, err := second.Store.BootstrapEnrollment(context.Background(), res.EnrollmentToken, second.Cfg)
	require.NoError(t, err)
	require.True(t, pending.EmailVerified)
	// A different owner cannot replace the verified reservation.
	_, err = second.Store.BeginBootstrapEnrollmentLocked(context.Background(), second.Cfg, identity.ClusterSettingsRow{BootstrapEmail: "competitor@example.test"}, nil, true)
	require.Error(t, err)
	// Re-verification rotates only the grant; it keeps the original identity.
	token := beginBootstrap(t, second, true)
	resumed, err := first.Store.BootstrapEnrollment(context.Background(), token, first.Cfg)
	require.NoError(t, err)
	require.Equal(t, pending.UserID, resumed.UserID)
	_, err = first.Store.BootstrapEnrollment(context.Background(), res.EnrollmentToken, first.Cfg)
	require.Error(t, err)
	begin := bootstrapRegister(t, first, token, false, map[string]string{})
	require.Equal(t, 200, begin.Code, begin.Body.String())
	challenge := decodeBegin(t, begin)
	a := newHTTPSoftwareAuthenticator(t)
	done := bootstrapRegister(t, second, token, true, WebAuthnRegisterFinishRequest{ChallengeId: challenge.ChallengeId, Credential: a.create(challenge.CreationOptions.Response.Challenge.String())})
	require.Equal(t, 200, done.Code, done.Body.String())
	require.Equal(t, 1, e.users)
	require.Equal(t, "Example Organization", e.organization)
	require.Equal(t, e.user["id"], e.organizationOwner)
	require.NotEmpty(t, e.settings["bootstrappedAt"])
}

func TestBootstrapVerifiedCredentialSurvivesFailureAndLostCookie(t *testing.T) {
	for _, resumeWithAssertion := range []bool{false, true} {
		t.Run(fmt.Sprint(resumeWithAssertion), func(t *testing.T) {
			first, second, e := bootstrapServers(t, "docker-local")
			token := beginBootstrap(t, first, false)
			competing := beginBootstrap(t, second, false)
			a := newHTTPSoftwareAuthenticator(t)
			begin := bootstrapRegister(t, first, token, false, map[string]string{})
			require.Equal(t, 200, begin.Code, begin.Body.String())
			challenge := decodeBegin(t, begin)
			e.fail = "createPasskeyIdentity"
			failed := bootstrapRegister(t, second, token, true, WebAuthnRegisterFinishRequest{ChallengeId: challenge.ChallengeId, Credential: a.create(challenge.CreationOptions.Response.Challenge.String())})
			require.NotEqual(t, 200, failed.Code)
			require.Zero(t, e.users)
			require.Empty(t, failed.Result().Cookies())
			denied := bootstrapRegister(t, first, competing, false, map[string]string{})
			require.NotEqual(t, 200, denied.Code)
			e.fail = ""
			if resumeWithAssertion {
				pending, err := first.Store.ReservedBootstrap(context.Background())
				require.NoError(t, err)
				// The enrollment cookie and grant are absent; only a real signed assertion
				// over a new shared challenge can recover the saved attestation.
				login := beginPasskeyLogin(t, first, WebAuthnLoginBeginRequest{FirstParty: true})
				req := decodeLoginBegin(t, login)
				a.signCount = 1
				done := drivePasskey(t, second, "/auth/webauthn/login/finish", "", WebAuthnLoginFinishRequest{ChallengeId: req.ChallengeId, Credential: a.assert(req.RequestOptions.Response.Challenge.String(), pending.UserID)}, second.handleWebAuthnLoginFinish)
				require.Equal(t, 200, done.Code, done.Body.String())
			} else {
				begin := bootstrapRegister(t, first, token, false, map[string]string{})
				require.Contains(t, begin.Body.String(), `"resume":true`)
				done := bootstrapRegister(t, second, token, true, map[string]string{})
				require.Equal(t, 200, done.Code, done.Body.String())
			}
			require.Equal(t, 1, e.users)
			require.NotEmpty(t, e.settings["bootstrappedAt"])
		})
	}
}

func TestOnlyConfiguredLocalProviderCanSkipEmail(t *testing.T) {
	for _, provider := range []string{"", "azure", "unknown", "docker-local"} {
		t.Run(provider, func(t *testing.T) {
			cfg := identity.Config{DeployProvider: provider, BaseURL: "https://identity.localhost"}
			require.Equal(t, provider == "docker-local", cfg.LocalPasskeyOnly())
		})
	}
	s, _ := newPasskeyLoginServer(t)
	s.Cfg.DeployProvider = "docker-local"
	issuer := &magiclink.Issuer{Cfg: s.Cfg, Store: s.Store}
	_, err := issuer.Issue(context.Background(), magiclink.IssueInput{Email: "local@example.test", Bootstrap: true})
	require.ErrorContains(t, err, "passkeys only")
	// No forwarded header, browser origin or proposed hostname affects the mode.
	s.Cfg.DeployProvider = "azure"
	w := bootstrapRegister(t, s, "forged", false, map[string]string{"provider": "docker-local"})
	require.NotEqual(t, 200, w.Code)
}

func TestBootstrapCompetingLocalPasskeysCreateOnlyOneOwner(t *testing.T) {
	first, second, e := bootstrapServers(t, "docker-local")
	tokenA, tokenB := beginBootstrap(t, first, false), beginBootstrap(t, second, false)
	a, b := newHTTPSoftwareAuthenticator(t), newHTTPSoftwareAuthenticator(t)
	ca := decodeBegin(t, bootstrapRegister(t, first, tokenA, false, map[string]string{}))
	cb := decodeBegin(t, bootstrapRegister(t, second, tokenB, false, map[string]string{}))
	bodyA := WebAuthnRegisterFinishRequest{ChallengeId: ca.ChallengeId, Credential: a.create(ca.CreationOptions.Response.Challenge.String())}
	bodyB := WebAuthnRegisterFinishRequest{ChallengeId: cb.ChallengeId, Credential: b.create(cb.CreationOptions.Response.Challenge.String())}
	results := make(chan int, 2)
	start := make(chan struct{})
	go func() { <-start; results <- bootstrapRegister(t, first, tokenA, true, bodyA).Code }()
	go func() { <-start; results <- bootstrapRegister(t, second, tokenB, true, bodyB).Code }()
	close(start)
	codes := []int{<-results, <-results}
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, codes)
	require.Equal(t, 1, e.users)
}

func TestBootstrapOrganizationFailureDoesNotSealOrIssueSessionAndResumesAcrossReplica(t *testing.T) {
	first, second, e := bootstrapServers(t, "docker-local")
	token := beginBootstrap(t, first, false)
	begin := bootstrapRegister(t, first, token, false, map[string]string{})
	require.Equal(t, 200, begin.Code, begin.Body.String())
	challenge := decodeBegin(t, begin)
	a := newHTTPSoftwareAuthenticator(t)
	e.fail = "configureSelfAccount"
	failed := bootstrapRegister(t, second, token, true, WebAuthnRegisterFinishRequest{ChallengeId: challenge.ChallengeId, Credential: a.create(challenge.CreationOptions.Response.Challenge.String())})
	require.NotEqual(t, 200, failed.Code)
	require.Empty(t, failed.Result().Cookies())
	require.Empty(t, e.settings["bootstrappedAt"])
	require.Empty(t, e.organization)
	require.Equal(t, 1, e.users)
	e.fail = ""
	resumed := bootstrapRegister(t, first, token, true, map[string]string{})
	require.Equal(t, 200, resumed.Code, resumed.Body.String())
	require.Equal(t, 1, e.users)
	require.Equal(t, "Example Organization", e.organization)
	require.Equal(t, e.user["id"], e.organizationOwner)
	require.NotEmpty(t, e.settings["bootstrappedAt"])
	require.NotEmpty(t, resumed.Result().Cookies())
}
