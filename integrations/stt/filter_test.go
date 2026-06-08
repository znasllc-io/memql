package stt

import "testing"

func TestResolveLanguage(t *testing.T) {
	t.Setenv(EnvLanguage, "")
	if got := ResolveLanguage(); got != "en" {
		t.Errorf("ResolveLanguage default = %q, want %q", got, "en")
	}
	t.Setenv(EnvLanguage, "es")
	if got := ResolveLanguage(); got != "es" {
		t.Errorf("ResolveLanguage override = %q, want %q", got, "es")
	}
	t.Setenv(EnvLanguage, "  en-GB  ")
	if got := ResolveLanguage(); got != "en-GB" {
		t.Errorf("ResolveLanguage trim = %q, want %q", got, "en-GB")
	}
}

func TestResolveMinConfidence(t *testing.T) {
	t.Setenv(EnvMinConfidence, "")
	if got := ResolveMinConfidence(); got != defaultMinConfidence {
		t.Errorf("default = %v, want %v", got, defaultMinConfidence)
	}
	t.Setenv(EnvMinConfidence, "0.8")
	if got := ResolveMinConfidence(); got != 0.8 {
		t.Errorf("override = %v, want 0.8", got)
	}
	// Out-of-range / malformed fall back to default.
	for _, bad := range []string{"-0.1", "1.5", "abc"} {
		t.Setenv(EnvMinConfidence, bad)
		if got := ResolveMinConfidence(); got != defaultMinConfidence {
			t.Errorf("ResolveMinConfidence(%q) = %v, want default %v", bad, got, defaultMinConfidence)
		}
	}
	// 0 explicitly disables the gate.
	t.Setenv(EnvMinConfidence, "0")
	if got := ResolveMinConfidence(); got != 0 {
		t.Errorf("ResolveMinConfidence(0) = %v, want 0", got)
	}
}

func TestTranscriptFilter_Keep(t *testing.T) {
	f := TranscriptFilter{MinConfidence: 0.6}

	cases := []struct {
		name       string
		text       string
		isFinal    bool
		confidence float32
		want       bool
	}{
		{"empty final dropped", "", true, 0.99, false},
		{"whitespace-only dropped", "   ", true, 0.99, false},
		{"high-confidence final kept", "hello there", true, 0.92, true},
		{"low-confidence final dropped", "schön guten tag", true, 0.31, false},
		{"interim always kept regardless of confidence", "hel", false, 0.0, true},
		{"interim kept even low confidence", "hel", false, 0.1, true},
		// No-speech-gated denylist: a hallucinated "thank you" at low
		// confidence is dropped; a genuine high-confidence "okay" is kept.
		{"hallucinated thank-you (low conf) dropped", "Thank you.", true, 0.20, false},
		{"hallucinated thanks-for-watching (low conf) dropped", "Thanks for watching!", true, 0.15, false},
		{"genuine okay (high conf) kept", "okay", true, 0.88, true},
		{"genuine thank-you (high conf) kept", "Thank you.", true, 0.91, true},
		// Final with no confidence signal at all (e.g. OpenAI realtime
		// stamps 1.0; a 0 here means "no signal"): empty/denylist still
		// apply, but a normal phrase passes.
		{"no-confidence-signal normal phrase kept", "let us begin", true, 0.0, true},
		{"no-confidence-signal denylisted phrase dropped", "thank you", true, 0.0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.Keep(tc.text, tc.isFinal, tc.confidence); got != tc.want {
				t.Errorf("Keep(%q, final=%v, conf=%v) = %v, want %v",
					tc.text, tc.isFinal, tc.confidence, got, tc.want)
			}
		})
	}
}

func TestTranscriptFilter_DisabledConfidenceGate(t *testing.T) {
	// MinConfidence 0 disables the confidence gate AND the no-speech
	// denylist gate (both require a positive floor), so only the empty
	// filter remains.
	f := TranscriptFilter{MinConfidence: 0}
	if !f.Keep("anything low conf", true, 0.01) {
		t.Error("disabled gate should keep low-confidence finals")
	}
	if !f.Keep("thank you", true, 0.01) {
		t.Error("disabled gate should keep denylisted phrases")
	}
	if f.Keep("", true, 0.99) {
		t.Error("empty must still be dropped even with gate disabled")
	}
}

func TestNormalizeTranscript(t *testing.T) {
	cases := map[string]string{
		"Thank you.":              "thank you",
		"  Thanks for watching! ": "thanks for watching",
		"OKAY":                    "okay",
	}
	for in, want := range cases {
		if got := normalizeTranscript(in); got != want {
			t.Errorf("normalizeTranscript(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeepgramLanguageMapping(t *testing.T) {
	cases := map[string]string{
		"":      "en-US",
		"en":    "en-US",
		"en-US": "en-US",
		"es-MX": "es-MX",
		" en ":  "en-US",
	}
	for in, want := range cases {
		if got := deepgramLanguage(in); got != want {
			t.Errorf("deepgramLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAILanguageMapping(t *testing.T) {
	cases := map[string]string{
		"":        "en",
		"en":      "en",
		"en-US":   "en",
		"es-MX":   "es",
		" en-GB ": "en",
	}
	for in, want := range cases {
		if got := openaiLanguage(in); got != want {
			t.Errorf("openaiLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}
