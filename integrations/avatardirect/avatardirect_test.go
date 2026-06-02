package avatardirect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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

func TestStartSession_AnamHappyPath(t *testing.T) {
	eng := &fakeEngine{vendor: "anam", personaId: "persona-x"}
	client := &stubVendorClient{res: avatarvendor.AvatarStartResult{
		SessionID: "anam-sess-1", AvatarIdentity: avatarvendor.AvatarParticipantIdentity, LiveKitSampleRate: 16000,
	}}
	i := newTestIntegration(eng, client)

	nodes, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "v1:agents:agent:abc", "spaceId": "sp1"}, 0)
	out := decode(t, nodes, err)

	assert.Equal(t, "wss://lk.public", out["livekit_url"])
	assert.NotEmpty(t, out["livekit_client_token"])
	assert.Equal(t, "anam-sess-1", out["session_id"])
	assert.Equal(t, "anam", out["vendor"])
	assert.True(t, strings.HasPrefix(out["room_name"].(string), "avatar-"), "dedicated room name")

	// Anam was handed the public URL + the avatar token (NOT the browser token)
	// for the same room copresent will join.
	assert.Equal(t, "wss://lk.public", client.startedURL)
	assert.NotEmpty(t, client.startedTok)
	assert.NotEqual(t, out["livekit_client_token"], client.startedTok, "avatar token differs from browser token")
	assert.Equal(t, out["room_name"], client.startedRoom)

	// The lookup queried the agent's avatar fields.
	assert.Contains(t, eng.gotQuery, "payload.avatarVendor")
	assert.Contains(t, eng.gotQuery, "payload.avatarPersonaId")
}

func TestStartSession_NonAnamVendorRejected(t *testing.T) {
	eng := &fakeEngine{vendor: "simli", personaId: "face-1"}
	i := newTestIntegration(eng, &stubVendorClient{})
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "a"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on the direct Anam path")
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

func TestStartSession_VendorStartFailurePropagates(t *testing.T) {
	eng := &fakeEngine{vendor: "anam", personaId: "persona-x"}
	client := &stubVendorClient{err: errors.New("anam 503")}
	i := newTestIntegration(eng, client)
	_, err := i.handleStartSession(context.Background(), map[string]any{"agentId": "a"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bring up anam avatar")
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
