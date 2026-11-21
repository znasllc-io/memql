package calls

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessingVisualPublicationCapacityPrecedesSamplerMintAndRevokesRenewal(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	call := consentProcessing(t, cw, host, true, true, true)
	tracks := []MediaTrack{{SID: "TR_microphone", Source: "microphone"}}
	for n := 0; n < 11; n++ {
		source := "camera"
		if n%2 == 0 {
			source = "screen_share"
		}
		tracks = append(tracks, MediaTrack{SID: fmt.Sprintf("TR_visual_%02d", n), Source: source})
	}
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: call.Identity, Tracks: tracks})
	refused, err := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.ErrorContains(t, err, "processing_capacity")
	require.Empty(t, refused.MediaToken)
	j, err := cw.svc.store.processing(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.Empty(t, j.Lease.ID, "an oversized lease must not mint a sampler identity")
	require.Empty(t, j.Lease.Identity)
	require.Equal(t, "processing_capacity", j.Reason)

	// Ten visual publications plus audio fit: the visual cap is independent of
	// the total publication budget, and cameras and screen shares both count.
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: call.Identity, Tracks: tracks[:11]})
	lease, err := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.NoError(t, err)
	require.Len(t, lease.Tracks, 11)
	require.NotEmpty(t, lease.MediaToken)

	// Growth above the provider receive cap retires existing authority instead
	// of renewing a token while silently leaving the eleventh video pending.
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: call.Identity, Tracks: tracks})
	refused, err = cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", lease.LeaseID, false, 100)
	require.ErrorContains(t, err, "processing_capacity")
	require.Empty(t, refused.MediaToken)
	j, err = cw.svc.store.processing(ctx, cw.owner, start.Call.ID)
	require.NoError(t, err)
	require.Empty(t, j.Lease.ID)
	require.Empty(t, j.Lease.Tracks)
	require.Equal(t, "processing_capacity", j.Reason)
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: cw.media.created[0].Server, Room: start.Call.ID, Identity: lease.MediaIdentity})
	_, err = cw.svc.processingPublish(ctx, cw.ref, start.Call.ID, "operator", lease.LeaseID, lease.Revision, speechEvent(lease))
	require.ErrorContains(t, err, "processing_lease_expired")
}
