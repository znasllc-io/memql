package parser

import (
	"math"
	"testing"
)

func TestLCSPairsRejectsOverflowingDimensions(t *testing.T) {
	for _, size := range [][2]int{{math.MaxInt, 2}, {2, math.MaxInt}, {math.MaxInt, math.MaxInt}, {positionLCSMaxCells + 1, 1}} {
		pairs, ok := lcsPairs(size[0], size[1], func(int, int) bool { return false })
		if ok || pairs != nil {
			t.Fatalf("lcsPairs(%d, %d) must decline the oversized matrix", size[0], size[1])
		}
	}
}

func TestLCSLinePairsBoundsAtAllocation(t *testing.T) {
	// Call directly so the allocation remains safe independently of NewLineMap.
	a, b := make([]string, 4000), make([]string, 4000)
	if got := lcsLinePairs(a, b); got != nil {
		t.Fatal("oversized matrix should use the caller's unmatched-line fallback")
	}
	if got := lcsLinePairs(nil, []string{"a"}); got != nil {
		t.Fatal("empty input should have no matches")
	}
	got := lcsLinePairs([]string{"a", "b", "c"}, []string{"a", "c"})
	if len(got) != 2 || got[0] != (linePair{0, 0}) || got[1] != (linePair{2, 1}) {
		t.Fatalf("ordinary alignment changed: %v", got)
	}
}
