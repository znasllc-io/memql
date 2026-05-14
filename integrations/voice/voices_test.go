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
	if got := ResolveVoice("tenor", "openai-realtime"); got != "echo" {
		t.Errorf("ResolveVoice(tenor, openai-realtime) = %q, want echo", got)
	}
}
