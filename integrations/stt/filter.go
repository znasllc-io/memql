package stt

import (
	"os"
	"strconv"
	"strings"
)

// Environment knobs that harden the streaming transcription path against
// the two live-testing failure modes: wrong/mixed-language output and
// short-audio hallucinations. Both are read once at process start via the
// resolver helpers below and apply to whichever provider MEMQL_STT_PROVIDER
// selects (the filtering lives at the provider-agnostic pumpDeltas
// chokepoint).
const (
	// EnvLanguage hard-pins the transcription language on the live
	// streaming request, overriding any client-supplied LanguageHint by
	// default. Sourced as a single knob so one setting drives BOTH the
	// Deepgram stream URL (language=en-US) and the OpenAI Realtime session
	// config (language=en). Default "en".
	//
	// The classic cause of multi-language + short-word hallucination is an
	// unpinned language: gpt-4o-transcribe/whisper and Deepgram Nova-3
	// will auto-detect and "drift" to another language on noisy/short
	// audio. Pinning it server-side fixes that regardless of what the
	// browser passes.
	EnvLanguage = "MEMQL_STT_LANGUAGE"

	// EnvMinConfidence is the floor a FINAL transcript's confidence must
	// clear to be emitted to the client. Deepgram exposes a real
	// per-alternative confidence; OpenAI Realtime has none and stamps 1.0
	// on finals (so they always clear the floor and rely on server-VAD +
	// the empty/denylist filters instead). Default 0.6.
	EnvMinConfidence = "MEMQL_STT_MIN_CONFIDENCE"

	// defaultLanguage is the pinned-language default. Bare ISO-639-1 "en";
	// the per-provider adapter expands it (Deepgram wants en-US, OpenAI
	// wants en) so both lanes share this one knob.
	defaultLanguage = "en"

	// defaultMinConfidence is the low-confidence final cutoff. 0.6 is a
	// sensible threshold for Deepgram Nova-3: genuine speech sits well
	// above it, while noise/silence hallucinations come back at or below
	// it. Tunable via EnvMinConfidence.
	defaultMinConfidence float32 = 0.6
)

// silenceHallucinationDenylist is the set of well-known phrases that
// Whisper/Realtime/Deepgram fabricate from silence or non-speech audio.
// These are normalized (lowercase, trimmed, trailing punctuation stripped)
// before comparison. They are dropped ONLY when the no-speech / low-
// confidence signal also fires -- never on the word alone -- so a genuine
// short "okay" or "thank you" the user really said is preserved.
var silenceHallucinationDenylist = map[string]struct{}{
	"thank you":                  {},
	"thanks for watching":        {},
	"thank you for watching":     {},
	"thanks for watching!":       {},
	"please subscribe":           {},
	"like and subscribe":         {},
	"you":                        {},
	"bye":                        {},
	"bye bye":                    {},
	"thank you very much":        {},
	"thank you so much":          {},
	"subtitles by the amara.org": {},
}

// ResolveLanguage returns the hard-pinned transcription language. The
// server config (MEMQL_STT_LANGUAGE) wins; an empty env falls back to "en".
// The client-supplied hint is intentionally NOT consulted here -- pinning
// is the whole point. Callers that genuinely want to honor a client hint
// must opt in explicitly.
func ResolveLanguage() string {
	if v := strings.TrimSpace(os.Getenv(EnvLanguage)); v != "" {
		return v
	}
	return defaultLanguage
}

// ResolveMinConfidence returns the low-confidence final cutoff from
// MEMQL_STT_MIN_CONFIDENCE, clamped to [0,1]. A malformed or out-of-range
// value falls back to the default. 0 explicitly disables the confidence
// gate (empty + denylist filters still apply).
func ResolveMinConfidence() float32 {
	raw := strings.TrimSpace(os.Getenv(EnvMinConfidence))
	if raw == "" {
		return defaultMinConfidence
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v < 0 || v > 1 {
		return defaultMinConfidence
	}
	return float32(v)
}

// TranscriptFilter decides whether a streaming transcription result should
// be emitted to the client. It is provider-agnostic: it runs at the
// pumpDeltas chokepoint so it covers whichever STT provider is active.
type TranscriptFilter struct {
	// MinConfidence is the floor a FINAL must clear. 0 disables the gate.
	MinConfidence float32
}

// NewTranscriptFilter builds a filter from the env-resolved threshold.
func NewTranscriptFilter() TranscriptFilter {
	return TranscriptFilter{MinConfidence: ResolveMinConfidence()}
}

// Keep reports whether a result should be forwarded to the client.
//
// Rules (in order):
//   - empty / whitespace-only text -> drop.
//   - interim results -> keep (so the UI can render word-by-word); the
//     confidence/denylist gates apply only to FINALs, which are the
//     committed utterances the user actually sees as a turn.
//   - low-confidence FINAL (confidence below MinConfidence, and a real
//     confidence signal is present) -> drop. OpenAI-realtime finals carry
//     1.0 so they pass; Deepgram noise hallucinations come back low.
//   - known silence-hallucination phrase on a FINAL, GATED on the no-speech
//     signal (low confidence) -> drop. A high-confidence "okay"/"thank you"
//     the user genuinely said is kept.
func (f TranscriptFilter) Keep(text string, isFinal bool, confidence float32) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	if !isFinal {
		return true
	}

	lowConfidence := f.MinConfidence > 0 && confidence > 0 && confidence < f.MinConfidence

	// Low-confidence final: drop outright. Genuine speech clears the floor.
	if lowConfidence {
		return false
	}

	// No-speech-gated denylist. Deepgram emits low/zero confidence on the
	// noise-hallucination finals; only then do we apply the denylist, so a
	// real high-confidence short phrase survives. The gate fires when the
	// confidence signal is absent (0) or sits below the floor -- i.e. we
	// have no positive evidence of real speech.
	noSpeechEvidence := f.MinConfidence > 0 && confidence < f.MinConfidence
	if noSpeechEvidence && isDenylistedHallucination(text) {
		return false
	}

	return true
}

// isDenylistedHallucination reports whether the normalized text matches a
// known silence-hallucination phrase.
func isDenylistedHallucination(text string) bool {
	_, ok := silenceHallucinationDenylist[normalizeTranscript(text)]
	return ok
}

// normalizeTranscript lowercases, trims, and strips trailing sentence
// punctuation so "Thank you." and "thank you" collapse to one key.
func normalizeTranscript(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimRight(t, ".!?,")
	return strings.TrimSpace(t)
}
