package docsmd

import (
	"regexp"
	"strings"
)

// A fence opens with three or more backticks or tildes. CommonMark allows at
// most three spaces of indent, but a fence inside a list item sits deeper, so
// any indent is accepted: reading a line as a fence can only leave its content
// alone, never strip or rewrite it. It closes on a line of the same character,
// at least as long, with nothing after it but whitespace.
var fenceLine = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})")

// fenceOpens reports the fence marker a line opens, or "".
func fenceOpens(body string) string {
	m := fenceLine.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// fenceCloses reports whether body closes a fence opened with marker.
func fenceCloses(body, marker string) bool {
	m := fenceLine.FindStringSubmatchIndex(body)
	if m == nil {
		return false
	}
	run := body[m[2]:m[3]]
	return run[0] == marker[0] && len(run) >= len(marker) && strings.TrimSpace(body[m[1]:]) == ""
}

// backtickRun returns the start and end of the first run of backticks in s at
// or after from, or -1, -1.
func backtickRun(s string, from int) (int, int) {
	i := strings.IndexByte(s[from:], '`')
	if i < 0 {
		return -1, -1
	}
	start := from + i
	end := start
	for end < len(s) && s[end] == '`' {
		end++
	}
	return start, end
}

// closingRun finds the end of the first run of exactly n backticks at or after
// from: the run that closes a code span opened by a run of n. It returns -1
// when there is none, and the opening run is then literal backticks.
func closingRun(s string, from, n int) int {
	for {
		start, end := backtickRun(s, from)
		if start < 0 {
			return -1
		}
		if end-start == n {
			return end
		}
		from = end
	}
}

// segment is one byte range of a line, and whether it is an HTML comment.
type segment struct {
	start, end int
	comment    bool
	code       bool
}

// scanLine walks one line that is outside a fenced block. It returns the
// line's segments in order -- text, code spans and comments -- and whether a
// comment is still open at its end. A code span runs to the next backtick run
// of the same length; an unmatched run is literal backticks and is text.
//
// This is the one reading of "what on this line is code and what is a
// comment", shared by StripHTMLComments and Mask so the bundle strips exactly
// the comments whose links it did not rewrite.
func scanLine(line string, inComment bool) (segs []segment, open bool) {
	i := 0
	for i < len(line) {
		if inComment {
			end := strings.Index(line[i:], "-->")
			if end < 0 {
				segs = append(segs, segment{i, len(line), true, false})
				return segs, true
			}
			segs = append(segs, segment{i, i + end + 3, true, false})
			i += end + 3
			inComment = false
			continue
		}
		opener := strings.Index(line[i:], "<!--")
		if opener >= 0 {
			opener += i
		}
		tickStart, tickEnd := backtickRun(line, i)
		if tickStart >= 0 && (opener < 0 || tickStart < opener) {
			if tickStart > i {
				segs = append(segs, segment{i, tickStart, false, false})
			}
			if stop := closingRun(line, tickEnd, tickEnd-tickStart); stop >= 0 {
				segs = append(segs, segment{tickStart, stop, false, true})
				i = stop
			} else {
				segs = append(segs, segment{tickStart, tickEnd, false, false})
				i = tickEnd
			}
			continue
		}
		if opener < 0 {
			segs = append(segs, segment{i, len(line), false, false})
			break
		}
		if opener > i {
			segs = append(segs, segment{i, opener, false, false})
		}
		// The comment's own segment starts at its opener; the loop's
		// in-comment branch extends it to the closer or the end of the line.
		i = opener + 4
		inComment = true
		segs = append(segs, segment{opener, i, true, false})
	}
	return segs, inComment
}

// splitLines splits text into lines that keep their line endings. The last
// line has none when text does not end in a newline.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineBody returns a line without its trailing carriage returns and newlines,
// and the ending it removed.
func lineBody(line string) (body, ending string) {
	body = strings.TrimRight(line, "\r\n")
	return body, line[len(body):]
}

// StripHTMLComments returns markdown with its HTML comments removed outside
// fenced code blocks and code spans, where a comment is content.
//
// The comments in docs/public are gate markers (`<!-- corpus: -->`,
// `<!-- proving: -->`, `<!-- retired-vocabulary-ok: -->`,
// `<!-- BEGIN GENERATED -->`), read from the source tree and never prose. The
// site renders markdown without raw HTML, so a comment that reached the
// bundle printed as literal text in the middle of a page.
//
// A comment on a line of its own leaves one blank line, which keeps the block
// boundary it made (CommonMark reads it as an HTML block). A trailing comment
// leaves its line's text, with the whitespace before the comment trimmed so it
// cannot become a hard line break. A file with no comment comes back byte for
// byte.
//
// These are the rules of the Python bundler this package replaced
// (scripts/docs/_bundle.py on the docs-readers branch, memql#5721), ported
// line for line so the two agree on every input.
func StripHTMLComments(text string) string {
	var out strings.Builder
	fence := ""
	inComment := false
	for _, line := range splitLines(text) {
		body, ending := lineBody(line)
		if !inComment {
			if fence != "" {
				if fenceCloses(body, fence) {
					fence = ""
				}
				out.WriteString(line)
				continue
			}
			if marker := fenceOpens(body); marker != "" {
				fence = marker
				out.WriteString(line)
				continue
			}
		}
		continued := inComment
		segs, open := scanLine(body, inComment)
		inComment = open
		removed := false
		var kept strings.Builder
		for _, s := range segs {
			if s.comment {
				removed = true
				continue
			}
			kept.WriteString(body[s.start:s.end])
		}
		if !removed {
			out.WriteString(line)
			continue
		}
		textOut := kept.String()
		if inComment || strings.HasSuffix(strings.TrimRight(body, " \t\r\n\v\f"), "-->") {
			textOut = strings.TrimRight(textOut, " \t")
		}
		if strings.TrimSpace(textOut) == "" {
			if !continued {
				if ending == "" {
					ending = "\n"
				}
				out.WriteString(ending)
			}
			continue
		}
		out.WriteString(textOut + ending)
	}
	return out.String()
}

// Mask returns content with every fenced code block, code span and HTML
// comment blanked to spaces, byte for byte, line endings kept. Offsets into
// the result are offsets into content, so a pattern matched on the mask can be
// read and rewritten in the original -- which a stripping pass that deletes
// the code cannot offer.
func Mask(content string) string {
	masked := []byte(content)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	fence := ""
	inComment := false
	offset := 0
	for _, line := range splitLines(content) {
		body, _ := lineBody(line)
		start := offset
		offset += len(line)
		if !inComment {
			if fence != "" {
				if fenceCloses(body, fence) {
					fence = ""
				}
				blank(start, start+len(body))
				continue
			}
			if marker := fenceOpens(body); marker != "" {
				fence = marker
				blank(start, start+len(body))
				continue
			}
		}
		segs, open := scanLine(body, inComment)
		inComment = open
		for _, s := range segs {
			if s.comment || s.code {
				blank(start+s.start, start+s.end)
			}
		}
	}
	return string(masked)
}
