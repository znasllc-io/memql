package agent

import (
	"github.com/znasllc-io/memql/component/polyphon"
)

// stt_pipeline.go holds the pure-Go STT glue for the cascade: the ASR
// session config the room layer opens against the OpenAI Realtime
// transcription client (integrations/openai/asr.go).
//
// The OpenAI client owns the WebSocket, the interim/final accumulation,
// and surfaces the server-VAD onset as a polyphon.ASRKindSpeechStarted
// result. This file only decides the per-session ASRConfig; the room glue
// (room_audio_voice.go) opens the stream and feeds it PCM, and the cascade
// orchestrator (cascade.go) consumes the result channel.

// sttSampleRate is the PCM16 sample rate the ASR client expects on
// SendAudio (the OpenAI client upsamples to its 24kHz wire format
// internally). The room remote-track decoder is configured to resample
// participant audio to this rate before feeding the STT stream.
const sttSampleRate = 16000

// asrConfigFor builds the per-session polyphon.ASRConfig for the cascade.
// The end-of-utterance tuning lives on the OpenAI client's own session
// config (POLYPHON_OPENAI_VAD_SILENCE_MS); this config carries only the
// per-stream language + rate so the agent's resolved VoiceLanguage flows
// through.
func asrConfigFor(cfg Config) polyphon.ASRConfig {
	language := cfg.VoiceLanguage
	if language == "" {
		language = "en"
	}
	return polyphon.ASRConfig{
		SampleRate: sttSampleRate,
		Language:   language,
	}
}
