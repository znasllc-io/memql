package parser

// source_size_test.go -- CheckSourceSize is the bound itself: a length
// comparison, a typed refusal, and no work on the source.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// checkers is the two bounds and what each is for: one FILE and one BUNDLE of
// constructs. A wire surface picks by the payload field's name.
var checkers = []struct {
	name  string
	check func(string) error
	limit int
	label string
}{
	{"CheckSourceSize", CheckSourceSize, MaxSourceBytes, "512 KiB"},
	{"CheckBundleSize", CheckBundleSize, MaxBundleBytes, "2 MiB"},
}

func TestEachBoundRefusesOnlyPastItself(t *testing.T) {
	for _, c := range checkers {
		t.Run(c.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				n       int
				refused bool
			}{
				{"empty", 0, false},
				{"one byte under", c.limit - 1, false},
				{"exactly the bound", c.limit, false},
				{"one byte over", c.limit + 1, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					err := c.check(strings.Repeat("a", tc.n))
					if !tc.refused {
						if err != nil {
							t.Fatalf("%d bytes must be accepted: %v", tc.n, err)
						}
						return
					}
					var refusal *SourceTooLargeError
					if !errors.As(err, &refusal) {
						t.Fatalf("refusal must be a *SourceTooLargeError, got %T: %v", err, err)
					}
					if refusal.Bytes != tc.n || refusal.Limit != c.limit {
						t.Errorf("Bytes/Limit = %d/%d, want %d/%d", refusal.Bytes, refusal.Limit, tc.n, c.limit)
					}
					if refusal.RuleCode() != RuleSourceTooLarge {
						t.Errorf("RuleCode() = %q, want %q", refusal.RuleCode(), RuleSourceTooLarge)
					}
					for _, want := range []string{
						fmt.Sprintf("source is %d bytes, over the %s limit", tc.n, c.label),
						"send one file at a time, or split it into smaller files",
						"[" + RuleSourceTooLarge + "]",
					} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("refusal is missing %q\n  full: %s", want, err.Error())
						}
					}
					if !strings.HasSuffix(err.Error(), "["+RuleSourceTooLarge+"]") {
						t.Errorf("the code is printed last, in brackets: %s", err.Error())
					}
				})
			}
		})
	}
}

// The two bounds are not one: a bundle between them is accepted as a bundle
// and refused as a file. Collapsing them would put the file bound 1.18x over
// the largest whole domain in the tree.
func TestABundleBetweenTheBoundsIsAcceptedAsABundleOnly(t *testing.T) {
	between := strings.Repeat("a", MaxSourceBytes+1)
	if err := CheckBundleSize(between); err != nil {
		t.Errorf("a bundle over the FILE bound and under the bundle bound must be accepted: %v", err)
	}
	if err := CheckSourceSize(between); err == nil {
		t.Error("the same source must be refused as one file")
	}
}

// Each bound leaves headroom over what it is sized against, and they are
// sized against different things (source_size.go): a Sense request carries the
// one file an author has open, an authoring payload carries a bundle of
// constructs, and the largest whole DOMAIN in this tree is 3.3 times the
// largest single file. A bound under its own figure would refuse real input
// the first time somebody sent it.
func TestEachBoundIsWellClearOfWhatItIsSizedAgainst(t *testing.T) {
	const (
		largestRealFile   = 135_297 // dsl/identity/mutations.memql
		largestRealDomain = 444_924 // dsl/identity/
	)
	if MaxSourceBytes < 3*largestRealFile {
		t.Errorf("MaxSourceBytes is %d, under three times the largest .memql file in the tree (%d)",
			MaxSourceBytes, largestRealFile)
	}
	if MaxBundleBytes < 3*largestRealDomain {
		t.Errorf("MaxBundleBytes is %d, under three times the largest whole domain in the tree (%d)",
			MaxBundleBytes, largestRealDomain)
	}
}

// The check reads a length and nothing else, which is the whole point: the
// source it refuses is one that would cost gigabytes to tokenise.
func TestCheckSourceSizeNeitherReadsNorCopiesTheSource(t *testing.T) {
	source := strings.Repeat("1+", (20<<20)/2) // 20 MiB, ~20 million tokens

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := CheckSourceSize(source)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("20 MiB must be refused")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("refused 20 MiB in %v, allocating %d bytes", elapsed, allocated)
	if elapsed > time.Millisecond {
		t.Errorf("the refusal took %v; it compares a length", elapsed)
	}
	if allocated > 1<<20 {
		t.Errorf("the refusal allocated %d bytes; it must not touch the source", allocated)
	}
}
