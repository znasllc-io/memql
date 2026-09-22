package procedure

import (
	"sort"
	"strings"
)

// Occurrence is one place a pattern was found: which sequence, and the
// position of each of its symbols in that sequence.
type Occurrence struct {
	Sequence  int
	Positions []int
}

// Pattern is a recurring sub-sequence and the evidence for it.
type Pattern struct {
	Symbols     []string
	Occurrences []Occurrence
	// Support is the number of distinct SEQUENCES the pattern occurs in, not
	// the number of occurrences. Two repetitions inside one run are one run's
	// habit; two runs doing the same thing is a procedure.
	Support int
	// Coverage is the fraction of the corpus's symbols this pattern accounts
	// for.
	Coverage float64
	// Cohesion is 1/(1+mean gap) over the pattern's occurrences: 1 when every
	// occurrence is contiguous, falling as they scatter.
	Cohesion float64
}

// Mine finds closed frequent sub-sequences with a gap tolerance, ranked by
// coverage and cohesion BEFORE frequency, removing each winner from the corpus
// and re-mining until nothing clears the support floor.
//
// Three choices carry the weight.
//
// CLOSED ONLY. A pattern with the same support as a longer pattern containing
// it is not reported. Without closure the output is every prefix of every
// pattern at identical support, and a ranking over that list means nothing.
//
// COVERAGE AND COHESION BEFORE FREQUENCY. A pair that occurs in every sequence
// with fifteen symbols between its halves is a coincidence of two common
// actions; a contiguous run of five occurring twice is a procedure. Ranking on
// frequency alone reliably prefers the coincidence.
//
// REMOVE AND RE-MINE. After the winner is taken its occurrences leave the
// corpus, so the runner-up is scored against what is LEFT. Otherwise the whole
// list fills with the winner's own sub-sequences, each a pattern nobody would
// lift on its own.
func Mine(sequences [][]string, p Params) []Pattern {
	minSupport := p.MinSupport
	if minSupport < 2 {
		minSupport = 2
	}
	total := 0
	for _, s := range sequences {
		total += len(s)
	}
	if total == 0 {
		return nil
	}

	work := make([][]string, len(sequences))
	for i, s := range sequences {
		work[i] = append([]string(nil), s...)
	}

	var out []Pattern
	for {
		cands := closedCandidates(work, minSupport, p.Gap)
		if len(cands) == 0 {
			return out
		}
		for i := range cands {
			score(&cands[i], total)
		}
		sort.SliceStable(cands, func(i, j int) bool { return betterPattern(cands[i], cands[j]) })
		winner := cands[0]
		out = append(out, winner)
		removeOccurrences(work, winner)
	}
}

// betterPattern is the ranking, and it is a total order so that two replicas
// mining one corpus agree. Coverage first, cohesion second, then length,
// then support, then the symbols themselves -- the last tiebreak exists only
// so the answer never depends on map iteration order.
func betterPattern(a, b Pattern) bool {
	switch {
	case a.Coverage != b.Coverage:
		return a.Coverage > b.Coverage
	case a.Cohesion != b.Cohesion:
		return a.Cohesion > b.Cohesion
	case len(a.Symbols) != len(b.Symbols):
		return len(a.Symbols) > len(b.Symbols)
	case a.Support != b.Support:
		return a.Support > b.Support
	default:
		return strings.Join(a.Symbols, "\x00") < strings.Join(b.Symbols, "\x00")
	}
}

func score(p *Pattern, totalSymbols int) {
	p.Support = len(p.Occurrences)
	p.Coverage = float64(p.Support*len(p.Symbols)) / float64(totalSymbols)
	gapTotal, gapCount := 0, 0
	for _, occ := range p.Occurrences {
		for i := 1; i < len(occ.Positions); i++ {
			gapTotal += occ.Positions[i] - occ.Positions[i-1] - 1
			gapCount++
		}
	}
	if gapCount == 0 {
		p.Cohesion = 1
		return
	}
	p.Cohesion = 1 / (1 + float64(gapTotal)/float64(gapCount))
}

// closedCandidates grows patterns breadth-first and keeps only the closed
// ones. The corpus this runs against is a learning corpus -- tens of steps per
// run, tens of runs -- so the straightforward enumeration is the right one;
// anything cleverer would be harder to check against the reference
// implementations the parity harness runs.
func closedCandidates(sequences [][]string, minSupport, gap int) []Pattern {
	frontier := map[string]Pattern{}
	for _, sym := range distinctSymbols(sequences) {
		p := Pattern{Symbols: []string{sym}}
		p.Occurrences = findOccurrences(sequences, p.Symbols, gap)
		if len(p.Occurrences) >= minSupport {
			frontier[sym] = p
		}
	}
	all := map[string]Pattern{}
	for len(frontier) > 0 {
		next := map[string]Pattern{}
		for key, p := range frontier {
			all[key] = p
			for _, sym := range distinctSymbols(sequences) {
				grown := append(append([]string(nil), p.Symbols...), sym)
				occ := findOccurrences(sequences, grown, gap)
				if len(occ) < minSupport {
					continue
				}
				gk := strings.Join(grown, "\x00")
				if _, seen := next[gk]; !seen {
					next[gk] = Pattern{Symbols: grown, Occurrences: occ}
				}
			}
		}
		frontier = next
	}

	// Closure: drop any pattern that a longer one contains at the same
	// support.
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Pattern
	for _, k := range keys {
		p := all[k]
		if len(p.Symbols) < 2 {
			continue
		}
		closed := true
		for _, ok2 := range keys {
			q := all[ok2]
			if len(q.Symbols) > len(p.Symbols) &&
				len(q.Occurrences) == len(p.Occurrences) &&
				containsSubsequence(q.Symbols, p.Symbols) {
				closed = false
				break
			}
		}
		if closed {
			out = append(out, p)
		}
	}
	return out
}

// findOccurrences finds at most one occurrence of pattern per sequence,
// leftmost-greedy, with at most `gap` unmatched symbols between each ADJACENT
// pair. Per-pair is deliberate: a tolerance spent across the whole occurrence
// would let one long interruption be amortized against a tight one, and the
// pattern would claim a cohesion it does not have.
func findOccurrences(sequences [][]string, pattern []string, gap int) []Occurrence {
	var out []Occurrence
	for si, seq := range sequences {
		if pos, ok := matchFrom(seq, pattern, gap); ok {
			out = append(out, Occurrence{Sequence: si, Positions: pos})
		}
	}
	return out
}

func matchFrom(seq, pattern []string, gap int) ([]int, bool) {
	for start := 0; start <= len(seq)-len(pattern); start++ {
		if seq[start] != pattern[0] {
			continue
		}
		pos := []int{start}
		cur := start
		ok := true
		for _, want := range pattern[1:] {
			found := -1
			for j := cur + 1; j <= cur+1+gap && j < len(seq); j++ {
				if seq[j] == want {
					found = j
					break
				}
			}
			if found < 0 {
				ok = false
				break
			}
			pos = append(pos, found)
			cur = found
		}
		if ok {
			return pos, true
		}
	}
	return nil, false
}

// removeOccurrences blanks the winner's positions so the next round scores
// against what is left. A blanked slot keeps the sequence's LENGTH, which
// matters: positions are how cohesion is measured, and compacting the slice
// would make every later pattern look more contiguous than it was.
func removeOccurrences(sequences [][]string, p Pattern) {
	for _, occ := range p.Occurrences {
		for _, pos := range occ.Positions {
			sequences[occ.Sequence][pos] = ""
		}
	}
}

func distinctSymbols(sequences [][]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sequences {
		for _, sym := range s {
			if sym == "" || seen[sym] {
				continue
			}
			seen[sym] = true
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

func containsSubsequence(hay, needle []string) bool {
	i := 0
	for _, h := range hay {
		if i < len(needle) && h == needle[i] {
			i++
		}
	}
	return i == len(needle)
}
