package calls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lkjson "github.com/livekit/protocol/utils/protojson"
	"github.com/stretchr/testify/require"
)

// webhook_sdk_test.go -- a media server's webhook body, as LiveKit's SDK writes one,
// read as the event the calls service applies (events.go parseMediaEvent).
//
// WHY THE SDK AND NOT A HAND-WRITTEN BODY. LiveKit's notifier marshals a
// livekit.WebhookEvent with protojson (webhook/url_notifier.go send), and protojson's
// spelling is not what a hand-written fixture would guess: an int64 such as createdAt
// is a STRING, an enum such as a track's source is its NAME, and a field at its zero
// value is LEFT OUT, so a track whose source is unknown carries no source at all. The
// body here is made by the same marshaller, so a fixture cannot agree with a parser
// that LiveKit would not.
//
// The other half -- that the token LiveKit's SDK signs a body with is one the inbound
// receiver verifies -- needs the receiver, which this module may not import
// (docs/internal/ops/ci-design.md section D3: component/inbound sits above the
// integrations tier). It is app/transport_inbound_livekit_test.go
// TestALiveKitSignedRequestVerifies, beside the policy that wires the two together.

// TestTheAutomationReadsTheWebhookSource: the source the bff registers for the media
// server's webhooks (app/transport_inbound.go, from WebhookSource) and the one the
// applyMediaEvent automation's trigger filters on are one name. A drift is silent: the
// receiver stages every delivery, nothing fires on any of them, and only reconciliation's
// pass keeps the calls right, minutes late.
func TestTheAutomationReadsTheWebhookSource(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dsl", "calls", "automations.memql"))
	require.NoError(t, err)
	want := `@filter(row => row.source == "` + WebhookSource + `" && row.status == "received")`
	require.Contains(t, string(raw), want, "the applyMediaEvent automation does not filter on the source the media server's webhooks arrive under")
}

// liveKitBody is a webhook body exactly as LiveKit's notifier writes it.
func liveKitBody(t *testing.T, ev *livekit.WebhookEvent) []byte {
	t.Helper()
	raw, err := lkjson.Marshal(ev)
	require.NoError(t, err)
	return raw
}

// TestALiveKitWebhookBodyReadsAsItsEvent: each of the four events the calls service
// applies reads back as what LiveKit sent -- its kind, the media room by name, the
// participant by identity, the track by sid and source, and its time to the second --
// whatever protojson made of each. An event the service does not act on still reads,
// as itself, and a body that is no event at all does not.
func TestALiveKitWebhookBodyReadsAsItsEvent(t *testing.T) {
	at := time.Unix(1_763_400_000, 0).UTC()
	room := &livekit.Room{Sid: "RM_abc", Name: "room-short-id"}
	who := &livekit.ParticipantInfo{Sid: "PA_abc", Identity: "seat-identity", JoinedAt: at.Unix()}

	for _, tc := range []struct {
		name  string
		event *livekit.WebhookEvent
		want  mediaEvent
	}{
		{
			name:  "participant_joined",
			event: &livekit.WebhookEvent{Event: "participant_joined", Id: "EV_1", Room: room, Participant: who, CreatedAt: at.Unix()},
			want:  mediaEvent{ID: "EV_1", Kind: eventParticipantJoined, Room: "room-short-id", Identity: "seat-identity", ParticipantSID: "PA_abc", At: at},
		},
		{
			name:  "participant_left",
			event: &livekit.WebhookEvent{Event: "participant_left", Id: "EV_2", Room: room, Participant: who, CreatedAt: at.Unix() + 7},
			want:  mediaEvent{ID: "EV_2", Kind: eventParticipantLeft, Room: "room-short-id", Identity: "seat-identity", ParticipantSID: "PA_abc", At: at.Add(7 * time.Second)},
		},
		{
			name: "track_published, a screen share",
			event: &livekit.WebhookEvent{Event: "track_published", Id: "EV_3", Room: room, Participant: who, CreatedAt: at.Unix(),
				Track: &livekit.TrackInfo{Sid: "TR_share", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_SCREEN_SHARE}},
			want: mediaEvent{ID: "EV_3", Kind: eventTrackPublished, Room: "room-short-id", Identity: "seat-identity", ParticipantSID: "PA_abc", TrackSID: "TR_share", TrackSource: "screen_share", At: at},
		},
		{
			name: "track_published, the share's audio",
			event: &livekit.WebhookEvent{Event: "track_published", Id: "EV_4", Room: room, Participant: who, CreatedAt: at.Unix(),
				Track: &livekit.TrackInfo{Sid: "TR_audio", Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_SCREEN_SHARE_AUDIO}},
			want: mediaEvent{ID: "EV_4", Kind: eventTrackPublished, Room: "room-short-id", Identity: "seat-identity", ParticipantSID: "PA_abc", TrackSID: "TR_audio", TrackSource: "screen_share_audio", At: at},
		},
		{
			// protojson leaves a zero enum out, so this body has no "source" at all.
			name: "track_published, a source nobody named",
			event: &livekit.WebhookEvent{Event: "track_published", Id: "EV_5", Room: room, Participant: who, CreatedAt: at.Unix(),
				Track: &livekit.TrackInfo{Sid: "TR_unknown"}},
			want: mediaEvent{ID: "EV_5", Kind: eventTrackPublished, Room: "room-short-id", Identity: "seat-identity", ParticipantSID: "PA_abc", TrackSID: "TR_unknown", TrackSource: "unknown", At: at},
		},
		{
			name:  "room_finished",
			event: &livekit.WebhookEvent{Event: "room_finished", Id: "EV_6", Room: room, CreatedAt: at.Unix()},
			want:  mediaEvent{ID: "EV_6", Kind: eventRoomFinished, Room: "room-short-id", At: at},
		},
		{
			name:  "an event the service does not act on",
			event: &livekit.WebhookEvent{Event: "track_unpublished", Id: "EV_7", Room: room, Participant: who, CreatedAt: at.Unix()},
			want:  mediaEvent{ID: "EV_7", Kind: "track_unpublished", Room: "room-short-id", Identity: "seat-identity", ParticipantSID: "PA_abc", At: at},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := liveKitBody(t, tc.event)
			if tc.event.CreatedAt != 0 {
				require.Contains(t, string(body), `"createdAt":"`, "protojson no longer writes an int64 as a string: the reason this test builds its bodies with the SDK")
			}
			got, err := parseMediaEvent(body)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	// No time, and a body that is no event.
	untimed, err := parseMediaEvent(liveKitBody(t, &livekit.WebhookEvent{Event: "participant_left", Room: room, Participant: who}))
	require.NoError(t, err)
	require.True(t, untimed.At.IsZero(), "an event with no createdAt read as having a time")
	for _, body := range []string{``, `not json`, `[1,2]`, `{"event": 7}`} {
		_, err := parseMediaEvent([]byte(body))
		require.Error(t, err, "%q read as an event", body)
		require.False(t, strings.Contains(err.Error(), body) && body != "", "the error repeats the body it could not read")
	}
}
