package parser

import (
	"strings"
	"testing"
)

// The pinned fingerprint forces a CONSCIOUS GrammarVersion bump whenever the
// author-facing grammar surface changes. If this test fails: you changed the
// grammar -- bump GrammarVersion in grammar_version.go, decide whether the
// narrowing needs a `memqlmigrate --rewrite=<epic>` mode (see the rule at the
// top of that file; it is no longer an unconditional MUST), THEN update the pin
// (S6, memql#2361; widened by memql#3089).
func TestGrammarFingerprintPinned(t *testing.T) {
	const pinned = "36f7691bfd54a4f0"
	got := GrammarFingerprint()
	if pinned == "" {
		t.Logf("current fingerprint: %s", got)
		return
	}
	if got != pinned {
		t.Fatalf(`grammar surface changed (fingerprint %s != pinned %s).

Do these in order:
  1. bump GrammarVersion in grammar_version.go, and record the narrowing in the
     comment above it the way the memql#3089 entries are recorded;
  2. decide whether a `+"`memqlmigrate --rewrite=<epic>`"+` mode is required -- it is when the
     retired form can appear in authored source a consumer still holds (the dsl/
     tree, a product bundle, a durably-promoted v1:authoring:construct row), and
     it is not when it provably cannot. Say which arm applies in the commit, with
     the measurement;
  3. then update the pin here.

Updating the pin FIRST makes this test a rubber stamp, which is how it stayed
green through six unbumped narrowings before memql#3089.`, got, pinned)
	}
}

// TestGrammarFingerprintAxesAreAllLoadBearing proves every axis contributes to
// the hash.
//
// This is the test memql#3089 most needed and did not have. The old fingerprint
// had exactly one axis, that axis was the one part of the grammar nobody was
// changing, and the pin therefore stayed green across six narrowings while
// reading as a drift detector. An axis that cannot move the hash is worse than
// an absent axis: it is a lane that reads as covered.
//
// It works by dropping each axis in turn and asserting the hash changes. That
// catches the real regression -- an axis wired in but derived from something
// constant, or accidentally excluded from the join.
func TestGrammarFingerprintAxesAreAllLoadBearing(t *testing.T) {
	axes := grammarSurfaceAxes()
	if len(axes) < 4 {
		t.Fatalf("expected at least the 4 axes memql#3089 established, got %d -- an axis has been "+
			"dropped, and the fingerprint is now blind to whatever it covered", len(axes))
	}

	full := hashAxes(axes)
	if full != GrammarFingerprint() {
		t.Fatalf("hashAxes(grammarSurfaceAxes()) != GrammarFingerprint() -- this test is not " +
			"measuring the shipped function")
	}

	names := []string{"invocation keywords", "annotations", "retired expr builtins", "struct-query clauses"}
	for i := range axes {
		if len(axes[i]) == 0 {
			t.Errorf("axis %d (%s) is EMPTY, so it can never move the fingerprint -- it reads as "+
				"covered and is not", i, axisName(names, i))
			continue
		}
		reduced := make([][]string, 0, len(axes))
		for j, a := range axes {
			if j == i {
				continue
			}
			reduced = append(reduced, a)
		}
		if hashAxes(reduced) == full {
			t.Errorf("removing axis %d (%s) does not change the fingerprint, so nothing it covers "+
				"can ever trip the pin", i, axisName(names, i))
		}
	}
}

// TestGrammarFingerprintAxesCarryTheNarrowingsThatWereMissed pins that each axis
// actually contains the vocabulary whose change went undetected.
//
// Without this, the axes could be wired correctly and still watch the wrong
// sets. Each assertion below names a specific commit from the GrammarVersion
// comment, so a reader can check the claim rather than trust it.
func TestGrammarFingerprintAxesCarryTheNarrowingsThatWereMissed(t *testing.T) {
	// 93b365ed hard-retired these eight. They are IN retiredExprBuiltins, so the
	// axis moved when they landed.
	retired := strings.Join(retiredExprBuiltinAxis(), ",")
	for _, name := range []string{"year", "quarter", "month", "dayofmonth", "isanniversary"} {
		if !strings.Contains(retired, name) {
			t.Errorf("the retired-expr-builtin axis does not carry %q, one of the eight retired in "+
				"93b365ed -- that narrowing would still be invisible", name)
		}
	}

	// 6e7d09ac added `asOf` as a clause. The struct-query axis must carry it.
	clauses := strings.Join(structQueryClauseAxis(), ",")
	if !strings.Contains(clauses, "asOf") {
		t.Error("the struct-query axis does not carry `asOf`, the clause 6e7d09ac added and #3085 " +
			"reshaped -- that narrowing would still be invisible")
	}

	// d53bad46 / 489a414b / 0d13dd96 buried @role, @permission and construct-level
	// @internal. They are ABSENT from annotations.ByReceiver now, which is what
	// makes the annotation axis the thing that would have caught them. Assert the
	// axis is non-trivial and that the buried names are gone, so a re-introduction
	// also trips the pin.
	anns := strings.ToLower(strings.Join(annotationAxis(), ","))
	if len(annotationAxis()) < 10 {
		t.Fatalf("the annotation axis has only %d entries -- it is not projecting the registry",
			len(annotationAxis()))
	}
	for _, buried := range []string{".role", ".permission"} {
		if strings.Contains(anns, buried) {
			t.Errorf("annotations.ByReceiver has regained %q, buried in d53bad46/489a414b. That is "+
				"a grammar move: bump GrammarVersion rather than only re-pinning.", buried)
		}
	}
}

// TestStructQueryClauseAxisMatchesTheRewriter is the drift guard between the
// clause vocabulary and the switch that implements it.
//
// The axis is a hand-maintained list sitting beside a switch. That is exactly
// the "two definitions of one answer" shape this repo keeps getting bitten by,
// so it is asserted BEHAVIOURALLY: every listed keyword must be recognised by
// the rewriter (not fall through to "unknown struct-query field"), and a word
// that is not listed must NOT be recognised.
func TestStructQueryClauseAxisMatchesTheRewriter(t *testing.T) {
	// The concept-binding check runs BEFORE the clause loop, so the probe must
	// use the signature form `query <Concept> <name>`. Without it every clause --
	// valid or not -- fails with "missing concept binding" and never reaches the
	// switch, which makes the harness report every word as recognised.
	recognised := func(clause string) bool {
		src := "query space probe {\n  " + clause + "\n}\n"
		_, err := NormaliseQuerySource(src)
		// "unknown struct-query field" is the only error meaning the clause WORD
		// was not recognised. Any other error (the deliberately-rejected
		// `concept` line, a malformed value) still proves recognition.
		return err == nil || !strings.Contains(err.Error(), "unknown struct-query field")
	}

	// Each clause needs a value its own parser accepts, or the probe measures
	// value parsing rather than clause recognition.
	probes := map[string]string{
		"concept":  "concept x",
		"filter":   "filter x==1",
		"count":    "count",
		"shape":    "shape s",
		"sort":     `sort "a", "desc"`,
		"paginate": "paginate 50",
		"asOf":     "asOf args.at ?? latest",
	}

	for _, kw := range structQueryClauseKeywords {
		probe, ok := probes[kw]
		if !ok {
			t.Errorf("structQueryClauseKeywords lists %q but this test has no probe for it -- add "+
				"one, or the new clause is listed and unverified", kw)
			continue
		}
		if !recognised(probe) {
			t.Errorf("structQueryClauseKeywords lists %q but the rewriter does not recognise it -- "+
				"the fingerprint axis and the switch have drifted, so the axis is hashing a "+
				"vocabulary the parser does not implement", kw)
		}
	}

	// The converse: a word absent from the list must be unrecognised. If the
	// switch grows a clause and the list does not, this fires.
	for _, absent := range []string{"groupby", "having", "limit", "offset"} {
		if recognised(absent + " x") {
			t.Errorf("the rewriter recognises the clause %q, which structQueryClauseKeywords does "+
				"not list. Add it -- until then the grammar fingerprint cannot see changes to it, "+
				"which is the memql#3089 defect exactly", absent)
		}
	}
}

func hashAxes(axes [][]string) string {
	var parts []string
	for _, axis := range axes {
		parts = append(parts, strings.Join(axis, ","))
	}
	return hashJoined(parts)
}

func axisName(names []string, i int) string {
	if i < len(names) {
		return names[i]
	}
	return "unnamed"
}
