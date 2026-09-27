package procedure

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

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

// TestMineIsMineWeightedWithNoWeights: the unweighted miner is the weighted
// one with every sequence weighing one, and a corpus whose weights are all
// one ranks exactly as an unweighted corpus -- so weighing the corpus changes
// no procedure until somebody likes something. The corpus is the one case the
// ranking decides on LENGTH before support ([a b c] twice against [x y] three
// times: equal coverage, equal cohesion), which is exactly where a tie-break
// on raw weighted support would have reordered it.
func TestMineIsMineWeightedWithNoWeights(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c"},
		{"a", "b", "c"},
		{"x", "y"},
		{"x", "y"},
		{"x", "y"},
	}
	want := Mine(seqs, DefaultParams())
	if len(want) < 2 || strings.Join(want[0].Symbols, " ") != "a b c" || want[0].Coverage != want[1].Coverage {
		t.Fatalf("control: the fixture must tie [a b c] and [x y] on coverage and pick the longer; got %s", patternKey(want))
	}
	for name, weights := range map[string][]float64{
		"nil":      nil,
		"all ones": {1, 1, 1},
		"short":    {1},
		"invalid":  {0, -3, math.NaN()},
	} {
		got := MineWeighted(seqs, weights, DefaultParams().MinSupport, DefaultParams().Gap)
		if patternKey(got) != patternKey(want) {
			t.Errorf("%s weights reordered the unweighted ranking:\n got %s\nwant %s", name, patternKey(got), patternKey(want))
		}
	}
}

// TestMineWeighted_ALikedSequenceBreaksATieAfterCoverageAndCohesion: two
// patterns equal on coverage, cohesion, length and support, where the
// unweighted order picks [a b] by its symbols. Liking the sequence only [x y]
// occurs in puts [x y] first -- and its occurrences keep sequence order, so
// the instances a template is generalized from are the same ones either way.
func TestMineWeighted_ALikedSequenceBreaksATieAfterCoverageAndCohesion(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "x", "y"},
		{"a", "b"},
		{"x", "y"},
	}
	p := DefaultParams()
	unweighted := MineWeighted(seqs, nil, p.MinSupport, p.Gap)
	if len(unweighted) == 0 || strings.Join(unweighted[0].Symbols, " ") != "a b" {
		t.Fatalf("control: the unweighted order must pick [a b] by its symbols, got %s", patternKey(unweighted))
	}
	liked := MineWeighted(seqs, []float64{1, 1, 2}, p.MinSupport, p.Gap)
	if len(liked) == 0 || strings.Join(liked[0].Symbols, " ") != "x y" {
		t.Fatalf("a liked sequence must break the tie for the pattern it holds, got %s", patternKey(liked))
	}
	top := liked[0]
	if top.Support != 2 || top.WeightedSupport != 3 {
		t.Errorf("support = %d, weighted = %v; want 2 and 3", top.Support, top.WeightedSupport)
	}
	if len(top.Occurrences) != 2 || top.Occurrences[0].Sequence != 0 || top.Occurrences[1].Sequence != 2 {
		t.Errorf("occurrences must stay in sequence order: %+v", top.Occurrences)
	}
}

// TestMineWeighted_AWeightNeverOutranksCoverage: a like is a TIE-break. A
// pattern that accounts for more of the corpus wins however much the other
// one's sequences are liked.
func TestMineWeighted_AWeightNeverOutranksCoverage(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c", "x", "y"},
		{"a", "b", "c"},
		{"x", "y"},
	}
	p := DefaultParams()
	got := MineWeighted(seqs, []float64{1, 1, 100}, p.MinSupport, p.Gap)
	if len(got) == 0 || strings.Join(got[0].Symbols, " ") != "a b c" {
		t.Fatalf("coverage must outrank a weight, got %s", patternKey(got))
	}
}

// TestOneLikedRecordingIsStillOneUse: the two-use floor counts DISTINCT
// recordings, unweighted. A pattern in one liked recording weighs two and is
// still one use, so nothing is mined -- a like is a judgment of a recording,
// not a second recording.
func TestOneLikedRecordingIsStillOneUse(t *testing.T) {
	seqs := [][]string{
		{"a", "b", "c"},
		{"x", "y", "z"},
	}
	p := DefaultParams()
	if got := MineWeighted(seqs, []float64{2, 1}, p.MinSupport, p.Gap); len(got) != 0 {
		t.Fatalf("one liked recording cleared the two-use floor: %s", patternKey(got))
	}
	// The positive control: a second recording of the same run is two uses.
	seqs = append(seqs, []string{"a", "b", "c"})
	if got := MineWeighted(seqs, []float64{2, 1, 1}, p.MinSupport, p.Gap); len(got) == 0 || got[0].Support != 2 {
		t.Fatalf("two recordings must clear the floor, got %s", patternKey(got))
	}
}

// patternKey renders a mined list for comparison and for a failure message.
func patternKey(ps []Pattern) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("[")
		b.WriteString(strings.Join(p.Symbols, " "))
		b.WriteString("]")
		for _, o := range p.Occurrences {
			b.WriteString(" ")
			b.WriteString(strconv.Itoa(o.Sequence))
			b.WriteString(":")
			for i, pos := range o.Positions {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(strconv.Itoa(pos))
			}
		}
		b.WriteString("; ")
	}
	return b.String()
}
