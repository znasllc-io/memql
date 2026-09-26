// Package voice is MemQL's Go LiveKit media adapter. It owns no model calls,
// business tools or transcript storage; those are supplied by the engine.
package voice

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/znasllc-io/memql/core/audio"
)

type Config struct{ URL, PublicURL, APIKey, APISecret string }
type Transport struct{ config Config }

func New(config Config) (*Transport, error) {
	if config.URL == "" || config.PublicURL == "" || config.APIKey == "" || len(config.APISecret) < 32 {
		return nil, fmt.Errorf("LiveKit requires internal/public URLs, an API key and a secret of at least 32 characters")
	}
	return &Transport{config: config}, nil
}

type room struct {
	sdk         *lksdk.Room
	track       *webrtc.TrackLocalStaticSample
	credentials audio.RoomCredentials
	identity    string
	ctx         context.Context
	cancel      context.CancelFunc
	once        sync.Once
	publishMu   sync.Mutex
	inputActive atomic.Bool
}

func (t *Transport) Open(ctx context.Context, options audio.RoomOptions, onAudio func([]byte), disconnected func()) (audio.Room, error) {
	ctx, cancel := context.WithCancel(ctx)
	r := &room{identity: options.Identity, ctx: ctx, cancel: cancel}
	client := lksdk.NewRoomServiceClient(t.config.URL, t.config.APIKey, t.config.APISecret)
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: options.Name, EmptyTimeout: 30, DepartureTimeout: 10, MaxParticipants: 2})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create voice room: %w", err)
	}
	joined := make(chan struct{})
	var joinOnce sync.Once
	callback := lksdk.NewRoomCallback()
	callback.OnParticipantConnected = func(p *lksdk.RemoteParticipant) {
		if p.Identity() == options.Identity {
			joinOnce.Do(func() { close(joined) })
		}
	}
	callback.OnDisconnected = func() { cancel(); disconnected() }
	callback.OnParticipantDisconnected = func(p *lksdk.RemoteParticipant) {
		if p.Identity() == options.Identity {
			cancel()
			disconnected()
		}
	}
	callback.OnTrackSubscribed = func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, p *lksdk.RemoteParticipant) {
		if p.Identity() != options.Identity || publication.Source() != livekit.TrackSource_MICROPHONE || track.Kind() != webrtc.RTPCodecTypeAudio || !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
			return
		}
		if !r.inputActive.CompareAndSwap(false, true) {
			return
		}
		go func() { defer r.inputActive.Store(false); r.receive(track, onAudio) }()
	}
	r.sdk, err = lksdk.ConnectToRoom(t.config.URL, lksdk.ConnectInfo{APIKey: t.config.APIKey, APISecret: t.config.APISecret, RoomName: options.Name, ParticipantIdentity: "memql", ParticipantName: "MemQL"}, callback, lksdk.WithConnectTimeout(10*time.Second))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("join voice room: %w", err)
	}
	if err = ctx.Err(); err != nil {
		r.Close()
		return nil, err
	}
	r.track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "memql-voice", "memql")
	if err == nil {
		_, err = r.sdk.LocalParticipant.PublishTrack(r.track, &lksdk.TrackPublicationOptions{Name: "MemQL", Source: livekit.TrackSource_MICROPHONE})
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	yes, no := true, false
	grant := &lkauth.VideoGrant{RoomJoin: true, Room: options.Name, CanPublish: &yes, CanSubscribe: &yes, CanPublishData: &no, CanUpdateOwnMetadata: &no}
	grant.SetCanPublishSources([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	token, err := lkauth.NewAccessToken(t.config.APIKey, t.config.APISecret).SetIdentity(options.Identity).SetName("You").SetVideoGrant(grant).SetValidFor(2 * time.Minute).ToJWT()
	if err != nil {
		r.Close()
		return nil, err
	}
	r.credentials = audio.RoomCredentials{URL: t.config.PublicURL, Token: token, Room: options.Name}
	go func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-joined:
		case <-timer.C:
			r.Close()
			disconnected()
			return
		case <-ctx.Done():
			r.Close()
			return
		}
		<-ctx.Done()
		r.Close()
	}()
	return r, nil
}
func (r *room) Credentials() audio.RoomCredentials { return r.credentials }
func (r *room) Event(event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(data) > 15000 {
		return fmt.Errorf("voice event exceeds data packet limit")
	}
	return r.sdk.LocalParticipant.PublishDataPacket(lksdk.UserData(data), lksdk.WithDataPublishReliable(true), lksdk.WithDataPublishDestination([]string{r.identity}))
}
func (r *room) Close() {
	r.once.Do(func() {
		r.cancel()
		if r.sdk != nil {
			r.sdk.Disconnect()
		}
	})
}
func (r *room) receive(track *webrtc.TrackRemote, onAudio func([]byte)) {
	decoder, err := opus.NewDecoderWithOutput(24000, 1)
	if err != nil {
		return
	}
	// Opus packets can contain up to 120 ms. WebRTC supplies NACK recovery;
	// discard duplicates and late packets.
	samples := make([]int16, 2880)
	var sequence uint16
	initialized := false
	for {
		packet, _, err := track.ReadRTP()
		if err != nil || r.ctx.Err() != nil {
			return
		}
		if initialized && int16(packet.SequenceNumber-sequence) <= 0 {
			continue
		}
		sequence, initialized = packet.SequenceNumber, true
		n, err := decoder.DecodeToInt16(packet.Payload, samples)
		if err != nil {
			continue
		}
		pcm := make([]byte, n*2)
		for i, sample := range samples[:n] {
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
		}
		onAudio(pcm)
	}
}
func (r *room) Publish(ctx context.Context, pcm []byte, sampleRate int) error {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if sampleRate != 48000 {
		resampler, err := audio.NewPCM16Resampler(sampleRate, 48000)
		if err != nil {
			return err
		}
		pcm = resampler.Resample(pcm)
	}
	encoder, err := opus.NewEncoder(opus.WithSampleRate(48000), opus.WithChannels(1), opus.WithBitrate(48000))
	if err != nil {
		return err
	}
	const frameBytes = 960 * 2
	packet := make([]byte, 4000)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for len(pcm) > 0 {
		if err := audio.WaitForPlayback(ctx); err != nil {
			return err
		}
		frame := pcm[:min(frameBytes, len(pcm))]
		pcm = pcm[len(frame):]
		if len(frame) < frameBytes {
			padded := make([]byte, frameBytes)
			copy(padded, frame)
			frame = padded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-tick.C:
		}
		n, err := encoder.Encode(frame, packet)
		if err != nil {
			return err
		}
		if err = r.track.WriteSample(media.Sample{Data: packet[:n], Duration: 20 * time.Millisecond}); err != nil {
			return err
		}
	}
	return nil
}
