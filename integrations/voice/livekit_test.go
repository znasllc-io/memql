package voice

import (
	"encoding/binary"
	"github.com/pion/opus"
	"math"
	"testing"
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
