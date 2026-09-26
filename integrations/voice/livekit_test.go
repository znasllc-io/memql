package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/pion/opus"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/znasllc-io/memql/core/audio"
	"math"
	"testing"
	"time"
)

func TestVoiceCodecRoundTripAtMicrophoneRate(t *testing.T) {
	encoder, err := opus.NewEncoder(opus.WithSampleRate(48000), opus.WithChannels(1), opus.WithBitrate(48000))
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := opus.NewDecoderWithOutput(24000, 1)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 1920)
	for i := 0; i < 960; i++ {
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(8000*math.Sin(2*math.Pi*440*float64(i)/48000))))
	}
	packet := make([]byte, 4000)
	samples := make([]int16, 2880)
	energy := 0.0
	for i := 0; i < 5; i++ {
		n, err := encoder.Encode(frame, packet)
		if err != nil {
			t.Fatal(err)
		}
		count, err := decoder.DecodeToInt16(packet[:n], samples)
		if err != nil {
			t.Fatal(err)
		}
		if count != 480 {
			t.Fatalf("20ms at 24kHz decoded %d samples", count)
		}
		for _, sample := range samples[:count] {
			energy += float64(sample) * float64(sample)
		}
	}
	if energy == 0 {
		t.Fatal("voice codec returned silence")
	}
}

func TestPlaybackPauseSendsSilenceWithoutLosingSpeech(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gate := &audio.PlaybackGate{}
	gate.Pause(time.Minute)
	ctx = audio.WithPlaybackGate(ctx, gate)
	pcm := make([]byte, 3*1920)
	for i := 0; i < len(pcm)/2; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(8000*math.Sin(2*math.Pi*440*float64(i)/48000))))
	}
	ticks := make(chan time.Time)
	written := make(chan media.Sample, 1)
	done := make(chan error, 1)
	go func() {
		done <- publishPCM(ctx, ctx, pcm, ticks, func(sample media.Sample) error {
			sample.Data = append([]byte(nil), sample.Data...)
			written <- sample
			return nil
		})
	}()
	decoder, err := opus.NewDecoderWithOutput(48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]int16, 960)
	for frame := 0; frame < 5; frame++ {
		if frame == 2 {
			gate.Resume()
		}
		select {
		case ticks <- time.Now():
		case err := <-done:
			t.Fatalf("speech finished before all retained frames were sent: %v", err)
		case <-ctx.Done():
			t.Fatal("publisher stopped advancing its clock")
		}
		var sample media.Sample
		select {
		case sample = <-written:
		case <-ctx.Done():
			t.Fatal("media tick produced no packet")
		}
		if sample.Duration != 20*time.Millisecond {
			t.Fatalf("media clock step = %s", sample.Duration)
		}
		n, err := decoder.DecodeToInt16(sample.Data, samples)
		if err != nil {
			t.Fatal(err)
		}
		if n != 960 {
			t.Fatalf("decoded frame has %d samples", n)
		}
		var energy float64
		for _, sample := range samples {
			energy += float64(sample) * float64(sample)
		}
		if frame < 2 && energy > 1000 {
			t.Fatal("paused speech reached the receiver")
		}
		if frame >= 2 && energy < 100000 {
			t.Fatal("retained speech did not resume")
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("publisher did not finish after retained speech")
	}
}

func TestPausedPlaybackStillHonorsCancellation(t *testing.T) {
	for _, roomCancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		gate := &audio.PlaybackGate{}
		gate.Pause(time.Minute)
		playCtx, roomCtx := audio.WithPlaybackGate(ctx, gate), context.Background()
		if roomCancelled {
			playCtx, roomCtx = audio.WithPlaybackGate(context.Background(), gate), ctx
		}
		cancel()
		err := publishPCM(playCtx, roomCtx, make([]byte, 1920), make(chan time.Time), func(media.Sample) error {
			t.Error("cancelled playback emitted a packet")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	}
}
