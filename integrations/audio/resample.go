package audio

import (
	"encoding/binary"

	"github.com/zeozeozeo/gomplerate"
)

// Audio pipeline sample rate constants.
const (
	// PolyphonSampleRate is the standard sample rate used by the Polyphon pipeline
	// (LiveKit audio tracks, ASR providers, TTS consumers). All providers must
	// convert to/from this rate.
	PolyphonSampleRate = 16000

	// OpenAISampleRate is PCM16 24kHz mono (OpenAI Realtime API).
	OpenAISampleRate = 24000
)

// PCM16Resampler wraps gomplerate.Resampler for streaming PCM16 byte-level
// resampling. It maintains filter state across calls so that chunk boundaries
// do not introduce discontinuities (clicks/pops).
//
// Create one per audio stream session and reuse for all chunks in that session.
type PCM16Resampler struct {
	resampler *gomplerate.Resampler
}

// NewPCM16Resampler creates a resampler that converts PCM16 audio between
// the given sample rates. The resampler maintains internal filter state for
// artifact-free streaming.
func NewPCM16Resampler(fromRate, toRate int) (*PCM16Resampler, error) {
	r, err := gomplerate.NewResampler(1, fromRate, toRate)
	if err != nil {
		return nil, err
	}
	return &PCM16Resampler{resampler: r}, nil
}

// Resample converts PCM16 little-endian byte data from the source sample rate
// to the target sample rate configured at construction time.
//
// Input and output are raw PCM16 bytes (2 bytes per sample, little-endian).
// Returns nil for empty or single-byte input.
func (r *PCM16Resampler) Resample(input []byte) []byte {
	if len(input) < 2 {
		return nil
	}

	// Convert bytes to int16 samples.
	numSamples := len(input) / 2
	samples := make([]int16, numSamples)
	for i := 0; i < numSamples; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(input[i*2:]))
	}

	// Resample using persistent filter state.
	resampled := r.resampler.ResampleInt16(samples)

	// Convert back to little-endian bytes.
	output := make([]byte, len(resampled)*2)
	for i, s := range resampled {
		binary.LittleEndian.PutUint16(output[i*2:], uint16(s))
	}

	return output
}
