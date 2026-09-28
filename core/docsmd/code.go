package docsmd

import (
	"regexp"
	"strings"
)

// A fence opens with three or more backticks or tildes. CommonMark allows at
// most three spaces of indent, but a fence inside a list item sits deeper, so
// any indent is accepted here; where the indent makes the line indented code
// instead, the scanner reads that first (scan.go). It closes on a line of the
// same character, at least as long, with nothing after it but whitespace.
var fenceLine = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})")

// fenceOpens reports the fence marker a line opens, or "". A backtick fence's
// info string cannot hold a backtick: "```inline```" opens no fence, it is a
// code span in a paragraph, and reading it as a fence would hide the rest of
// the page from the link gate.
func fenceOpens(body string) string {
	m := fenceLine.FindStringSubmatchIndex(body)
	if m == nil {
		return ""
	}
	marker := body[m[2]:m[3]]
	if marker[0] == '`' && strings.Contains(body[m[3]:], "`") {
		return ""
	}
	return marker
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
// What is a comment and what is code is the reading Mask and Links use (see
// scan.go), so the bundle strips exactly the comments whose links it did not
// read. The line rules are the Python bundler's this package replaced
// (scripts/docs/_bundle.py on the docs-readers branch, memql#5721); the
// fixture that pinned that bundler still pins this one.
func StripHTMLComments(text string) string {
	var comments []region
	for _, g := range read(text).regions {
		if g.kind == regionComment {
			comments = append(comments, g)
		}
	}
	if len(comments) == 0 {
		return text
	}
	var out strings.Builder
	next := 0 // the first comment that does not end before this line
	off := 0
	for _, raw := range splitLines(text) {
		body, ending := lineBody(raw)
		start, end := off, off+len(body)
		off += len(raw)
		for next < len(comments) && comments[next].end <= start {
			next++
		}
		var kept strings.Builder
		removed, continued, openAtEnd := false, false, false
		pos := start
		for k := next; k < len(comments) && comments[k].start < end; k++ {
			g := comments[k]
			from, to := max(g.start, start), min(g.end, end)
			if from > pos {
				kept.WriteString(text[pos:from])
			}
			if to > from {
				removed = true
			}
			pos = max(pos, to)
			if g.start < start {
				continued = true
			}
			if g.end > end || g.open {
				openAtEnd = true
			}
		}
		if !removed {
			out.WriteString(raw)
			continue
		}
		if pos < end {
			kept.WriteString(text[pos:end])
		}
		textOut := kept.String()
		if openAtEnd || strings.HasSuffix(strings.TrimRight(body, " \t\r\n\v\f"), "-->") {
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

// Mask returns content with every fenced or indented code block, code span
// and HTML comment blanked to spaces, byte for byte, line endings kept.
// Offsets into the result are offsets into content, so a pattern matched on
// the mask can be read and rewritten in the original -- which a stripping
// pass that deletes the code cannot offer.
func Mask(content string) string {
	return read(content).mask(content)
}
