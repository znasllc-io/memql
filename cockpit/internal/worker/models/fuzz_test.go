package models

import (
	"strings"
	"testing"
)

// FuzzAttributesRoundTrip. The label value is the ONE string this
// machine writes that the engine parses, and `quant` inside it is the
// only free-text field in a comma-and-equals delimited list -- so it is
// the only value that could forge a second attribute the machine never
// claimed.
//
// Two properties, and the second is the security one:
//
//   - what String() renders, ParseAttributes reads back unchanged. A
//     label that did not round trip would mean the engine acts on
//     something other than what this machine meant to say.
//
//   - NO ATTRIBUTE IS EVER FORGED. Whatever a runtime called its
//     quantization level, the rendered label must contain no more `=`
//     signs than the number of keys actually emitted. A quant carrying a
//     comma or an equals would otherwise be re-read on the far side as
//     some other key.
func FuzzAttributesRoundTrip(f *testing.F) {
	for _, seed := range []struct {
		ctx                            int
		params                         int64
		quant                          string
		structured, tools, vision, gen bool
	}{
		{8192, 9_000_000_000, "Q4_K_M", true, true, true, true},
		{0, 0, "", false, false, false, false},
		{1, 1, "F16", true, false, false, false},
		// The forging attempts: a level carrying the delimiters.
		{4096, 7_000_000_000, "Q4,tools=1", true, true, false, false},
		{4096, 7_000_000_000, "Q4=1", true, true, false, false},
		{4096, 7_000_000_000, "a,b=c,d=e", false, false, false, false},
		// And levels that are not printable at all.
		{4096, 0, "Q4\nK", false, false, false, false},
		{4096, 0, "Q4\x00", false, false, false, false},
		{4096, 0, "quant with spaces", false, false, false, false},
		{4096, 0, "🙂", false, false, false, false},
		{4096, 0, strings.Repeat("Q", 400), false, false, false, false},
	} {
		f.Add(seed.ctx, seed.params, seed.quant, seed.structured, seed.tools, seed.vision, seed.gen)
	}

	f.Fuzz(func(t *testing.T, ctx int, params int64, quant string,
		structured, tools, vision, gen bool) {
		a := Attributes{
			ContextWindow: ctx, Params: params, Quant: quant,
			StructuredOutput: structured, Tools: tools, Vision: vision, ImageGen: gen,
		}
		label := a.String()

		// NO FORGERY. Every `=` in the rendered label belongs to a key
		// this machine actually emitted, so counting the separators
		// counts the claims.
		if got, want := strings.Count(label, "="), strings.Count(label, ",")+1; label != "" && got != want {
			t.Fatalf("Attributes%+v rendered %q: %d separators for %d fields", a, label, got, want)
		}

		// ROUND TRIP. What the engine reads back is what was meant.
		back := ParseAttributes(label)
		if back.String() != label {
			t.Fatalf("Attributes%+v rendered %q, which parses back to %q", a, label, back.String())
		}

		// And every attribute that SURVIVED is the one that was set --
		// a parse that invented a flag would grant this machine an
		// eligibility it never claimed.
		if back.Vision && !a.Vision {
			t.Fatalf("vision was forged from %q", label)
		}
		if back.Tools && !a.Tools {
			t.Fatalf("tools was forged from %q", label)
		}
		if back.StructuredOutput && !a.StructuredOutput {
			t.Fatalf("structured output was forged from %q", label)
		}
		if back.ImageGen && !a.ImageGen {
			t.Fatalf("image generation was forged from %q", label)
		}
	})
}

// FuzzParseAttributes from the OTHER direction: arbitrary label text,
// which is what a machine reads back off a registration written by some
// other version of this code.
//
// The property is FAIL-CLOSED. Nothing a label says can be worse than
// the label saying nothing, so a parse must never produce a value that
// re-renders into a different claim -- and it must never panic on text
// no version of String() would have produced.
func FuzzParseAttributes(f *testing.F) {
	for _, seed := range []string{
		"", "ctx=8192,structured=1,tools=1",
		"ctx=abc", "ctx=-1", "ctx=99999999999999999999",
		"params=-5", "max=0", "quant=", "quant=Q4=1",
		"=", ",,,,", "vision=1,vision=0", "unknown=1",
		"vision=TRUE", "vision=yes", "vision=2",
		strings.Repeat("a=1,", 500),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, label string) {
		a := ParseAttributes(label)

		// Re-rendering is IDEMPOTENT: whatever survived a parse must
		// survive a second one unchanged, or two machines reading the
		// same registration would disagree about it.
		once := a.String()
		if twice := ParseAttributes(once).String(); twice != once {
			t.Fatalf("ParseAttributes(%q) rendered %q, which re-parses to %q", label, once, twice)
		}

		// A NUMBER IS NEVER NEGATIVE. Every numeric attribute is a
		// count or a size, and a negative one reaching the engine's
		// ordering would sort a machine somewhere no comparison
		// intended.
		if a.ContextWindow < 0 || a.Params < 0 || a.MaxConcurrent < 0 {
			t.Fatalf("ParseAttributes(%q) produced a negative attribute: %+v", label, a)
		}

		// And a quant that survived must be one String() could have
		// emitted, so the set of levels that round trip is closed
		// rather than "whatever arrived".
		if a.Quant != "" && !quantSafe(a.Quant) {
			t.Fatalf("ParseAttributes(%q) kept an unsafe quant %q", label, a.Quant)
		}
	})
}
