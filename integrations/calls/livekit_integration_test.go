package calls

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// TestLiveKitBackendRoundTrip is the opt-in check against a running media server,
// as integrations/voice's media check is. It skips unless the media environment is
// set; run it against the local media target with the same values the bff reads:
//
//	MEMQL_CALLS_MEDIA_API_URL, MEMQL_CALLS_MEDIA_PUBLIC_URL,
//	MEMQL_CALLS_MEDIA_API_KEY and MEMQL_CALLS_MEDIA_API_SECRET
//
// It creates a room, joins it with a pass MintPass minted, sees the participant and
// its published microphone through the backend, mutes that track, removes the
// participant and deletes the room, then deletes it again to show that a retried
// end is not a failure.
func TestLiveKitBackendRoundTrip(t *testing.T) {
	apiURL := os.Getenv(EnvMediaAPIURL)
	if apiURL == "" {
		t.Skip("set the MEMQL_CALLS_MEDIA_* values to check a running media server")
	}
	publicURL := os.Getenv(EnvMediaPublicURL)
	key, secretValue := os.Getenv(EnvMediaAPIKey), os.Getenv(EnvMediaAPISecret)
	backend, err := NewLiveKitBackend(apiURL, key, secretValue)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	room := fmt.Sprintf("calls-roundtrip-%d", time.Now().UnixNano())
	if err := backend.CreateRoom(ctx, room, 3); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	defer func() { _ = backend.DeleteRoom(context.Background(), room) }()
	if people, err := backend.Participants(ctx, room); err != nil || len(people) != 0 {
		t.Fatalf("a new room answered %v, %v; want nobody", people, err)
	}

	pass, err := MintPass(key, secretValue, callrules.KindPerson, room, "seat-roundtrip", "Round trip", nil, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := lksdk.ConnectToRoomWithToken(publicURL, pass, lksdk.NewRoomCallback(), lksdk.WithConnectTimeout(15*time.Second))
	if err != nil {
		t.Fatalf("joining with a minted pass: %v", err)
	}
	defer client.Disconnect()
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "calls-mic", "calls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Source: livekit.TrackSource_MICROPHONE}); err != nil {
		t.Fatalf("publishing a microphone the pass allows: %v", err)
	}

	var seen MediaParticipant
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		people, err := backend.Participants(ctx, room)
		if err != nil {
			t.Fatalf("Participants: %v", err)
		}
		if len(people) == 1 && len(people[0].Tracks) == 1 {
			seen = people[0]
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if seen.Identity != "seat-roundtrip" || seen.Kind != string(callrules.KindPerson) || seen.JoinedAt.IsZero() {
		t.Fatalf("the participant reads back as %+v", seen)
	}
	mic := seen.Tracks[0]
	if mic.Source != "microphone" || strings.TrimSpace(mic.SID) == "" {
		t.Fatalf("the published microphone reads back as %+v", mic)
	}

	if err := backend.MuteTrack(ctx, room, seen.Identity, mic.SID); err != nil {
		t.Fatalf("MuteTrack: %v", err)
	}
	if err := backend.RemoveParticipant(ctx, room, seen.Identity); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	if err := backend.DeleteRoom(ctx, room); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	if err := backend.DeleteRoom(ctx, room); err != nil {
		t.Fatalf("deleting a room that is already gone failed: %v", err)
	}
}
