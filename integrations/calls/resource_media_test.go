package calls

import (
	"context"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

type resourceIngressFake struct {
	request *livekit.CreateIngressRequest
	entries []*livekit.IngressInfo
	deletes []string
}

func (f *resourceIngressFake) CreateIngress(_ context.Context, r *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	f.request = r
	v := &livekit.IngressInfo{IngressId: "IN_resource", Name: r.Name, Url: "http://127.0.0.1:8080/whip", StreamKey: "revocable-secret", ParticipantIdentity: r.ParticipantIdentity}
	f.entries = append(f.entries, v)
	return v, nil
}
func (f *resourceIngressFake) ListIngress(_ context.Context, r *livekit.ListIngressRequest) (*livekit.ListIngressResponse, error) {
	return &livekit.ListIngressResponse{Items: f.entries}, nil
}
func (f *resourceIngressFake) DeleteIngress(_ context.Context, r *livekit.DeleteIngressRequest) (*livekit.IngressInfo, error) {
	f.deletes = append(f.deletes, r.IngressId)
	f.entries = nil
	return &livekit.IngressInfo{}, nil
}
func resourceTestSpec() callrules.ResourceSpec {
	return callrules.ResourceSpec{Label: "Synthetic source", Kind: "synthetic", Video: true, Audio: true, MaxWidth: 640, MaxHeight: 360, MaxFPS: 10, MaxBitrateKbps: 500}
}
func TestResourceIngressBoundedRevocableAuthority(t *testing.T) {
	f := &resourceIngressFake{}
	b := &liveKitResources{client: f}
	ctx := context.Background()
	v, err := b.CreateResourceIngress(ctx, "native-room", "lease-one", "native-seat", resourceTestSpec())
	require.NoError(t, err)
	require.Equal(t, "revocable-secret", v.StreamKey)
	r := f.request
	require.Equal(t, livekit.IngressInput_WHIP_INPUT, r.InputType)
	require.Empty(t, r.Url)
	require.True(t, r.GetEnableTranscoding())
	require.False(t, r.BypassTranscoding)
	require.Equal(t, livekit.TrackSource_SCREEN_SHARE, r.Video.Source)
	require.Equal(t, livekit.TrackSource_SCREEN_SHARE_AUDIO, r.Audio.Source)
	require.Len(t, r.Video.GetOptions().Layers, 1)
	require.EqualValues(t, 500000, r.Video.GetOptions().Layers[0].Bitrate)
	require.EqualValues(t, 10, r.Video.GetOptions().FrameRate)
	require.EqualValues(t, 64000, r.Audio.GetOptions().Bitrate)
	found, err := b.FindResourceIngress(ctx, "native-room", "lease-one")
	require.NoError(t, err)
	require.Equal(t, v, found)
	require.NoError(t, b.DeleteResourceIngress(ctx, v.ID))
	found, err = b.FindResourceIngress(ctx, "native-room", "lease-one")
	require.NoError(t, err)
	require.Nil(t, found)
}
func TestResourceIngressNeverAcceptsPullOrCredentialURLs(t *testing.T) {
	for _, endpoint := range []string{"rtsp://camera", "http://example.com/whip", "http://192.168.1.10/whip", "https://secret@example.com/whip", "https://example.com/whip?secret=x"} {
		_, err := resourceIngressInfo(&livekit.IngressInfo{Url: endpoint, StreamKey: "key"})
		require.Error(t, err)
	}
}
func TestResourceProjectionDoesNotDisclosePublisherCredential(t *testing.T) {
	d := &resourceRow{State: resourceState{Sources: []callrules.ResourceInfo{{ID: "source", ResourceSpec: resourceTestSpec(), Revision: 1}}, Leases: []resourceLease{{ID: "lease", ResourceID: "source", Status: "publishing", IngressID: "private-ingress"}}}}
	out := resourceAnswer(d)
	require.Nil(t, out.Publication)
	require.Equal(t, "publishing", out.Resources[0].Status)
}

func TestResourceHumanHoldCannotBeClearedByAgent(t *testing.T) {
	d := &resourceRow{State: resourceState{Sources: []callrules.ResourceInfo{{ID: "held", ResourceSpec: resourceTestSpec(), Revision: 1, HumanHold: true}}}}
	s := &Service{}
	ctx := context.WithValue(context.Background(), agentCallContext{}, "previously-approved-grant")
	_, err := s.startResource(ctx, keyHolder{}, d, roomRow{}, callrules.ResourceRequest{Action: "start", ResourceID: "held", ExpectedRevision: 1, RequestID: "new-agent-request"})
	require.Equal(t, "resource_human_hold", callrules.ReasonOf(err))
	require.True(t, d.State.Sources[0].HumanHold)
}

func TestResourceSingleTrackAuthorityNeverSilentlyBroadens(t *testing.T) {
	spec := resourceTestSpec()
	spec.Audio = false
	require.True(t, callrules.ValidResourceOutputBounds(spec), "earlier metadata remains inspectable")
	require.False(t, callrules.ValidResourceSpec(spec), "single-track grants are unsupported by stock ingress")
	d := &resourceRow{State: resourceState{Sources: []callrules.ResourceInfo{{ID: "earlier-source", ResourceSpec: spec, Revision: 1}}}}
	_, err := (&Service{}).startResource(context.Background(), keyHolder{}, d, roomRow{}, callrules.ResourceRequest{Action: "start", ResourceID: "earlier-source", ExpectedRevision: 1, RequestID: "source-start-0001"})
	require.Equal(t, "unsupported_track_isolation", callrules.ReasonOf(err))
	require.False(t, d.State.Sources[0].Audio)
}
