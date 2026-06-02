package avatarvendor

// anam.go is the direct Go integration of the Anam REST API, replacing the
// retired Python Anam LiveKit plugin + the AnamPersonaSession subclass in the
// Python voice-agent's anam_persona_session.py. No Python, no LiveKit plugin --
// just Anam's documented HTTP endpoints.
//
// Flow (the ephemeral client-audio path, so the avatar lip-syncs OUR audio
// rather than speaking with Anam's bundled voice):
//
//  1. If the plan carries a personaId, GET /v1/personas/{id} and pull its
//     avatarId (the face-model id). A bare avatarId plan skips this.
//  2. POST /v1/auth/session-token with an EPHEMERAL personaConfig
//     ({type: ephemeral, name, avatarId, llmId: CUSTOMER_CLIENT_V1}) and an
//     environment carrying the LiveKit url + the avatar participant's join
//     token. The CUSTOMER_CLIENT_V1 llmId disables Anam's own LLM/TTS so the
//     engine lip-syncs the PCM we publish. -> sessionToken.
//  3. POST /v1/engine/session (Bearer sessionToken) to start the engine
//     session; Anam's cloud engine then dials INTO the LiveKit room as the
//     avatar participant and subscribes to our published audio byte-stream.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// anamClient is the CGO-free Anam REST integration for one resolved plan.
type anamClient struct {
	plan AvatarPlan
	doer HTTPDoer
}

// anamPersonaRecord is the subset of GET /v1/personas/{id} we read: the
// face-model avatarId. Anam surfaces it either top-level or nested under
// personaConfig depending on the account; we probe both, matching
// _fetch_avatar_id_for_persona.
type anamPersonaRecord struct {
	AvatarID      string `json:"avatarId"`
	PersonaConfig struct {
		AvatarID string `json:"avatarId"`
	} `json:"personaConfig"`
}

// anamSessionTokenResponse is the POST /v1/auth/session-token reply.
type anamSessionTokenResponse struct {
	SessionToken string `json:"sessionToken"`
}

// anamEngineSessionResponse is the POST /v1/engine/session reply.
type anamEngineSessionResponse struct {
	SessionID string `json:"sessionId"`
}

// Start performs the full Anam bring-up for one session and returns the
// started-session details. livekitToken is the join token minted for the
// avatar participant identity; Anam embeds it in the session-token environment
// so its engine can join the room.
func (c *anamClient) Start(ctx context.Context, roomName, livekitURL, livekitToken string) (AvatarStartResult, error) {
	avatarID, err := c.resolveAvatarID(ctx)
	if err != nil {
		return AvatarStartResult{}, err
	}

	sessionToken, err := c.createSessionToken(ctx, avatarID, livekitURL, livekitToken)
	if err != nil {
		return AvatarStartResult{}, err
	}

	sessionID, err := c.startEngineSession(ctx, sessionToken)
	if err != nil {
		return AvatarStartResult{}, err
	}

	return AvatarStartResult{
		SessionID:         sessionID,
		AvatarIdentity:    AvatarParticipantIdentity,
		LiveKitSampleRate: DefaultPCMSampleRate,
	}, nil
}

// resolveAvatarID returns the face-model avatarId to render. For a bare
// avatarId plan it is used verbatim; for a personaId plan it is fetched from
// the persona record so the avatar shows the chosen face but speaks with our
// audio (the ephemeral path). Port of _fetch_avatar_id_for_persona.
func (c *anamClient) resolveAvatarID(ctx context.Context) (string, error) {
	if id := strings.TrimSpace(c.plan.AvatarID); id != "" {
		return id, nil
	}
	personaID := strings.TrimSpace(c.plan.PersonaID)
	if personaID == "" {
		return "", fmt.Errorf("anam: plan carries neither personaId nor avatarId")
	}

	url := fmt.Sprintf("%s/v1/personas/%s", c.base(), personaID)
	var rec anamPersonaRecord
	if err := doJSON(ctx, c.doer, http.MethodGet, url, nil, c.bearerAPIKey, &rec); err != nil {
		return "", fmt.Errorf("anam: persona lookup %s: %w", personaID, err)
	}
	avatarID := rec.AvatarID
	if avatarID == "" {
		avatarID = rec.PersonaConfig.AvatarID
	}
	if avatarID == "" {
		return "", fmt.Errorf("anam: persona %s has no avatarId in the response", personaID)
	}
	return avatarID, nil
}

// createSessionToken POSTs the ephemeral personaConfig + LiveKit environment to
// /v1/auth/session-token and returns the sessionToken. Port of
// _create_session_token_with_persona_id: the ephemeral path with
// llmId=CUSTOMER_CLIENT_V1 so OUR audio is what the avatar lip-syncs.
func (c *anamClient) createSessionToken(ctx context.Context, avatarID, livekitURL, livekitToken string) (string, error) {
	payload := map[string]any{
		"personaConfig": map[string]any{
			"type":     "ephemeral",
			"name":     c.displayName(),
			"avatarId": avatarID,
			"llmId":    anamClientAudioLLM,
		},
		"environment": map[string]any{
			"livekitUrl":   livekitURL,
			"livekitToken": livekitToken,
		},
	}
	url := c.base() + "/v1/auth/session-token"
	var resp anamSessionTokenResponse
	if err := doJSON(ctx, c.doer, http.MethodPost, url, payload, c.bearerAPIKey, &resp); err != nil {
		return "", fmt.Errorf("anam: session-token: %w", err)
	}
	if strings.TrimSpace(resp.SessionToken) == "" {
		return "", fmt.Errorf("anam: session-token response missing sessionToken")
	}
	return resp.SessionToken, nil
}

// startEngineSession POSTs to /v1/engine/session with the session token as a
// Bearer credential, kicking off the engine session (the avatar then joins the
// room). Port of the engine_url POST in AnamPersonaSession.start.
func (c *anamClient) startEngineSession(ctx context.Context, sessionToken string) (string, error) {
	url := c.base() + "/v1/engine/session"
	auth := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+sessionToken) }
	var resp anamEngineSessionResponse
	if err := doJSON(ctx, c.doer, http.MethodPost, url, map[string]any{}, auth, &resp); err != nil {
		return "", fmt.Errorf("anam: engine session: %w", err)
	}
	return resp.SessionID, nil
}

func (c *anamClient) base() string {
	if b := strings.TrimSpace(c.plan.APIBase); b != "" {
		return strings.TrimRight(b, "/")
	}
	return anamAPIBase
}

func (c *anamClient) displayName() string {
	if n := strings.TrimSpace(c.plan.DisplayName); n != "" {
		return n
	}
	return AvatarParticipantName
}

// bearerAPIKey stamps the Anam API key as the request's Bearer credential (the
// persona-lookup + session-token endpoints authenticate with the raw API key,
// not the session token).
func (c *anamClient) bearerAPIKey(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.plan.APIKey)
}
