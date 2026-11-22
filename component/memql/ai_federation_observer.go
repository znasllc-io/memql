package memql

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/config"

	"github.com/znasllc-io/memql/component/metrics"
)

// The federation-exchange observer (memql#4335).
//
// One call in the whole engine has this shape: it spends no tokens, it is not
// an LLM request, and when it stops working every Claude provider in the mesh
// stops working with it -- but not immediately. The SDK holds a valid bearer
// for up to an hour, so a rule that was deleted, a service account that was
// disabled, or a projected token whose audience drifted keeps serving traffic
// right up until the last good token expires. The failure is therefore
// invisible at the moment it is caused and arrives, un-attributable, up to an
// hour later.
//
// That is what this observer exists for, and it is why it is a counter plus a
// log line carrying Anthropic's OWN error body rather than a wrapped message:
// the body names the reason the Console's authentication-events tab will show
// (`match_subject_prefix`, `workspace_id_required`, ...), and an operator who
// has that string can act without reading any of our code.

// federationTokenPath is the SDK's token endpoint, taken from the SDK's own
// exported constant rather than re-typed. If Anthropic moves it, the SDK bump
// moves this with it instead of leaving a matcher that silently matches
// nothing (which would read as "no exchanges are happening").
const federationTokenPath = config.TokenEndpoint

// openaiFederationTokenPath is OpenAI's RFC 8693 exchange path. Unlike
// Anthropic's it is not exported by an SDK -- the engine performs this exchange
// itself (design D3) -- so it is written here beside the matcher that reads it.
const openaiFederationTokenPath = "/oauth/token"

// federationExchangeVendor reports which vendor's token exchange a request is,
// or "" when it is not one.
//
// Matched on METHOD + PATH, and deliberately not on host. The host at the
// moment of the check is whatever base URL the client was built with, which in
// tests is a loopback httptest server; pinning api.anthropic.com or
// auth.openai.com would make the observer untestable and would silently stop
// observing anything against a base-URL override.
//
// THE ORDER OF THESE TWO CHECKS IS LOAD-BEARING. Anthropic's path is
// /v1/oauth/token and OpenAI's is /oauth/token, so the OpenAI suffix matches
// BOTH. Testing Anthropic's longer, more specific path first is what keeps
// every Anthropic exchange from being attributed to OpenAI -- which would not
// fail anything, it would just quietly move one vendor's entire rate onto the
// other's series and leave a permanent zero where a real number belongs.
func federationExchangeVendor(method, urlPath string) string {
	if !strings.EqualFold(method, http.MethodPost) {
		return ""
	}
	trimmed := strings.TrimSuffix(urlPath, "/")
	switch {
	case strings.HasSuffix(trimmed, federationTokenPath):
		return metrics.FederationVendorAnthropic
	case strings.HasSuffix(trimmed, openaiFederationTokenPath):
		return metrics.FederationVendorOpenAI
	default:
		return ""
	}
}

// isFederationExchange reports whether a request is a federation token
// exchange for either vendor.
func isFederationExchange(method, urlPath string) bool {
	return federationExchangeVendor(method, urlPath) != ""
}

// federationExchangeRecord is the last exchange this process observed. It
// exists for `memql provider-auth check`, which needs to report the token's
// expiry -- a fact only the exchange response carries, and one the SDK keeps
// in an internal cache it exposes no reader for. The observer is already
// reading that response, so recording it here costs nothing and avoids
// re-implementing the exchange to learn something the SDK just learned.
type federationExchangeRecord struct {
	At time.Time
	// Vendor is which vendor's exchange this was -- "anthropic" or "openai".
	// With two federating vendors, a record that does not say which one it
	// describes is a record `provider-auth check --provider openai` could
	// answer with Anthropic's last exchange.
	Vendor    string
	Outcome   string
	Status    int
	ExpiresIn time.Duration
	// ExpiresAt is At+ExpiresIn, precomputed so a caller printing it does not
	// have to know whether ExpiresIn was present.
	ExpiresAt time.Time
	// Detail carries Anthropic's error body on a denial, truncated. Empty on
	// success -- a successful body holds the bearer token and must not be
	// retained anywhere.
	Detail string
}

var (
	lastFederationExchangeMu sync.RWMutex
	lastFederationExchange   *federationExchangeRecord
)

// LastFederationExchange returns a copy of the most recent federation
// exchange this process observed, or nil if there has not been one.
func LastFederationExchange() *federationExchangeRecord {
	lastFederationExchangeMu.RLock()
	defer lastFederationExchangeMu.RUnlock()
	if lastFederationExchange == nil {
		return nil
	}
	cp := *lastFederationExchange
	return &cp
}

func recordFederationExchange(rec federationExchangeRecord) {
	lastFederationExchangeMu.Lock()
	lastFederationExchange = &rec
	lastFederationExchangeMu.Unlock()
}

// maxFederationBodyNote bounds what a denial puts into a log line. Anthropic's
// error bodies are a sentence; anything much larger is not a reason and does
// not belong in a log at request rate.
const maxFederationBodyNote = 512

// observeFederationExchange performs the exchange over base and records its
// outcome.
//
// It never changes the outcome: an error is returned as-is and a response is
// handed back with its body intact (re-wrapped over a buffer, since reading it
// consumes it). Observation that could break the thing it observes would be a
// bad trade here -- this call is the cluster's access to Claude.
func observeFederationExchange(base http.RoundTripper, req *http.Request) (*http.Response, error) {
	started := time.Now()
	vendor := federationExchangeVendor(req.Method, req.URL.Path)
	resp, err := base.RoundTrip(req)
	if err != nil {
		metrics.AIFederationExchange(vendor, metrics.FederationExchangeError)
		recordFederationExchange(federationExchangeRecord{
			Vendor:  vendor,
			At:      started,
			Outcome: metrics.FederationExchangeError,
			Detail:  err.Error(),
		})
		slog.Warn("federation: token exchange did not complete",
			"vendor", vendor,
			"endpoint", req.URL.Redacted(),
			"err", err,
			"runbook", federationRunbookFor(vendor))
		return nil, err
	}

	// net/http guarantees a non-nil Body from a real Transport, but this
	// wraps whatever RoundTripper it was given -- and a nil here would panic
	// on the credential path, taking the process down over an observation.
	// Hand the response back unread rather than that.
	if resp.Body == nil {
		metrics.AIFederationExchange(vendor, metrics.FederationExchangeError)
		recordFederationExchange(federationExchangeRecord{
			Vendor:  vendor,
			At:      started,
			Outcome: metrics.FederationExchangeError,
			Status:  resp.StatusCode,
			Detail:  "response carried no body",
		})
		return resp, nil
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		// The body is gone and cannot be handed on; report it as the transport
		// failure it now is rather than returning a response with no body.
		metrics.AIFederationExchange(vendor, metrics.FederationExchangeError)
		recordFederationExchange(federationExchangeRecord{
			Vendor:  vendor,
			At:      started,
			Outcome: metrics.FederationExchangeError,
			Status:  resp.StatusCode,
			Detail:  readErr.Error(),
		})
		slog.Warn("federation: could not read the token exchange response",
			"vendor", vendor,
			"status", resp.StatusCode, "err", readErr)
		return nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	rec := federationExchangeRecord{Vendor: vendor, At: started, Status: resp.StatusCode}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		rec.Outcome = metrics.FederationExchangeOK
		// expires_in is the ONLY field read out of a successful body. The
		// access token is in there too and is never recorded, logged or
		// returned by LastFederationExchange -- a short-lived bearer is still
		// a bearer.
		var parsed struct {
			ExpiresIn *int `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.ExpiresIn != nil {
			rec.ExpiresIn = time.Duration(*parsed.ExpiresIn) * time.Second
			rec.ExpiresAt = started.Add(rec.ExpiresIn)
		}
	case signInStatusRefuses(resp.StatusCode):
		// DENIED: the vendor understood the assertion and refused it. This is a
		// configuration answer -- the rule, the subject prefix, the audience,
		// the workspace -- and its reason is in the body. 408, 425 and 429 are
		// NOT this (signInStatusRefuses): they fall to the fault arm below, so
		// a rate limit does not tick the counter an operator alerts on.
		rec.Outcome = metrics.FederationExchangeDenied
		rec.Detail = truncateForLog(string(body), maxFederationBodyNote)
		slog.Warn("federation: token exchange DENIED -- the cluster is running on a credential the vendor will not renew",
			"vendor", vendor,
			"status", resp.StatusCode,
			"endpoint", req.URL.Redacted(),
			"vendorError", rec.Detail,
			"requestId", resp.Header.Get("Request-Id"),
			"hint", "the same reason appears in that vendor's console -- Anthropic: Workload identity -> authentication events; OpenAI: the identity provider's activity",
			"runbook", federationRunbookFor(vendor))
	default:
		rec.Outcome = metrics.FederationExchangeError
		rec.Detail = truncateForLog(string(body), maxFederationBodyNote)
		slog.Warn("federation: token exchange faulted",
			"vendor", vendor,
			"status", resp.StatusCode,
			"endpoint", req.URL.Redacted(),
			"body", rec.Detail)
	}
	metrics.AIFederationExchange(vendor, rec.Outcome)
	recordFederationExchange(rec)
	return resp, nil
}

func truncateForLog(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// federationRunbookFor names the runbook for the vendor whose exchange failed.
// A log line that sends an operator to the wrong vendor's runbook is worse
// than one that sends them to none.
func federationRunbookFor(vendor string) string {
	if vendor == metrics.FederationVendorOpenAI {
		return "docs/public/operate/auth/openai-federation.md"
	}
	return "docs/public/operate/auth/anthropic-federation.md"
}
