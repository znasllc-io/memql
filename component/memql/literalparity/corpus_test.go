package literalparity

import (
	"encoding/json"
	"testing"
)

// corpus_test.go -- memql#2835.
//
// Every number this package's prose states must be enforced, because three
// consecutive revisions of this change shipped an unpinned figure that was
// wrong:
//
//	the "~5,000x" parse margin, wrong by two orders of magnitude, twice
//	"all three copies agree", false on 7 of 16 rows
//	the divergent-row count, stated in a doc comment and checked by nothing
//
// A number in a comment is a claim, and this package's whole purpose is that
// unenforced claims drift.

// wantDivergentRows and wantRows are the counts the package prose states.
//
// BOTH, because pinning the numerator alone is defeatable: converging one row
// while adding another divergent one keeps the divergent count at 8 while the
// doc still reads "eight of the SEVENTEEN". Narrow -- the realistic version of
// that edit reds a per-package parity test first -- but a half-pinned number is
// the shape this whole package exists to remove.
const (
	wantDivergentRows = 8
	wantRows          = 17
)

func TestRowCountMatchesTheDoc(t *testing.T) {
	if len(Cases) != wantRows {
		t.Errorf("the corpus has %d rows, but the prose says %d. Update both, or the divergent "+
			"count below is a fraction with an unpinned denominator.", len(Cases), wantRows)
	}
}

func TestDivergentRowCountMatchesTheDoc(t *testing.T) {
	got := 0
	for _, c := range Cases {
		if c.Divergent() {
			got++
		}
	}
	if got != wantDivergentRows {
		t.Errorf("%d rows diverge, but the docs say %d.\n\n"+
			"If a divergence was FIXED, the row's three values converged -- lower this constant "+
			"and the count in Divergent()'s doc. If a NEW one appeared, that is a copy drifting "+
			"and is the thing this package exists to surface. Either way the number in the prose "+
			"must not be left stale (memql#2835).", got, wantDivergentRows)
	}
}

// TestAcceptedAgreesWithTheRecordedValues exercises the exported Accepted
// helper.
//
// Stated accurately, because the first version of this comment overclaimed: it
// compares the function against an inline copy of its own one-line body, so it
// validates nothing about the corpus DATA -- it is a change-detector. The
// justification is the second sentence, not the first: Accepted was exported
// dead code, and an exported helper nothing calls is a helper nothing checks.
func TestAcceptedAgreesWithTheRecordedValues(t *testing.T) {
	for _, c := range Cases {
		for _, v := range []struct {
			copyName, value string
		}{{"memql", c.MemQL}, {"compiler", c.Compiler}, {"steps", c.Steps}} {
			if Accepted(v.value) != (v.value != "ERR") {
				t.Errorf("Accepted(%q) disagrees with the recorded value for %s on %q",
					v.value, v.copyName, c.Src)
			}
		}
	}
}

// TestEveryRowRecordsAllThreeCopies guards the shape: a row missing a column
// would silently assert the empty string, which no parser produces, so it would
// red confusingly at the call site instead of here.
func TestEveryRowRecordsAllThreeCopies(t *testing.T) {
	for _, c := range Cases {
		if c.Src == "" {
			t.Error("a row has no Src")
		}
		for _, v := range []string{c.MemQL, c.Compiler, c.Steps} {
			if v != "ERR" && !json.Valid([]byte(v)) {
				t.Errorf("row %q records %q, which is not valid JSON. A corrupted value would "+
					"otherwise surface as a confusing divergent-count failure rather than here.",
					c.Src, v)
			}
		}
		if c.MemQL == "" || c.Compiler == "" || c.Steps == "" {
			t.Errorf("row %q is missing a recorded value (memql=%q compiler=%q steps=%q); "+
				"every row must record all three, and \"ERR\" is how a rejection is spelled",
				c.Src, c.MemQL, c.Compiler, c.Steps)
		}
	}
}
