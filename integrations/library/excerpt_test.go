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

// vitest 5 colours its output with no terminal and no CI variable: it stops
// only for NO_COLOR, TERM=dumb or FORCE_TTY=false, and a step's container sets
// none. These lines are what it printed for a failing test in a Linux container
// on Node 22, the toolchain's, with its output in a file.
const (
	vitestFailLine      = "\x1b[41m\x1b[1m FAIL \x1b[22m\x1b[49m fail.test.js\x1b[2m > \x1b[22mboom"
	vitestCrossLine     = "\x1b[31m   \x1b[31m×\x1b[31m boom\x1b[39m\x1b[32m 3\x1b[2mms\x1b[22m\x1b[39m"
	vitestAssertionLine = "\x1b[31m\x1b[1mAssertionError\x1b[22m: expected 1 to be 2 // Object.is equality\x1b[39m"
)

func TestExcerptMatchesAColouredLineAndKeepsItWithoutItsColour(t *testing.T) {
	input := lines(3000, func(i int) string {
		switch i {
		case 1500:
			return vitestFailLine
		case 1600:
			return vitestCrossLine
		case 1700:
			return vitestAssertionLine
		}
		return fmt.Sprintf("\x1b[32m✓\x1b[39m src/app%d.test.ts \x1b[2m(1 test)\x1b[22m", i)
	})
	// The recipe's own patterns for these lines, each anchored.
	ex := mustExcerpt(t, input, 8192, "^ *(FAIL|×|✗) ", "^AssertionError")
	if ex.MatchedLines != 3 || ex.Complete || len(ex.Text) > 8192 {
		t.Fatalf("matched=%d complete=%v len=%d", ex.MatchedLines, ex.Complete, len(ex.Text))
	}
	if strings.Contains(ex.Text, "\x1b") {
		t.Fatalf("a colour code survived in a kept line:\n%q", ex.Text)
	}
	for _, want := range []string{" FAIL  fail.test.js > boom\n", "   × boom 3ms\n", "AssertionError: expected 1 to be 2 // Object.is equality\n", "✓ src/app3000.test.ts (1 test)\n"} {
		if !strings.Contains(ex.Text, want) {
			t.Errorf("missing %q in:\n%s", want, ex.Text)
		}
	}
}

// complete means the file itself, so the colour stays. Its lines are still
// matched without it, and counted.
func TestExcerptReturnsAColouredFileThatFitsWithItsBytes(t *testing.T) {
	input := "ok\n" + vitestFailLine + "\n"
	ex := mustExcerpt(t, input, 1024, "^ *FAIL ")
	if ex.Text != input || !ex.Complete || ex.MatchedLines != 1 || ex.TotalBytes != int64(len(input)) {
		t.Fatalf("a whole file was changed or its line missed: %+v", ex)
	}
}

// A kept line is measured and cut without its colour: a line short of the limit
// but for its codes is kept whole, and no code is cut in half.
func TestExcerptMeasuresAColouredLineWithoutItsColour(t *testing.T) {
	short := strings.Repeat("\x1b[31mx\x1b[39m", 1000) // 11 KB written, 1 KB of text
	long := strings.Repeat("\x1b[31mxyz\x1b[39m", 900) // 2,700 bytes of text
	huge := strings.Repeat("\x1b[31mxyz\x1b[39m", 8000)
	input := lines(4000, func(i int) string {
		switch i {
		case 3998:
			return short
		case 3999:
			return long
		case 4000:
			return huge
		}
		return "progress"
	})
	ex := mustExcerpt(t, input, 64<<10, "^FAIL")
	got := strings.Split(strings.TrimSuffix(ex.Text, "\n"), "\n")
	if strings.Contains(ex.Text, "\x1b") || len(got) < 3 {
		t.Fatalf("a colour code survived, or lines went missing: %d lines", len(got))
	}
	last := got[len(got)-3:]
	if last[0] != strings.Repeat("x", 1000) {
		t.Errorf("a line short of the limit was changed: %.60q", last[0])
	}
	// The cut is at 2 KiB of text, and says how much of the rest it cut.
	if want := strings.Repeat("xyz", 682) + "xy [... 652 bytes cut ...]"; last[1] != want {
		t.Errorf("long line kept as %.40q ... %q", last[1], last[1][max(0, len(last[1])-40):])
	}
	if !strings.HasPrefix(last[2], strings.Repeat("xyz", 682)+"xy [... ") || !strings.HasSuffix(last[2], " bytes cut ...]") {
		t.Errorf("a line past the hold kept as %.40q ... %q", last[2], last[2][max(0, len(last[2])-40):])
	}
}

func TestStripExcerptCSI(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"no escape":                {"plain text", "plain text"},
		"colour":                   {"\x1b[31mred\x1b[0m", "red"},
		"256 colours":              {"\x1b[38;5;196mred\x1b[39m", "red"},
		"colon parameters":         {"\x1b[38:2::1:2:3mrgb\x1b[m", "rgb"},
		"erase line":               {"\x1b[2Kline", "line"},
		"private mode":             {"\x1b[?25lhidden\x1b[?25h", "hidden"},
		"intermediate byte":        {"a\x1b[1 qb", "ab"},
		"keeps invalid UTF-8":      {"\xff\x1b[1m\xfe", "\xff\xfe"},
		"not a control sequence":   {"a\x1b]0;title\x07b \x1bc", "a\x1b]0;title\x07b \x1bc"},
		"an unfinished sequence":   {"text\x1b[3", "text\x1b[3"},
		"a bracket with no escape": {"[31mtext", "[31mtext"},
	} {
		if got := string(stripExcerptCSI([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: %q became %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// An excerpt that comes back empty looks like an empty file, and windows may
// take three quarters of a share, so at the floor what is left has to hold both
// markers at their longest.
func TestTheExcerptFloorLeavesRoomForItsMarkers(t *testing.T) {
	markers := len(excerptScanMarker(excerptScanLimit)) + 1 + len(excerptGapMarker(excerptScanLimit-1, excerptScanLimit)) + 1
	if left := excerptMinBytes / 4; left < markers {
		t.Fatalf("the floor leaves %d bytes past the windows; the markers can take %d", left, markers)
	}
}

func TestExcerptOfACappedFileAtTheFloorIsNotEmpty(t *testing.T) {
	const limit = 1 << 20
	for name, input := range map[string]string{
		"every line matches":      lines(300000, func(int) string { return "FAIL" }),
		"windows, then long ends": lines(60000, func(i int) string { return fmt.Sprintf("FAIL %s", strings.Repeat("x", i%300)) }),
		"one line past the cap":   strings.Repeat("x", 2*limit),
	} {
		ex, err := excerptTextLimited(context.Background(), strings.NewReader(input), mustExcerptPatterns(t, "^FAIL"), excerptMinBytes, limit)
		if err != nil {
			t.Fatal(err)
		}
		if !ex.ScanCapped || len(ex.Text) > excerptMinBytes || !strings.HasSuffix(ex.Text, excerptScanMarker(limit)+"\n") {
			t.Errorf("%s: capped=%v len=%d: %.200q", name, ex.ScanCapped, len(ex.Text), ex.Text)
		}
	}
}

func TestExcerptNeverExceedsItsShareAndKeepsLinesInOrder(t *testing.T) {
	cut := regexp.MustCompile(` \[\.\.\. [0-9]+ bytes cut \.\.\.\]$`)
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		input := make([]string, 1+rng.Intn(3000))
		written := make([]string, len(input)) // input as the file holds it: every other seed's is coloured
		failures := 0
		for i := range input {
			if rng.Intn(40) == 0 {
				input[i] = fmt.Sprintf("FAIL %d", i)
				failures++
			} else {
				input[i] = strings.Repeat(string(rune('a'+rng.Intn(26))), rng.Intn(3001))
			}
			written[i] = input[i]
			if seed%2 == 0 {
				written[i] = "\x1b[31m" + input[i] + "\x1b[39m"
			}
		}
		// Every share the handler can hand out, the floor itself among them.
		share := excerptMinBytes + rng.Intn(excerptMaxBytes-excerptMinBytes+1)
		if seed%10 == 0 {
			share = excerptMinBytes
		}
		raw := strings.Join(written, "\n") + "\n"
		ex := mustExcerpt(t, raw, share, "^FAIL")
		if len(ex.Text) > share || !utf8.ValidString(ex.Text) || ex.Text == "" {
			t.Fatalf("seed %d: len %d over share %d, invalid UTF-8, or empty", seed, len(ex.Text), share)
		}
		if ex.MatchedLines != failures {
			t.Fatalf("seed %d: matched %d of %d failure lines", seed, ex.MatchedLines, failures)
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
