package agent

import (
	"github.com/znasllc-io/memql/component/polyphon"
)

// stt_pipeline.go holds the pure-Go STT glue for the cascade: the ASR
// session config the room layer opens against the existing Deepgram Nova-3
// client (integrations/deepgram/asr.go), carrying the production
// endpointing tuning the Python stt_plugin.py ports.
//
// The Deepgram client owns the WebSocket, the interim/final accumulation,
// the client-side EOU watchdog, and -- after the #455 additive change --
// surfaces SpeechStarted as a polyphon.ASRKindSpeechStarted result. This
// file only decides the per-session ASRConfig; the room glue
// (room_audio_voice.go) opens the stream and feeds it PCM, and the cascade
// orchestrator (cascade.go) consumes the result channel.

// sttSampleRate is the PCM16 sample rate the Deepgram ASR client expects
// (deepgram.asrSampleRate). The room remote-track decoder is configured to
// resample participant audio to this rate before feeding the STT stream.
const sttSampleRate = 16000

// asrConfigFor builds the per-session polyphon.ASRConfig for the cascade.
// The endpointing knobs (endpointing_ms / utterance_end_ms / the client
// watchdog) live on the Deepgram client's own Config (resolved from the
// POLYPHON_DEEPGRAM_* env family, the same names the Go agent's Config
// mirrors); this config carries only the per-stream language + rate so the
// agent's resolved DGLanguage flows through.
func asrConfigFor(cfg Config) polyphon.ASRConfig {
	language := cfg.DGLanguage
	if language == "" {
		language = "en-US"
	}
	return polyphon.ASRConfig{
		SampleRate: sttSampleRate,
		Language:   language,
	}
}
