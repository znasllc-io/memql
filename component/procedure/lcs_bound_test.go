package procedure

import (
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"testing"
)

// lcs_bound_test.go -- lcsPairs is bounded in memory, and below the bound it is
// exactly the whole-table algorithm it always was.

// lcsPairsWhole is the whole-table LCS lcsPairs was before it was bounded,
// kept here as the reference the bounded one must agree with.
func lcsPairsWhole(a, b []*Node) [][2]int {
	n, m := len(a), len(b)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if sameMeaning(a[i], b[j]) {
				table[i][j] = table[i+1][j+1] + 1
			} else {
				table[i][j] = maxInt(table[i+1][j], table[i][j+1])
			}
		}
	}
	var pairs [][2]int
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case sameMeaning(a[i], b[j]):
			pairs = append(pairs, [2]int{i, j})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pairs
}

func words(r *rand.Rand, n, alphabet int) []*Node {
	out := make([]*Node, n)
	for i := range out {
		out[i] = Lit(fmt.Sprintf("w%d", r.Intn(alphabet)))
	}
	return out
}

// checkCommonSubsequence fails unless pairs is strictly increasing on both
// sides and every pair is two elements that mean the same.
func checkCommonSubsequence(t *testing.T, a, b []*Node, pairs [][2]int) {
	t.Helper()
	for k, p := range pairs {
		if p[0] < 0 || p[0] >= len(a) || p[1] < 0 || p[1] >= len(b) || !sameMeaning(a[p[0]], b[p[1]]) {
			t.Fatalf("pair %d %v is not a match", k, p)
		}
		if k > 0 && (p[0] <= pairs[k-1][0] || p[1] <= pairs[k-1][1]) {
			t.Fatalf("pairs %v then %v are not increasing", pairs[k-1], p)
		}
	}
}

// allocatedBy reports the bytes f allocated.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// BELOW THE BOUND NOTHING CHANGED: on random arrays whose table fits, the
// bounded lcsPairs answers the same pairs as the whole-table algorithm, tie
// for tie -- so every template the symbolizer built before, it builds now.
func TestLCSPairsBelowTheBoundIsTheWholeTableAnswer(t *testing.T) {
	r := rand.New(rand.NewSource(5408))
	for trial := 0; trial < 500; trial++ {
		a, b := words(r, r.Intn(60), 1+r.Intn(6)), words(r, r.Intn(60), 1+r.Intn(6))
		if got, want := lcsPairs(a, b), lcsPairsWhole(a, b); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: lcsPairs = %v, want the whole-table %v", trial, got, want)
		}
	}
}

// TWO LONG RECORDINGS OF ONE COMMAND: 4000 words each, differing in five in the
// middle. The whole table would be 4001 x 4001 ints (about 128 MB); the bounded
// one pairs the shared prefix and suffix directly and solves only the middle,
// and still finds the longest common subsequence.
func TestLCSPairsOfTwoLongNearlyEqualCommandsIsExactAndSmall(t *testing.T) {
	r := rand.New(rand.NewSource(5409))
	a := words(r, 4000, 50)
	b := append([]*Node(nil), a...)
	for k := 1998; k < 2003; k++ {
		b[k] = Lit(fmt.Sprintf("changed%d", k))
	}
	var pairs [][2]int
	bytes := allocatedBy(func() { pairs = lcsPairs(a, b) })
	checkCommonSubsequence(t, a, b, pairs)
	if len(pairs) != 3995 {
		t.Fatalf("%d pairs, want the 3995 words the two commands share", len(pairs))
	}
	if bytes > 16<<20 {
		t.Fatalf("lcsPairs allocated %d bytes for two 4000-word commands, want the table bounded", bytes)
	}
}

// NOTHING IN COMMON AT EITHER END, AND TOO LONG TO SOLVE: two 3000-word arrays
// whose middle is past the bound. The answer is a common subsequence -- the
// elements that agree at the same offset -- and memory stays bounded.
func TestLCSPairsPastTheBoundStaysBoundedAndValid(t *testing.T) {
	r := rand.New(rand.NewSource(5410))
	a, b := words(r, 3000, 3), words(r, 3000, 3)
	a[0], b[0] = Lit("left"), Lit("right")
	a[len(a)-1], b[len(b)-1] = Lit("end-a"), Lit("end-b")
	var pairs [][2]int
	bytes := allocatedBy(func() { pairs = lcsPairs(a, b) })
	checkCommonSubsequence(t, a, b, pairs)
	if bytes > 16<<20 {
		t.Fatalf("lcsPairs allocated %d bytes for two 3000-word arrays, want the table bounded", bytes)
	}
	if len(pairs) == 0 {
		t.Fatal("arrays from a three-word alphabet share words at the same offset, and none were paired")
	}
}

// THE BOUND ITSELF: a table that fits is solved whole, one that does not is
// refused before any product is taken, so no length can overflow it.
func TestLCSFitsBoundsEachLengthBeforeTheProduct(t *testing.T) {
	for _, tc := range []struct {
		n, m int
		want bool
	}{
		{0, 0, true},
		{1023, 1023, true},
		{1024, 1024, false},
		{maxLCSCells, 0, false},
		{int(^uint(0) >> 1), int(^uint(0) >> 1), false},
		{-1, 5, false},
	} {
		if got := lcsFits(tc.n, tc.m); got != tc.want {
			t.Errorf("lcsFits(%d, %d) = %v, want %v", tc.n, tc.m, got, tc.want)
		}
	}
}
