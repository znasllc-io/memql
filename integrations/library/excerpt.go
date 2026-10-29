package library

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

// excerpt.go -- a bounded excerpt of a text file too long to hand over whole
// (docs/superpowers/specs/2026-10-29-failed-run-diagnosis-evidence-design.md).
//
// The case it was written for: a failed pipeline step's retained log can run
// to 64 MiB, and the diagnosis reading it has a few tens of KiB. What a reader
// of a failure needs is where the failure is printed and how the log ends, so
// an excerpt keeps, in file order: the first lines, the lines a caller's
// patterns match with three lines before and twelve after (earliest first,
// while the window budget lasts), and the end of the file. Everything left out is one marker line naming the omitted range,
// so a reader can tell an excerpt from the whole and where its gaps are.
//
// THE BUDGET IS A HARD BOUND ON THE OUTPUT, markers included: windows stop at
// three quarters of the share, and the tail takes exactly what the head, the
// windows and the markers leave. Matches past the window budget are counted,
// never kept, so a pattern matching every line cannot crowd out the end of the
// file. Memory is bounded the same way: a line is held to its first 64 KiB
// (enough to return a file that fits whole) and kept as at most its first
// 2 KiB, so one enormous line is read through rather than held.
//
// CPU IS BOUNDED TOO, because any signed-in caller can name the patterns and
// the file: patterns are refused past a total compiled size, a line is matched
// against only its first 4 KiB, and the scan stops when its context ends (the
// handler gives every call a deadline).
//
// It adds no secret masking. A pipeline step's archive was masked when it was
// captured (integrations/pipelinesteps/capture.go); any other file is its
// reader's own.

const (
	// excerptMinBytes and excerptMaxBytes bound one call's total budget.
	excerptMinBytes = 1 << 10
	excerptMaxBytes = 64 << 10
	// excerptMaxFiles and excerptMaxPatterns bound one call's inputs.
	excerptMaxFiles        = 8
	excerptMaxPatterns     = 32
	excerptPatternMaxBytes = 256
	// excerptScanLimit is the step-log archive cap
	// (integrations/pipelinesteps/config.go, ArchiveMaxBytes).
	excerptScanLimit = 64 << 20
	// excerptHeadMax bounds the leading lines, which also stop at a sixteenth
	// of a small share.
	excerptHeadMax = 2 << 10
	// excerptLineMax is how much of one line an excerpt keeps.
	excerptLineMax = 2 << 10
	// excerptBefore and excerptAfter are a match's context, in lines.
	excerptBefore = 3
	excerptAfter  = 12
	// excerptHoldMax is how much of one line is held: enough for a line of a
	// file small enough to return whole.
	excerptHoldMax = 64 << 10
	// excerptMatchMax is how much of one line the patterns are matched against.
	// Failure markers start their lines; a match deep in a minified line is not
	// worth a scan whose cost grows with the line.
	excerptMatchMax = 4 << 10
	// excerptProgramMax bounds the patterns' total compiled size, in RE2
	// instructions. Matching costs about bytes times instructions, and a
	// 256-byte pattern can compile to tens of thousands; a failure-diagnosis
	// set of seventeen patterns compiles to a few hundred.
	excerptProgramMax = 1024
	// excerptCheckEvery is how many lines are read between context checks.
	excerptCheckEvery = 256
)

// textExcerpt is one file's excerpt and what was read to make it.
type textExcerpt struct {
	Text         string
	Complete     bool
	TotalBytes   int64
	TotalLines   int
	MatchedLines int
	ScanCapped   bool
}

// compileExcerptPatterns compiles a caller's line patterns. A blank pattern is
// refused because it matches every line.
func compileExcerptPatterns(raw []string) ([]*regexp.Regexp, error) {
	if len(raw) == 0 || len(raw) > excerptMaxPatterns {
		return nil, fmt.Errorf("give 1 to %d line patterns", excerptMaxPatterns)
	}
	out := make([]*regexp.Regexp, 0, len(raw))
	size := 0
	for _, p := range raw {
		if strings.TrimSpace(p) == "" || len(p) > excerptPatternMaxBytes {
			return nil, fmt.Errorf("a line pattern must be 1 to %d bytes and not blank: %.40q", excerptPatternMaxBytes, p)
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("line pattern %.40q is not a valid RE2 expression: %v", p, err)
		}
		// regexp.Compile accepted it, so the same parse and compile succeed;
		// they are repeated only to count the program regexp keeps private.
		parsed, _ := syntax.Parse(p, syntax.Perl)
		prog, _ := syntax.Compile(parsed.Simplify())
		size += len(prog.Inst)
		out = append(out, re)
	}
	if size > excerptProgramMax {
		return nil, fmt.Errorf("the line patterns compile to %d instructions, more than the %d an excerpt allows; use fewer or simpler patterns", size, excerptProgramMax)
	}
	return out, nil
}

// excerptText is excerptTextLimited at the step-log archive cap.
func excerptText(ctx context.Context, r io.Reader, patterns []*regexp.Regexp, share int) (textExcerpt, error) {
	return excerptTextLimited(ctx, r, patterns, share, excerptScanLimit)
}

// excerptLine is one line as an excerpt keeps it: repaired, cut, without its
// line ending, and numbered from 1.
type excerptLine struct {
	n    int
	text string
}

func (l excerptLine) cost() int { return len(l.text) + 1 }

// excerptTextLimited reads r once, up to scanLimit bytes, and returns it whole
// when it fits share, else an excerpt of at most share bytes. It returns the
// context's error when ctx ends first.
func excerptTextLimited(ctx context.Context, r io.Reader, patterns []*regexp.Regexp, share int, scanLimit int64) (textExcerpt, error) {
	var ex textExcerpt
	br := bufio.NewReaderSize(io.LimitReader(r, scanLimit), excerptHoldMax)
	headMax := min(excerptHeadMax, share/16)
	windowMax := share - share/4
	var (
		whole       strings.Builder
		wholeFits   = true
		head        []excerptLine
		keptBytes   int // head and windows, the markers between them included
		headOpen    = true
		windows     []excerptLine
		windowsFull bool
		windowUntil int
		lastKept    int
		before      []excerptLine // unkept lines just before the current one
		tail        []excerptLine // the last lines whose cost fits share
		tailBytes   int
	)
	add := func(l excerptLine) bool {
		c := l.cost()
		if l.n != lastKept+1 {
			c += len(excerptGapMarker(lastKept+1, l.n-1)) + 1
		}
		if windowsFull || keptBytes+c > windowMax {
			windowsFull = true
			return false
		}
		windows = append(windows, l)
		keptBytes += c
		lastKept = l.n
		return true
	}
	for {
		if ex.TotalLines%excerptCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return textExcerpt{}, err
			}
		}
		held, n, complete, ended, err := readExcerptLine(br)
		if err != nil {
			return textExcerpt{}, err
		}
		if n == 0 {
			break
		}
		ex.TotalBytes += n
		ex.TotalLines++
		if wholeFits {
			if complete {
				whole.WriteString(repairExcerptText(held))
			}
			if !complete || whole.Len() > share {
				wholeFits = false
				whole.Reset()
			}
		}
		content, contentLen := held, int(n)
		if ended {
			contentLen--
		}
		if complete {
			content = trimExcerptLineEnd(held)
			contentLen = len(content)
		}
		line := excerptLine{n: ex.TotalLines, text: keepExcerptLine(content, contentLen)}
		matchText := repairExcerptText(content[:min(len(content), excerptMatchMax)])
		matched := false
		for _, p := range patterns {
			if p.MatchString(matchText) {
				matched = true
				break
			}
		}
		if matched {
			ex.MatchedLines++
			windowUntil = max(windowUntil, line.n+excerptAfter)
		}
		inHead := false
		if headOpen {
			if keptBytes+line.cost() <= headMax {
				head = append(head, line)
				keptBytes += line.cost()
				lastKept = line.n
				inHead = true
			} else {
				headOpen = false
			}
		}
		if !inHead && !windowsFull {
			switch {
			case matched:
				for _, b := range before {
					if !add(b) {
						break
					}
				}
				before = before[:0]
				add(line)
			case line.n <= windowUntil:
				add(line)
			default:
				before = append(before, line)
				if len(before) > excerptBefore {
					before = before[1:]
				}
			}
		}
		tail = append(tail, line)
		tailBytes += line.cost()
		for tailBytes > share && len(tail) > 1 {
			tailBytes -= tail[0].cost()
			tail = tail[1:]
		}
	}
	var probe [1]byte
	switch k, err := io.ReadFull(r, probe[:]); {
	case k == 1:
		ex.ScanCapped = true
	case err != nil && !errors.Is(err, io.EOF):
		return textExcerpt{}, err
	}
	if wholeFits && !ex.ScanCapped {
		ex.Text, ex.Complete = whole.String(), true
		return ex, nil
	}
	last := ""
	if ex.ScanCapped {
		last = excerptScanMarker(scanLimit)
	}
	ex.Text = assembleExcerpt(append(head, windows...), tail, ex.TotalLines, share, last)
	return ex, nil
}

// assembleExcerpt writes the kept lines, the longest end of the file that fits
// what they leave, a marker for every gap and then last, in at most share
// bytes. kept is in line order; tail is contiguous and ends at line total.
//
// The tail starts at the earliest line t for which the whole fits. Lines kept
// earlier that fall inside the tail are counted once, as tail.
func assembleExcerpt(kept, tail []excerptLine, total, share int, last string) string {
	gap := func(from, to int) int {
		if from > to {
			return 0
		}
		return len(excerptGapMarker(from, to)) + 1
	}
	lastCost := 0
	if last != "" {
		lastCost = len(last) + 1
	}
	// prefix[j] is the cost of kept[:j] with the markers before each of them.
	prefix := make([]int, len(kept)+1)
	prev := 0
	for j, l := range kept {
		prefix[j+1] = prefix[j] + gap(prev+1, l.n-1) + l.cost()
		prev = l.n
	}
	size := func(j, t, tailSum int) int {
		prev := 0
		if j > 0 {
			prev = kept[j-1].n
		}
		return prefix[j] + gap(prev+1, t-1) + tailSum + lastCost
	}
	bestT, bestJ := -1, 0
	j, tailSum := len(kept), 0
	for i := len(tail); i >= 0; i-- {
		t := total + 1
		if i < len(tail) {
			t = tail[i].n
			tailSum += tail[i].cost()
			if tailSum > share {
				break
			}
		}
		for j > 0 && kept[j-1].n >= t {
			j--
		}
		if size(j, t, tailSum) <= share {
			bestT, bestJ = t, j
		}
	}
	if bestT < 0 {
		return ""
	}
	var b strings.Builder
	prev = 0
	for _, l := range kept[:bestJ] {
		writeExcerptGap(&b, prev+1, l.n-1)
		b.WriteString(l.text)
		b.WriteByte('\n')
		prev = l.n
	}
	writeExcerptGap(&b, prev+1, bestT-1)
	for _, l := range tail {
		if l.n >= bestT {
			b.WriteString(l.text)
			b.WriteByte('\n')
		}
	}
	if last != "" {
		b.WriteString(last)
		b.WriteByte('\n')
	}
	return b.String()
}

func writeExcerptGap(b *strings.Builder, from, to int) {
	if from <= to {
		b.WriteString(excerptGapMarker(from, to))
		b.WriteByte('\n')
	}
}

func excerptGapMarker(from, to int) string {
	if from == to {
		return fmt.Sprintf("[... line %d omitted ...]", from)
	}
	return fmt.Sprintf("[... lines %d-%d omitted ...]", from, to)
}

func excerptScanMarker(limit int64) string {
	return fmt.Sprintf("[... the file continues past the first %d MiB, which is all this excerpt reads ...]", limit>>20)
}

// readExcerptLine reads one line. held is at most its first excerptHoldMax
// bytes, its line ending included when it fits; n counts every byte of the
// line; complete says held is the whole line; ended says the line ended in a
// newline. n is zero at the end of the input.
func readExcerptLine(br *bufio.Reader) (held []byte, n int64, complete, ended bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		n += int64(len(chunk))
		if room := excerptHoldMax - len(held); room > 0 {
			held = append(held, chunk[:min(len(chunk), room)]...)
		}
		switch {
		case rerr == nil:
			return held, n, int64(len(held)) == n, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
		case errors.Is(rerr, io.EOF):
			return held, n, int64(len(held)) == n, false, nil
		default:
			return nil, 0, false, false, rerr
		}
	}
}

// trimExcerptLineEnd drops a line's newline and a carriage return before it.
func trimExcerptLineEnd(line []byte) []byte {
	if k := len(line); k > 0 && line[k-1] == '\n' {
		line = line[:k-1]
	}
	if k := len(line); k > 0 && line[k-1] == '\r' {
		line = line[:k-1]
	}
	return line
}

// keepExcerptLine is a line as an excerpt keeps it: cut at excerptLineMax
// bytes on a character boundary, with the number of bytes cut, then repaired.
// content may hold only the start of a longer line of contentLen bytes.
func keepExcerptLine(content []byte, contentLen int) string {
	if contentLen <= excerptLineMax {
		return repairExcerptText(content)
	}
	cut := min(excerptLineMax, len(content))
	for i := 0; i < utf8.UTFMax-1 && cut > 0 && cut < len(content) && !utf8.RuneStart(content[cut]); i++ {
		cut--
	}
	return repairExcerptText(content[:cut]) + fmt.Sprintf(" [... %d bytes cut ...]", contentLen-cut)
}

// repairExcerptText drops NUL bytes, which neither Postgres text nor jsonb
// stores, and replaces invalid UTF-8.
func repairExcerptText(b []byte) string {
	return strings.ToValidUTF8(strings.ReplaceAll(string(b), "\x00", ""), "�")
}
