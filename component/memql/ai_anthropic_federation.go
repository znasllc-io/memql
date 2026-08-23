package memql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
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
// THE ONE THING THAT MAKES THIS A CODE CHANGE RATHER THAN A CONFIG CHANGE:
// `option.WithAPIKey` prepends the default options and DISABLES the SDK's
// whole credential chain (SDK client.go:33-51). Setting every ANTHROPIC_*
// variable on the pod would change nothing while the constructor still passed
// a key, so the constructor has to choose federation deliberately. Hence this
// file, and hence anthropicCredential being the ONLY place either Anthropic
// constructor decides what to authenticate with.
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
	envAnthropicAPIKey            = "MEMQL_AI_ANTHROPIC_API_KEY"
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
	authKeyAPIKey            = "apiKey"
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
	credentialPathAPIKey     credentialPath = "api-key"
)

// federationWarnOnce keeps the "both configured" warning to one line per
// process rather than one per provider: the base `anthropic` provider is
// @extends'd by every Claude model entry, so the same auth map is evaluated a
// dozen times at boot and an un-deduplicated warning would bury itself.
var federationWarnOnce sync.Once

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
// The four-way switch is D3 of the design, and the middle case is the one
// worth defending. A half-configured federation is a MISCONFIGURATION, and
// the alternative -- silently falling back to a key that the cutover is about
// to delete -- is exactly the failure this change exists to remove: it would
// work in every test, work at boot, and stop working the hour an operator
// finished the runbook, on a node nobody was looking at.
//
// httpClient is the guarded client (the LLM circuit breaker); it is attached
// in EVERY branch, federation included, because the federation exchange rides
// the same client and the exchange observer lives on that transport.
func anthropicCredential(cfg ProviderConfig, httpClient *http.Client) ([]option.RequestOption, credentialPath, error) {
	apiKey := strings.TrimSpace(cfg.Auth[authKeyAPIKey])
	fed := anthropicFederationFrom(cfg.Auth)
	present, missing := fed.present(), fed.missing()

	switch {
	case len(missing) == 0:
		// Federation. A key alongside it is ignored, loudly but once.
		if apiKey != "" {
			federationWarnOnce.Do(func() {
				slog.Warn("anthropic: workload identity federation is configured; the static API key is IGNORED",
					"ignoredKey", envAnthropicAPIKey,
					"federationRuleId", fed.RuleID,
					"serviceAccountId", fed.ServiceAccountID,
					"runbook", "docs/public/operate/auth/anthropic-federation.md")
			})
		}
		if err := preflightIdentityToken(fed.TokenFile); err != nil {
			return nil, "", fmt.Errorf("provider %q anthropic federation preflight: %w", cfg.Name, err)
		}
		opts := []option.RequestOption{
			// ORDER IS LOAD-BEARING: the http.Client goes on FIRST.
			// WithFederationTokenProvider builds its TokenCache around
			// `r.HTTPClient.Do` AT THE MOMENT THE OPTION IS APPLIED
			// (SDK internal/auth/middleware.go:140-146), and options apply in
			// slice order. Listed after it, our guarded client would front
			// every Messages call while the token exchange quietly used the
			// SDK's default client instead -- so the exchange would work,
			// traffic would flow, and the exchange observer would see nothing
			// at all. A zero on memql_ai_federation_exchanges_total reads as
			// "federation is not in use", which is the most misleading answer
			// available. TestFederationExchangeHappyPath holds this order.
			option.WithHTTPClient(httpClient),
			option.WithFederationTokenProvider(
				option.IdentityTokenFile(fed.TokenFile),
				option.FederationOptions{
					FederationRuleID: fed.RuleID,
					OrganizationID:   fed.OrganizationID,
					ServiceAccountID: fed.ServiceAccountID,
					WorkspaceID:      fed.WorkspaceID,
				},
			),
			// AMBIENT CREDENTIALS ARE CLEARED, and this is load-bearing.
			// anthropic.NewClient prepends DefaultClientOptions, which walks
			// the SDK's own credential chain: a bare ANTHROPIC_API_KEY or
			// ANTHROPIC_AUTH_TOKEN in the process environment becomes a
			// persistent X-Api-Key / authorization header on the request
			// config. The federation middleware then sees a request that
			// already carries a static auth header and SKIPS ITSELF
			// (SDK internal/auth/middleware.go:34-36) -- so a stray env var
			// would silently return the cluster to a long-lived key while
			// every log line and this constructor still said "federation".
			// That is precisely the failure D3 refuses to allow, in the one
			// form no configuration of ours controls. Deleting the config
			// headers is the SDK's own documented recipe for it
			// (betasessiontoolrunner.go:98-104); the middleware sets
			// Authorization per request, so nothing legitimate is lost.
			option.WithHeaderDel("X-Api-Key"),
			option.WithHeaderDel("Authorization"),
		}
		return opts, credentialPathFederation, nil

	case len(present) > 0:
		// One to three set: refuse. Naming both halves is deliberate -- the
		// operator is mid-cutover and needs to know which of the four they
		// have, not only which they lack.
		return nil, "", fmt.Errorf(
			"provider %q is HALF-CONFIGURED for Anthropic workload identity federation: %s set, %s missing. "+
				"Set all four (or none, to keep using %s). A partial federation config is refused rather than "+
				"silently falling back to the key, because the key is what the cutover removes. "+
				"Runbook: docs/public/operate/auth/anthropic-federation.md",
			cfg.Name, strings.Join(present, ", "), strings.Join(missing, ", "), envAnthropicAPIKey)

	case apiKey == "":
		// Neither credential. Same failure as before federation existed, with
		// the seeding hint the auth resolver used to carry (it no longer
		// errors on these names -- see optionalAuthEnvNames).
		// NAMES REAL THINGS ONLY (memql#4338), and it is fixed here rather
		// than only at the resolver because these two are the SAME story
		// told at two moments -- the resolver's error for an ordinary
		// provider, and this one for the Anthropic path where the resolver
		// deliberately stays silent. Fixing one and leaving the other
		// saying `make secret-set` would leave a pair that
		// docs/public/operate/env-vars.md describes together disagreeing.
		return nil, "", fmt.Errorf(
			"provider %q has no Anthropic credential: neither %s nor workload identity federation is configured. "+
				"Seed the key under %s -- in the node's environment (locally `make secrets`; in a cluster, "+
				"whichever secret store the deployment reads), or as a v1:platform:globalSecret row -- "+
				"or configure federation (docs/public/operate/auth/anthropic-federation.md)",
			cfg.Name, envAnthropicAPIKey, envAnthropicAPIKey)

	default:
		return []option.RequestOption{
			option.WithAPIKey(apiKey),
			option.WithHTTPClient(httpClient),
		}, credentialPathAPIKey, nil
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
func preflightIdentityToken(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s is empty", envAnthropicIdentityTokenFile)
	}
	raw, err := readFileTrimmed(path)
	if err != nil {
		return fmt.Errorf(
			"cannot read the projected identity token at %s (%s): %v. In the cluster this file is a projected "+
				"serviceAccountToken volume; check that the Deployment carries the anthropic-identity volume and "+
				"its mount",
			path, envAnthropicIdentityTokenFile, err)
	}
	claims, err := parseJWTClaims(raw)
	if err != nil {
		return fmt.Errorf("the identity token at %s is not a JWT: %v", path, err)
	}
	if !jwtAudienceContains(claims, anthropicAudience) {
		return fmt.Errorf(
			"the identity token at %s does not carry the %q audience (aud=%v). Kubernetes mints a projected "+
				"token FOR one audience; the default kube-api-access token is not interchangeable with it. Check "+
				"the serviceAccountToken source's `audience` field",
			path, anthropicAudience, jwtAudienceValues(claims))
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
	envAnthropicAPIKey:            {},
	envAnthropicFederationRuleID:  {},
	envAnthropicOrganizationID:    {},
	envAnthropicServiceAccountID:  {},
	envAnthropicWorkspaceID:       {},
	envAnthropicIdentityTokenFile: {},
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
