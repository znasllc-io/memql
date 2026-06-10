package avatardirect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/avatarvendor"
)

// fakeEngine implements just the Execute method of IntegrationEngineAccess by
// embedding the interface (the other methods panic if ever called, which they
// are not on this path). It returns a GraphBundle whose single node carries the
// agent's avatar fields.
type fakeEngine struct {
	memql.IntegrationEngineAccess
	vendor    string
	personaId string
	err       error
	gotQuery  string
}

func (f *fakeEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	payload, _ := structpb.NewStruct(map[string]any{
		"avatarVendor":    f.vendor,
		"avatarPersonaId": f.personaId,
	})
	return &memql.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{
				Id:      "v1:agents:agent:abc",
				Concept: "v1:agents:agent",
				Payload: payload,
			}},
		},
	}, nil
}

// notFoundEngine returns an empty bundle (agent not found).
type notFoundEngine struct{ memql.IntegrationEngineAccess }

func (notFoundEngine) Execute(_ context.Context, _ string) (*memql.ExecuteResult, error) {
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

type stubVendorClient struct {
	res         avatarvendor.AvatarStartResult
	err         error
	startedURL  string
	startedTok  string
	startedRoom string
}

func (s *stubVendorClient) Start(_ context.Context, roomName, livekitURL, livekitToken string) (avatarvendor.AvatarStartResult, error) {
	s.startedRoom = roomName
	s.startedURL = livekitURL
	s.startedTok = livekitToken
	return s.res, s.err
}

func testLKConfig() polyphon.Config {
	return polyphon.Config{
		LiveKitURL:       "ws://livekit:7880",
		LiveKitPublicURL: "wss://lk.public",
		LiveKitAPIKey:    "api-key",
		LiveKitAPISecret: "api-secret",
	}
}

func newTestIntegration(eng memql.IntegrationEngineAccess, client *stubVendorClient) *Integration {
	i := New(eng, func(context.Context, string) (string, error) { return "anam-key", nil }, testLKConfig(), nil)
	if client != nil {
		i.newClient = func(avatarvendor.AvatarPlan) (avatarvendor.AvatarVendorClient, error) { return client, nil }
	}
	return i
}

func decode(t *testing.T, nodes []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	var out map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &out))
	return out
}

// TestMintBrowserToken_KindByVendor pins the memql#1236 fix: the browser join
// token's participant KIND is vendor-specific. anam needs a STANDARD (non-agent)
// participant in the room or its engine self-leaves at ~26-27s; simli needs the
// browser to be AGENT-kind so its receiver picks it as the audio source. The
// match is case-insensitive (the vendor string flows from the agent record).
func TestMintBrowserToken_KindByVendor(t *testing.T) {
	i := newTestIntegration(&fakeEngine{}, nil)

	cases := []struct {
		vendor string
		want   livekit.ParticipantInfo_Kind
	}{
		{"anam", livekit.ParticipantInfo_STANDARD},
		{"ANAM", livekit.ParticipantInfo_STANDARD},
		{"simli", livekit.ParticipantInfo_AGENT},
		{"", livekit.ParticipantInfo_AGENT}, // unknown/empty defaults to the simli-compatible agent kind
	}
	for _, tc := range cases {
		t.Run("vendor="+tc.vendor, func(t *testing.T) {
			tok, err := i.mintBrowserToken("avatar-room", "viewer-1", tc.vendor)
			require.NoError(t, err)

			v, err := auth.ParseAPIToken(tok)
			require.NoError(t, err)
			_, grants, err := v.Verify(testLKConfig().LiveKitAPISecret)
			require.NoError(t, err)

			assert.Equal(t, tc.want, grants.GetParticipantKind())
			assert.Equal(t, "viewer-1", grants.Identity)
			require.NotNil(t, grants.Video)
			assert.Equal(t, "avatar-room", grants.Video.Room)
		})
	}
}

func TestStartSession_Phase1MintsCredsWithoutEngine(t *testing.T) {
	// Phase 1 mints the room + browser creds and validates the persona, but
	// must NOT bring the vendor engine up (the browser joins + forwards audio
	// first, then engageVendor starts the engine, memql#782).
	eng := &fakeEngine{vendor: "anam", personaId: "persona-x"}
	client := &stubVendorClient{res: avatarvendor.AvatarStartResult{
		SessionID: "anam-sess-1", AvatarIdentity: avatarvendor.AvatarParticipantIdentity, LiveKitSampleRate: 16000,
	}}
	i := newTestIntegration(eng, client)

	nodes, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "v1:agents:agent:abc", "spaceId": "sp1"}, 0)
	out := decode(t, nodes, err)

	assert.Equal(t, "wss://lk.public", out["livekit_url"])
	assert.NotEmpty(t, out["livekit_client_token"])
	assert.Equal(t, "anam", out["vendor"])
	assert.True(t, strings.HasPrefix(out["room_name"].(string), "avatar-"), "dedicated room name")
	assert.True(t, strings.HasPrefix(out["browser_identity"].(string), "viewer-"), "browser identity returned for the on-behalf attribution")

	// The engine was NOT started in phase 1.
	assert.Empty(t, client.startedRoom, "vendor engine must not start in phase 1")
	assert.Nil(t, out["session_id"], "no session id until engageVendor")

	// The lookup used the named queryAgentById (no raw DSL).
	assert.Contains(t, eng.gotQuery, "queryAgentById")
	assert.Contains(t, eng.gotQuery, "v1:agents:agent:abc")
}

func TestEngageVendor_Phase2StartsEngine(t *testing.T) {
	eng := &fakeEngine{vendor: "anam", personaId: "persona-x"}
	client := &stubVendorClient{res: avatarvendor.AvatarStartResult{
		SessionID: "anam-sess-1", AvatarIdentity: avatarvendor.AvatarParticipantIdentity, LiveKitSampleRate: 16000,
	}}
	i := newTestIntegration(eng, client)

	nodes, err := i.handleEngageVendor(context.Background(), map[string]any{
		"agentId":          "v1:agents:agent:abc",
		"room_name":        "avatar-xyz",
		"browser_identity": "viewer-123",
	}, 0)
	out := decode(t, nodes, err)

	assert.Equal(t, "anam-sess-1", out["session_id"])
	assert.Equal(t, "anam", out["vendor"])
	assert.Equal(t, avatarvendor.AvatarParticipantIdentity, out["avatar_identity"])
	assert.Equal(t, true, out["ok"])

	// The engine was handed the public URL + the avatar token for the room from
	// phase 1.
	assert.Equal(t, "wss://lk.public", client.startedURL)
	assert.Equal(t, "avatar-xyz", client.startedRoom)
	assert.NotEmpty(t, client.startedTok)
}

func TestEngageVendor_RequiresRoomAndBrowserIdentity(t *testing.T) {
	i := newTestIntegration(&fakeEngine{vendor: "simli", personaId: "f"}, &stubVendorClient{})
	_, err := i.handleEngageVendor(context.Background(), map[string]any{"agentId": "a", "browser_identity": "viewer-1"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "room_name is required")

	_, err = i.handleEngageVendor(context.Background(), map[string]any{"agentId": "a", "room_name": "avatar-x"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "browser_identity is required")
}

func TestStartSession_SimliVendorAccepted(t *testing.T) {
	// memql#782: the direct path now supports the simli cloud-engine vendor.
	eng := &fakeEngine{vendor: "simli", personaId: "face-1"}
	i := newTestIntegration(eng, &stubVendorClient{})
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "a"}, 0)
	require.NoError(t, err)
}

func TestStartSession_UnknownVendorRejected(t *testing.T) {
	// Vendors other than anam / simli (e.g. the retired liveavatar) are not
	// supported on the direct path.
	eng := &fakeEngine{vendor: "liveavatar", personaId: "p-1"}
	i := newTestIntegration(eng, &stubVendorClient{})
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "a"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on the direct avatar path")
}

func TestStartSession_AgentNotFound(t *testing.T) {
	i := newTestIntegration(notFoundEngine{}, &stubVendorClient{})
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "missing"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStartSession_NoPersonaIsError(t *testing.T) {
	// anam vendor but no persona id and no platform default -> ResolveAvatarPlan
	// returns nil (audio-only) -> started=false -> specific error.
	eng := &fakeEngine{vendor: "anam", personaId: ""}
	i := newTestIntegration(eng, &stubVendorClient{})
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "a"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no avatar persona")
}

func TestEngageVendor_VendorStartFailurePropagates(t *testing.T) {
	eng := &fakeEngine{vendor: "anam", personaId: "persona-x"}
	client := &stubVendorClient{err: errors.New("anam 503")}
	i := newTestIntegration(eng, client)
	_, err := i.handleEngageVendor(context.Background(), map[string]any{
		"agentId": "a", "room_name": "avatar-x", "browser_identity": "viewer-1",
	}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bring up anam avatar")
}

func TestEngageVendor_DevTunnelMediaTimeoutSurfacesDeterminationHint(t *testing.T) {
	// memql#1277: on the local dev path the engine URL is an ngrok tunnel and a
	// vendor-join timeout means the WebRTC media leg never connected. The cryptic
	// "context deadline exceeded" is replaced with a definitive, documented error
	// pointing at the local limitation + the staging-validated path.
	t.Setenv("LIVEKIT_PUBLIC_URL", "wss://unscrutinisingly-nondetonating-taisha.ngrok-free.dev")
	eng := &fakeEngine{vendor: "simli", personaId: "face-1"}
	client := &stubVendorClient{err: errors.New(`simli: livekit agent: Post "https://api.simli.ai/integrations/livekit/agents": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)}
	i := newTestIntegration(eng, client)

	_, err := i.handleEngageVendor(context.Background(), map[string]any{
		"agentId": "a", "room_name": "avatar-x", "browser_identity": "viewer-1",
	}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on the dev cluster")
	assert.Contains(t, err.Error(), "audio-only locally")
	assert.Contains(t, err.Error(), "validated on staging")
	assert.Contains(t, err.Error(), "voice-turn-relay.md")
	// The underlying vendor error is still wrapped for debugging.
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestEngageVendor_DevTunnelNonTimeoutKeepsRawError(t *testing.T) {
	// A fast 4xx/5xx (bad key, bad face id) on the dev tunnel is NOT a media
	// failure, so it keeps the raw "bring up <vendor> avatar" wrapping rather
	// than the misleading media-relay determination hint.
	t.Setenv("LIVEKIT_PUBLIC_URL", "wss://foo.ngrok-free.dev")
	eng := &fakeEngine{vendor: "simli", personaId: "face-1"}
	client := &stubVendorClient{err: errors.New("simli: compose token: POST .../compose/token -> 401: invalid api key")}
	i := newTestIntegration(eng, client)

	_, err := i.handleEngageVendor(context.Background(), map[string]any{
		"agentId": "a", "room_name": "avatar-x", "browser_identity": "viewer-1",
	}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bring up simli avatar")
	assert.NotContains(t, err.Error(), "not supported on the dev cluster")
}

func TestStartSession_MissingAgentId(t *testing.T) {
	i := newTestIntegration(&fakeEngine{vendor: "anam"}, &stubVendorClient{})
	_, err := i.handleStartSession(context.Background(), map[string]any{}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agentId is required")
}

func TestStartSession_LiveKitNotConfigured(t *testing.T) {
	i := New(&fakeEngine{vendor: "anam", personaId: "p"}, func(context.Context, string) (string, error) { return "k", nil }, polyphon.Config{}, nil)
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "a"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LiveKit not configured")
}

func TestStopSession_AlwaysOk(t *testing.T) {
	i := newTestIntegration(&fakeEngine{}, &stubVendorClient{})
	nodes, err := i.handleStopSession(context.Background(), map[string]any{"session_id": "s", "vendor": "anam", "room_name": "avatar-x"}, 0)
	out := decode(t, nodes, err)
	assert.Equal(t, true, out["ok"])
}
