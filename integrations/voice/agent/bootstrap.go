package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// bootstrapHTTPTimeout caps how long the self-bootstrap waits for the
// identity service to mint a token before giving up. Generous so a
// slow-start identity service in compose doesn't trigger a false
// negative, but bounded so a misconfigured endpoint doesn't block
// voice-agent startup forever. Mirrors the Python agent's 30s
// (_VOICE_AGENT_BOOTSTRAP_HTTP_TIMEOUT_SEC) and the Go node-bootstrap
// (node.nodeBootstrapHTTPTimeout).
const bootstrapHTTPTimeout = 30 * time.Second

// bootstrapRequest is the POST /node/bootstrap body for the
// class="voice_agent" path. Shape mirrors
// component/identity/http.bootstrapRequest exactly (tokenClass +
// instanceId), so the Go agent self-bootstraps through the same
// surface + bootstrap secret the Python agent used (memql#342).
type bootstrapRequest struct {
	TokenClass string `json:"tokenClass"`
	InstanceID string `json:"instanceId"`
}

// bootstrapResponse is the relevant subset of the identity service's
// /node/bootstrap response (component/identity/http.bootstrapResponse).
type bootstrapResponse struct {
	Success    bool   `json:"success"`
	PlainToken string `json:"plainToken"`
	IdentityID string `json:"identityId"`
	ExpiresAt  string `json:"expiresAt"`
	Error      string `json:"error"`
	ErrorCode  string `json:"errorCode"`
}

// TokenLogger is the minimal logging surface ResolveVoiceAgentToken
// needs. Satisfied by *slog.Logger.
type TokenLogger interface {
	Info(msg string, args ...any)
}

// ResolveVoiceAgentToken returns the JWT to authenticate the voice-agent on
// MemqlService.Stream. Resolution order mirrors the Python agent's
// _resolve_voice_agent_token:
//
//  1. VOICE_AGENT_TOKEN env var (operator-provisioned, the production path).
//  2. Self-bootstrap via POST /node/bootstrap with tokenClass="voice_agent"
//     (the dev path from memql#342, gated on the same preconditions as
//     component/node.maybeBootstrapNodeToken).
//
// It returns an error when neither path yields a token, matching the
// Python contract that config load fails loudly when auth can't be set up.
//
// getenv may be nil (os.Getenv is used). httpClient may be nil (a client with
// bootstrapHTTPTimeout is used) -- overridable in tests.
func ResolveVoiceAgentToken(ctx context.Context, getenv Getenv, httpClient *http.Client, log TokenLogger) (string, error) {
	if getenv == nil {
		return "", fmt.Errorf("voice-agent token resolution: getenv is nil")
	}
	if direct := strings.TrimSpace(getenv("VOICE_AGENT_TOKEN")); direct != "" {
		return direct, nil
	}
	bootstrapped, err := maybeBootstrapVoiceAgentToken(ctx, getenv, httpClient, log)
	if err != nil {
		return "", err
	}
	if bootstrapped != "" {
		return bootstrapped, nil
	}
	return "", fmt.Errorf(
		"required env var VOICE_AGENT_TOKEN is unset; either provision a " +
			"voice-agent JWT out-of-band (see docs/auth/voice-agent-jwt.md) or " +
			"set MEMQL_NODE_BOOTSTRAP_TOKEN + IDENTITY_VERIFIER_BASE_URL + " +
			"MEMQL_VOICE_AGENT_INSTANCE_ID for the self-bootstrap path (memql#342)")
}

// maybeBootstrapVoiceAgentToken self-bootstraps a class="voice_agent" JWT
// against the identity service. Returns the minted token when all
// preconditions are present and the call succeeds; returns "" when
// self-bootstrap isn't opted into (the operator is provisioning the token
// out-of-band). Returns an error when bootstrap is configured but fails, so
// the operator sees a clear diagnostic instead of a downstream auth failure.
//
// Preconditions (mirroring component/node.maybeBootstrapNodeToken and the
// Python _maybe_bootstrap_voice_agent_token):
//
//  1. VOICE_AGENT_TOKEN is empty (operator-provisioned token wins).
//  2. MEMQL_NODE_BOOTSTRAP_TOKEN is set (the shared bootstrap secret).
//  3. IDENTITY_VERIFIER_BASE_URL is set (somewhere to POST).
//  4. MEMQL_VOICE_AGENT_INSTANCE_ID is set (the audit instance label).
func maybeBootstrapVoiceAgentToken(ctx context.Context, getenv Getenv, httpClient *http.Client, log TokenLogger) (string, error) {
	if strings.TrimSpace(getenv("VOICE_AGENT_TOKEN")) != "" {
		return "", nil
	}
	secret := strings.TrimSpace(getenv("MEMQL_NODE_BOOTSTRAP_TOKEN"))
	if secret == "" {
		// Self-bootstrap not opted into.
		return "", nil
	}
	identityBase := strings.TrimRight(strings.TrimSpace(getenv("IDENTITY_VERIFIER_BASE_URL")), "/")
	if identityBase == "" {
		return "", fmt.Errorf(
			"voice-agent bootstrap requested (MEMQL_NODE_BOOTSTRAP_TOKEN set) " +
				"but IDENTITY_VERIFIER_BASE_URL is empty -- cannot reach identity service")
	}
	instanceID := strings.TrimSpace(getenv("MEMQL_VOICE_AGENT_INSTANCE_ID"))
	if instanceID == "" {
		return "", fmt.Errorf(
			"voice-agent bootstrap requested but MEMQL_VOICE_AGENT_INSTANCE_ID is empty " +
				"(stamp an instance label so audit can correlate this process)")
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: bootstrapHTTPTimeout}
	}

	endpoint := identityBase + "/node/bootstrap"
	payload, err := json.Marshal(bootstrapRequest{TokenClass: "voice_agent", InstanceID: instanceID})
	if err != nil {
		return "", fmt.Errorf("voice-agent bootstrap: marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, bootstrapHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("voice-agent bootstrap: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bootstrap "+secret)

	if log != nil {
		log.Info("voice-agent bootstrap: requesting voice_agent JWT from identity",
			"endpoint", endpoint, "instance", instanceID)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice-agent bootstrap: POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("voice-agent bootstrap: read response: %w", err)
	}

	var decoded bootstrapResponse
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			snippet := body
			if len(snippet) > 512 {
				snippet = snippet[:512]
			}
			return "", fmt.Errorf(
				"voice-agent bootstrap: identity returned %d with non-JSON body: %q",
				resp.StatusCode, string(snippet))
		}
	}

	if resp.StatusCode != http.StatusOK || !decoded.Success {
		errCode := decoded.ErrorCode
		if errCode == "" {
			errCode = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return "", fmt.Errorf(
			"voice-agent bootstrap: identity rejected request (status=%d errorCode=%s): %s",
			resp.StatusCode, errCode, strings.TrimSpace(decoded.Error))
	}

	token := strings.TrimSpace(decoded.PlainToken)
	if token == "" {
		return "", fmt.Errorf(
			"voice-agent bootstrap: identity returned 200 success but empty plainToken")
	}
	if log != nil {
		log.Info("voice-agent bootstrap: minted voice_agent JWT",
			"identity_id", decoded.IdentityID, "expires_at", decoded.ExpiresAt)
	}
	return token, nil
}
