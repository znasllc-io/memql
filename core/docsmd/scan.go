package docsmd

import (
	"regexp"
	"strings"
)

// This file is the one reading of a page that Links, Mask and
// StripHTMLComments share: where its code and its HTML comments are, and
// where its inline links and images are.
//
// The reading follows CommonMark with GFM tables, the markdown the site
// renders, in two phases the way the spec does. The block phase walks lines:
// fenced and indented code blocks, blank lines, the paragraphs and table
// cells whose text is inline content. The inline phase reads one paragraph or
// cell left to right: a backtick run opens a code span that closes on the
// next run of the same length anywhere in the paragraph, lines included; a
// `]` closes the nearest `[` or `![` into a link when an inline destination
// follows it, with an optional title and whitespace around both; brackets
// nest, a link's text may hold images, and a link inside a link's text
// makes the outer brackets text, as the spec requires.
//
// Where the block phase has to approximate -- it does not model list-item
// columns -- it errs toward reading more text as text: a link-shaped string
// the gate reads as a link fails closed, while a link it misses would ship.
// The one rule this package does not share with CommonMark is kept on
// purpose: an HTML comment runs to the first `-->`, even across blank lines,
// so that every comment this reading hides from Links is one
// StripHTMLComments removes.

type regionKind uint8

const (
	regionCode regionKind = iota + 1
	regionComment
)

// region is a byte range of the page that is code or an HTML comment.
type region struct {
	start, end int
	kind       regionKind
	// open marks a comment the page ends inside.
	open bool
}

// reading is what the scanner found on a page.
type reading struct {
	regions []region // in document order, never overlapping
	links   []Link   // inline links and images, in the order they close
}

// mask returns content with every region blanked to spaces, byte for byte,
// line endings kept.
func (r reading) mask(content string) string {
	b := []byte(content)
	for _, g := range r.regions {
		for i := g.start; i < g.end; i++ {
			if b[i] == '\n' || (b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n') {
				continue
			}
			b[i] = ' '
		}
	}
	return string(b)
}

// line is one line of a page: the offset of its first byte, and its text
// without the line ending.
type line struct {
	start int
	body  string
}

func pageLines(content string) []line {
	var out []line
	off := 0
	for _, raw := range splitLines(content) {
		body, _ := lineBody(raw)
		out = append(out, line{off, body})
		off += len(raw)
	}
	return out
}

type scanner struct {
	src   string
	lines []line
	out   reading

	fence        string // the marker of the open fenced block, or ""
	inComment    bool
	commentStart int

	prevBlank bool // the last line was blank, or there was none
	paraOpen  bool // a paragraph may still continue, so no indented code
	listOpen  bool // a list item or footnote may still own indented lines
	indented  bool // inside an indented code block
}

// read scans content once, block phase and inline phase together.
func read(content string) reading {
	s := &scanner{src: content, lines: pageLines(content), prevBlank: true}
	s.run()
	return s.out
}

func (s *scanner) run() {
	li := 0
	for li < len(s.lines) {
		ln := s.lines[li]
		if s.inComment {
			// A comment open from an earlier line runs to its first `-->`,
			// and nothing on the way -- a fence line included -- is read.
			end := strings.Index(ln.body, "-->")
			if end < 0 {
				// Whether the comment sits in a paragraph is not
				// modelled: treating it as one reads what follows as text.
				s.prevBlank, s.paraOpen, s.indented = false, true, false
				li++
				continue
			}
			s.closeComment(ln.start + end + 3)
			li = s.context(li, end+3)
			continue
		}
		if s.block(li) {
			li++
			continue
		}
		if s.tableHeader(li) {
			li = s.table(li)
			continue
		}
		li = s.context(li, 0)
	}
	if s.inComment {
		s.out.regions = append(s.out.regions, region{s.commentStart, len(s.src), regionComment, true})
	}
}

// block consumes line li when it is a fence line, blank, or indented code,
// and reports whether it did.
func (s *scanner) block(li int) bool {
	ln := s.lines[li]
	if s.fence != "" {
		s.code(ln)
		if fenceCloses(ln.body, s.fence) {
			s.fence = ""
		}
		s.prevBlank, s.paraOpen = false, false
		return true
	}
	if isBlank(ln.body) {
		// An indented code block runs on across blank lines.
		s.prevBlank, s.paraOpen = true, false
		return true
	}
	// Indented code cannot interrupt a paragraph, and inside a list item the
	// same indent is the item's own content. Lists are not modelled column
	// by column, so once one opens no indented line is code until a line at
	// column 0 closes it: an indented line read as text fails closed.
	if indentWidth(ln.body) >= 4 && !s.listOpen && (!s.paraOpen || s.indented) {
		s.indented = true
		s.prevBlank, s.paraOpen = false, false
		s.code(ln)
		return true
	}
	s.indented = false
	if marker := fenceOpens(ln.body); marker != "" {
		s.note(ln.body)
		s.fence = marker
		s.code(ln)
		return true
	}
	return false
}

// note tracks, from a line that starts a block, whether a list may still own
// the indented lines that follow.
func (s *scanner) note(body string) {
	t := strings.TrimLeft(body, " \t")
	column0 := len(t) == len(body)
	switch {
	case breakLine.MatchString(t):
		// `* * *` is a thematic break, not a list item.
		if column0 {
			s.listOpen = false
		}
	case anyListItem.MatchString(t) || footnoteDefinition.MatchString(t):
		s.listOpen = true
	case column0 && (s.prevBlank || interrupts(body)):
		s.listOpen = false
	}
	s.prevBlank = false
}

func (s *scanner) code(ln line) {
	s.add(region{ln.start, ln.start + len(ln.body), regionCode, false})
}

func (s *scanner) add(g region) {
	if g.end > g.start {
		s.out.regions = append(s.out.regions, g)
	}
}

func (s *scanner) openComment(at int) {
	s.inComment = true
	s.commentStart = at
}

func (s *scanner) closeComment(end int) {
	s.inComment = false
	s.add(region{s.commentStart, end, regionComment, false})
}

// context reads one paragraph as inline content: line li from byte col, and
// every line after it that continues the paragraph. It returns the line after
// the paragraph.
func (s *scanner) context(li, col int) int {
	ln := s.lines[li]
	if col > 0 && isBlank(ln.body[col:]) {
		return li + 1
	}
	if col == 0 {
		s.note(ln.body)
	}
	s.prevBlank = false
	end := li
	_, rest := quoteSplit(ln.body)
	// A heading or a thematic break is a block of its own: no line continues
	// it, and indented code may follow it directly.
	s.paraOpen = !standalone(rest)
	if s.paraOpen {
		for end+1 < len(s.lines) && continues(s.lines[end].body, s.lines[end+1].body) && !s.tableHeader(end+1) {
			end++
		}
	}
	last := s.lines[end]
	s.inline(ln.start+col, last.start+len(last.body))
	return end + 1
}

// tableHeader reports whether line li is a GFM table's header row: a line
// with a cell separator, followed by a delimiter row of as many cells.
func (s *scanner) tableHeader(li int) bool {
	if li+1 >= len(s.lines) {
		return false
	}
	hd, head := quoteSplit(s.lines[li].body)
	dd, delim := quoteSplit(s.lines[li+1].body)
	if hd != dd || !strings.Contains(delim, "|") || !delimiterRow.MatchString(strings.TrimSpace(delim)) {
		return false
	}
	return unescapedPipe(head) >= 0 && len(splitCells(head)) == len(splitCells(delim))
}

// table reads a table whose header is line li: every cell of every row is
// inline content of its own, so a code span or a link never spans two cells.
// It returns the line after the table.
func (s *scanner) table(li int) int {
	s.note(s.lines[li].body)
	depth, _ := quoteSplit(s.lines[li].body)
	s.cells(li)
	li += 2 // the delimiter row holds no content
	for li < len(s.lines) {
		d, rest := quoteSplit(s.lines[li].body)
		if isBlank(rest) || d != depth || interrupts(rest) {
			break
		}
		s.cells(li)
		li++
	}
	s.paraOpen = false
	return li
}

func (s *scanner) cells(li int) {
	ln := s.lines[li]
	_, rest := quoteSplit(ln.body)
	off := ln.start + len(ln.body) - len(rest)
	for _, c := range splitCells(rest) {
		s.inline(off+c[0], off+c[1])
	}
}

// splitCells returns the byte ranges, within a table row, of its cells: the
// text between unescaped pipes, less the optional leading and trailing pipe.
func splitCells(row string) [][2]int {
	start := len(row) - len(strings.TrimLeft(row, " \t"))
	end := len(strings.TrimRight(row, " \t"))
	if start < end && row[start] == '|' {
		start++
	}
	if end > start && row[end-1] == '|' && (end-2 < start || row[end-2] != '\\') {
		end--
	}
	var cells [][2]int
	for {
		i := unescapedPipe(row[start:end])
		if i < 0 {
			return append(cells, [2]int{start, end})
		}
		cells = append(cells, [2]int{start, start + i})
		start += i + 1
	}
}

// unescapedPipe returns the index of the first `|` in s that no backslash
// escapes, or -1. GFM splits cells there before any inline parsing, inside
// code spans included.
func unescapedPipe(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '|':
			return i
		}
	}
	return -1
}

// opener is a `[` or `![` waiting for its `]`.
type opener struct {
	at, text int // the bracket, and the first byte of the text after it
	image    bool
	active   bool
}

// inline reads src[from:to], one paragraph or cell, and records its code
// spans, comments, links and images.
func (s *scanner) inline(from, to int) {
	src := s.src
	var open []opener
	i := from
	for i < to {
		if s.inComment {
			end := strings.Index(src[i:to], "-->")
			if end < 0 {
				return // the comment runs past this paragraph
			}
			s.closeComment(i + end + 3)
			i += end + 3
			continue
		}
		switch c := src[i]; {
		case c == '\\' && i+1 < to && isPunct(src[i+1]):
			i += 2
		case c == '`':
			run := i
			for run < to && src[run] == '`' {
				run++
			}
			if stop := closingRun(src[:to], run, run-i); stop >= 0 {
				s.add(region{i, stop, regionCode, false})
				i = stop
			} else {
				i = run // an unmatched run is literal backticks
			}
		case c == '<' && strings.HasPrefix(src[i:to], "<!--"):
			s.openComment(i)
			i += 4
			// `<!-->` and `<!--->` are whole comments (CommonMark 0.31).
			if strings.HasPrefix(src[i:to], ">") {
				s.closeComment(i + 1)
				i++
			} else if strings.HasPrefix(src[i:to], "->") {
				s.closeComment(i + 2)
				i += 2
			}
		case c == '<':
			// An autolink or an HTML tag is read before any bracket or
			// backtick inside it.
			if m := autolinkOrTag.FindStringIndex(src[i:to]); m != nil {
				i += m[1]
			} else {
				i++
			}
		case c == '!' && i+1 < to && src[i+1] == '[':
			open = append(open, opener{at: i, text: i + 2, image: true, active: true})
			i += 2
		case c == '[':
			open = append(open, opener{at: i, text: i + 1, active: true})
			i++
		case c == ']' && len(open) > 0:
			op := open[len(open)-1]
			open = open[:len(open)-1]
			if !op.active {
				i++
				break
			}
			t, ok := parseTail(src, i+1, to)
			if !ok {
				i++
				break
			}
			s.out.links = append(s.out.links, Link{
				Start: op.at, End: t.end,
				TextStart: op.text, TextEnd: i,
				TargetStart: t.destStart, TargetEnd: t.destEnd,
				Text:   src[op.text:i],
				Target: src[t.destStart:t.destEnd],
				Image:  op.image,
			})
			if !op.image {
				// Links do not nest: every `[` before this one is now text.
				for k := range open {
					if !open[k].image {
						open[k].active = false
					}
				}
			}
			i = t.end
		default:
			i++
		}
	}
}

// tail is what follows a link's text: `(destination "title")`.
type tail struct {
	destStart, destEnd int // the destination, without its angle brackets
	end                int // the byte after the closing parenthesis
}

// parseTail reads an inline link's tail at src[p:to]: `(`, optional
// whitespace (one line ending at most), an optional destination, optional
// whitespace and a title in double quotes, single quotes or parentheses,
// optional whitespace, `)`.
func parseTail(src string, p, to int) (tail, bool) {
	if p >= to || src[p] != '(' {
		return tail{}, false
	}
	i := skipSpaceNewline(src, p+1, to)
	start, end, next, ok := parseDestination(src, i, to)
	if !ok {
		return tail{}, false
	}
	i = next
	// A title needs whitespace between it and the destination.
	if j := skipSpaceNewline(src, i, to); j > i && j < to && strings.IndexByte(`"'(`, src[j]) >= 0 {
		after, ok := titleEnd(src, j, to)
		if !ok {
			return tail{}, false
		}
		i = after
	}
	i = skipSpaceNewline(src, i, to)
	if i >= to || src[i] != ')' {
		return tail{}, false
	}
	return tail{start, end, i + 1}, true
}

// parseDestination reads a link destination at src[i:to]: `<...>` on one
// line, or a run of non-space bytes whose parentheses balance. It returns the
// destination's bounds (inside the angle brackets) and the byte after it.
// An empty run is a destination only when `)` follows it.
func parseDestination(src string, i, to int) (start, end, next int, ok bool) {
	if i < to && src[i] == '<' {
		for j := i + 1; j < to; j++ {
			switch c := src[j]; {
			case c == '\\' && j+1 < to && isPunct(src[j+1]):
				j++
			case c == '>':
				return i + 1, j, j + 1, true
			case c == '<' || c == '\n' || c == '\r':
				return 0, 0, 0, false
			}
		}
		return 0, 0, 0, false
	}
	j, depth := i, 0
loop:
	for j < to {
		c := src[j]
		switch {
		case c == '\\' && j+1 < to && isPunct(src[j+1]):
			j += 2
			continue
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				break loop
			}
			depth--
		case c <= ' ' || c == 0x7f:
			break loop
		}
		j++
	}
	if depth != 0 || (j == i && (j >= to || src[j] != ')')) {
		return 0, 0, 0, false
	}
	return i, j, j, true
}

// titleEnd returns the byte after the link title that opens at src[i].
func titleEnd(src string, i, to int) (int, bool) {
	open := src[i]
	closer := open
	if open == '(' {
		closer = ')'
	}
	for j := i + 1; j < to; j++ {
		c := src[j]
		switch {
		case c == '\\' && j+1 < to && isPunct(src[j+1]):
			j++
		case c == closer:
			return j + 1, true
		case open == '(' && c == '(':
			return 0, false
		}
	}
	return 0, false
}

// skipSpaceNewline skips spaces and tabs, at most one line ending, and the
// block-quote markers that open the next line inside a quote.
func skipSpaceNewline(src string, i, to int) int {
	i = skipSpaceTab(src, i, to)
	switch {
	case i+1 < to && src[i] == '\r' && src[i+1] == '\n':
		i += 2
	case i < to && src[i] == '\n':
		i++
	default:
		return i
	}
	for {
		i = skipSpaceTab(src, i, to)
		if i < to && src[i] == '>' {
			i++
			continue
		}
		return i
	}
}

func skipSpaceTab(src string, i, to int) int {
	for i < to && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i
}

// isPunct reports whether c is ASCII punctuation, which a backslash escapes.
func isPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// indentWidth returns a line's leading whitespace in columns, a tab reaching
// the next multiple of four.
func indentWidth(s string) int {
	w := 0
	for _, c := range []byte(s) {
		switch c {
		case ' ':
			w++
		case '\t':
			w += 4 - w%4
		default:
			return w
		}
	}
	return w
}

// quoteSplit strips a line's block-quote markers, returning how many there
// were and the rest of the line.
func quoteSplit(body string) (int, string) {
	depth := 0
	for {
		t := strings.TrimLeft(body, " \t")
		if !strings.HasPrefix(t, ">") {
			return depth, body
		}
		depth++
		body = t[1:]
	}
}

// continues reports whether cur, the line after prev, continues prev's
// paragraph: it is not blank, it opens no deeper block quote, and it starts
// no block that interrupts a paragraph. A lazy line -- one with fewer quote
// markers than its paragraph -- continues it.
func continues(prev, cur string) bool {
	dp, rp := quoteSplit(prev)
	dc, rc := quoteSplit(cur)
	return !isBlank(rc) && dc <= dp && !standalone(rp) && !interrupts(rc)
}

// standalone reports whether a line is a block of its own that no following
// line continues: an ATX heading, a thematic break, a setext underline.
func standalone(rest string) bool {
	t := strings.TrimLeft(rest, " \t")
	return atxHeading.MatchString(t) || breakLine.MatchString(t)
}

// interrupts reports whether rest (a line with its quote markers removed)
// starts a block that ends a paragraph: a fence, an ATX heading, a thematic
// break or setext underline, a list item with content, or an HTML block of
// the kinds that may interrupt one.
func interrupts(rest string) bool {
	t := strings.TrimLeft(rest, " \t")
	return fenceOpens(rest) != "" ||
		atxHeading.MatchString(t) ||
		breakLine.MatchString(t) ||
		listStart.MatchString(t) ||
		htmlBlockStart.MatchString(t)
}

var (
	atxHeading = regexp.MustCompile(`^#{1,6}([ \t]|$)`)
	// breakLine matches a thematic break or a setext heading underline.
	breakLine = regexp.MustCompile(`^((\*[ \t]*){3,}|(-[ \t]*){3,}|(_[ \t]*){3,}|=+[ \t]*|-+[ \t]*)$`)
	// listStart matches a list item with content, the kind that may end a
	// paragraph; anyListItem also matches an empty one.
	listStart          = regexp.MustCompile(`^([-*+]|\d{1,9}[.)])[ \t]+\S`)
	anyListItem        = regexp.MustCompile(`^([-*+]|\d{1,9}[.)])([ \t]|$)`)
	footnoteDefinition = regexp.MustCompile(`^\[\^[^\]]+\]:`)
	delimiterRow       = regexp.MustCompile(`^\|?[ \t]*:?-+:?[ \t]*(\|[ \t]*:?-+:?[ \t]*)*\|?$`)
	// htmlBlockStart matches the HTML block starts (CommonMark kinds 1 to 6)
	// that may interrupt a paragraph.
	htmlBlockStart = regexp.MustCompile(`(?i)^<(` +
		`(script|pre|style|textarea)([ \t>]|$)` +
		`|!--|\?|![a-z]|!\[CDATA\[` +
		`|/?(address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|` +
		`fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|` +
		`menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|` +
		`track|ul)([ \t]|/?>|$))`)
	// autolinkOrTag matches a URI autolink, or an HTML open or closing tag.
	autolinkOrTag = regexp.MustCompile(`^(<[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\x00-\x20<>]*>` +
		`|<[A-Za-z][A-Za-z0-9-]*(\s+[A-Za-z_:][A-Za-z0-9_.:-]*(\s*=\s*([^"'=<>` + "`" + `\x00-\x20]+|'[^']*'|"[^"]*"))?)*\s*/?>` +
		`|</[A-Za-z][A-Za-z0-9-]*\s*>)`)
)
