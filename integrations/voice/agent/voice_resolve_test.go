package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/znasllc-io/memql/integrations/voice"
)

// TestResolveTTSVoice_ConsumesCatalog asserts the TTS voice id comes straight
// from the canonical catalog (voices.go) for the active provider -- the agent
// does NOT fork the mapping. We pin the provider via env so the assertion is
// deterministic regardless of the CI host's provider configuration.
func TestResolveTTSVoice_ConsumesCatalog(t *testing.T) {
	t.Setenv("MEMQL_POLYPHON_VOICE_PROVIDER", "openai")

	p := ResolvePersona(SessionAck{CanonicalVoice: "alto"}, Config{})
	got := ResolveTTSVoice(p)

	// Must equal what the catalog itself resolves -- same source of truth.
	want := voice.ResolveVoice("alto", "openai")
	assert.Equal(t, want, got)
	assert.NotEmpty(t, got)
}

func TestResolveTTSVoice_ExplicitProviderOverride(t *testing.T) {
	t.Setenv("MEMQL_POLYPHON_VOICE_PROVIDER", "openai")

	p := ResolvePersona(SessionAck{CanonicalVoice: "tenor"}, Config{})
	assert.Equal(t, voice.ResolveVoice("tenor", "openai"), ResolveTTSVoice(p))
}

// TestResolveRealtimeVoice_ConsumesCatalog asserts the realtime voice id
// resolves through the same catalog via the openai-realtime provider key.
func TestResolveRealtimeVoice_ConsumesCatalog(t *testing.T) {
	for _, canonical := range []string{"alto", "soprano", "tenor", "bass"} {
		p := ResolvePersona(SessionAck{CanonicalVoice: canonical}, Config{})
		got := ResolveRealtimeVoice(p)
		want := voice.ResolveVoice(canonical, "openai-realtime")
		assert.Equal(t, want, got, "canonical %q", canonical)
		assert.NotEmpty(t, got)
	}
}

// TestResolveVoice_UnknownCanonicalNeverSilent: an unknown canonical voice
// (which ResolvePersona keeps verbatim when non-empty) still resolves to a
// non-empty provider default, so the audio path never goes silent.
func TestResolveVoice_UnknownCanonicalNeverSilent(t *testing.T) {
	t.Setenv("MEMQL_POLYPHON_VOICE_PROVIDER", "openai")
	p := ResolvePersona(SessionAck{CanonicalVoice: "does-not-exist"}, Config{})
	assert.NotEmpty(t, ResolveTTSVoice(p))
	assert.NotEmpty(t, ResolveRealtimeVoice(p))
}

// TestResolveVoice_DefaultCanonicalForEmptyAck: an empty canonical falls back
// to "alto" in ResolvePersona, which resolves to the catalog's alto voice.
func TestResolveVoice_DefaultCanonicalForEmptyAck(t *testing.T) {
	t.Setenv("MEMQL_POLYPHON_VOICE_PROVIDER", "openai")
	p := ResolvePersona(SessionAck{}, Config{})
	assert.Equal(t, "alto", p.CanonicalVoice)
	assert.Equal(t, voice.ResolveVoice("alto", "openai"), ResolveTTSVoice(p))
}
