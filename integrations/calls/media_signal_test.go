package calls

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestMediaSignalBindingProof(t *testing.T) {
	pepper := []byte("native-admission-test-pepper-0123456789")
	b := mediaSignalBinding{Version: 1, RoomID: "room", Identity: "identity", Kind: string(callrules.KindPerson), SeatID: "seat", SessionID: "session", JoinedAt: time.Now().UTC()}
	signed, e := signMediaBinding(b, pepper)
	require.NoError(t, e)
	got, e := verifyMediaBinding(signed, pepper)
	require.NoError(t, e)
	require.Equal(t, b, got)
	_, e = verifyMediaBinding(signed, []byte("different-native-pepper-0123456789"))
	require.Error(t, e)
	_, e = verifyMediaBinding("x"+signed[1:], pepper)
	require.Error(t, e)
	_, e = verifyMediaBinding(strings.Repeat("x", 4097), pepper)
	require.Error(t, e)
	b.Identity = "other"
	other, e := signMediaBinding(b, pepper)
	require.NoError(t, e)
	require.NotEqual(t, signed, other)
	b.Kind = string(callrules.KindSampler)
	_, e = signMediaBinding(b, pepper)
	require.Error(t, e, "samplers never borrow a human seat binding")
	b = mediaSignalBinding{Version: 1, RoomID: "room", Identity: "sampler-one", Kind: string(callrules.KindSampler), LeaseID: "lease"}
	_, e = signMediaBinding(b, pepper)
	require.NoError(t, e)
}

type permissionRooms struct {
	fakeRooms
	response *livekit.ParticipantInfo
	request  *livekit.UpdateParticipantRequest
}

func (f *permissionRooms) UpdateParticipant(_ context.Context, r *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	f.request = r
	if f.response != nil {
		return f.response, nil
	}
	return &livekit.ParticipantInfo{Identity: r.Identity, Permission: r.Permission}, nil
}
func TestMediaPermissionUpdateRequiresExplicitSourcesAndReadback(t *testing.T) {
	f := &permissionRooms{}
	b := newLiveKitBackend(f).(MediaPermissionBackend)
	spec, _ := callrules.GrantFor(callrules.KindPerson)
	spec.Sources = []string{"microphone"}
	spec.CanPublishData = false
	got, e := b.UpdatePermissions(context.Background(), "room", "person", spec)
	require.NoError(t, e)
	require.Equal(t, spec, got)
	require.False(t, f.request.Permission.CanUpdateMetadata)
	require.Equal(t, []livekit.TrackSource{livekit.TrackSource_MICROPHONE}, f.request.Permission.CanPublishSources)
	spec.Sources = nil
	_, e = b.UpdatePermissions(context.Background(), "room", "person", spec)
	require.Error(t, e, "empty sources with publish=true means unrestricted to LiveKit")
	spec.CanPublish = false
	_, e = b.UpdatePermissions(context.Background(), "room", "person", spec)
	require.NoError(t, e)
	f.response = &livekit.ParticipantInfo{Identity: "person", Permission: &livekit.ParticipantPermission{CanPublish: true}}
	_, e = b.UpdatePermissions(context.Background(), "room", "person", spec)
	require.ErrorContains(t, e, "readback differs")
	spec.Sources = []string{"screen_share_audio"}
	spec.CanPublish = true
	_, e = b.UpdatePermissions(context.Background(), "room", "person", spec)
	require.ErrorContains(t, e, "screen audio")
}
