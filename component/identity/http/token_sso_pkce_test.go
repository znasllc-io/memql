package http

// End-to-end PKCE-binding test for an SSO-minted auth code (memql#1556).
//
// The SSO short-circuit on /authorize now binds the PKCE code_challenge
// onto the minted auth-code row (web/redirect_authenticated.go + the mint
// adapter in app/integrations_identity.go). This test proves the payoff at
// the consuming end: /oauth/token must
//   - REQUIRE the matching code_verifier when the code carries a challenge
//     (a PKCE-required connector like claude.ai succeeds only with the
//     correct verifier), and
//   - REJECT a wrong / missing verifier.
//
// It mirrors the staging connector flow: a DCR client
// (mcp_..., redirect https://claude.ai/api/mcp/auth_callback) exchanges a
// PKCE-bound code that the SSO mint produced.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// RFC 7636 Appendix B example pair.
const (
	ssoVerifier      = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	ssoChallenge     = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	ssoDCRClientId   = "mcp_968c8934-abcd-4321-efgh-0123456789ab"
	ssoDCRRedirect   = "https://claude.ai/api/mcp/auth_callback"
	ssoAuthCodePlain = "SSO-PLAIN-CODE"
)

// ssoTokenFakeEngine serves the rows the authorization_code grant reads:
// the DCR oauth client, the PKCE-bound auth-code row (keyed by codeHash),
// and a minimal user row. Mutations (consume / create session) are no-ops.
type ssoTokenFakeEngine struct {
	codeHash      string
	codeChallenge string // empty => non-PKCE code
}

func (f *ssoTokenFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case strings.HasPrefix(q, "queryOAuthClientByClientId("):
		node := &memqlv1.MemoryNode{
			Id: ssoDCRClientId,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"clientId":         structpb.NewStringValue(ssoDCRClientId),
				"redirectURIsJSON": structpb.NewStringValue(`["` + ssoDCRRedirect + `"]`),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	case strings.HasPrefix(q, "queryAuthCodeByCodeHash("):
		presented := extractField(q, "codeHash")
		if presented != f.codeHash {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		fields := map[string]*structpb.Value{
			"id":          structpb.NewStringValue("v1:identity:authCode:sso1"),
			"codeHash":    structpb.NewStringValue(f.codeHash),
			"clientId":    structpb.NewStringValue(ssoDCRClientId),
			"redirectURI": structpb.NewStringValue(ssoDCRRedirect),
			"userId":      structpb.NewStringValue("v1:identity:user:owner1"),
			"expiresAt":   structpb.NewStringValue(time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)),
		}
		if f.codeChallenge != "" {
			fields["codeChallenge"] = structpb.NewStringValue(f.codeChallenge)
			fields["codeChallengeMethod"] = structpb.NewStringValue("S256")
		}
		node := &memqlv1.MemoryNode{Id: "v1:identity:authCode:sso1", Payload: &structpb.Struct{Fields: fields}}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	case strings.HasPrefix(q, "queryUserById("):
		node := &memqlv1.MemoryNode{
			Id: "v1:identity:user:owner1",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue("v1:identity:user:owner1"),
				"primaryEmail": structpb.NewStringValue("owner@example.com"),
				"role":         structpb.NewStringValue("owner"),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	default:
		// Mutations (consume auth code, create session) -> success no-op.
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
}

func newSSOTokenServer(t *testing.T, codeChallenge string) *Server {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("KeyManager.Load: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	store := &identity.Store{Engine: &ssoTokenFakeEngine{
		codeHash:      hashCode(ssoAuthCodePlain),
		codeChallenge: codeChallenge,
	}}
	return &Server{
		Cfg:    cfg,
		Store:  store,
		Issuer: iss,
	}
}

func postToken(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleToken(rec, req)
	return rec
}

// TestSSOAuthCode_PKCEBound_CorrectVerifierSucceeds: a PKCE-bound SSO code
// + the matching verifier exchanges successfully (200 + access_token).
func TestSSOAuthCode_PKCEBound_CorrectVerifierSucceeds(t *testing.T) {
	s := newSSOTokenServer(t, ssoChallenge)

	rec := postToken(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {ssoAuthCodePlain},
		"client_id":     {ssoDCRClientId},
		"redirect_uri":  {ssoDCRRedirect},
		"code_verifier": {ssoVerifier},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "access_token") {
		t.Errorf("expected access_token in response; body=%s", rec.Body.String())
	}
}

// TestSSOAuthCode_PKCEBound_WrongVerifierFails: a PKCE-bound SSO code with
// the wrong verifier is rejected (invalid_grant).
func TestSSOAuthCode_PKCEBound_WrongVerifierFails(t *testing.T) {
	s := newSSOTokenServer(t, ssoChallenge)

	rec := postToken(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {ssoAuthCodePlain},
		"client_id":     {ssoDCRClientId},
		"redirect_uri":  {ssoDCRRedirect},
		"code_verifier": {"the-wrong-verifier-aaaaaaaaaaaaaaaaaaaaaaa"},
	})

	if rec.Code == http.StatusOK {
		t.Fatalf("wrong verifier must NOT succeed; got 200 body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("expected invalid_grant; got body=%s", rec.Body.String())
	}
}

// TestSSOAuthCode_PKCEBound_MissingVerifierFails: a PKCE-bound SSO code
// with NO verifier is rejected (PKCE is required once a challenge is bound).
func TestSSOAuthCode_PKCEBound_MissingVerifierFails(t *testing.T) {
	s := newSSOTokenServer(t, ssoChallenge)

	rec := postToken(t, s, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {ssoAuthCodePlain},
		"client_id":    {ssoDCRClientId},
		"redirect_uri": {ssoDCRRedirect},
		// no code_verifier
	})

	if rec.Code == http.StatusOK {
		t.Fatalf("missing verifier must NOT succeed for a PKCE-bound code; got 200 body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("expected invalid_grant; got body=%s", rec.Body.String())
	}
}
