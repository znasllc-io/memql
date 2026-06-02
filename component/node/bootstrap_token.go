package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/httptls"
)

// bootstrap_token.go implements the node-side companion to the
// identity service's `POST /node/bootstrap` endpoint (memql#338).
// When a cluster binary (bff / agent / cognition / planner / voice
// / etc.) starts up without `MEMQL_NODE_TOKEN` set, this code path
// checks whether the operator has opted in to self-bootstrap by
// setting the matching shared secret
// (`MEMQL_NODE_BOOTSTRAP_TOKEN`) plus the identity-service base URL
// (`IDENTITY_VERIFIER_BASE_URL`); if so, it hits `/node/bootstrap`
// with the secret and uses the minted JWT for outbound
// NodeService.Stream calls.
//
// Why this exists: without it, the docker-compose dev stack came up
// with zero node tokens wired and every peer call failed with
// "authorization header missing" -- bff <-> cognition / agent /
// planner / voice all broken. See #338 for the full trace.
//
// What this is NOT: a production credential-issuance flow. The
// bootstrap secret is a single shared key the operator opts into
// for dev convenience; production deploys should mint per-node
// tokens out-of-band via the identity admin CLI and inject them via
// `MEMQL_NODE_TOKEN` directly. The bootstrap path stays disabled
// (returns 503) when the secret isn't configured on the identity
// service, so leaving the env var unset in production is the safe
// default.
//
// Distinct from bootstrap.go in this same package -- that one
// orchestrates peer / event-bus / engine bring-up at process start.
// This file is auth-token-only.

// nodeBootstrapHTTPTimeout caps how long a node will wait on a SINGLE
// mint attempt before giving up. Generous so slow-start identity in
// compose doesn't trigger a false negative, but bounded so a
// misconfigured identity endpoint doesn't block a single attempt forever.
const nodeBootstrapHTTPTimeout = 30 * time.Second

// Retry budget for the startup mint (memql#542). The original code minted
// ONCE at startup with no retry: rolling all node deployments at once meant
// any worker that restarted while identity was briefly down came up with no
// token and stayed broken until manually re-rolled. We now retry transient
// failures (identity unreachable / still starting / 5xx) on an exponential
// backoff until a token is obtained or the budget is exhausted. Permanent
// rejections (wrong secret, bad request -- 4xx) are NOT retried.
//
// The budget defaults generously enough to cover an identity pod restart
// during a simultaneous roll, and is overridable via env for ops + tests.
const (
	defaultNodeBootstrapRetryBudget = 90 * time.Second
	nodeBootstrapRetryBaseBackoff   = 2 * time.Second
	nodeBootstrapRetryMaxBackoff    = 15 * time.Second
	nodeBootstrapRetryBudgetEnv     = "MEMQL_NODE_BOOTSTRAP_RETRY_BUDGET"
)

// envAllowInsecureNodeBootstrap is the node-side companion to the
// identity service's MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP escape
// hatch (component/identity/http/node_bootstrap.go). The bootstrap
// POST must be https end-to-end (memql#626): the identity handler
// rejects plaintext with `403 insecure_transport`, so a node pointed
// at an http:// IDENTITY_VERIFIER_BASE_URL would fire a doomed request
// the server bounces. We enforce the same invariant client-side so the
// misconfig fails fast and loud at the node instead. Set to "1" to
// allow plaintext in dev (the docker-compose stack already sets the
// matching server-side hatch); staging/prod (k8s) leave both unset, so
// http there is a hard, non-retryable failure -- which is the
// regression this guard exists to catch.
const envAllowInsecureNodeBootstrap = "MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP"

// nodeBootstrapRetryBudget resolves the overall retry window from
// MEMQL_NODE_BOOTSTRAP_RETRY_BUDGET (a Go duration string, e.g. "90s",
// "2m"), falling back to the default. A budget of 0 means "single
// attempt, no retry" (handy for tests + opt-out).
func nodeBootstrapRetryBudget() time.Duration {
	if v := strings.TrimSpace(os.Getenv(nodeBootstrapRetryBudgetEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultNodeBootstrapRetryBudget
}

// bootstrapTokenResponse mirrors identity/http/node_bootstrap.go's
// NodeBootstrapResponse. Defined locally to avoid a node ->
// identity/http package cycle (identity-side HTTP types live in
// the identity binary; nodes consume them via JSON over the wire).
type bootstrapTokenResponse struct {
	Success    bool   `json:"success"`
	PlainToken string `json:"plainToken,omitempty"`
	IdentityId string `json:"identityId,omitempty"`
	NodeType   string `json:"nodeType,omitempty"`
	NodeId     string `json:"nodeId,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty"`
}

// maybeBootstrapNodeToken returns a minted node JWT when:
//
//  1. The local MEMQL_NODE_TOKEN env var is empty (otherwise the
//     operator has explicitly provisioned a token out-of-band; we
//     never override).
//  2. MEMQL_NODE_BOOTSTRAP_TOKEN is set (the shared secret the
//     identity service validates against).
//  3. IDENTITY_VERIFIER_BASE_URL is set + reachable (we need
//     somewhere to POST).
//
// When any precondition is missing, returns ("", false, nil) and
// the caller proceeds with an empty BearerToken (the legacy
// behaviour). When all preconditions are present but the bootstrap
// call fails, returns ("", false, error) so the caller can decide
// whether to block startup (the production-grade choice) or warn +
// proceed (the lenient dev choice).
//
// The split between "no attempt made" and "attempt failed" lets the
// caller emit clearer logs: a missing precondition is normal config
// state, a failed attempt is a problem worth surfacing.
func maybeBootstrapNodeToken(ctx context.Context, logger *slog.Logger, nodeId, nodeType string) (string, bool, error) {
	if strings.TrimSpace(os.Getenv("MEMQL_NODE_TOKEN")) != "" {
		// Operator-provisioned token wins; no bootstrap needed.
		return "", false, nil
	}
	secret := strings.TrimSpace(os.Getenv("MEMQL_NODE_BOOTSTRAP_TOKEN"))
	if secret == "" {
		// Bootstrap not opted into.
		return "", false, nil
	}
	identityBase := strings.TrimRight(strings.TrimSpace(os.Getenv("IDENTITY_VERIFIER_BASE_URL")), "/")
	if identityBase == "" {
		return "", false, fmt.Errorf("node bootstrap requested (MEMQL_NODE_BOOTSTRAP_TOKEN set) but IDENTITY_VERIFIER_BASE_URL is empty -- cannot reach identity service")
	}

	token, err := bootstrapWithRetry(ctx, logger, identityBase, secret, nodeId, nodeType,
		nodeBootstrapRetryBudget(), nodeBootstrapRetryBaseBackoff)
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}

// bootstrapWithRetry drives attemptBootstrapMint on an exponential backoff
// until it succeeds, hits a permanent (non-retryable) rejection, the context
// is cancelled, or the overall budget is exhausted. A budget of 0 collapses
// to a single attempt. Permanent rejections (wrong secret / bad request)
// short-circuit immediately so a misconfig surfaces fast instead of
// spinning for the whole budget.
func bootstrapWithRetry(ctx context.Context, logger *slog.Logger, identityBase, secret, nodeId, nodeType string, budget, baseBackoff time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	backoff := baseBackoff
	attempt := 0
	var lastErr error

	for {
		attempt++
		token, retryable, err := attemptBootstrapMint(ctx, logger, identityBase, secret, nodeId, nodeType)
		if err == nil {
			if attempt > 1 && logger != nil {
				logger.Info("node bootstrap: minted node-class JWT after retry",
					"attempts", attempt, "node_type", nodeType, "node_id", nodeId)
			}
			return token, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := backoff
		if sleep > remaining {
			sleep = remaining
		}
		if logger != nil {
			logger.Warn("node bootstrap: attempt failed, retrying",
				"attempt", attempt, "backoff", sleep.String(),
				"node_type", nodeType, "node_id", nodeId, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("node bootstrap: context cancelled after %d attempt(s): %w", attempt, ctx.Err())
		case <-time.After(sleep):
		}
		if backoff *= 2; backoff > nodeBootstrapRetryMaxBackoff {
			backoff = nodeBootstrapRetryMaxBackoff
		}
	}

	return "", fmt.Errorf("node bootstrap: giving up after %d attempt(s) over %s: %w", attempt, budget, lastErr)
}

// attemptBootstrapMint performs ONE POST /node/bootstrap and returns the
// minted token, a retryable flag, and an error. retryable is true for
// transient failures worth a retry (identity unreachable, response-read
// failure, non-JSON proxy body, or a 5xx) and false for permanent
// rejections (a structured 4xx like wrong-secret/bad-request, or a 200
// with an empty token -- none of which a retry would fix).
func attemptBootstrapMint(ctx context.Context, logger *slog.Logger, identityBase, secret, nodeId, nodeType string) (string, bool, error) {
	endpoint, err := url.Parse(identityBase + "/node/bootstrap")
	if err != nil {
		// Misformed base URL won't fix itself -- permanent.
		return "", false, fmt.Errorf("node bootstrap: parse identity base url %q: %w", identityBase, err)
	}

	// HTTPS-only by default (memql#626). Refuse a plaintext
	// IDENTITY_VERIFIER_BASE_URL unless the operator opts into the dev
	// escape hatch, mirroring the identity service's own `403
	// insecure_transport` guard. This is a permanent (non-retryable)
	// failure -- a retry won't turn http into https, and we'd rather
	// surface the misconfig immediately than burn the retry budget on a
	// POST identity will bounce.
	if !strings.EqualFold(endpoint.Scheme, "https") &&
		strings.TrimSpace(os.Getenv(envAllowInsecureNodeBootstrap)) != "1" {
		return "", false, fmt.Errorf(
			"node bootstrap: refusing insecure %q scheme for IDENTITY_VERIFIER_BASE_URL %q -- "+
				"the bootstrap call must be https end-to-end (set %s=1 to allow plaintext in dev)",
			endpoint.Scheme, identityBase, envAllowInsecureNodeBootstrap)
	}

	payload, err := json.Marshal(map[string]string{
		"nodeId":   nodeId,
		"nodeType": nodeType,
	})
	if err != nil {
		return "", false, fmt.Errorf("node bootstrap: marshal request body: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, nodeBootstrapHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dialCtx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("node bootstrap: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bootstrap "+secret)

	if logger != nil {
		logger.Info("node bootstrap: requesting node-class JWT from identity",
			"endpoint", endpoint.String(),
			"node_type", nodeType,
			"node_id", nodeId,
		)
	}

	// Trust the system roots plus the internal CA (MEMQL_HTTP_TLS_CA_FILE)
	// when set, so the bootstrap POST works against an identity service
	// that serves https with a self-signed cluster CA. The request
	// context (dialCtx) bounds the call; the client itself is unbounded.
	client, err := httptls.Client(0, logger)
	if err != nil {
		return "", false, fmt.Errorf("node bootstrap: build http client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level failure (connection refused, dial timeout,
		// TLS handshake) -- exactly the "identity briefly down" case
		// we want to retry through.
		return "", true, fmt.Errorf("node bootstrap: POST %s: %w", endpoint.String(), err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if readErr != nil {
		return "", true, fmt.Errorf("node bootstrap: read response (status %d): %w", resp.StatusCode, readErr)
	}

	var decoded bootstrapTokenResponse
	if jsonErr := json.Unmarshal(body, &decoded); jsonErr != nil {
		// A non-JSON body usually means an LB/proxy 502/503 HTML page
		// while identity is still coming up -- retryable.
		return "", true, fmt.Errorf("node bootstrap: identity returned %d with non-JSON body (%d bytes): %s",
			resp.StatusCode, len(body), strings.TrimSpace(string(body)))
	}

	if resp.StatusCode != http.StatusOK || !decoded.Success {
		// Surface the structured error code so operators can
		// distinguish "endpoint disabled" / "wrong secret" / "bad
		// request" / etc. without grepping identity logs. A 5xx is
		// transient (identity still starting); a 4xx is a permanent
		// config error a retry won't fix.
		errCode := decoded.ErrorCode
		if errCode == "" {
			errCode = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		retryable := resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("node bootstrap: identity rejected request (status=%d errorCode=%s): %s",
			resp.StatusCode, errCode, strings.TrimSpace(decoded.Error))
	}

	if strings.TrimSpace(decoded.PlainToken) == "" {
		return "", false, fmt.Errorf("node bootstrap: identity returned 200 success but empty plainToken")
	}

	if logger != nil {
		logger.Info("node bootstrap: minted node-class JWT",
			"identity_id", decoded.IdentityId,
			"expires_at", decoded.ExpiresAt,
			"node_type", nodeType,
			"node_id", nodeId,
		)
	}

	return decoded.PlainToken, false, nil
}
