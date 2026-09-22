package procedure

import "testing"

// TestMine_ACorpusWithNoRepeatsYieldsNoPattern is issue #5404's first
// acceptance criterion and the negative control the whole file rests on: a
// miner that reports something for everything passes every other test here.
func TestMine_ACorpusWithNoRepeatsYieldsNoPattern(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c"},
		{"d", "e", "f"},
		{"g", "h", "i"},
	}
	if got := Mine(seqs, DefaultParams()); len(got) != 0 {
		t.Fatalf("got %d patterns, want 0:\n%+v", len(got), got)
	}
}

func TestMine_FindsARepeatedRunAtTheSupportFloor(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c", "z"},
		{"q", "a", "b", "c"},
	}
	got := Mine(seqs, DefaultParams())
	if len(got) == 0 {
		t.Fatal("a run occurring in both sequences must be found")
	}
	if len(got[0].Symbols) != 3 || got[0].Support != 2 {
		t.Fatalf("top pattern = %v support %d, want [a b c] support 2", got[0].Symbols, got[0].Support)
	}
}

// TestMine_AContiguousRunOutranksAMoreFrequentScatteredPair states the ranking
// rule as the case that would falsify it. A frequent but scattered pair is a
// coincidence; a slightly rarer contiguous run is a procedure.
func TestMine_AContiguousRunOutranksAMoreFrequentScatteredPair(t *testing.T) {
	seqs := [][]string{
		{"p", "x", "x", "q", "r", "s", "t"},
		{"p", "x", "x", "q", "r", "s", "t"},
		{"p", "y", "y", "y", "q"},
		{"p", "y", "y", "y", "q"},
	}
	got := Mine(seqs, DefaultParams())
	if len(got) == 0 {
		t.Fatal("expected at least one pattern")
	}
	top := got[0]
	if len(top.Symbols) < 4 {
		t.Fatalf("top pattern = %v (coverage %.3f cohesion %.3f); the contiguous run r,s,t with p,q "+
			"must outrank the scattered p..q pair", top.Symbols, top.Coverage, top.Cohesion)
	}
}

func TestMine_OnlyClosedPatternsAreReported(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c"},
		{"a", "b", "c"},
		{"a", "b", "c"},
	}
	got := Mine(seqs, DefaultParams())
	for _, p := range got {
		if len(p.Symbols) == 2 && p.Symbols[0] == "a" && p.Symbols[1] == "b" {
			t.Fatalf("[a b] has the same support as [a b c] and must not be reported: %+v", got)
		}
	}
}

func TestMine_GapToleranceIsPerAdjacentPair(t *testing.T) {
	p := DefaultParams()
	p.Gap = 1
	// One unmatched symbol between b and c is inside the tolerance. The
	// interrupting symbol DIFFERS between the sequences on purpose: a shared
	// one is itself part of a longer contiguous pattern, which would win on
	// merit and prove nothing about the gap.
	inside := [][]string{
		{"a", "b", "z1", "c"},
		{"a", "b", "z2", "c"},
	}
	if got := Mine(inside, p); len(got) == 0 || len(got[0].Symbols) != 3 {
		t.Fatalf("a gap of 1 must be tolerated; got %+v", got)
	}
	// Two is not, and the tolerance must not be amortized across the
	// occurrence.
	outside := [][]string{
		{"a", "b", "z1", "z2", "c"},
		{"a", "b", "z3", "z4", "c"},
	}
	for _, pat := range Mine(outside, p) {
		if len(pat.Symbols) == 3 {
			t.Fatalf("a gap of 2 exceeds Gap=1 and must not match: %+v", pat)
		}
	}
}

func TestMine_RemoveAndRemineDoesNotRefillTheListWithTheWinnersParts(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c", "m", "n"},
		{"a", "b", "c", "m", "n"},
	}
	got := Mine(seqs, DefaultParams())
	seen := map[string]bool{}
	for _, p := range got {
		for _, s := range p.Symbols {
			if seen[s] {
				t.Fatalf("symbol %q appears in two reported patterns; remove-and-remine should have "+
					"taken it out of the corpus:\n%+v", s, got)
			}
			seen[s] = true
		}
	}
}

func TestMine_IsDeterministic(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c", "d"},
		{"a", "b", "c", "d"},
		{"x", "a", "b", "c", "d"},
	}
	first, second := Mine(seqs, DefaultParams()), Mine(seqs, DefaultParams())
	if len(first) != len(second) {
		t.Fatalf("%d patterns then %d", len(first), len(second))
	}
	for i := range first {
		if len(first[i].Symbols) != len(second[i].Symbols) {
			t.Fatalf("pattern %d differs between runs", i)
		}
		for j := range first[i].Symbols {
			if first[i].Symbols[j] != second[i].Symbols[j] {
				t.Fatalf("pattern %d differs between runs: %v vs %v", i, first[i].Symbols, second[i].Symbols)
			}
		}
	}
}

func TestMine_RespectsMinSupport(t *testing.T) {
	p := DefaultParams()
	p.MinSupport = 3
	seqs := [][]string{
		{"a", "b"},
		{"a", "b"},
	}
	if got := Mine(seqs, p); len(got) != 0 {
		t.Fatalf("support 2 must not clear a floor of 3; got %+v", got)
	}
}
