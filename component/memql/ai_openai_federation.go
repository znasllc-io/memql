package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/option"

	"github.com/znasllc-io/memql/component/metrics"
)

// OpenAI workload identity federation (epic memql#5088).
//
// The engine reached OpenAI with a static key that never rotated, lived in
// every engine pod's environment, and let anyone who could read the
// `memql-secrets` Secret spend against the account from anywhere. Federation
// replaces it: the pod presents the OIDC JWT Kubernetes projects for it, the
// engine exchanges that at OpenAI's auth host for a bearer that expires within
// the hour, and nothing long-lived is at rest.
//
// The 2026-08-22 Anthropic record said OpenAI offered no federation. THAT WAS
// WRONG WHEN IT WAS WRITTEN -- OpenAI's workload identity federation has been
// generally available since 2025-05-26 -- and the sentence outlived its
// correction for long enough to shape a design. It is deleted from that
// runbook rather than annotated.
//
// WHY THE ENGINE OWNS THE EXCHANGE RATHER THAN THE SDK (design D3).
// option.WithWorkloadIdentity exists on openai-go/v3 and would serve the HTTP
// clients perfectly well. It would NOT serve the Realtime transcription
// WebSocket, which dials with a raw `Authorization: Bearer` header of its own
// and never goes through the SDK's request pipeline. Letting the SDK federate
// would leave two exchangers with two caches and two refresh clocks, one of
// them invisible to `provider-auth check` -- so the operator command that
// exists to answer "is federation working" would be answering it about half
// the traffic. One exchanger here, and every consumer takes its bearer.
//
// Runbook (Platform console setup, the two ids, verification):
// docs/public/operate/auth/openai-federation.md

// The three federation env names, as the DSL provider's auth block references
// them (dsl/providers/providers.memql). They appear here for the error
// messages ONLY -- the values are read out of the provider's RESOLVED auth
// map, never with os.Getenv, so the three-tier resolution (globalSecret ->
// globalVariable -> env) applies to them and the env registry's "registered
// but read nowhere" gate sees the DSL reference.
const (
	envOpenAIIdentityProviderID = "MEMQL_AI_OPENAI_IDENTITY_PROVIDER_ID"
	envOpenAIServiceAccountID   = "MEMQL_AI_OPENAI_SERVICE_ACCOUNT_ID"
	envOpenAIIdentityTokenFile  = "MEMQL_AI_OPENAI_IDENTITY_TOKEN_FILE"
)

// openaiAudience is the audience the projected Kubernetes token must carry and
// the one the identity-provider mapping pins. Kubernetes mints a token FOR one
// audience, so the Anthropic token mounted beside it is not interchangeable
// with this one -- which is the point, and the reason every engine Deployment
// carries two projected volumes rather than sharing one.
const openaiAudience = "https://api.openai.com/v1"

// openaiTokenEndpoint is where the RFC 8693 exchange is posted. It is a var so
// the exchange tests can point it at an httptest server; nothing else writes
// it.
var openaiTokenEndpoint = "https://auth.openai.com/oauth/token"

// The auth-map keys the DSL's openai auth block declares.
const (
	authKeyIdentityProviderID     = "identityProviderId"
	authKeyOpenAIServiceAccountID = "serviceAccountId"
	authKeyOpenAITokenFile        = "identityTokenFile"
	// authKeyOpenAITokenEndpoint overrides the exchange endpoint. It exists for
	// the same reason authKeyBaseURL does -- pointing a provider at a local
	// server -- and is not something an operator sets.
	authKeyOpenAITokenEndpoint = "tokenEndpoint"
)

// credentialPathUnavailable names the branch where no credential is
// configured at all.
//
// It is a PATH rather than an error because it is the normal state of a fresh
// cluster, local or cloud: no vendor has been set up yet, and the cluster has
// no key to fall back to. The readiness model reports that as `ai`
// unconfigured; nothing about it is a fault.
const credentialPathUnavailable credentialPath = "unavailable"

// openaiFederation is the resolved federation half of an OpenAI provider's
// auth block.
type openaiFederation struct {
	IdentityProviderID string
	ServiceAccountID   string
	TokenFile          string
	// TokenEndpoint is empty in every real configuration; see
	// authKeyOpenAITokenEndpoint.
	TokenEndpoint string
}

// requiredFederationFields is the set federation needs: all three present
// means federate. Whether the vendor is configured at all is asked of the two
// ids alone (anyIdSet): the token file's path is set by every Deployment on
// every cluster and is not a configuration on its own (the Anthropic twin of
// this function says what counting it cost).
//
// The order is the order the missing-field error names them in, which is the
// order the runbook creates them in.
func (f openaiFederation) requiredFederationFields() []struct {
	envName string
	value   string
} {
	return []struct {
		envName string
		value   string
	}{
		{envOpenAIIdentityProviderID, f.IdentityProviderID},
		{envOpenAIServiceAccountID, f.ServiceAccountID},
		{envOpenAIIdentityTokenFile, f.TokenFile},
	}
}

func (f openaiFederation) present() []string {
	var out []string
	for _, field := range f.requiredFederationFields() {
		if field.value != "" {
			out = append(out, field.envName)
		}
	}
	return out
}

// anyIdSet reports whether an operator set either id. The token file is not
// asked: see requiredFederationFields.
func (f openaiFederation) anyIdSet() bool {
	return f.IdentityProviderID != "" || f.ServiceAccountID != ""
}

func (f openaiFederation) missing() []string {
	var out []string
	for _, field := range f.requiredFederationFields() {
		if field.value == "" {
			out = append(out, field.envName)
		}
	}
	return out
}

func (f openaiFederation) tokenEndpoint() string {
	if endpoint := strings.TrimSpace(f.TokenEndpoint); endpoint != "" {
		return endpoint
	}
	return openaiTokenEndpoint
}

// openaiFederationFrom reads the federation half out of a provider's resolved
// auth map.
func openaiFederationFrom(auth map[string]string) openaiFederation {
	return openaiFederation{
		IdentityProviderID: strings.TrimSpace(auth[authKeyIdentityProviderID]),
		ServiceAccountID:   strings.TrimSpace(auth[authKeyOpenAIServiceAccountID]),
		TokenFile:          strings.TrimSpace(auth[authKeyOpenAITokenFile]),
		TokenEndpoint:      strings.TrimSpace(auth[authKeyOpenAITokenEndpoint]),
	}
}

// BearerSource hands out the current OpenAI bearer, exchanging when needed.
//
// It is an interface rather than the concrete exchanger because it has three
// consumers with nothing else in common: the SDK middleware, the Realtime
// transcription WebSocket, and `provider-auth check`. Every one of them wants
// the same thing -- a token that is valid right now -- and none of them should
// know how it is obtained or cached.
type BearerSource interface {
	Bearer(ctx context.Context) (token string, expiresAt time.Time, err error)
}

// openaiExchanger performs the RFC 8693 token exchange: one post, no cache.
//
// IT USED TO CACHE, and there used to be one per provider record plus one more
// for each transcription path, each presenting the pod's identity token for
// itself (design record 2026-11-21-vendor-accounts-and-api-keys, A1). The
// cache, the clock and the lock now belong to the process's vendorSignIn, which
// every OpenAI consumer shares; what is left here is the exchange.
type openaiExchanger struct {
	fed        openaiFederation
	httpClient *http.Client
}

// openaiSignInConfig describes the cluster's OpenAI account as a sign-in.
//
// NOT REPLAY-PROTECTED: OpenAI documents no rule against presenting an identity
// token twice, and its bearer never outlives the token it was minted from, so
// the sign-in may present the same token again once its bearer has died.
func openaiSignInConfig(fed openaiFederation, httpClient *http.Client) vendorSignInConfig {
	if httpClient == nil {
		httpClient = guardedHTTPClient(nil)
	}
	exchanger := &openaiExchanger{fed: fed, httpClient: httpClient}
	return vendorSignInConfig{
		Vendor: metrics.FederationVendorOpenAI,
		Fingerprint: signInFingerprint(metrics.FederationVendorOpenAI,
			fed.IdentityProviderID, fed.ServiceAccountID, fed.TokenFile, fed.tokenEndpoint()),
		TokenFile: fed.TokenFile,
		Ids: map[string]string{
			authKeyIdentityProviderID:     fed.IdentityProviderID,
			authKeyOpenAIServiceAccountID: fed.ServiceAccountID,
		},
		Exchange: exchanger.exchange,
	}
}

// newOpenAIExchanger builds a sign-in for one OpenAI federation that is NOT the
// process's shared one. Production code takes the shared sign-in from
// openaiCredential; this is for the tests that drive one exchange on a clock of
// their own.
func newOpenAIExchanger(fed openaiFederation, httpClient *http.Client) *vendorSignIn {
	return newVendorSignIn(openaiSignInConfig(fed, httpClient), time.Now, vendorSignInProcessStarted)
}

// exchangeRequest is the RFC 8693 token-exchange body OpenAI expects.
type exchangeRequest struct {
	GrantType        string `json:"grant_type"`
	SubjectTokenType string `json:"subject_token_type"`
	SubjectToken     string `json:"subject_token"`
	IdentityProvider string `json:"identity_provider_id"`
	ServiceAccount   string `json:"service_account_id"`
}

const (
	tokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	subjectTokenTypeJWT    = "urn:ietf:params:oauth:token-type:jwt"
)

// exchange posts one identity token and returns the bearer it is exchanged
// for. A zero expiry means OpenAI named none. A status that refuses the token
// (signInStatusRefuses: a 4xx other than 408, 425 and 429) is returned as
// *vendorExchangeRefused, which is what tells the sign-in the vendor answered
// and refused rather than never answering.
func (x *openaiExchanger) exchange(ctx context.Context, subject string) (string, time.Time, error) {
	body, err := json.Marshal(exchangeRequest{
		GrantType:        tokenExchangeGrantType,
		SubjectTokenType: subjectTokenTypeJWT,
		SubjectToken:     subject,
		IdentityProvider: x.fed.IdentityProviderID,
		ServiceAccount:   x.fed.ServiceAccountID,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal openai token exchange: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.fed.tokenEndpoint(), bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create openai token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Through the guarded client, so the federation observer on that transport
	// sees this exchange exactly as it sees Anthropic's.
	resp, err := x.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("openai token exchange did not complete: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read openai token exchange response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The vendor's own error body is carried through, truncated, for the
		// reason the Anthropic observer carries Anthropic's: it names the
		// reason the Platform console will show, and an operator holding that
		// string can act without reading any of our code. The SUBJECT TOKEN is
		// never in an error body -- it is something we sent, not something the
		// vendor echoes -- and is never logged from here either.
		note := truncateForLog(string(raw), maxFederationBodyNote)
		if signInStatusRefuses(resp.StatusCode) {
			return "", time.Time{}, &vendorExchangeRefused{Status: resp.StatusCode, Body: note}
		}
		return "", time.Time{}, fmt.Errorf("openai token exchange faulted (status %d): %s", resp.StatusCode, note)
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("parse openai token exchange response: %w", err)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return "", time.Time{}, fmt.Errorf("openai token exchange response carried no access_token")
	}
	if parsed.ExpiresIn <= 0 {
		// OpenAI documents at most an hour and always sends expires_in. A
		// missing value is left for the sign-in to treat as short-lived rather
		// than as forever.
		return parsed.AccessToken, time.Time{}, nil
	}
	return parsed.AccessToken, time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second), nil
}

// openaiBearerMiddleware sets Authorization from the bearer source on every
// request the SDK makes.
//
// PER REQUEST, not once at construction, and that is the whole reason this is
// a middleware rather than option.WithAPIKey: a bearer expires within the hour
// and a header fixed at construction would serve a stale one for the rest of
// the process's life.
//
// A 401 DOES NOT DROP THE BEARER, unlike Anthropic's middleware. OpenAI answers
// 401 for more than a bearer it no longer accepts: a service account without
// one endpoint's permission gets a 401 there and is served everywhere else
// (its error-code reference lists "Invalid Authentication" for exactly that).
// The bearer is the whole process's, so dropping it on any 401 let one such
// request sign every OpenAI consumer on the node out. It is kept until it
// expires, at most an hour, which is what this did before the shared sign-in.
// TestOpenAIKeepsItsBearerThroughA401.
//
// A SERVED CALL IS PROOF. The source is told when the vendor serves a request
// made with its bearer: that is the only proof a bearer bought by a renewal
// ever gets, and what `memql provider-auth check` reports as verified.
func openaiBearerMiddleware(src BearerSource) option.Middleware {
	prover, _ := src.(interface{ NoteServed(bearer string) })
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		token, _, err := src.Bearer(req.Context())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := next(req)
		if prover != nil && err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			prover.NoteServed(token)
		}
		return resp, err
	}
}

// openaiCredential decides how one OpenAI client authenticates and returns the
// request options that do it, the bearer source behind them, and which path it
// chose.
//
// The three-way switch is D2 of the design, and the middle case is the one
// worth defending. A half-configured federation is a MISCONFIGURATION, and the
// alternative -- carrying on with whatever else is around -- is exactly the
// failure this change exists to remove. There is no key to fall back to any
// more, so the only other option would be to treat "one id set" as "no ids
// set", which would silently disable OpenAI on a cluster whose operator is
// halfway through the runbook and believes it is being enabled.
//
// httpClient is the guarded client; it is attached on every branch, federation
// included, because the exchange rides the same client and the exchange
// observer lives on that transport.
func openaiCredential(cfg ProviderConfig, httpClient *http.Client) ([]option.RequestOption, BearerSource, credentialPath, error) {
	fed := openaiFederationFrom(cfg.Auth)
	present, missing := fed.present(), fed.missing()

	switch {
	case !fed.anyIdSet():
		// No id. NOT AN ERROR: nobody set OpenAI up on this cluster. The token
		// file's path is set by the Deployment on every pod and is not a
		// configuration on its own. The provider registers as unavailable and
		// the readiness model reports `ai` unconfigured.
		return nil, nil, credentialPathUnavailable, nil

	case len(missing) == 0:
		if err := preflightIdentityToken(fed.TokenFile, openaiAudience, envOpenAIIdentityTokenFile); err != nil {
			return nil, nil, "", fmt.Errorf("provider %q openai federation preflight: %w", cfg.Name, err)
		}
		// ONE SIGN-IN FOR EVERY OPENAI CONSUMER IN THIS PROCESS (design record
		// 2026-11-21-vendor-accounts-and-api-keys, A1): chat, streaming,
		// speech, transcription, embeddings and the transcription socket all
		// receive this same source. The exchange rides a guarded client of the
		// sign-in's own, never the calling client's, so a streaming client's
		// timeouts do not apply to it and the exchange observer still sees it.
		// TestOpenAIClientsAndTranscriptionShareOneSignIn.
		source := processVendorSignIn(openaiSignInConfig(fed, nil))
		opts := []option.RequestOption{
			option.WithHTTPClient(httpClient),
			option.WithMiddleware(openaiBearerMiddleware(source)),
			// AMBIENT CREDENTIALS ARE CLEARED, and this is load-bearing.
			// openai.NewClient prepends DefaultClientOptions, which reads
			// OPENAI_API_KEY out of the process environment and turns it into a
			// persistent Authorization header. The middleware would then
			// OVERWRITE that header per request, so the key would not actually
			// be used -- but a header set at config time is also what
			// determines whether other credential machinery engages, and
			// leaving a stray key in the request config means a future SDK
			// version could act on it while every log line here still says
			// "federation". Deleting it costs nothing: the middleware sets
			// Authorization on every request that goes out.
			option.WithHeaderDel("Authorization"),
		}
		return opts, source, credentialPathFederation, nil

	default:
		// An id set and something missing: refuse. Naming both halves is
		// deliberate -- the operator is mid-runbook and needs to know which of
		// the three they have, not only which they lack.
		return nil, nil, "", fmt.Errorf(
			"provider %q is HALF-CONFIGURED for OpenAI workload identity federation: %s set, %s missing. "+
				"Set all three, or clear the ids to leave the OpenAI providers unavailable. A partial federation "+
				"config is refused rather than quietly read as 'not configured', because an operator halfway "+
				"through the runbook believes they are ENABLING OpenAI, not disabling it. "+
				"Runbook: docs/public/operate/auth/openai-federation.md",
			cfg.Name, strings.Join(present, ", "), strings.Join(missing, ", "))
	}
}

// OpenAIBearer returns a bearer source for this cluster's OpenAI federation,
// or false when OpenAI is not federated here.
//
// It exists for the ONE consumer that cannot take an SDK client: the Realtime
// transcription WebSocket, which dials with an Authorization header of its own
// (app/integrations_stt.go wires it).
//
// THE SOURCE FOLLOWS A RELOAD. The transcription path takes it once, when the
// node is wired, and keeps it for the life of the process. It used to be the
// sign-in object of that moment, so after an operator corrected an id and
// pressed Apply every other OpenAI consumer moved to a sign-in under the new
// ids and transcription stayed on the old one until the node restarted: a
// second sign-in in the process (the one hole in "one sign-in per vendor"),
// under ids somebody had just replaced. The source now asks the registry for
// the current sign-in each time a bearer is wanted, and says so when OpenAI is
// no longer set up. TestTheTranscriptionBearerFollowsAnApply.
//
// It resolves from a registered OpenAI provider's auth map rather than reading
// the environment, so it goes through the same three-tier resolution
// (globalSecret -> globalVariable -> env) as every other credential and sees
// ids an operator applied since boot. Reading os.Getenv here instead would
// have given the transcription path a DIFFERENT answer from every other OpenAI
// consumer on the same node, which is the kind of split that presents as
// "transcription is broken" long after the change that caused it.
func (e *MemQLEngine) OpenAIBearer() (BearerSource, bool) {
	if _, ok := e.openaiSignIn(); !ok {
		return nil, false
	}
	return openaiEngineBearer{engine: e}, true
}

// openaiEngineBearer is the bearer source OpenAIBearer hands out: the engine's
// CURRENT OpenAI sign-in, looked up on every call.
type openaiEngineBearer struct {
	engine *MemQLEngine
}

func (b openaiEngineBearer) Bearer(ctx context.Context) (string, time.Time, error) {
	signIn, ok := b.engine.openaiSignIn()
	if !ok {
		return "", time.Time{}, fmt.Errorf("OpenAI is not set up on this node: its federation ids are not set, " +
			"or were removed since this consumer started. Runbook: docs/public/operate/auth/openai-federation.md")
	}
	return signIn.Bearer(ctx)
}

// openaiSignIn returns the process's sign-in for the cluster's OpenAI account,
// from the ids the registry's OpenAI records carry now.
func (e *MemQLEngine) openaiSignIn() (*vendorSignIn, bool) {
	if e == nil || e.providers == nil {
		return nil, false
	}
	for _, name := range e.providers.Names() {
		entry, ok := e.providers.Entry(name)
		if !ok || entry == nil || !strings.HasPrefix(strings.ToLower(entry.Config.Type), "openai") {
			continue
		}
		fed := openaiFederationFrom(entry.Config.Auth)
		if len(fed.missing()) != 0 {
			continue
		}
		// preflight is deliberately NOT re-run here: the provider that carried
		// these ids was constructed at boot, which is where a structurally
		// wrong token is refused. Repeating it would make a transcription
		// session fail for a reason the provider registry already reported.
		//
		// The process's shared sign-in, never one of its own: this used to
		// build a new exchanger on every call, which was one more sign-in with
		// the pod's identity token for each transcription path.
		return processVendorSignIn(openaiSignInConfig(fed, nil)), true
	}
	return nil, false
}
