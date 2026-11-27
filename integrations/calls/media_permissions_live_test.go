package calls

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/server"
)

type permissionSignalFixture func(context.Context, string) (*callrules.MediaSignalAdmission, error)

func (f permissionSignalFixture) BeginMediaSignal(ctx context.Context, token string) (*callrules.MediaSignalAdmission, error) {
	return f(ctx, token)
}

func (f permissionSignalFixture) CheckMediaSignal(ctx context.Context, token string) error {
	_, err := f(ctx, token)
	return err
}

// This qualifies the actual pinned provider's permission-update behavior through
// the fixed signalling transport. Authority and grant selection are an explicit
// fixture, not an assertion about native presenter policy. An original broad
// JWT is retained while the fixture remints current restricted grants on reentry.
// No persistent listener, generic proxy or production bypass is installed.
func TestMediaPermissionsPinnedLocalNarrowRestoreAndReentry(t *testing.T) {
	if os.Getenv("MEMQL_TEST_MEDIA_PERMISSIONS_LOCAL") != "1" {
		t.Skip("opt in to disposable k3d-memql provider permission qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lksdk.SetLogger(logger.LogRLogger(logr.Discard()))
	defer lksdk.SetLogger(logger.GetLogger())
	raw, err := exec.CommandContext(ctx, "kubectl", "--context", "k3d-memql", "-n", "memql", "get", "secret", "memql-calls-media", "-o", "json").Output()
	if err != nil {
		t.Fatal("local media Secret unavailable")
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(raw, &secret) != nil {
		t.Fatal("local media Secret malformed")
	}
	read := func(name string) string {
		v, e := base64.StdEncoding.DecodeString(secret.Data[name])
		if e != nil {
			t.Fatal("local media Secret malformed")
		}
		return string(v)
	}
	if read(EnvMediaAPIURL) != "http://host.k3d.internal:7880" {
		t.Fatal("refusing nonlocal media configuration")
	}
	key, value := read(EnvMediaAPIKey), read(EnvMediaAPISecret)
	if key == "" || value == "" {
		t.Fatal("local media credentials missing")
	}
	control := lksdk.NewRoomServiceClient("http://127.0.0.1:7880", key, value)
	backend, err := NewLiveKitBackend("http://127.0.0.1:7880", key, value)
	if err != nil {
		t.Fatal("local backend configuration failed")
	}
	permissions, ok := backend.(MediaPermissionBackend)
	if !ok {
		t.Fatal("provider permission seam missing")
	}
	room := fmt.Sprintf("callkey-permissions-fixture-%d", time.Now().UnixNano())
	const identity = "permission-fixture"
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, e := control.DeleteRoom(cleanup, &livekit.DeleteRoomRequest{Room: room}); e != nil {
			t.Error("fixture room deletion failed")
		}
		left, e := control.ListRooms(cleanup, &livekit.ListRoomsRequest{Names: []string{room}})
		if e != nil || len(left.GetRooms()) != 0 {
			t.Error("fixture cleanup readback failed")
		}
	}()
	if _, e := control.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: room, MaxParticipants: 2, EmptyTimeout: 10, DepartureTimeout: 5}); e != nil {
		t.Fatal("fixture room creation failed")
	}
	full, _ := callrules.GrantFor(callrules.KindPerson)
	full.CanPublishData = false
	narrow := full
	narrow.Sources = []string{"microphone", "camera"}
	mint := func(spec callrules.GrantSpec, who string) (string, error) {
		grant := &lkauth.VideoGrant{RoomJoin: true, Room: room, CanPublishSources: slices.Clone(spec.Sources)}
		grant.SetCanPublish(spec.CanPublish)
		grant.SetCanSubscribe(spec.CanSubscribe)
		grant.SetCanPublishData(false)
		grant.SetCanUpdateOwnMetadata(false)
		return lkauth.NewAccessToken(key, value).SetIdentity(who).SetVideoGrant(grant).SetValidFor(2 * time.Minute).ToJWT()
	}
	issued, err := mint(full, identity)
	if err != nil {
		t.Fatal("fixture token creation failed")
	}
	observerSpec := full
	observerSpec.Sources = []string{"microphone"}
	observerToken, err := mint(observerSpec, "subscription-fixture")
	if err != nil {
		t.Fatal("fixture observer token creation failed")
	}
	var current atomic.Pointer[callrules.GrantSpec]
	current.Store(&full)
	guard := sha256.Sum256([]byte("memql-media-signal-v1:" + value))
	h := httptest.NewServer(server.NewMediaSignalHandler(permissionSignalFixture(func(_ context.Context, token string) (*callrules.MediaSignalAdmission, error) {
		who, spec := identity, *current.Load()
		if token == observerToken {
			who, spec = "subscription-fixture", observerSpec
		} else if token != issued {
			return nil, errors.New("unrecognized fixture credential")
		}
		upstream, e := mint(spec, who)
		if e != nil {
			return nil, errors.New("fixture remint failed")
		}
		return &callrules.MediaSignalAdmission{UpstreamURL: "http://127.0.0.1:7880", UpstreamToken: upstream, UpstreamGuard: hex.EncodeToString(guard[:]), Room: room, Identity: who, ExpiresAt: time.Now().Add(15 * time.Second), Forward: func(ctx context.Context, run func(context.Context) error) error { return run(ctx) }, Confirm: func(context.Context) error { return nil }, Close: func(context.Context, bool) error { return nil }}, nil
	})))
	defer h.Close()
	var received atomic.Int64
	observation := make(chan lksdk.DataReceiveParams, 2)
	var nonrecipientPackets atomic.Int64
	var delayedScreenWindow atomic.Bool
	var delayedScreenReceived atomic.Int64
	connect := func() *lksdk.Room {
		t.Helper()
		callbacks := lksdk.NewRoomCallback()
		callbacks.OnDataReceived = func(data []byte, params lksdk.DataReceiveParams) {
			if params.Topic == callrules.ObservationProtocol && string(data) == "synthetic observation fixture" {
				select {
				case observation <- params:
				default:
				}
			}
		}
		callbacks.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, participant *lksdk.RemoteParticipant) {
			if participant.Identity() != "subscription-fixture" {
				return
			}
			go func() {
				_ = track.SetReadDeadline(time.Now().Add(80 * time.Second))
				for {
					if _, _, err := track.ReadRTP(); err != nil {
						return
					}
					received.Add(1)
				}
			}()
		}
		r, e := lksdk.ConnectToRoomWithToken(h.URL+"/media", issued, callbacks, lksdk.WithConnectTimeout(15*time.Second), lksdk.WithLogger(logger.LogRLogger(logr.Discard())))
		if e != nil {
			t.Fatal("fixture SDK connection failed")
		}
		if r.ServerInfo().GetVersion() != "1.13.7" {
			r.Disconnect()
			t.Fatal("provider is not the reviewed1.13.7 pin")
		}
		return r
	}
	client := connect()
	defer func() { client.Disconnect() }()
	observerCallbacks := lksdk.NewRoomCallback()
	observerCallbacks.OnTrackSubscribed = func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
		if publication.Source() != livekit.TrackSource_SCREEN_SHARE {
			return
		}
		go func() {
			_ = track.SetReadDeadline(time.Now().Add(80 * time.Second))
			for {
				if _, _, e := track.ReadRTP(); e != nil {
					return
				}
				if delayedScreenWindow.Load() {
					delayedScreenReceived.Add(1)
				}
			}
		}()
	}
	observerCallbacks.OnDataReceived = func(_ []byte, params lksdk.DataReceiveParams) {
		if params.Topic == callrules.ObservationProtocol {
			nonrecipientPackets.Add(1)
		}
	}
	observer, err := lksdk.ConnectToRoomWithToken(h.URL+"/media", observerToken, observerCallbacks, lksdk.WithConnectTimeout(15*time.Second), lksdk.WithLogger(logger.LogRLogger(logr.Discard())))
	if err != nil {
		t.Fatal("subscription fixture SDK connection failed")
	}
	defer observer.Disconnect()
	remoteMic, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "subscription-mic", "fixture")
	if err != nil {
		t.Fatal("subscription fixture microphone creation failed")
	}
	if _, err = observer.LocalParticipant.PublishTrack(remoteMic, &lksdk.TrackPublicationOptions{Source: livekit.TrackSource_MICROPHONE}); err != nil {
		t.Fatal("subscription fixture microphone publication failed")
	}
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				_ = remoteMic.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
			}
		}
	}()
	trackCount := 0
	// One synthetic16x16 black VP8 keyframe generated with libvpx through
	// PyAV; this fixture checks source permissions, not video decode quality.
	blackFrame, _ := hex.DecodeString("1002009d012a100010000047088585888584880202000c0d6000feffab5080")
	publish := func(source livekit.TrackSource) error {
		trackCount++
		codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
		if source == livekit.TrackSource_MICROPHONE {
			codec = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
		}
		track, e := webrtc.NewTrackLocalStaticSample(codec, fmt.Sprintf("fixture-track-%d", trackCount), "fixture")
		if e != nil {
			return errors.New("fixture track creation failed")
		}
		_, e = client.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Source: source, Name: "permission fixture"})
		if e == nil {
			payload, period := blackFrame, 200*time.Millisecond
			if source == livekit.TrackSource_MICROPHONE {
				payload, period = []byte{0xf8, 0xff, 0xfe}, 20*time.Millisecond
			}
			go func() {
				ticker := time.NewTicker(period)
				defer ticker.Stop()
				for {
					select {
					case <-streamCtx.Done():
						return
					case <-ticker.C:
						_ = track.WriteSample(media.Sample{Data: payload, Duration: period})
					}
				}
			}()
		}
		return e
	}
	readParticipant := func() *livekit.ParticipantInfo {
		t.Helper()
		result, e := control.GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
		if e != nil {
			t.Fatal("provider participant readback failed")
		}
		return result
	}
	contains := func(participant *livekit.ParticipantInfo, source livekit.TrackSource) bool {
		for _, track := range participant.Tracks {
			if track.Source == source {
				return true
			}
		}
		return false
	}
	awaitTracks := func(expected []livekit.TrackSource) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p := readParticipant()
			match := len(p.Tracks) == len(expected)
			for _, source := range expected {
				match = match && contains(p, source)
			}
			if match {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		var got []string
		for _, track := range readParticipant().Tracks {
			got = append(got, track.Source.String())
		}
		t.Fatalf("provider track state did not match current permissions: expected=%v actual=%v", expected, got)
	}
	if err := publish(livekit.TrackSource_MICROPHONE); err != nil {
		t.Fatal("allowed microphone publication failed")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	// AddTrack is acknowledged while broad authority is valid, but withhold
	// every screen RTP packet until after narrowing and its provider readback.
	// This exercises a queued publication, not merely a later AddTrack refusal.
	delayedScreen, e := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "delayed-screen", "fixture")
	if e != nil {
		t.Fatal("delayed screen fixture creation failed")
	}
	delayedPublication, e := client.LocalParticipant.PublishTrack(delayedScreen, &lksdk.TrackPublicationOptions{Source: livekit.TrackSource_SCREEN_SHARE, Name: "delayed screen fixture"})
	if e != nil {
		t.Fatal("broad-authority delayed AddTrack acknowledgement failed")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	current.Store(&narrow)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, narrow); e != nil {
		t.Fatal("delayed screen permission narrowing/readback failed")
	}
	delayedScreenWindow.Store(true)
	delayedStop := make(chan struct{})
	defer func() { delayedScreenWindow.Store(false) }()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-delayedStop:
				return
			case <-ticker.C:
				_ = delayedScreen.WriteSample(media.Sample{Data: blackFrame, Duration: 20 * time.Millisecond})
			}
		}
	}()
	defer func() {
		select {
		case <-delayedStop:
		default:
			close(delayedStop)
		}
	}()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if contains(readParticipant(), livekit.TrackSource_SCREEN_SHARE) {
			t.Fatal("queued screen appeared in provider inventory after narrowed permission readback")
		}
		if delayedScreenReceived.Load() != 0 {
			t.Fatal("queued screen reached a subscriber after narrowed permission readback")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(delayedStop)
	delayedScreenWindow.Store(false)
	_ = client.LocalParticipant.UnpublishTrack(delayedPublication.SID())
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	t.Log("queued AddTrack followed by narrowing and delayed screen RTP produced no observed screen inventory or subscriber RTP over 2 seconds")
	current.Store(&full)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, full); e != nil {
		t.Fatal("delayed screen fixture permission restoration failed")
	}
	if err := publish(livekit.TrackSource_SCREEN_SHARE); err != nil {
		t.Fatal("initial screen publication failed")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_SCREEN_SHARE})
	current.Store(&narrow)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, narrow); e != nil {
		t.Fatal("provider permission narrowing/readback failed")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	if err := publish(livekit.TrackSource_SCREEN_SHARE); !errors.Is(err, lksdk.ErrTrackPublishTimeout) {
		t.Fatal("restricted screen did not receive the expected provider publication refusal")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	if err := publish(livekit.TrackSource_CAMERA); err != nil {
		t.Fatal("allowed camera publication failed after narrowing")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_CAMERA})
	t.Log("narrowing removed screen, denied republish, retained microphone and camera")
	current.Store(&full)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, full); e != nil {
		t.Fatal("provider permission restoration/readback failed")
	}
	// A permission change may enable future publication, never start a source.
	time.Sleep(500 * time.Millisecond)
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_CAMERA})
	if err := publish(livekit.TrackSource_SCREEN_SHARE); err != nil {
		t.Fatal("restored screen publication failed")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE})
	audience := full
	audience.CanPublish = false
	audience.Sources = []string{}
	current.Store(&audience)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, audience); e != nil {
		t.Fatal("audience permission update/readback failed")
	}
	awaitTracks(nil)
	before := received.Load()
	deadline := time.Now().Add(5 * time.Second)
	for received.Load() < before+5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if received.Load() < before+5 {
		t.Fatal("audience lost subscription to the other participant's RTP")
	}
	t.Logf("audience retained subscription: %d additional RTP packets with zero published tracks", received.Load()-before)
	// Provider-only destination/origin check. The second destination is
	// deliberately absent; native consent/admission is tested separately.
	if e := backend.(observationPublisher).PublishObservation(ctx, room, []byte("synthetic observation fixture"), []string{identity, "absent-fixture"}); e != nil {
		t.Fatal("fixed observation server forwarding failed")
	}
	select {
	case packet := <-observation:
		if packet.Sender != nil || packet.SenderIdentity != "" {
			t.Fatal("server observation carried a participant sender")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scoped server observation was not delivered")
	}
	time.Sleep(250 * time.Millisecond)
	if nonrecipientPackets.Load() != 0 {
		t.Fatal("observation reached a connected nonrecipient")
	}
	t.Log("fixed observation topic arrived with empty sender; connected nonrecipient received zero packets")
	current.Store(&full)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, full); e != nil {
		t.Fatal("audience permission restoration failed")
	}
	time.Sleep(500 * time.Millisecond)
	awaitTracks(nil)
	t.Log("restoration left all sources off until explicit publication")
	current.Store(&narrow)
	if _, e := permissions.UpdatePermissions(ctx, room, identity, narrow); e != nil {
		t.Fatal("second permission narrowing failed")
	}
	client.Disconnect()
	// The SDK sends Leave itself. Read that completed removal instead of
	// racing a second RemoveParticipant RPC against the disappearing peer.
	absent := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		people, e := control.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
		if e != nil {
			t.Fatal("disconnected fixture readback failed")
		}
		absent = true
		for _, person := range people.Participants {
			if person.Identity == identity {
				absent = false
			}
		}
		if absent {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !absent {
		t.Fatal("disconnected fixture remained in provider room")
	}
	client = connect() // original broad JWT, recomputed narrow upstream permission
	if got := participantPermissionSpec(readParticipant().GetPermission()); !slices.Equal(got.Sources, narrow.Sources) {
		t.Fatal("stale JWT restored old source authority on reconnect")
	}
	if err := publish(livekit.TrackSource_SCREEN_SHARE); !errors.Is(err, lksdk.ErrTrackPublishTimeout) {
		t.Fatal("stale broad JWT screen did not receive provider publication refusal after reentry")
	}
	awaitTracks(nil)
	if err := publish(livekit.TrackSource_MICROPHONE); err != nil {
		t.Fatal("reentered current microphone grant failed")
	}
	awaitTracks([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	t.Log("original broad JWT reentry retained current narrow grant; screen denied, microphone worked")
}
