package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/znasllc-io/memql/component/polyphon"
)

// tts_pipeline.go adapts the OpenAI TTS client
// (integrations/openai/tts.go) onto the cascade's ttsSynthesizer seam.
// It is pure Go: it asks the TTS provider for linear16 PCM and chunks the
// response into fixed-size frames on a channel, aborting promptly when the
// playout context is cancelled (barge-in). The room-publish side
// (room_audio_voice.go) constructs the LiveKit PCMLocalTrack at the same
// source sample rate, so no manual resampling lives here -- the media-sdk
// encoder upsamples 16k PCM16 -> 48k Opus on publish (per
// docs/internal/design/voice-451-livekit-go-room-participation.md section 4).

// ttsPCMSampleRate is the PCM16 rate the OpenAI TTS client emits (it
// downsamples its native 24kHz output to 16kHz). The room local track is
// constructed with this same source rate.
const ttsPCMSampleRate = 16000

// ttsFrameBytes is one publish frame: 20 ms of 16 kHz mono PCM16 =
// 16000 * 0.020 * 2 bytes = 640 bytes. 20 ms is the LiveKit/Opus default
// frame duration, so each frame maps cleanly onto one encoder step.
const ttsFrameBytes = 640

// pcmSynthesizer is the polyphon.TTSProvider surface the adapter drives. It
// is satisfied by *openai.TTSClient.Synthesize (which returns PCM16 at
// ttsPCMSampleRate). Defined as a local interface so this file does not
// import the openai package directly, keeping the cascade glue decoupled
// from the concrete provider.
type pcmSynthesizer interface {
	Synthesize(ctx context.Context, config polyphon.TTSConfig) (io.ReadCloser, error)
}

// pcmTTSAdapter wraps a pcmSynthesizer as a cascade ttsSynthesizer.
type pcmTTSAdapter struct {
	provider pcmSynthesizer
	logger   *slog.Logger
}

// newPCMTTSAdapter builds the adapter. provider is typically a
// *openai.TTSClient.
func newPCMTTSAdapter(provider pcmSynthesizer, logger *slog.Logger) *pcmTTSAdapter {
	return &pcmTTSAdapter{provider: provider, logger: logger}
}

// SynthesizePCM synthesizes text at voiceID and streams 20 ms PCM16 frames
// onto the returned channel, closing it on completion or ctx cancel. The
// underlying TTS HTTP call streams its body, so frames begin flowing
// as soon as the first bytes land rather than after the full synthesis.
func (a *pcmTTSAdapter) SynthesizePCM(ctx context.Context, text, voiceID string) (<-chan []byte, error) {
	cfg := polyphon.DefaultTTSConfig(text, voiceID)
	cfg.SampleRate = ttsPCMSampleRate
	body, err := a.provider.Synthesize(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("voice-agent tts synthesize: %w", err)
	}

	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		defer body.Close()
		buf := make([]byte, ttsFrameBytes)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, readErr := io.ReadFull(body, buf)
			if n > 0 {
				frame := make([]byte, n)
				copy(frame, buf[:n])
				select {
				case out <- frame:
				case <-ctx.Done():
					return
				}
			}
			if readErr != nil {
				// io.EOF / ErrUnexpectedEOF (the trailing short frame)
				// are clean stream ends; anything else is logged.
				if readErr != io.EOF && readErr != io.ErrUnexpectedEOF && a.logger != nil {
					a.logger.Warn("voice-agent tts read failed", "err", readErr)
				}
				return
			}
		}
	}()
	return out, nil
}
