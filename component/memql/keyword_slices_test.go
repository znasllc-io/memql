package memql

import (
	"strings"
	"testing"
)

// TestExtractKeywordSlices_HyphenatedSeedNames is the regression
// pin for memql#180. The keyword_slices regex used to require
// `[A-Za-z_][A-Za-z0-9_]*` for the NAME identifier -- no hyphen.
// Seed declarations like `seed agentRole graphic-designer { ... }`
// silently failed to match the regex; the slice was never
// extracted, the seed was never materialized, and no error was
// raised. The fix admits hyphens in the NAME group; this test
// fails LOUDLY against a regression.
func TestExtractKeywordSlices_HyphenatedSeedNames(t *testing.T) {
	src := `
@description("Graphic designer role.")
seed agentRole graphic-designer {
  id:       "graphic-designer"
  roleSlug: "graphic-designer"
}

seed agentRole music-theory-teacher {
  id: "music-theory-teacher"
}

// Single-identifier form (legacy, Form A use clause).
use agents.skill
seed copresent-voice-agents {
  id:   "copresent-voice-agents"
  slug: "copresent-voice-agents"
}

// Sanity: a plain non-hyphenated seed still extracts.
seed agentRole assistant {
  id: "assistant"
}
`
	slices := ExtractKeywordSlices(src, "seed")

	wantNames := []string{
		"graphic-designer",
		"music-theory-teacher",
		"copresent-voice-agents",
		"assistant",
	}
	if len(slices) != len(wantNames) {
		t.Fatalf("expected %d slices, got %d (names=%v)", len(wantNames), len(slices), sliceNames(slices))
	}
	for i, want := range wantNames {
		if slices[i].Name != want {
			t.Errorf("slice[%d].Name = %q, want %q", i, slices[i].Name, want)
		}
		// The extracted source should carry the full slice -- a
		// header truncated at the first hyphen would still extract
		// SOMETHING, so we double-check the slice body actually
		// contains the full hyphenated name.
		if !strings.Contains(slices[i].Source, want) {
			t.Errorf("slice[%d].Source must contain %q; got %q", i, want, slices[i].Source)
		}
	}
}

// TestExtractKeywordSlices_NonHyphenatedStillWorks guards against
// the hyphen-extension regressing the common case.
func TestExtractKeywordSlices_NonHyphenatedStillWorks(t *testing.T) {
	src := `
seed agentRole assistant {
  id: "assistant"
}
seed agentRole specialist {
  id: "specialist"
}
`
	slices := ExtractKeywordSlices(src, "seed")
	if len(slices) != 2 {
		t.Fatalf("expected 2 slices, got %d", len(slices))
	}
	if slices[0].Name != "assistant" || slices[1].Name != "specialist" {
		t.Errorf("names = %v, want [assistant specialist]", sliceNames(slices))
	}
}

// TestExtractKeywordSlices_KeywordIsolation: the regex's `keyword`
// is matched as a whole token via the trailing `[ \t]+` separator.
// A keyword embedded in a longer identifier (e.g. `seedling`) must
// NOT be extracted as a `seed`.
func TestExtractKeywordSlices_KeywordIsolation(t *testing.T) {
	src := `
seedling agentRole notARealSeed {
  id: "noise"
}

seed agentRole actuallyASeed {
  id: "real"
}
`
	slices := ExtractKeywordSlices(src, "seed")
	if len(slices) != 1 {
		t.Fatalf("expected 1 slice, got %d (%v)", len(slices), sliceNames(slices))
	}
	if slices[0].Name != "actuallyASeed" {
		t.Errorf("slice[0].Name = %q, want actuallyASeed", slices[0].Name)
	}
}

// sliceNames is a small helper for failure messages.
func sliceNames(slices []KeywordSlice) []string {
	out := make([]string, len(slices))
	for i, s := range slices {
		out[i] = s.Name
	}
	return out
}
