package voice

import "testing"

func TestResolveVoice_DeepgramKnownCanonicals(t *testing.T) {
	cases := map[string]string{
		"alto":     "aura-2-thalia-en",
		"soprano":  "aura-2-asteria-en",
		"tenor":    "aura-2-arcas-en",
		"baritone": "aura-2-orion-en",
		"marcus":   "aura-2-angus-en",
	}
	for canon, want := range cases {
		got := ResolveVoice(canon, "deepgram")
		if got != want {
			t.Errorf("ResolveVoice(%q, deepgram) = %q, want %q", canon, got, want)
		}
	}
}

func TestResolveVoice_DeepgramUnknownCanonicalFallsBackByGender(t *testing.T) {
	// "alto" is in the catalog -- swap with something off-catalog to
	// hit the gender-bucket fallback. Manually compute the gender we
	// expect by checking CanonicalGender.
	if id := ResolveVoice("not-a-real-voice", "deepgram"); id != "aura-2-thalia-en" {
		t.Errorf("unknown canonical fell to %q, want aura-2-thalia-en (female default)", id)
	}
}

func TestResolveVoice_DeepgramEmptyCanonicalReturnsDefault(t *testing.T) {
	if id := ResolveVoice("", "deepgram"); id != "aura-2-thalia-en" {
		t.Errorf("empty canonical fell to %q, want aura-2-thalia-en", id)
	}
}

func TestResolveVoice_OpenAIStillMapsCorrectly(t *testing.T) {
	// Regression guard: adding the deepgram branch shouldn't perturb
	// the OpenAI resolution path.
	if got := ResolveVoice("alto", "openai"); got != "nova" {
		t.Errorf("ResolveVoice(alto, openai) = %q, want nova", got)
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
