package memql

// How one account's credential reaches one request (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K3, K10, K11 and safety rules S2, S4, S5).
//
// ONE SET OF CLIENTS PER TEXT RECORD. A vendor's text provider keeps what it
// takes to build its SDK client for an account (openaiTextClients,
// anthropicTextClients): the cluster's client, built once, and a client per
// key binding, built for the call that uses it. They differ in one thing, the
// middleware that puts the credential on the request, so a call paid by a key
// takes the same guarded transport, the same circuit breaker and the same
// retry policy as any other.
//
//   - THE CLUSTER'S ACCOUNT is the process's one sign-in, exactly as part 1
//     left it (ai_vendor_signin.go). A record built on a cluster with no
//     account at its vendor still builds: its cluster client refuses every
//     request before it is sent, and a key can pay through the same record.
//   - A KEY is bound to a copy of the provider for one call.
//
// THE KEY PATH, IN ORDER, AND WHY THE ORDER:
//
//  1. THE ADDRESS IS CHECKED BEFORE THE KEY IS UNSEALED. A key is sent to the
//     vendor's own address and nowhere else (K3). A cluster can point a
//     vendor's clients at another address, and a key sent there is a key given
//     away, so the request is refused before any value exists in memory.
//  2. THE VALUE IS UNSEALED FOR THIS REQUEST and set on its header. Nothing
//     keeps it: not this middleware, not the binding, not the provider (S2).
//  3. THE ANSWER IS READ ONCE. On a refusal the body is scrubbed of anything
//     shaped like a key BEFORE the SDK builds its error from it, because
//     Anthropic's SDK puts the whole body in the error's text and OpenAI
//     echoes part of a rejected key (S4). Everything downstream of the SDK
//     (the router's ledger row, a log line, a person's screen) therefore sees
//     scrubbed words and no code after this has to remember to scrub.
//  4. WHAT THE ANSWER SAYS ABOUT THE KEY IS RECORDED (K11): a served call
//     marks it good, a refusal marks it refused, an exhausted balance holds
//     it. A rate limit or an outage says nothing about the key, and neither
//     does a missing permission: the key is good and may not do this.
//
// Regression tests: vendor_account_test.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
)

// vendorKeyOrigins is where a person's API key may be sent. A variable only so
// a test can stand a vendor in; nothing in the product changes it.
var vendorKeyOrigins = map[string]string{
	VendorAnthropic: "https://api.anthropic.com",
	VendorOpenAI:    "https://api.openai.com",
}

// maxAccountAnswerBody bounds how much of a refusal's body is read to scrub
// and classify it. A vendor's error body is a few hundred bytes.
const maxAccountAnswerBody = 64 << 10

// keyGoodStampEvery is how often one process reports a key as good. Every
// served call is evidence, and one stamp an hour is all the state needs.
const keyGoodStampEvery = time.Hour

// keyObserveTimeout bounds one report about a key.
const keyObserveTimeout = 10 * time.Second

// accountNext sends the request on. It is both SDKs' MiddlewareNext.
type accountNext func(*http.Request) (*http.Response, error)

func requestOrigin(req *http.Request) string {
	return req.URL.Scheme + "://" + req.URL.Host
}

// vendorAccountBinding is one key bound to one provider for its calls.
type vendorAccountBinding struct {
	account VendorAccount
	keys    VendorKeys
}

// keyGoodStamps is when this process last reported each key as good.
var keyGoodStamps sync.Map // key id -> time.Time

// roundTrip sends one request paid by the bound key.
func (b *vendorAccountBinding) roundTrip(vendor string, req *http.Request, next accountNext) (*http.Response, error) {
	name := vendorDisplayName(vendor)
	if !strings.EqualFold(requestOrigin(req), vendorKeyOrigins[vendor]) {
		return nil, &VendorAccountError{Vendor: vendor, Kind: accountFailureUnavailable, Reason: fmt.Sprintf(
			"this cluster sends %s calls to another address, and an API key is only ever sent to %s's own", name, name)}
	}
	value, err := b.keys.Secret(req.Context(), b.account)
	if err != nil {
		// The seam's error names the key's row at most, never its value; it is
		// scrubbed all the same before it becomes somebody's error text.
		return nil, &VendorAccountError{Vendor: vendor, Kind: accountFailureUnavailable,
			Reason: "that API key cannot be used: " + scrubKeyShapes(err.Error())}
	}

	switch vendor {
	case VendorAnthropic:
		req.Header.Del("Authorization")
		req.Header.Set("X-Api-Key", value)
	case VendorOpenAI:
		req.Header.Set("Authorization", "Bearer "+value)
		// The cluster's own project and organization belong to the cluster's
		// account. Sent beside a person's key they name somebody else's project.
		req.Header.Del("OpenAI-Project")
		req.Header.Del("OpenAI-Organization")
	}

	resp, err := next(req)
	if err != nil || resp == nil {
		// No answer is not a verdict on the key.
		return resp, err
	}
	at := time.Now()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		b.observeGood(req.Context(), at)
		return resp, nil
	}

	body := scrubAnswerBody(resp)
	switch kind, reason := classifyVendorAnswer(vendor, resp.StatusCode, body); kind {
	case accountFailureRefused:
		b.observe(req.Context(), VendorKeyRefused, at, reason)
	case accountFailureCredit:
		b.observe(req.Context(), VendorKeyCredit, at, reason)
	}
	return resp, nil
}

// scrubAnswerBody reads the head of a refusal's body, removes anything shaped
// like a key, and puts the scrubbed body back for the SDK to read. It returns
// what it put back.
func scrubAnswerBody(resp *http.Response) []byte {
	if resp.Body == nil {
		return nil
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, maxAccountAnswerBody))
	scrubbed := []byte(scrubKeyShapes(string(head)))
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(scrubbed), rest), rest}
	// The length changed with the text.
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return scrubbed
}

// observe reports a refusal or an exhausted balance, and waits for it: the
// router is about to try the next account, and the state should say why
// before it does.
func (b *vendorAccountBinding) observe(ctx context.Context, verdict VendorKeyVerdict, at time.Time, detail string) {
	keyGoodStamps.Delete(b.account.KeyId)
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyObserveTimeout)
	defer cancel()
	b.keys.Observe(reportCtx, b.account, verdict, at, detail)
}

// observeGood reports a served call, at most once an hour per key, off the
// request's path: a served call must not wait for a row to be written.
func (b *vendorAccountBinding) observeGood(ctx context.Context, at time.Time) {
	if last, ok := keyGoodStamps.Load(b.account.KeyId); ok && at.Sub(last.(time.Time)) < keyGoodStampEvery {
		return
	}
	keyGoodStamps.Store(b.account.KeyId, at)
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keyObserveTimeout)
	go func() {
		defer cancel()
		b.keys.Observe(reportCtx, b.account, VendorKeyGood, at, "")
	}()
}

// noClusterAccount is the refusal for a call that carries no key on a cluster
// with no account at the vendor.
func noClusterAccount(vendor string) error {
	return &VendorAccountError{Vendor: vendor, Kind: accountFailureUnavailable,
		Reason: fmt.Sprintf("the cluster has no account at %s", vendorDisplayName(vendor))}
}

// errOriginProbe ends the dry run that learns which address a client dials.
var errOriginProbe = errors.New("vendor origin probe: no request is sent")

// ---------------------------------------------------------------------------
// OpenAI
// ---------------------------------------------------------------------------

// openaiTextClients builds one OpenAI text record's SDK clients.
type openaiTextClients struct {
	cfg        ProviderConfig
	httpClient *http.Client
	// source is the cluster's sign-in. Nil when the cluster has no account at
	// OpenAI, and missing then says so in the registry's words.
	source  BearerSource
	missing error
	// origin is the address the clients dial, learned at construction.
	origin string
}

// newOpenAITextClients decides the cluster's credential for a text record and
// learns the address it dials. A half-configured federation, or an identity
// token the engine cannot use, still refuses the record: only "nobody set
// OpenAI up" builds a record a key can pay through.
func newOpenAITextClients(cfg ProviderConfig, httpClient *http.Client) (*openaiTextClients, error) {
	_, source, path, err := openaiCredential(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	k := &openaiTextClients{cfg: cfg, httpClient: httpClient, source: source}
	if path == credentialPathUnavailable {
		k.source = nil
		k.missing = openaiCredentialMissing(cfg)
	}
	k.origin = k.learnOrigin()
	return k, nil
}

func (k *openaiTextClients) options(credential openaioption.Middleware, forKey bool) []openaioption.RequestOption {
	opts := []openaioption.RequestOption{
		openaioption.WithHTTPClient(k.httpClient),
		openaioption.WithMiddleware(credential),
		// AMBIENT CREDENTIALS ARE CLEARED, for the reason openaiCredential
		// gives: the SDK reads a stray key out of the process environment into
		// a persistent header, and the middleware above is what authenticates.
		openaioption.WithHeaderDel("Authorization"),
	}
	if baseURL := strings.TrimSpace(k.cfg.Auth["baseURL"]); baseURL != "" {
		opts = append(opts, openaioption.WithBaseURL(baseURL))
	}
	// The project id names the CLUSTER'S project. A person's key belongs to
	// their own, so it is never sent beside one.
	if !forKey {
		if projectId := resolveOpenAIProjectId(k.cfg); projectId != "" {
			opts = append(opts, openaioption.WithProject(projectId))
		}
	}
	return opts
}

// client builds the SDK client for one account: the cluster's when binding is
// nil, the bound key's otherwise.
func (k *openaiTextClients) client(binding *vendorAccountBinding) *openai.Client {
	var credential openaioption.Middleware
	switch {
	case binding != nil:
		credential = func(req *http.Request, next openaioption.MiddlewareNext) (*http.Response, error) {
			return binding.roundTrip(VendorOpenAI, req, accountNext(next))
		}
	case k.source != nil:
		credential = openaiBearerMiddleware(k.source)
	default:
		credential = func(*http.Request, openaioption.MiddlewareNext) (*http.Response, error) {
			return nil, noClusterAccount(VendorOpenAI)
		}
	}
	client := openai.NewClient(k.options(credential, binding != nil)...)
	return &client
}

// learnOrigin asks the SDK where it would send a request, and sends none: the
// probe's middleware reads the address and ends the call.
func (k *openaiTextClients) learnOrigin() string {
	var origin string
	probe := func(req *http.Request, _ openaioption.MiddlewareNext) (*http.Response, error) {
		origin = requestOrigin(req)
		return nil, errOriginProbe
	}
	client := openai.NewClient(append(k.options(probe, true), openaioption.WithMaxRetries(0))...)
	_, _ = client.Models.List(context.Background())
	return origin
}

// ---------------------------------------------------------------------------
// Anthropic
// ---------------------------------------------------------------------------

// anthropicTextClients builds one Anthropic text record's SDK clients.
type anthropicTextClients struct {
	httpClient *http.Client
	// signIn is the cluster's sign-in. Nil when the cluster has no account at
	// Anthropic, and missing then says so in the registry's words.
	signIn  *vendorSignIn
	missing error
	// origin is the address the clients dial, learned at construction.
	origin string
}

// newAnthropicTextClients is newOpenAITextClients for Anthropic.
func newAnthropicTextClients(cfg ProviderConfig, httpClient *http.Client) (*anthropicTextClients, error) {
	signIn, err := anthropicSignInFor(cfg)
	if err != nil {
		return nil, err
	}
	k := &anthropicTextClients{httpClient: httpClient, signIn: signIn}
	if signIn == nil {
		k.missing = anthropicCredentialMissing(cfg)
	}
	k.origin = k.learnOrigin()
	return k, nil
}

func (k *anthropicTextClients) options(credential anthropicoption.Middleware) []anthropicoption.RequestOption {
	return []anthropicoption.RequestOption{
		anthropicoption.WithHTTPClient(k.httpClient),
		anthropicoption.WithMiddleware(credential),
		// AMBIENT CREDENTIALS ARE CLEARED, for the reason anthropicCredential
		// gives. The middleware above sets the one credential this request
		// carries, on the request itself.
		anthropicoption.WithHeaderDel("X-Api-Key"),
		anthropicoption.WithHeaderDel("Authorization"),
	}
}

// client builds the SDK client for one account: the cluster's when binding is
// nil, the bound key's otherwise.
func (k *anthropicTextClients) client(binding *vendorAccountBinding) anthropic.Client {
	var credential anthropicoption.Middleware
	switch {
	case binding != nil:
		credential = func(req *http.Request, next anthropicoption.MiddlewareNext) (*http.Response, error) {
			return binding.roundTrip(VendorAnthropic, req, accountNext(next))
		}
	case k.signIn != nil:
		credential = anthropicBearerMiddleware(k.signIn)
	default:
		credential = func(*http.Request, anthropicoption.MiddlewareNext) (*http.Response, error) {
			return nil, noClusterAccount(VendorAnthropic)
		}
	}
	return anthropic.NewClient(k.options(credential)...)
}

// learnOrigin is the OpenAI probe for Anthropic's client.
func (k *anthropicTextClients) learnOrigin() string {
	var origin string
	probe := func(req *http.Request, _ anthropicoption.MiddlewareNext) (*http.Response, error) {
		origin = requestOrigin(req)
		return nil, errOriginProbe
	}
	client := anthropic.NewClient(append(k.options(probe), anthropicoption.WithMaxRetries(0))...)
	_, _ = client.Models.List(context.Background(), anthropic.ModelListParams{})
	return origin
}
