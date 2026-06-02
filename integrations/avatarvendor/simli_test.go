package avatarvendor

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimliClient_StartShape(t *testing.T) {
	stub := &stubHTTP{responses: []stubResp{
		{status: 200, body: `{"session_token":"simli-sess"}`},
		{status: 200, body: `{"session_id":"sid-1"}`},
	}}
	c := &simliClient{
		plan: AvatarPlan{
			Vendor:    avatarVendorSimli,
			PersonaID: "face-42",
			APIKey:    "simli-key",
			APIBase:   "https://api.simli.test",
		},
		doer: stub,
	}

	res, err := c.Start(context.Background(), "polyphon-space1", "wss://lk.public", "lk-join-token")
	require.NoError(t, err)
	assert.Equal(t, "sid-1", res.SessionID)
	assert.Equal(t, simliAvatarIdentity, res.AvatarIdentity)
	require.Len(t, stub.calls, 2)

	// 1. compose/token with x-simli-api-key header + faceId body.
	tok := stub.calls[0]
	assert.Equal(t, http.MethodPost, tok.method)
	assert.Equal(t, "https://api.simli.test/compose/token", tok.url)
	assert.Equal(t, "simli-key", tok.header.Get("x-simli-api-key"))
	assert.Equal(t, "face-42", tok.body["faceId"])
	assert.Equal(t, true, tok.body["handleSilence"])

	// 2. integrations/livekit/agents with the session token + LiveKit room.
	agent := stub.calls[1]
	assert.Equal(t, http.MethodPost, agent.method)
	assert.Equal(t, "https://api.simli.test/integrations/livekit/agents", agent.url)
	assert.Equal(t, "simli-sess", agent.body["session_token"])
	assert.Equal(t, "lk-join-token", agent.body["livekit_token"])
	assert.Equal(t, "wss://lk.public", agent.body["livekit_url"])
}

func TestSimliClient_TokenFallbackField(t *testing.T) {
	// Simli may return the token under "token" rather than "session_token".
	stub := &stubHTTP{responses: []stubResp{
		{status: 200, body: `{"token":"alt-tok"}`},
		{status: 200, body: `{"session_id":"sid"}`},
	}}
	c := &simliClient{plan: AvatarPlan{PersonaID: "f", APIKey: "k", APIBase: "https://x"}, doer: stub}
	_, err := c.Start(context.Background(), "r", "u", "t")
	require.NoError(t, err)
	assert.Equal(t, "alt-tok", stub.calls[1].body["session_token"])
}

func TestSimliClient_ComposeErrorPropagates(t *testing.T) {
	stub := &stubHTTP{responses: []stubResp{{status: 500, body: `boom`}}}
	c := &simliClient{plan: AvatarPlan{PersonaID: "f", APIKey: "k", APIBase: "https://x"}, doer: stub}
	_, err := c.Start(context.Background(), "r", "u", "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compose token")
}
