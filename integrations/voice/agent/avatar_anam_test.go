package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHTTP is a scripted httpDoer: it records every request (method, URL,
// headers, decoded JSON body) and returns the next queued response. It lets the
// vendor REST clients be tested for exact wire shape without a network.
type stubHTTP struct {
	responses []stubResp
	calls     []stubCall
}

type stubResp struct {
	status int
	body   string
}

type stubCall struct {
	method string
	url    string
	auth   string
	header http.Header
	body   map[string]any
}

func (s *stubHTTP) Do(req *http.Request) (*http.Response, error) {
	call := stubCall{
		method: req.Method,
		url:    req.URL.String(),
		auth:   req.Header.Get("Authorization"),
		header: req.Header.Clone(),
	}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.body)
		}
	}
	s.calls = append(s.calls, call)

	resp := stubResp{status: 200, body: "{}"}
	if len(s.responses) > 0 {
		resp = s.responses[0]
		s.responses = s.responses[1:]
	}
	return &http.Response{
		StatusCode: resp.status,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Header:     make(http.Header),
	}, nil
}

func TestAnamClient_StartPersonaIDShape(t *testing.T) {
	// Persona-id path: persona lookup -> session-token (ephemeral) -> engine.
	stub := &stubHTTP{responses: []stubResp{
		{status: 200, body: `{"avatarId":"avatar-from-persona"}`},
		{status: 200, body: `{"sessionToken":"sess-tok-123"}`},
		{status: 200, body: `{"sessionId":"engine-sess-1"}`},
	}}
	c := &anamClient{
		plan: AvatarPlan{
			Vendor:      avatarVendorAnam,
			PersonaID:   "persona-abc",
			APIKey:      "anam-key",
			APIBase:     "https://api.anam.test",
			DisplayName: "Sofia",
		},
		doer: stub,
	}

	res, err := c.Start(context.Background(), "polyphon-space1", "wss://lk.public", "lk-join-token")
	require.NoError(t, err)
	assert.Equal(t, "engine-sess-1", res.SessionID)
	assert.Equal(t, avatarParticipantIdentity, res.AvatarIdentity)
	require.Len(t, stub.calls, 3)

	// 1. Persona lookup: GET /v1/personas/{id} with Bearer API key.
	lookup := stub.calls[0]
	assert.Equal(t, http.MethodGet, lookup.method)
	assert.Equal(t, "https://api.anam.test/v1/personas/persona-abc", lookup.url)
	assert.Equal(t, "Bearer anam-key", lookup.auth)

	// 2. Session-token: POST /v1/auth/session-token with the EPHEMERAL
	//    personaConfig (the resolved avatarId, CUSTOMER_CLIENT_V1 llm) and the
	//    LiveKit environment -- the load-bearing persona-id-as-ephemeral shape.
	tok := stub.calls[1]
	assert.Equal(t, http.MethodPost, tok.method)
	assert.Equal(t, "https://api.anam.test/v1/auth/session-token", tok.url)
	assert.Equal(t, "Bearer anam-key", tok.auth)
	pc, ok := tok.body["personaConfig"].(map[string]any)
	require.True(t, ok, "personaConfig present")
	assert.Equal(t, "ephemeral", pc["type"])
	assert.Equal(t, "avatar-from-persona", pc["avatarId"])
	assert.Equal(t, anamClientAudioLLM, pc["llmId"])
	assert.Equal(t, "Sofia", pc["name"])
	env, ok := tok.body["environment"].(map[string]any)
	require.True(t, ok, "environment present")
	assert.Equal(t, "wss://lk.public", env["livekitUrl"])
	assert.Equal(t, "lk-join-token", env["livekitToken"])

	// 3. Engine session: POST /v1/engine/session with the session token Bearer.
	eng := stub.calls[2]
	assert.Equal(t, http.MethodPost, eng.method)
	assert.Equal(t, "https://api.anam.test/v1/engine/session", eng.url)
	assert.Equal(t, "Bearer sess-tok-123", eng.auth)
}

func TestAnamClient_StartBareAvatarIDSkipsLookup(t *testing.T) {
	// Bare avatarId plan: no persona lookup, avatarId used verbatim.
	stub := &stubHTTP{responses: []stubResp{
		{status: 200, body: `{"sessionToken":"sess-tok"}`},
		{status: 200, body: `{"sessionId":"eng"}`},
	}}
	c := &anamClient{
		plan: AvatarPlan{
			Vendor:      avatarVendorAnam,
			AvatarID:    "bare-avatar",
			APIKey:      "k",
			APIBase:     "https://api.anam.test",
			DisplayName: "Assistant",
		},
		doer: stub,
	}
	_, err := c.Start(context.Background(), "room", "wss://lk", "tok")
	require.NoError(t, err)
	require.Len(t, stub.calls, 2) // no /v1/personas lookup
	pc := stub.calls[0].body["personaConfig"].(map[string]any)
	assert.Equal(t, "bare-avatar", pc["avatarId"])
}

func TestAnamClient_PersonaLookupNestedAvatarID(t *testing.T) {
	stub := &stubHTTP{responses: []stubResp{
		{status: 200, body: `{"personaConfig":{"avatarId":"nested-avatar"}}`},
		{status: 200, body: `{"sessionToken":"st"}`},
		{status: 200, body: `{"sessionId":"e"}`},
	}}
	c := &anamClient{plan: AvatarPlan{PersonaID: "p", APIKey: "k", APIBase: "https://x"}, doer: stub}
	_, err := c.Start(context.Background(), "r", "u", "t")
	require.NoError(t, err)
	pc := stub.calls[1].body["personaConfig"].(map[string]any)
	assert.Equal(t, "nested-avatar", pc["avatarId"])
}

func TestAnamClient_SessionTokenErrorPropagates(t *testing.T) {
	stub := &stubHTTP{responses: []stubResp{
		{status: 200, body: `{"avatarId":"a"}`},
		{status: 401, body: `unauthorized`},
	}}
	c := &anamClient{plan: AvatarPlan{PersonaID: "p", APIKey: "k", APIBase: "https://x"}, doer: stub}
	_, err := c.Start(context.Background(), "r", "u", "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session-token")
}
