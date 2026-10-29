package library

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// lines builds a log of n lines, numbered from 1, each ending in a newline.
func lines(n int, f func(i int) string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(f(i))
		b.WriteByte('\n')
	}
	return b.String()
}

func mustExcerptPatterns(t *testing.T, raw ...string) []*regexp.Regexp {
	t.Helper()
	patterns, err := compileExcerptPatterns(raw)
	if err != nil {
		t.Fatal(err)
	}
	return patterns
}

func mustExcerpt(t *testing.T, input string, share int, raw ...string) textExcerpt {
	t.Helper()
	ex, err := excerptText(context.Background(), strings.NewReader(input), mustExcerptPatterns(t, raw...), share)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

var excerptRangeMarker = regexp.MustCompile(`^\[\.\.\. lines? ([0-9]+)(?:-([0-9]+))? omitted \.\.\.\]$`)

// omitted reports the 1-based inclusive range a marker line names.
func omitted(line string) (from, to int, ok bool) {
	m := excerptRangeMarker.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	from, _ = strconv.Atoi(m[1])
	to = from
	if m[2] != "" {
		to, _ = strconv.Atoi(m[2])
	}
	return from, to, true
}

func TestCompileExcerptPatternsRefusesBadInput(t *testing.T) {
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	for name, raw := range map[string][]string{
		"none":     nil,
		"empty":    {},
		"too many": many,
		"too long": {strings.Repeat("a", 257)},
		"blank":    {"  "},
		"invalid":  {"("},
	} {
		if _, err := compileExcerptPatterns(raw); err == nil {
			t.Errorf("%s: patterns %q were accepted", name, raw)
		}
	}
	patterns, err := compileExcerptPatterns([]string{"--- FAIL", "^FAIL", "(?i)^error:"})
	if err != nil || len(patterns) != 3 {
		t.Fatalf("valid patterns: %v, %d compiled", err, len(patterns))
	}
}

// A caller's patterns are matched against every line of up to 64 MiB, so their
// compiled size is bounded as well as their length: 256 bytes can compile to
// tens of thousands of instructions.
func TestCompileExcerptPatternsRefusesAnExpensivePattern(t *testing.T) {
	expensive := strings.Repeat("(?:.{0,30}){30}", 17) + "y"
	if len(expensive) != 256 {
		t.Fatalf("fixture is %d bytes", len(expensive))
	}
	if _, err := compileExcerptPatterns([]string{expensive}); err == nil {
		t.Fatal("a pattern compiling to thousands of instructions was accepted")
	}
	diagnosis := []string{"--- FAIL", "^FAIL", "^panic: ", "fatal error: ", "[.]go:[0-9]+:[0-9]+: ", "_test[.]go:[0-9]+: ",
		"Error Trace:", "error TS[0-9]+:", "^ *(FAIL|×|✗) ", "AssertionError", "^Error: ", ": [*][*][*] ",
		"^ERROR", "failed to solve", "Traceback [(]most recent call last[)]", "exit (status|code):? [1-9]", "(?i)^error:"}
	if _, err := compileExcerptPatterns(diagnosis); err != nil {
		t.Fatalf("a failure-diagnosis pattern set was refused: %v", err)
	}
}

func TestExcerptMatchesOnlyTheStartOfALongLine(t *testing.T) {
	input := strings.Repeat("x", 5000) + "FAIL late\n" + "FAIL early " + strings.Repeat("x", 5000) + "\n"
	if ex := mustExcerpt(t, input, 1024, "FAIL"); ex.MatchedLines != 1 {
		t.Fatalf("%d lines matched; only a match in a line's first 4 KiB counts", ex.MatchedLines)
	}
}

func TestExcerptStopsWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := lines(5000, func(i int) string { return fmt.Sprintf("line %d", i) })
	if _, err := excerptText(ctx, strings.NewReader(input), mustExcerptPatterns(t, "FAIL"), 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled scan answered %v", err)
	}
}

func TestExcerptReturnsAFileThatFitsWhole(t *testing.T) {
	input := "line one\nline two\n"
	ex := mustExcerpt(t, input, 1024, "FAIL")
	if ex.Text != input || !ex.Complete || ex.TotalLines != 2 || ex.TotalBytes != 18 || ex.MatchedLines != 0 || ex.ScanCapped {
		t.Fatalf("whole file not returned as it is: %+v", ex)
	}
}

func TestExcerptRepairsNULAndInvalidUTF8(t *testing.T) {
	ex := mustExcerpt(t, "a\x00b\n\xff\xfe ok\n", 1024, "FAIL")
	if ex.Text != "ab\n� ok\n" || !ex.Complete {
		t.Fatalf("NUL or invalid UTF-8 survived: %q", ex.Text)
	}
}

func TestExcerptKeepsHeadMatchesAndTailInFileOrder(t *testing.T) {
	input := lines(2000, func(i int) string {
		switch i {
		case 1000:
			return "--- FAIL: TestX (0.00s)"
		case 1001:
			return "    x_test.go:12: got 1, want 2"
		}
		return fmt.Sprintf("ok  pkg%d 0.1s", i)
	})
	ex := mustExcerpt(t, input, 4096, "--- FAIL", "_test[.]go:[0-9]+: ")
	if ex.Complete || len(ex.Text) > 4096 || ex.MatchedLines != 2 || ex.TotalLines != 2000 {
		t.Fatalf("excerpt bounds: complete=%v len=%d matched=%d lines=%d", ex.Complete, len(ex.Text), ex.MatchedLines, ex.TotalLines)
	}
	got := strings.Split(strings.TrimSuffix(ex.Text, "\n"), "\n")
	if got[0] != "ok  pkg1 0.1s" {
		t.Fatalf("head missing, first line %q", got[0])
	}
	if got[len(got)-1] != "ok  pkg2000 0.1s" {
		t.Fatalf("tail missing, last line %q", got[len(got)-1])
	}
	want := strings.Split(strings.TrimSuffix(lines(17, func(i int) string {
		return strings.Split(input, "\n")[996+i-1]
	}), "\n"), "\n")
	if !strings.Contains(ex.Text, strings.Join(want, "\n")+"\n") {
		t.Fatalf("lines 997-1013 are not kept together:\n%s", ex.Text)
	}
	if !strings.Contains(ex.Text, "[... lines ") {
		t.Fatalf("no omission marker:\n%s", ex.Text)
	}
}

func TestExcerptMergesOverlappingWindowsAndExtendsOnAMatch(t *testing.T) {
	input := lines(400, func(i int) string {
		if i == 100 || i == 108 {
			return fmt.Sprintf("FAIL %d", i)
		}
		return fmt.Sprintf("line %d", i)
	})
	ex := mustExcerpt(t, input, 8192, "^FAIL")
	var block []string
	for i := 97; i <= 120; i++ {
		block = append(block, strings.Split(input, "\n")[i-1])
	}
	if !strings.Contains(ex.Text, strings.Join(block, "\n")+"\n") {
		t.Fatalf("lines 97-120 are not one window:\n%s", ex.Text)
	}
}

func TestExcerptCountsMatchesPastTheBudgetAndKeepsTheTail(t *testing.T) {
	input := lines(10000, func(i int) string { return fmt.Sprintf("FAIL case %d", i) })
	ex := mustExcerpt(t, input, 4096, "^FAIL")
	got := strings.Split(strings.TrimSuffix(ex.Text, "\n"), "\n")
	if ex.MatchedLines != 10000 || got[len(got)-1] != "FAIL case 10000" || len(ex.Text) > 4096 {
		t.Fatalf("matched=%d last=%q len=%d", ex.MatchedLines, got[len(got)-1], len(ex.Text))
	}
}

func TestExcerptWithNoMatchKeepsHeadAndALongerTail(t *testing.T) {
	input := lines(5000, func(i int) string { return fmt.Sprintf("progress %d", i) })
	ex := mustExcerpt(t, input, 8192, "^FAIL")
	got := strings.Split(strings.TrimSuffix(ex.Text, "\n"), "\n")
	markers := 0
	tail := 0
	for i, line := range got {
		if _, _, ok := omitted(line); ok {
			markers++
			for _, rest := range got[i+1:] {
				tail += len(rest) + 1
			}
		}
	}
	if got[0] != "progress 1" || markers != 1 || tail <= 4096 || len(ex.Text) > 8192 {
		t.Fatalf("first=%q markers=%d tail=%d len=%d", got[0], markers, tail, len(ex.Text))
	}
}

func TestExcerptCutsALineLongerThanTwoKiB(t *testing.T) {
	input := strings.Repeat("x", 10<<20) + "\nFAIL end\n"
	ex := mustExcerpt(t, input, 4096, "^FAIL")
	if !strings.Contains(ex.Text, " [... ") || !strings.Contains(ex.Text, "bytes cut ...]") || !strings.Contains(ex.Text, "FAIL end") || len(ex.Text) > 4096 {
		t.Fatalf("long line not cut within the share: len=%d\n%.300s", len(ex.Text), ex.Text)
	}
}

func TestExcerptStopsAtTheScanLimitAndSaysSo(t *testing.T) {
	input := lines(40000, func(i int) string { return fmt.Sprintf("line %050d", i) })
	ex, err := excerptTextLimited(context.Background(), strings.NewReader(input), mustExcerptPatterns(t, "^FAIL"), 4096, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(ex.Text, "\n"), "\n")
	if !ex.ScanCapped || ex.TotalBytes != 1<<20 || got[len(got)-1] != "[... the file continues past the first 1 MiB, which is all this excerpt reads ...]" {
		t.Fatalf("capped=%v bytes=%d last=%q", ex.ScanCapped, ex.TotalBytes, got[len(got)-1])
	}
}

func TestExcerptNeverExceedsItsShareAndKeepsLinesInOrder(t *testing.T) {
	cut := regexp.MustCompile(` \[\.\.\. [0-9]+ bytes cut \.\.\.\]$`)
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		input := make([]string, 1+rng.Intn(3000))
		for i := range input {
			if rng.Intn(40) == 0 {
				input[i] = fmt.Sprintf("FAIL %d", i)
				continue
			}
			input[i] = strings.Repeat(string(rune('a'+rng.Intn(26))), rng.Intn(3001))
		}
		share := 1024 + rng.Intn(65536-1024+1)
		raw := strings.Join(input, "\n") + "\n"
		ex := mustExcerpt(t, raw, share, "^FAIL")
		if len(ex.Text) > share || !utf8.ValidString(ex.Text) {
			t.Fatalf("seed %d: len %d over share %d, or invalid UTF-8", seed, len(ex.Text), share)
		}
		if ex.Complete {
			if ex.Text != raw {
				t.Fatalf("seed %d: a complete excerpt differs from its file", seed)
			}
			continue
		}
		next := 0 // the 0-based input line the next excerpt line must show
		for _, line := range strings.Split(strings.TrimSuffix(ex.Text, "\n"), "\n") {
			if from, to, ok := omitted(line); ok {
				if from != next+1 || to < from {
					t.Fatalf("seed %d: marker %q does not start at line %d", seed, line, next+1)
				}
				next = to
				continue
			}
			if next >= len(input) {
				t.Fatalf("seed %d: excerpt continues past the file", seed)
			}
			if line != input[next] {
				prefix := cut.ReplaceAllString(line, "")
				if prefix == line || !strings.HasPrefix(input[next], prefix) {
					t.Fatalf("seed %d: line %d is %.40q, the file has %.40q", seed, next+1, line, input[next])
				}
			}
			next++
		}
		if next != len(input) {
			t.Fatalf("seed %d: excerpt stops at line %d of %d without a marker", seed, next, len(input))
		}
	}
}
