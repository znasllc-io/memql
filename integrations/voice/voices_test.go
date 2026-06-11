package voice

import "testing"

// TestResolveVoice_UnknownProviderPassesCanonicalThrough: an unrecognised
// provider hands back the canonical name so the provider client decides --
// the caller never receives an empty string.
func TestResolveVoice_UnknownProviderPassesCanonicalThrough(t *testing.T) {
	if got := ResolveVoice("alto", "someprovider"); got != "alto" {
		t.Errorf("ResolveVoice(alto, someprovider) = %q, want alto", got)
	}
}

func TestResolveVoice_OpenAIStillMapsCorrectly(t *testing.T) {
	if got := ResolveVoice("alto", "openai"); got != "nova" {
		t.Errorf("ResolveVoice(alto, openai) = %q, want nova", got)
	}
}

func TestResolveVoice_OpenAIUnknownCanonicalFallsBackToDefault(t *testing.T) {
	if id := ResolveVoice("not-a-real-voice", "openai"); id != "alloy" {
		t.Errorf("unknown canonical fell to %q, want alloy (openai default)", id)
	}
}

func TestResolveVoice_OpenAIEmptyCanonicalReturnsDefault(t *testing.T) {
	if id := ResolveVoice("", "openai"); id != "alloy" {
		t.Errorf("empty canonical fell to %q, want alloy", id)
	}
}

// TestResolveVoice_RealtimeUsesGAVoiceSet asserts the realtime provider
// resolves through its OWN GA voice set, never the TTS-only ids (#483).
func TestResolveVoice_RealtimeUsesGAVoiceSet(t *testing.T) {
	// gaRealtimeVoices is the exact set the gpt-realtime GA API accepts.
	gaRealtimeVoices := map[string]struct{}{
		"alloy": {}, "ash": {}, "ballad": {}, "cedar": {}, "coral": {},
		"echo": {}, "marin": {}, "sage": {}, "shimmer": {}, "verse": {},
	}

	// Every canonical voice must resolve to a valid GA realtime voice --
	// in particular NEVER the TTS-only "nova" / "onyx" ids.
	for _, canonical := range AllCanonicalNames() {
		got := ResolveVoice(canonical, "openai-realtime")
		if _, ok := gaRealtimeVoices[got]; !ok {
			t.Errorf("ResolveVoice(%q, openai-realtime) = %q, not a GA realtime voice", canonical, got)
		}
	}

	// Spot-check the pinned GA defaults.
	if got := ResolveVoice("alto", "openai-realtime"); got != "marin" {
		t.Errorf("ResolveVoice(alto, openai-realtime) = %q, want marin", got)
	}
	if got := ResolveVoice("tenor", "openai-realtime"); got != "cedar" {
		t.Errorf("ResolveVoice(tenor, openai-realtime) = %q, want cedar", got)
	}
	// Empty canonical falls back to the recommended GA default (marin).
	if got := ResolveVoice("", "openai-realtime"); got != "marin" {
		t.Errorf("ResolveVoice(\"\", openai-realtime) = %q, want marin", got)
	}
	// Unknown canonical with no resolvable gender gets the GA default (marin).
	if got := ResolveVoice("does-not-exist", "openai-realtime"); got != "marin" {
		t.Errorf("ResolveVoice(unknown, openai-realtime) = %q, want marin (gender-unknown default)", got)
	}
}
