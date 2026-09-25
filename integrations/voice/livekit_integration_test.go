package voice

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/znasllc-io/memql/core/audio"
)

// This opt-in media check complements the ordinary codec and forwarded-handler
// gates. Run against the local overlay through its public WSS/TURN front door.
func TestLiveKitPublicMediaRoundTrip(t *testing.T) {
	endpoint := os.Getenv("MEMQL_LIVEKIT_PUBLIC_URL")
	if endpoint == "" {
		t.Skip("set LiveKit public URL and credentials to check a running cluster")
	}
	transport, err := New(Config{URL: endpoint, PublicURL: endpoint, APIKey: os.Getenv("MEMQL_LIVEKIT_API_KEY"), APISecret: os.Getenv("MEMQL_LIVEKIT_API_SECRET")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	heard := make(chan int, 1)
	room, err := transport.Open(ctx, audio.RoomOptions{Name: fmt.Sprintf("ask-media-test-%d", time.Now().UnixNano()), Identity: "media-test"}, func(pcm []byte) {
		select {
		case heard <- len(pcm):
		default:
		}
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer room.Close()
	received := make(chan bool, 1)
	events := make(chan bool, 1)
	callback := lksdk.NewRoomCallback()
	callback.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
		go func() {
			packet, _, err := track.ReadRTP()
			if err == nil && len(packet.Payload) > 0 {
				select {
				case received <- true:
				default:
				}
			}
		}()
	}
	callback.OnDataReceived = func(data []byte, _ lksdk.DataReceiveParams) {
		if string(data) == `{"type":"test"}` {
			select {
			case events <- true:
			default:
			}
		}
	}
	credentials := room.Credentials()
	client, err := lksdk.ConnectToRoomWithToken(credentials.URL, credentials.Token, callback, lksdk.WithConnectTimeout(15*time.Second), lksdk.WithICETransportPolicy(webrtc.ICETransportPolicyRelay))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "test-mic", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Source: livekit.TrackSource_MICROPHONE}); err != nil {
		t.Fatal(err)
	}
	encoder, err := opus.NewEncoder(opus.WithSampleRate(48000), opus.WithChannels(1))
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 1920)
	for i := 0; i < 960; i++ {
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(8000*math.Sin(2*math.Pi*440*float64(i)/48000))))
	}
	packet := make([]byte, 4000)
	for i := 0; i < 50; i++ {
		n, err := encoder.Encode(frame, packet)
		if err != nil {
			t.Fatal(err)
		}
		if err = track.WriteSample(media.Sample{Data: packet[:n], Duration: 20 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case n := <-heard:
		if n == 0 {
			t.Fatal("empty microphone PCM")
		}
	case <-ctx.Done():
		t.Fatal("microphone did not reach Go adapter through public TURN")
	}
	if err = room.Publish(ctx, make([]byte, 24000*2), 24000); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-ctx.Done():
		t.Fatal("speech did not reach client through public TURN")
	}
	if err = room.Event(map[string]string{"type": "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("private room event did not reach client")
	}
}
