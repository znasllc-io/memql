package memql

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/config"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/znasllc-io/memql/component/metrics"
)

// Anthropic workload identity federation (epic memql#4333).
//
// The engine used to authenticate to Anthropic with one static key that never
// rotates, lives in every engine pod's environment, and lets anyone who can
// read `memql-secrets` spend against the account from anywhere. Federation
// replaces it: the pod presents the OIDC JWT Kubernetes projects for it, the
// SDK exchanges that at POST /v1/oauth/token for a one-hour bearer, and
// nothing long-lived is at rest.
//
// THE STATIC KEY IS GONE (epic memql#5088, design D2/D5). This file used to
// carry a four-way switch whose last arm was `option.WithAPIKey`; there is no
// manually entered vendor key anywhere in the product any more, so the switch
// is three-way and its last arm is "unavailable". What survives unchanged is
// the reason this is a code change rather than a config change:
// `option.WithAPIKey` prepends the default options and DISABLES the SDK's whole
// credential chain (SDK client.go:33-51), so an ambient ANTHROPIC_API_KEY in
// the pod's environment would silently return the cluster to a long-lived key.
// That is what the WithHeaderDel pair below is for, and it is why
// anthropicCredential is the ONLY place either Anthropic constructor decides
// what to authenticate with.
//
// Runbook (Console setup, cutover, key removal):
// docs/public/operate/auth/anthropic-federation.md

// The five federation-related env names, as the DSL provider's auth block
// references them (dsl/providers/providers.memql). They appear here for the
// error messages ONLY -- the values are read out of the provider's RESOLVED
// auth map, never with os.Getenv, so the three-tier resolution
// (globalSecret -> globalVariable -> env) applies to them exactly as it does
// to apiKey and the env registry's "registered but read nowhere" gate sees
// the DSL reference.
const (
	envAnthropicFederationRuleID  = "MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID"
	envAnthropicOrganizationID    = "MEMQL_AI_ANTHROPIC_ORGANIZATION_ID"
	envAnthropicServiceAccountID  = "MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID"
	envAnthropicWorkspaceID       = "MEMQL_AI_ANTHROPIC_WORKSPACE_ID"
	envAnthropicIdentityTokenFile = "MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE"
)

// anthropicAudience is the audience the projected Kubernetes token must carry
// and the one the federation rule pins. Kubernetes mints the token FOR this
// audience, so a token minted for the API server (the default
// kube-api-access-* one) is not interchangeable with it -- which is the point.
const anthropicAudience = "https://api.anthropic.com"

// The auth-map keys the DSL's anthropic auth block declares. Kept next to the
// env names they resolve from so a rename cannot half-land.
const (
	authKeyFederationRuleID  = "federationRuleId"
	authKeyOrganizationID    = "organizationId"
	authKeyServiceAccountID  = "serviceAccountId"
	authKeyWorkspaceID       = "workspaceId"
	authKeyIdentityTokenFile = "identityTokenFile"
)

// credentialPath names which branch of anthropicCredential a client was built
// on. It is what `memql provider-auth check` prints and what the tests assert,
// so it is a value rather than a log line.
type credentialPath string

const (
	credentialPathFederation credentialPath = "federation"
)

// anthropicFederation is the resolved federation half of an Anthropic
// provider's auth block.
type anthropicFederation struct {
	RuleID           string
	OrganizationID   string
	ServiceAccountID string
	WorkspaceID      string
	TokenFile        string
}

// requiredFederationFields is the set that decides the branch: all four
// present means federate, none means the key, anything between refuses. The
// workspace id is deliberately NOT in it -- Anthropic requires it only when a
// rule spans more than one workspace, and demanding it here would make the
// common single-workspace install carry a value it does not need.
//
// The order is the order the missing-key error names them in, which is the
// order the runbook creates them in.
func (f anthropicFederation) requiredFederationFields() []struct {
	envName string
	value   string
} {
	return []struct {
		envName string
		value   string
	}{
		{envAnthropicFederationRuleID, f.RuleID},
		{envAnthropicOrganizationID, f.OrganizationID},
		{envAnthropicServiceAccountID, f.ServiceAccountID},
		{envAnthropicIdentityTokenFile, f.TokenFile},
	}
}

// present returns the env names of the required federation fields that carry
// a value, and missing returns the ones that do not.
func (f anthropicFederation) present() []string {
	var out []string
	for _, field := range f.requiredFederationFields() {
		if field.value != "" {
			out = append(out, field.envName)
		}
	}
	return out
}

func (f anthropicFederation) missing() []string {
	var out []string
	for _, field := range f.requiredFederationFields() {
		if field.value == "" {
			out = append(out, field.envName)
		}
	}
	return out
}

// anthropicFederationFrom reads the federation half out of a provider's
// resolved auth map.
func anthropicFederationFrom(auth map[string]string) anthropicFederation {
	return anthropicFederation{
		RuleID:           strings.TrimSpace(auth[authKeyFederationRuleID]),
		OrganizationID:   strings.TrimSpace(auth[authKeyOrganizationID]),
		ServiceAccountID: strings.TrimSpace(auth[authKeyServiceAccountID]),
		WorkspaceID:      strings.TrimSpace(auth[authKeyWorkspaceID]),
		TokenFile:        strings.TrimSpace(auth[authKeyIdentityTokenFile]),
	}
}

// anthropicCredential decides how one Anthropic client authenticates and
// returns the request options that do it, plus which path it chose.
//
// The three-way switch is D3 of the design, and the middle case is the one
// worth defending. A half-configured federation is a MISCONFIGURATION, and
// the alternative -- silently treating it as "not configured" -- is exactly
// the failure this refusal exists to remove: an operator halfway through the
// runbook believes they are ENABLING Anthropic, not disabling it.
//
// EVERY CLIENT OF THIS VENDOR SHARES ONE SIGN-IN (design record
// 2026-11-21-vendor-accounts-and-api-keys, A1). This used to hand each client
// option.WithFederationTokenProvider, which builds a token cache per client:
// four Claude records made four sign-ins with one identity token, and
// Anthropic accepts a token once. The bearer now comes from the process's
// vendorSignIn through anthropicBearerMiddleware, so the number of clients no
// longer decides the number of sign-ins. TestAnthropicClientsShareOneSignIn
// holds it against a vendor that refuses a repeated token.
//
// httpClient is the caller's guarded client and fronts the API calls. The
// sign-in itself does NOT ride it: it uses a guarded client of its own, so a
// streaming client's timeouts never apply to an exchange, and the exchange
// observer on the guarded transport still sees every one.
func anthropicCredential(cfg ProviderConfig, httpClient *http.Client) ([]option.RequestOption, credentialPath, error) {
	signIn, err := anthropicSignInFor(cfg)
	if err != nil {
		return nil, "", err
	}
	if signIn == nil {
		// None set. NOT AN ERROR, and this arm is where the key used to be
		// (epic memql#5088, design D2). "No federation ids" means the vendor's
		// cluster account is not configured, which is the normal state of a
		// fresh cluster. The provider registers as unavailable and the
		// readiness model reports `ai` unconfigured; nothing falls back to
		// anything.
		return nil, credentialPathUnavailable, nil
	}
	opts := []option.RequestOption{
		option.WithHTTPClient(httpClient),
		option.WithMiddleware(anthropicBearerMiddleware(signIn)),
		// AMBIENT CREDENTIALS ARE CLEARED, and this is load-bearing.
		// anthropic.NewClient prepends DefaultClientOptions, which walks the
		// SDK's own credential chain: a bare ANTHROPIC_API_KEY or
		// ANTHROPIC_AUTH_TOKEN in the process environment becomes a persistent
		// X-Api-Key / authorization header on the request config. A stray env
		// var would silently return the cluster to a long-lived key while every
		// log line still said "federation". Deleting the config headers is the
		// SDK's own documented recipe for it; the middleware sets Authorization
		// per request and removes X-Api-Key from the request itself, so nothing
		// legitimate is lost. TestFederationSurvivesAnAmbientAnthropicKey.
		option.WithHeaderDel("X-Api-Key"),
		option.WithHeaderDel("Authorization"),
	}
	return opts, credentialPathFederation, nil
}

// anthropicSignInFor returns the process's sign-in for the cluster's Anthropic
// account, built from a provider's resolved auth map. It returns nil and no
// error when no federation id is set, and the half-configured refusal when
// some are.
func anthropicSignInFor(cfg ProviderConfig) (*vendorSignIn, error) {
	fed := anthropicFederationFrom(cfg.Auth)
	present, missing := fed.present(), fed.missing()
	switch {
	case len(missing) == 0:
	case len(present) > 0:
		// One to three set: refuse. Naming both halves is deliberate -- the
		// operator is mid-cutover and needs to know which of the four they
		// have, not only which they lack.
		return nil, fmt.Errorf(
			"provider %q is HALF-CONFIGURED for Anthropic workload identity federation: %s set, %s missing. "+
				"Set all four, or none to leave the Anthropic providers unavailable. A partial federation "+
				"config is refused rather than quietly read as 'not configured', because an operator halfway "+
				"through the runbook believes they are ENABLING Anthropic, not disabling it. "+
				"Runbook: docs/public/operate/auth/anthropic-federation.md",
			cfg.Name, strings.Join(present, ", "), strings.Join(missing, ", "))
	default:
		return nil, nil
	}

	if err := preflightIdentityToken(fed.TokenFile, anthropicAudience, envAnthropicIdentityTokenFile); err != nil {
		return nil, fmt.Errorf("provider %q anthropic federation preflight: %w", cfg.Name, err)
	}

	ids := map[string]string{
		authKeyFederationRuleID: fed.RuleID,
		authKeyOrganizationID:   fed.OrganizationID,
		authKeyServiceAccountID: fed.ServiceAccountID,
	}
	if fed.WorkspaceID != "" {
		ids[authKeyWorkspaceID] = fed.WorkspaceID
	}
	exchangeClient := guardedHTTPClient(nil)
	return processVendorSignIn(vendorSignInConfig{
		Vendor: metrics.FederationVendorAnthropic,
		Fingerprint: signInFingerprint(metrics.FederationVendorAnthropic,
			fed.RuleID, fed.OrganizationID, fed.ServiceAccountID, fed.WorkspaceID, fed.TokenFile),
		TokenFile: fed.TokenFile,
		// Anthropic documents that an identity token carrying a jti is accepted
		// once. A Kubernetes projected token carries one.
		ReplayProtected: true,
		Ids:             ids,
		Exchange: func(ctx context.Context, identityToken string) (string, time.Time, error) {
			// The SDK's own public exchange, so the wire contract (the body's
			// field names, the two beta values, the redirect policy) stays the
			// SDK's to keep right. BaseURL is the address the calling client
			// talks to; empty means the SDK's default.
			base, _ := ctx.Value(anthropicExchangeBaseKey{}).(string)
			creds, err := config.ExchangeFederationAssertion(ctx, config.FederationExchangeParams{
				Assertion:        identityToken,
				FederationRuleID: fed.RuleID,
				OrganizationID:   fed.OrganizationID,
				ServiceAccountID: fed.ServiceAccountID,
				WorkspaceID:      fed.WorkspaceID,
				BaseURL:          base,
				HTTPClient:       exchangeClient,
				UserAgent:        "memql-engine",
			})
			if err != nil {
				var refused *config.FederationExchangeError
				if errors.As(err, &refused) && refused.StatusCode >= 400 && refused.StatusCode < 500 {
					return "", time.Time{}, &vendorExchangeRefused{
						Status: refused.StatusCode,
						Body:   truncateForLog(config.RedactOAuthErrorBody(refused.Body), maxFederationBodyNote),
					}
				}
				return "", time.Time{}, err
			}
			var expiresAt time.Time
			if creds.ExpiresAt != nil {
				expiresAt = *creds.ExpiresAt
			}
			return creds.AccessToken, expiresAt, nil
		},
	}), nil
}

// signInFingerprint identifies one vendor configuration: two provider records
// with the same ids and token file are the same sign-in.
func signInFingerprint(vendor string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(vendor))
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// anthropicExchangeBaseKey carries the address a client talks to into the
// sign-in's exchange.
type anthropicExchangeBaseKey struct{}

// anthropicBearerMiddleware puts the process's federated bearer on every
// request a client sends to its own vendor address.
//
// PER REQUEST, not once at construction: the bearer expires within the hour.
//
// ONLY TO THE CLIENT'S OWN ADDRESS. The address of the client's first request
// is pinned, and a request to any other host is passed through with no
// credential and without an identity token being sent anywhere for it. The
// SDK's own middleware keeps the same rule against a request path that is an
// absolute URL for some other host.
func anthropicBearerMiddleware(signIn *vendorSignIn) option.Middleware {
	var (
		mu     sync.Mutex
		pinned string
	)
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		origin := req.URL.Scheme + "://" + req.URL.Host
		mu.Lock()
		if pinned == "" {
			pinned = origin
		}
		base := pinned
		mu.Unlock()
		if !strings.EqualFold(origin, base) {
			return next(req)
		}

		bearer, _, err := signIn.Bearer(context.WithValue(req.Context(), anthropicExchangeBaseKey{}, base))
		if err != nil {
			return nil, err
		}
		req.Header.Del("X-Api-Key")
		req.Header.Set("Authorization", "Bearer "+bearer)
		// A federated bearer is an OAuth bearer, and the API requires this beta
		// value beside one. Merged, never replaced: a call may carry betas of
		// its own.
		if existing := req.Header.Get("anthropic-beta"); existing == "" {
			req.Header.Set("anthropic-beta", config.OAuthAPIBetaHeader)
		} else if !strings.Contains(existing, config.OAuthAPIBetaHeader) {
			req.Header.Set("anthropic-beta", existing+","+config.OAuthAPIBetaHeader)
		}

		resp, err := next(req)
		if err == nil && resp != nil && resp.StatusCode == http.StatusUnauthorized {
			// The vendor no longer accepts this bearer. It is dropped, and the
			// next call signs in if there is an identity token that may be
			// presented. The 401 goes back to the caller as it is: retrying
			// here would need a second sign-in with a token already used.
			signIn.Invalidate(bearer)
		}
		return resp, err
	}
}

// newAnthropicClient builds the SDK client for a provider config, whichever
// credential it carries. Both Anthropic constructors go through it so the
// choice is made in exactly one place.
func newAnthropicClient(cfg ProviderConfig, httpClient *http.Client) (anthropic.Client, credentialPath, error) {
	opts, path, err := anthropicCredential(cfg, httpClient)
	if err != nil {
		return anthropic.Client{}, "", err
	}
	if path == credentialPathUnavailable {
		// REFUSING HERE IS THE WHOLE POINT OF THE UNAVAILABLE PATH.
		//
		// anthropicCredential returns no error for "nothing configured",
		// because that is a legitimate state rather than a fault. But
		// anthropic.NewClient() with no options is a perfectly constructible
		// client that carries no credential -- so returning it would register
		// every Claude provider as AVAILABLE on a cluster that cannot call
		// one, and the failure would arrive as a 401 on a user's turn instead
		// of as a line at boot. TestRealTreeBootsKeylessAndQuiet is what
		// caught exactly that.
		return anthropic.Client{}, path, fmt.Errorf(
			"provider %q has no Anthropic credential: workload identity federation is not configured on this "+
				"cluster. Set %s, %s and %s (docs/public/operate/auth/anthropic-federation.md). There is no API "+
				"key to fall back to -- a local cluster cannot federate, because its OIDC issuer is private, and "+
				"reaches models through a signed-in fleet machine or a local model instead",
			cfg.Name, envAnthropicFederationRuleID, envAnthropicOrganizationID, envAnthropicServiceAccountID)
	}
	return anthropic.NewClient(opts...), path, nil
}

// preflightIdentityToken refuses boot when the projected token is not a token
// this cluster could federate with.
//
// It runs at CONSTRUCTION, not at first call, for the same reason a missing
// API key does: a credential problem that waits for the first LLM request
// surfaces as a failed user turn hours later, on whichever node happened to
// take it. Everything checked here is knowable from the file alone -- whether
// Kubernetes projected a token at all, whether it was minted for Anthropic's
// audience rather than the API server's, and whether the subject is a service
// account. Whether ANTHROPIC accepts it is not knowable locally and is the
// runbook's step 4 (`memql provider-auth check`).
func preflightIdentityToken(path, audience, envName string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s is empty", envName)
	}
	raw, err := readFileTrimmed(path)
	if err != nil {
		return fmt.Errorf(
			"cannot read the projected identity token at %s (%s): %v. In the cluster this file is a projected "+
				"serviceAccountToken volume; check that the Deployment carries the matching identity volume and "+
				"its mount",
			path, envName, err)
	}
	claims, err := parseJWTClaims(raw)
	if err != nil {
		return fmt.Errorf("the identity token at %s is not a JWT: %v", path, err)
	}
	if !jwtAudienceContains(claims, audience) {
		return fmt.Errorf(
			"the identity token at %s does not carry the %q audience (aud=%v). Kubernetes mints a projected "+
				"token FOR one audience; neither the default kube-api-access token nor the OTHER vendor's "+
				"projected token is interchangeable with it. Check the serviceAccountToken source's `audience` field",
			path, audience, jwtAudienceValues(claims))
	}
	sub, _ := claims["sub"].(string)
	if !strings.HasPrefix(sub, "system:serviceaccount:") {
		return fmt.Errorf(
			"the identity token at %s has sub=%q, which is not a Kubernetes service account (expected a "+
				"`system:serviceaccount:<namespace>:<name>` subject -- the federation rule matches on its prefix)",
			path, sub)
	}
	return nil
}

// readFileTrimmed reads a file and trims surrounding whitespace. Kubernetes
// writes the projected token with no trailing newline, but a hand-placed test
// or dev token often has one, and a trailing newline is not a JWT.
func readFileTrimmed(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// parseJWTClaims decodes the payload of a JWS compact serialization WITHOUT
// verifying the signature.
//
// Not verifying is correct here and worth saying so nobody "fixes" it: the
// token is one kubelet wrote to this pod's own filesystem, and the party that
// must verify it is ANTHROPIC, against the cluster's published JWKS. Verifying
// it locally would prove only that the pod can read its own file. What this
// preflight is for is catching a token that is structurally the wrong token --
// wrong audience, wrong subject kind -- before it costs a failed user turn.
func parseJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("expected 3 dot-separated segments, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("payload is not base64url: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("payload is not JSON: %w", err)
	}
	return claims, nil
}

// jwtAudienceValues normalizes the `aud` claim, which RFC 7519 allows to be
// either a single string or an array of them. Kubernetes emits the array
// form; treating only one shape as valid would work in the cluster and fail
// against any other issuer.
func jwtAudienceValues(claims map[string]any) []string {
	switch v := claims["aud"].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}

func jwtAudienceContains(claims map[string]any, want string) bool {
	for _, aud := range jwtAudienceValues(claims) {
		if aud == want {
			return true
		}
	}
	return false
}

// optionalAuthEnvNames are the auth placeholders whose absence is NOT a
// provider-load failure.
//
// Every other placeholder that cannot be resolved makes its provider load as
// unavailable, which is right: a provider with an unresolvable key cannot
// work, and the message tells the operator how to seed it. The Anthropic
// credential is the one case where absence is a legitimate configuration
// rather than a mistake, in BOTH directions:
//
//   - the four federation ids and the token-file path are unset on every
//     local cluster (D4: same manifests everywhere, ids empty locally), and
//   - the API key is unset in the cloud once the cutover finishes (D6).
//
// Left non-optional, either state would take every Claude provider out of the
// registry with a warning rather than an error -- silently on the local side,
// since the resolver's failure is a log line, not a boot refusal. So these
// names resolve to absent, and anthropicCredential -- the one place that knows
// which combination is meaningful -- decides what an absence means. It carries
// the seeding hint the resolver would have printed.
var optionalAuthEnvNames = map[string]struct{}{
	envAnthropicFederationRuleID:  {},
	envAnthropicOrganizationID:    {},
	envAnthropicServiceAccountID:  {},
	envAnthropicWorkspaceID:       {},
	envAnthropicIdentityTokenFile: {},
	envOpenAIIdentityProviderID:   {},
	envOpenAIServiceAccountID:     {},
	envOpenAIIdentityTokenFile:    {},
}

// optionalAuthPlaceholder reports whether an unresolved placeholder for this
// env name should be left absent instead of failing the provider.
func optionalAuthPlaceholder(envName string) bool {
	_, ok := optionalAuthEnvNames[strings.TrimSpace(envName)]
	return ok
}

// optionalAuthEnvNameList returns the optional names sorted, for messages and
// tests.
func optionalAuthEnvNameList() []string {
	out := make([]string, 0, len(optionalAuthEnvNames))
	for name := range optionalAuthEnvNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
