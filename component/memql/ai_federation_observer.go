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

// isFederationExchange reports whether a request is the federation token
// exchange.
//
// Matched on METHOD + PATH, and deliberately not on host. The host at the
// moment of the check is whatever base URL the client was built with, which
// in tests is a loopback httptest server; pinning api.anthropic.com would
// make the observer untestable and would silently stop observing anything
// against a base-URL override. No other call in the engine posts to
// /v1/oauth/token, so the path alone is unambiguous.
func isFederationExchange(method, urlPath string) bool {
	return strings.EqualFold(method, http.MethodPost) &&
		strings.HasSuffix(strings.TrimSuffix(urlPath, "/"), federationTokenPath)
}

// federationExchangeRecord is the last exchange this process observed. It
// exists for `memql provider-auth check`, which needs to report the token's
// expiry -- a fact only the exchange response carries, and one the SDK keeps
// in an internal cache it exposes no reader for. The observer is already
// reading that response, so recording it here costs nothing and avoids
// re-implementing the exchange to learn something the SDK just learned.
type federationExchangeRecord struct {
	At        time.Time
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
	resp, err := base.RoundTrip(req)
	if err != nil {
		metrics.AIFederationExchange(metrics.FederationExchangeError)
		recordFederationExchange(federationExchangeRecord{
			At:      started,
			Outcome: metrics.FederationExchangeError,
			Detail:  err.Error(),
		})
		slog.Warn("anthropic federation: token exchange did not complete",
			"endpoint", req.URL.Redacted(),
			"err", err,
			"runbook", "docs/public/operate/auth/anthropic-federation.md")
		return nil, err
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		// The body is gone and cannot be handed on; report it as the transport
		// failure it now is rather than returning a response with no body.
		metrics.AIFederationExchange(metrics.FederationExchangeError)
		recordFederationExchange(federationExchangeRecord{
			At:      started,
			Outcome: metrics.FederationExchangeError,
			Status:  resp.StatusCode,
			Detail:  readErr.Error(),
		})
		slog.Warn("anthropic federation: could not read the token exchange response",
			"status", resp.StatusCode, "err", readErr)
		return nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	rec := federationExchangeRecord{At: started, Status: resp.StatusCode}
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
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// DENIED: Anthropic understood the assertion and refused it. This is a
		// configuration answer -- the rule, the subject prefix, the audience,
		// the workspace -- and its reason is in the body.
		rec.Outcome = metrics.FederationExchangeDenied
		rec.Detail = truncateForLog(string(body), maxFederationBodyNote)
		slog.Warn("anthropic federation: token exchange DENIED -- the cluster is running on a credential Anthropic will not renew",
			"status", resp.StatusCode,
			"endpoint", req.URL.Redacted(),
			"anthropicError", rec.Detail,
			"requestId", resp.Header.Get("Request-Id"),
			"hint", "the same reason appears in the Console's Workload identity -> authentication events tab",
			"runbook", "docs/public/operate/auth/anthropic-federation.md")
	default:
		rec.Outcome = metrics.FederationExchangeError
		rec.Detail = truncateForLog(string(body), maxFederationBodyNote)
		slog.Warn("anthropic federation: token exchange faulted",
			"status", resp.StatusCode,
			"endpoint", req.URL.Redacted(),
			"body", rec.Detail)
	}
	metrics.AIFederationExchange(rec.Outcome)
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
