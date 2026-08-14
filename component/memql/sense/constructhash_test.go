package sense

// constructhash_test.go covers ConstructHashes at the DOCUMENT level, which is
// a different claim from the one sourcehash_test.go makes.
//
// sourcehash_test.go pins the normalizer: given a construct's text, what
// changes the hash. These tests pin the SLICE: given a whole file, which bytes
// each construct's hash is computed over. That is where the two sides of the
// contract can actually diverge -- the hash function is shared and cannot -- so
// the acceptance conditions of memql#3758 are restated here against a document
// rather than against a fragment. A comment edit three constructs away moves
// every later byte offset in the buffer, and none of that may reach a hash.

import "testing"

// doc is a small file with the shape the corpus actually has: file-top imports,
// a doc comment above an annotation block above the declaration, and a second
// construct after it.
const doc = `use cognition.concepts.{ widget }

/// Restrict to one widget.
@description("the first")
query widget first {
  args {
    /// Which widget.
    id string @required
  }
  filter  id==args.id
}

@description("the second")
query widget second {
  filter  active==true
}
`

func hashOf(t *testing.T, source, name string) string {
	t.Helper()
	for _, c := range ConstructHashes(source) {
		if c.Name == name {
			return c.SourceHash
		}
	}
	t.Fatalf("ConstructHashes did not locate %q in:\n%s", name, source)
	return ""
}

// TestConstructHashesLocatesEveryKind: the drift state applies to every
// construct kind, not only the five runnable ones, so the scan must report a
// concept and a shape exactly as it reports a query. A kind the scan skips is
// one the editor can never label.
func TestConstructHashesLocatesEveryKind(t *testing.T) {
	const src = `concept widget {
  name string
}

@row
shape widget widgetCard {
  name
}

query widget listWidgets {
  filter  active==true
}
`
	found := map[string]string{}
	for _, c := range ConstructHashes(src) {
		found[c.Name] = c.Kind
	}
	for name, kind := range map[string]string{
		"widget":      "concept",
		"widgetCard":  "shape",
		"listWidgets": "query",
	} {
		if found[name] != kind {
			t.Errorf("%s %s was not located (got kind %q); every kind carries a drift state",
				kind, name, found[name])
		}
	}
}

// TestConstructHashesIgnoresAnEditToAnOrdinaryComment is the document form of
// the "comment-only edits do not change the hash" acceptance condition. The
// edit is deliberately placed in a DIFFERENT construct from the one checked, so
// it also proves the slice boundary holds: rewriting one construct's comment
// shifts every byte offset below it.
func TestConstructHashesIgnoresAnEditToAnOrdinaryComment(t *testing.T) {
	before := doc
	after := "// a note nobody had written before\n" + doc

	for _, name := range []string{"first", "second"} {
		if hashOf(t, before, name) != hashOf(t, after, name) {
			t.Errorf("adding an ordinary comment changed %s's hash, but cannot change what the engine runs",
				name)
		}
	}
}

// TestConstructHashesIgnoresReindentation is the document form of the
// "whitespace-only edits do not change the hash" acceptance condition.
func TestConstructHashesIgnoresReindentation(t *testing.T) {
	before := doc
	after := ""
	for _, line := range splitLines(doc) {
		after += "\t\t" + line + "\n"
	}

	for _, name := range []string{"first", "second"} {
		if hashOf(t, before, name) != hashOf(t, after, name) {
			t.Errorf("re-indenting the file changed %s's hash, but cannot change what the engine runs", name)
		}
	}
}

// TestConstructHashesKeepsALeadingDocComment is the acceptance condition that
// actually caught a bug: the `///` above a construct's annotation block is part
// of the construct, because it is the description the catalog serves.
//
// It is also the exact shape the token scan used to get wrong. Comments are not
// tokens, so the scan could only see back to the first `@` -- and 932
// constructs in dsl/ carry a doc comment above that point. Every one of them
// hashed differently from the engine's slice, which reads as "the whole cluster
// is drifted".
func TestConstructHashesKeepsALeadingDocComment(t *testing.T) {
	withDoc := doc
	edited := replaceFirst(doc, "/// Restrict to one widget.", "/// Restrict to exactly one widget.")
	removed := replaceFirst(doc, "/// Restrict to one widget.\n", "")

	if hashOf(t, withDoc, "first") == hashOf(t, edited, "first") {
		t.Error("editing the leading doc comment did not change the hash; it is the description the catalog serves")
	}
	if hashOf(t, withDoc, "first") == hashOf(t, removed, "first") {
		t.Error("removing the leading doc comment did not change the hash; the slice starts below it")
	}
	// The construct BELOW the edit must be untouched by it -- otherwise the
	// slice boundaries are wrong in the other direction.
	if hashOf(t, withDoc, "second") != hashOf(t, edited, "second") {
		t.Error("editing one construct's doc comment changed a different construct's hash")
	}
}

// TestConstructHashesKeepsTheAnnotationPreamble: an annotation changes what the
// construct declares, so it has to be inside the slice. This is the property
// the token scan always had -- pinned so a future rework of the span rule
// cannot trade it away while fixing the doc-comment case.
func TestConstructHashesKeepsTheAnnotationPreamble(t *testing.T) {
	edited := replaceFirst(doc, `@description("the second")`, `@description("the 2nd")`)
	if hashOf(t, doc, "second") == hashOf(t, edited, "second") {
		t.Error("editing an annotation did not change the hash, but it changes what the construct declares")
	}
}

// TestConstructHashesCoversTheTerseAutomationForm: the brace-less single-step
// automation is a declaration like any other and gets a real hash. The engine
// had no slice for this form at all until memql#3758, which stamped an empty
// hash and made all ten of them read as drifted forever.
func TestConstructHashesCoversTheTerseAutomationForm(t *testing.T) {
	const src = `/// Nightly.
automation nightly @trigger(schedule="0 0 2 * * *") => logic nightly
`
	found := ConstructHashes(src)
	if len(found) != 1 || found[0].Name != "nightly" || found[0].Kind != "automation" {
		t.Fatalf("the terse automation form was not located: %+v", found)
	}
	if found[0].SourceHash == "" {
		t.Fatal("the terse automation hashed to nothing; an empty hash can never compare equal to a real one")
	}
	edited := replaceFirst(src, "/// Nightly.", "/// Nightly, at 02:00.")
	if hashOf(t, edited, "nightly") == found[0].SourceHash {
		t.Error("the terse automation's doc comment is outside its slice")
	}
}

// TestConstructHashesOnAnEmptyOrUnparseableBuffer: the editor asks this on
// every keystroke, so a half-typed buffer is the ordinary case. Unlike the
// runnable projection -- which withholds an argument form it cannot vouch for
// -- a hash is computed from bytes and is withheld from nothing.
func TestConstructHashesOnAnEmptyOrUnparseableBuffer(t *testing.T) {
	if got := ConstructHashes(""); got == nil || len(got) != 0 {
		t.Errorf("ConstructHashes(\"\") = %v, want a non-nil empty slice", got)
	}
	// A construct whose body will not parse still has a hash: reporting none
	// would say "untrained", which is a claim about the cluster rather than
	// about the buffer.
	const halfTyped = `query widget half {
  filter  id==
}
`
	if got := ConstructHashes(halfTyped); len(got) != 1 || got[0].SourceHash == "" {
		t.Errorf("a mid-edit construct got no hash: %+v", got)
	}
}

// TestConstructHashesSpanIndexesTheDocument pins that Start/End really are the
// rune offsets of the text that was hashed. The parity gate's failure report
// re-slices the document by them to show WHAT differed, so an off-by-one here
// turns the one debuggable failure this contract has back into two hex strings.
func TestConstructHashesSpanIndexesTheDocument(t *testing.T) {
	runes := []rune(doc)
	for _, c := range ConstructHashes(doc) {
		if c.Start < 0 || c.End > len(runes) || c.Start >= c.End {
			t.Fatalf("%s %s: span [%d,%d) is not within the document (%d runes)",
				c.Kind, c.Name, c.Start, c.End, len(runes))
		}
		if got := ConstructSourceHash(string(runes[c.Start:c.End])); got != c.SourceHash {
			t.Errorf("%s %s: re-hashing doc[Start:End] gave a different answer\n  span:     %q\n  reported: %s\n  re-hash:  %s",
				c.Kind, c.Name, string(runes[c.Start:c.End]), c.SourceHash, got)
		}
	}
}

// splitLines splits on '\n' and drops the trailing empty element, so a source
// ending in a newline does not gain a blank line on re-join.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// replaceFirst replaces the first occurrence of old with replacement.
func replaceFirst(s, old, replacement string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + replacement + s[i+len(old):]
		}
	}
	return s
}
