package avatarvendor

// simli.go is the direct Go integration of the Simli REST API, replacing the
// retired Python Simli LiveKit plugin. Same two-step shape as the stock
// plugin's AvatarSession.start, ported 1:1:
//
//  1. POST {base}/compose/token (header x-simli-api-key) with the face id +
//     session knobs -> a session token.
//  2. POST {base}/integrations/livekit/agents with that session token plus the
//     LiveKit room url + the avatar participant's join token; Simli's engine
//     then joins the room as the avatar participant ("simli-avatar-agent") and
//     subscribes to our published audio byte-stream for lip-sync.
//
// As with Anam, this client is pure REST and CGO-free; the LiveKit token mint
// + media forwarding live in the caller's voice-tagged / direct-path glue.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const (
	// simliAvatarIdentity is the room identity the Simli engine joins under
	// (the stock plugin's default avatar_participant_identity). The forwarding
	// sink addresses the audio byte-stream to it.
	simliAvatarIdentity = "simli-avatar-agent"

	// simliMaxSessionLength / simliMaxIdleTime bound the Simli session
	// (seconds), mirroring the plugin defaults so a wedged session self-expires.
	simliMaxSessionLength = 3600
	simliMaxIdleTime      = 300

	// simliSampleRate is the PCM rate Simli's audio output operates at
	// (SAMPLE_RATE = 16000 in the plugin).
	simliSampleRate = 16000
)

// simliClient is the CGO-free Simli REST integration for one resolved plan.
type simliClient struct {
	plan AvatarPlan
	doer HTTPDoer
}

// simliTokenResponse is the POST /compose/token reply. Simli surfaces the
// token either as session_token or token depending on the API version; probe
// both.
type simliTokenResponse struct {
	SessionToken string `json:"session_token"`
	Token        string `json:"token"`
}

func (r simliTokenResponse) token() string {
	if strings.TrimSpace(r.SessionToken) != "" {
		return r.SessionToken
	}
	return r.Token
}

// simliAgentResponse is the POST /integrations/livekit/agents reply.
type simliAgentResponse struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
}

// Start performs the Simli bring-up for one session. livekitToken is the join
// token minted for the avatar participant identity.
func (c *simliClient) Start(ctx context.Context, roomName, livekitURL, livekitToken string) (AvatarStartResult, error) {
	sessionToken, err := c.createSessionToken(ctx)
	if err != nil {
		return AvatarStartResult{}, err
	}

	sessionID, err := c.startLiveKitAgent(ctx, sessionToken, livekitURL, livekitToken)
	if err != nil {
		return AvatarStartResult{}, err
	}

	return AvatarStartResult{
		SessionID:         sessionID,
		AvatarIdentity:    simliAvatarIdentity,
		LiveKitSampleRate: simliSampleRate,
	}, nil
}

// createSessionToken POSTs the face id + session knobs to /compose/token and
// returns the session token. Auth is the x-simli-api-key header (not Bearer).
func (c *simliClient) createSessionToken(ctx context.Context) (string, error) {
	faceID := strings.TrimSpace(c.plan.PersonaID)
	if faceID == "" {
		return "", fmt.Errorf("simli: plan carries no faceId")
	}
	payload := map[string]any{
		"faceId":           faceID,
		"handleSilence":    true,
		"maxSessionLength": simliMaxSessionLength,
		"maxIdleTime":      simliMaxIdleTime,
	}
	url := c.base() + "/compose/token"
	var resp simliTokenResponse
	if err := doJSON(ctx, c.doer, http.MethodPost, url, payload, c.apiKeyHeader, &resp); err != nil {
		return "", fmt.Errorf("simli: compose token: %w", err)
	}
	if resp.token() == "" {
		return "", fmt.Errorf("simli: compose token response missing token")
	}
	return resp.token(), nil
}

// startLiveKitAgent POSTs the session token + LiveKit room url/token to
// /integrations/livekit/agents so Simli's engine joins the room.
func (c *simliClient) startLiveKitAgent(ctx context.Context, sessionToken, livekitURL, livekitToken string) (string, error) {
	payload := map[string]any{
		"session_token": sessionToken,
		"livekit_token": livekitToken,
		"livekit_url":   livekitURL,
	}
	url := c.base() + "/integrations/livekit/agents"
	var resp simliAgentResponse
	if err := doJSON(ctx, c.doer, http.MethodPost, url, payload, c.apiKeyHeader, &resp); err != nil {
		return "", fmt.Errorf("simli: livekit agent: %w", err)
	}
	return resp.SessionID, nil
}

func (c *simliClient) base() string {
	if b := strings.TrimSpace(c.plan.APIBase); b != "" {
		return strings.TrimRight(b, "/")
	}
	return simliAPIBase
}

// apiKeyHeader stamps the Simli API key on the x-simli-api-key header.
func (c *simliClient) apiKeyHeader(req *http.Request) {
	req.Header.Set("x-simli-api-key", c.plan.APIKey)
}
