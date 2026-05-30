package agent

// voice_resolve.go maps a Persona's canonical voice (alto / tenor / ...) to
// the provider-specific voice id the executor hands to the TTS / realtime
// client. It is the Go analog of the voice-selection halves of
// persona_resolver.py (_resolve_tts_voice) and realtime_instructions.py
// (resolve_realtime_voice).
//
// CRITICAL: the canonical voice catalog is OWNED by integrations/voice
// (voices.go). The Python agent kept LOCAL mirror maps (DEEPGRAM_AURA2_VOICES,
// OPENAI_REALTIME_VOICES) to avoid a network call; the Go agent runs in the
// same module as the catalog, so we CONSUME voice.ResolveVoice directly and
// do NOT fork the mapping. This satisfies the issue's "voice catalog stays the
// single source of truth" acceptance criterion -- any catalog churn lands in
// voices.go and flows here for free.

import (
	"github.com/znasllc-io/memql/integrations/voice"
)

// ResolveTTSVoice maps the persona's canonical voice to the provider voice id
// for the active TTS provider (deepgram / openai, per POLYPHON_VOICE_PROVIDER /
// MEMQL_DEEPGRAM_API_KEY -- voice.ActiveProvider). This is the cascade path's
// voice (#455 consumes it at TTS synthesis time).
//
// voice.ResolveVoice never returns "" for a known provider (it falls back to a
// gender-appropriate default), so the audio path can never go silent for an
// unknown canonical -- matching _resolve_tts_voice's "fall back to alto".
func ResolveTTSVoice(p Persona) string {
	return voice.ResolveVoice(p.CanonicalVoice, voice.ActiveProvider())
}

// ResolveRealtimeVoice maps the persona's canonical voice to the OpenAI
// gpt-realtime voice id (#457 consumes it as the realtime session voice).
// It resolves through the SAME catalog via the "openai-realtime" provider key,
// which voice.ResolveVoice routes onto OpenAI's voice set -- so the realtime
// path and the catalog can never drift.
//
// Note: this consumes the canonical -> OpenAI mapping in voices.go rather than
// forking the Python module's separate OPENAI_REALTIME_VOICES table; keeping a
// single source of truth is the explicit intent of #456.
func ResolveRealtimeVoice(p Persona) string {
	return voice.ResolveVoice(p.CanonicalVoice, "openai-realtime")
}
