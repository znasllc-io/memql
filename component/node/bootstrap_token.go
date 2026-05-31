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

// nodeBootstrapHTTPTimeout caps how long a node will wait for the
// identity service to mint a token before giving up. Generous so
// slow-start identity in compose doesn't trigger a false negative,
// but bounded so a misconfigured identity endpoint doesn't block
// node startup forever.
const nodeBootstrapHTTPTimeout = 30 * time.Second

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

	endpoint, err := url.Parse(identityBase + "/node/bootstrap")
	if err != nil {
		return "", false, fmt.Errorf("node bootstrap: parse identity base url %q: %w", identityBase, err)
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
		return "", false, fmt.Errorf("node bootstrap: POST %s: %w", endpoint.String(), err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if readErr != nil {
		return "", false, fmt.Errorf("node bootstrap: read response (status %d): %w", resp.StatusCode, readErr)
	}

	var decoded bootstrapTokenResponse
	if jsonErr := json.Unmarshal(body, &decoded); jsonErr != nil {
		return "", false, fmt.Errorf("node bootstrap: identity returned %d with non-JSON body (%d bytes): %s",
			resp.StatusCode, len(body), strings.TrimSpace(string(body)))
	}

	if resp.StatusCode != http.StatusOK || !decoded.Success {
		// Surface the structured error code so operators can
		// distinguish "endpoint disabled" / "wrong secret" / "bad
		// request" / etc. without grepping identity logs.
		errCode := decoded.ErrorCode
		if errCode == "" {
			errCode = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return "", false, fmt.Errorf("node bootstrap: identity rejected request (status=%d errorCode=%s): %s",
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

	return decoded.PlainToken, true, nil
}
