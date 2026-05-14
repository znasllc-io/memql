package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestNewPCM16Resampler(t *testing.T) {
	r, err := NewPCM16Resampler(PolyphonSampleRate, OpenAISampleRate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil resampler")
	}
}

func TestPCM16Resampler_EmptyInput(t *testing.T) {
	r, err := NewPCM16Resampler(PolyphonSampleRate, OpenAISampleRate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result := r.Resample(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %d bytes", len(result))
	}

	result = r.Resample([]byte{})
	if result != nil {
		t.Errorf("expected nil for empty input, got %d bytes", len(result))
	}

	result = r.Resample([]byte{0x42}) // Single byte (invalid PCM16)
	if result != nil {
		t.Errorf("expected nil for single byte, got %d bytes", len(result))
	}
}

func TestPCM16Resampler_Upsample16kTo24k(t *testing.T) {
	r, err := NewPCM16Resampler(PolyphonSampleRate, OpenAISampleRate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Generate 100ms of 16kHz silence (1600 samples = 3200 bytes).
	numSamples := 1600
	input := make([]byte, numSamples*2)
	// All zeros = silence.

	output := r.Resample(input)
	if output == nil {
		t.Fatal("expected non-nil output for valid input")
	}

	// At 1.5x ratio (16k->24k), expect approximately 2400 samples = 4800 bytes.
	// gomplerate may produce slightly different counts due to filter state.
	expectedSamples := int(float64(numSamples) * float64(OpenAISampleRate) / float64(PolyphonSampleRate))
	expectedBytes := expectedSamples * 2
	tolerance := 100 // Allow some variance from filter.

	if abs(len(output)-expectedBytes) > tolerance {
		t.Errorf("expected ~%d bytes, got %d bytes", expectedBytes, len(output))
	}
}

func TestPCM16Resampler_Downsample24kTo16k(t *testing.T) {
	r, err := NewPCM16Resampler(OpenAISampleRate, PolyphonSampleRate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Generate 100ms of 24kHz tone (2400 samples = 4800 bytes).
	numSamples := 2400
	input := make([]byte, numSamples*2)

	// Fill with a 440Hz sine wave for a realistic test.
	for i := 0; i < numSamples; i++ {
		sample := int16(10000 * math.Sin(2*math.Pi*440*float64(i)/float64(OpenAISampleRate)))
		binary.LittleEndian.PutUint16(input[i*2:], uint16(sample))
	}

	output := r.Resample(input)
	if output == nil {
		t.Fatal("expected non-nil output for valid input")
	}

	// At 2/3 ratio (24k->16k), expect approximately 1600 samples = 3200 bytes.
	expectedSamples := int(float64(numSamples) * float64(PolyphonSampleRate) / float64(OpenAISampleRate))
	expectedBytes := expectedSamples * 2
	tolerance := 100

	if abs(len(output)-expectedBytes) > tolerance {
		t.Errorf("expected ~%d bytes, got %d bytes", expectedBytes, len(output))
	}
}

func TestPCM16Resampler_RoundTrip(t *testing.T) {
	// Upsample 16k -> 24k, then downsample 24k -> 16k.
	// Result should be similar in length to original.
	up, err := NewPCM16Resampler(PolyphonSampleRate, OpenAISampleRate)
	if err != nil {
		t.Fatalf("unexpected error creating upsampler: %v", err)
	}
	down, err := NewPCM16Resampler(OpenAISampleRate, PolyphonSampleRate)
	if err != nil {
		t.Fatalf("unexpected error creating downsampler: %v", err)
	}

	// 100ms of 16kHz tone.
	numSamples := 1600
	input := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		sample := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(PolyphonSampleRate)))
		binary.LittleEndian.PutUint16(input[i*2:], uint16(sample))
	}

	upsampled := up.Resample(input)
	roundTripped := down.Resample(upsampled)

	// Length should be approximately the same as input.
	tolerance := 200
	if abs(len(roundTripped)-len(input)) > tolerance {
		t.Errorf("round-trip length mismatch: input=%d, output=%d", len(input), len(roundTripped))
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
