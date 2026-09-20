package parser

// source_size_test.go -- CheckSourceSize is the bound itself: a length
// comparison, a typed refusal, and no work on the source.

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckSourceSizeRefusesOnlyPastTheBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n       int
		refused bool
	}{
		{"empty", 0, false},
		{"one byte under", MaxSourceBytes - 1, false},
		{"exactly the bound", MaxSourceBytes, false},
		{"one byte over", MaxSourceBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSourceSize(strings.Repeat("a", tc.n))
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
			if refusal.Bytes != tc.n {
				t.Errorf("Bytes = %d, want %d", refusal.Bytes, tc.n)
			}
			if refusal.RuleCode() != RuleSourceTooLarge {
				t.Errorf("RuleCode() = %q, want %q", refusal.RuleCode(), RuleSourceTooLarge)
			}
			for _, want := range []string{
				"source is 524289 bytes, over the 512 KiB limit",
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
}

// The bound leaves headroom over what anyone edits: the largest .memql file in
// this repository is 135,297 bytes. A bound below that would refuse a real
// file the moment an author opened it in the editor.
func TestTheBoundIsWellClearOfTheLargestRealFile(t *testing.T) {
	const largestRealFile = 135_297
	if MaxSourceBytes < 3*largestRealFile {
		t.Errorf("MaxSourceBytes is %d, under three times the largest .memql file in the tree (%d)",
			MaxSourceBytes, largestRealFile)
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
