package emaildoc

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Markdown subset of a text block.
//
// Blocks are paragraphs and lists. A blank line ends a paragraph or a list; a
// single line break inside a paragraph is a line break in the email. A line
// that starts with "- " or with "N. " (one to nine digits) is a list item at
// once, with no blank line before it, and an item never spans lines: the line
// after it is a new item, a paragraph, or the end of the list. Numbers are
// kept as written. Nothing nests.
//
// Inline text understands "***bold italic***", "**bold**", "*italic*" (they
// may nest, which flattens to a style on each run of text),
// "[label](address)" (the label is plain text and the address may hold
// balanced parentheses) and a backslash before one of \ * [ ] ( ) to write that
// character as it is. Everything else is literal: "a * b" stays "a * b", so
// does "{{displayName}}", HTML, headings, underscores and backticks.
//
// The parser never fails on odd input; it falls back to literal text. The one
// refusal is a link with no label, because nothing could be clicked. It does
// not judge addresses: Parse applies the link rule to every link it returns.
//
// It runs in time proportional to its input however hostile the input is. A
// text block can hold a megabyte of "*a " or "[a](", and a server parses it.

type mdElementKind int

const (
	mdParagraph mdElementKind = iota + 1
	mdBulletList
	mdNumberedList
)

type mdInlineKind int

const (
	mdText mdInlineKind = iota + 1
	mdBold
	mdItalic
	mdBoldItalic
	mdLink
	mdBreak
)

// mdInline is one run of a paragraph or a list item: text in one style, a
// link, or a line break. Adjacent runs of text in the same style are one run.
type mdInline struct {
	Kind mdInlineKind
	Text string // the text of the run, or a link's label
	URL  string // a link's address as written
}

// mdItem is one item of a list: its number when the list is numbered, and its
// text.
type mdItem struct {
	Number int
	Inline []mdInline
}

// mdElement is a paragraph (Inline) or a list (Items).
type mdElement struct {
	Kind   mdElementKind
	Inline []mdInline
	Items  []mdItem
}

var errEmptyLabel = errors.New("a link needs a label")

// parseMarkdown reads the subset described above.
func parseMarkdown(src string) ([]mdElement, error) {
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	var (
		elements  []mdElement
		paragraph []string
		list      *mdElement
	)
	endParagraph := func() error {
		if len(paragraph) == 0 {
			return nil
		}
		inline, err := parseInline(strings.Join(paragraph, "\n"))
		paragraph = paragraph[:0]
		if err != nil {
			return err
		}
		elements = append(elements, mdElement{Kind: mdParagraph, Inline: inline})
		return nil
	}
	endList := func() {
		if list != nil {
			elements = append(elements, *list)
			list = nil
		}
	}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if err := endParagraph(); err != nil {
				return nil, err
			}
			endList()
			continue
		}
		kind, number, rest := listItem(line)
		if kind == 0 {
			endList()
			paragraph = append(paragraph, line)
			continue
		}
		if err := endParagraph(); err != nil {
			return nil, err
		}
		if list != nil && list.Kind != kind {
			endList()
		}
		if list == nil {
			list = &mdElement{Kind: kind}
		}
		inline, err := parseInline(rest)
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, mdItem{Number: number, Inline: inline})
	}
	if err := endParagraph(); err != nil {
		return nil, err
	}
	endList()
	return elements, nil
}

// listItem recognises "- text" and "N. text" on a trimmed, non-empty line.
// kind is 0 when the line is not a list item.
func listItem(line string) (kind mdElementKind, number int, rest string) {
	if strings.HasPrefix(line, "- ") {
		return mdBulletList, 0, strings.TrimSpace(line[2:])
	}
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits >= 1 && digits <= 9 && strings.HasPrefix(line[digits:], ". ") {
		number, _ = strconv.Atoi(line[:digits]) // nine digits always fit
		return mdNumberedList, number, strings.TrimSpace(line[digits+2:])
	}
	return 0, 0, ""
}

func escapable(c byte) bool {
	return c == '\\' || c == '*' || c == '[' || c == ']' || c == '(' || c == ')'
}

type tokenKind uint8

const (
	tokText tokenKind = iota
	tokStar
	tokLink
	tokBreak
)

// token is a piece of inline text before emphasis is resolved. A hostile text
// block makes over a million of them, so the struct is kept small.
type token struct {
	text string // text; the stars as written; or a link's label
	url  string // a link's address
	// A run of stars (one to three of them, n). It may open emphasis when what
	// follows is not a space and close it when what precedes is not a space.
	// pair is the index of the star run it was matched with (-1 when it stays
	// literal) and opens says which of the two it is.
	pair              int32
	kind              tokenKind
	n                 uint8
	canOpen, canClose bool
	opens             bool
}

func parseInline(s string) ([]mdInline, error) {
	tokens, err := scanInline(s)
	if err != nil {
		return nil, err
	}
	pairStars(tokens)
	return emitInline(tokens), nil
}

// scanInline splits s into text, star runs, links and line breaks, resolving
// backslash escapes on the way.
func scanInline(s string) ([]token, error) {
	var parens []int32
	if strings.Contains(s, "](") {
		parens = matchParens(s)
	}
	var (
		tokens []token
		text   []byte
	)
	flush := func() {
		if len(text) > 0 {
			tokens = append(tokens, token{kind: tokText, text: string(text), pair: -1})
			text = text[:0]
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && escapable(s[i+1]):
			text = append(text, s[i+1])
			i += 2
		case c == '\n':
			flush()
			tokens = append(tokens, token{kind: tokBreak, pair: -1})
			i++
		case c == '*':
			j := i
			for j < len(s) && s[j] == '*' {
				j++
			}
			if j-i > 3 {
				text = append(text, s[i:j]...)
				i = j
				continue
			}
			before, _ := utf8.DecodeLastRuneInString(s[:i])
			after, _ := utf8.DecodeRuneInString(s[j:])
			flush()
			tokens = append(tokens, token{
				kind: tokStar, text: s[i:j], n: uint8(j - i), pair: -1,
				canClose: i > 0 && !unicode.IsSpace(before),
				canOpen:  j < len(s) && !unicode.IsSpace(after),
			})
			i = j
		case c == '[':
			link, end, ok, err := scanLink(s, i, parens)
			if err != nil {
				return nil, err
			}
			if !ok {
				text = append(text, c)
				i++
				continue
			}
			flush()
			tokens = append(tokens, link)
			i = end
		default:
			text = append(text, c)
			i++
		}
	}
	flush()
	return tokens, nil
}

// matchParens pairs every "(" with the ")" that closes it, the way brackets
// pair, skipping escaped ones. parens[i] is the index of the ")" for the "(" at
// i, or 0 when it has none (a ")" can never sit at index 0, so 0 is free).
func matchParens(s string) []int32 {
	parens := make([]int32, len(s))
	var open []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) && escapable(s[i+1]) {
				i++
			}
		case '(':
			open = append(open, i)
		case ')':
			if n := len(open); n > 0 {
				parens[open[n-1]] = int32(i)
				open = open[:n-1]
			}
		}
	}
	return parens
}

// labelEnd returns the index of the "]" that closes the "[" at s[i], or -1. A
// label holds no "[": in "[a [b](u)" the link is "[b](u)".
func labelEnd(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			if j+1 < len(s) && escapable(s[j+1]) {
				j++
			}
		case '[':
			return -1
		case ']':
			return j
		}
	}
	return -1
}

// scanLink reads "[label](address)" at s[i]. ok is false when the text there
// is not a link, so the "[" is just a character.
func scanLink(s string, i int, parens []int32) (link token, end int, ok bool, err error) {
	closeLabel := labelEnd(s, i)
	if closeLabel < 0 || closeLabel+1 >= len(s) || s[closeLabel+1] != '(' {
		return token{}, 0, false, nil
	}
	closeParen := 0
	if parens != nil {
		closeParen = int(parens[closeLabel+1])
	}
	if closeParen == 0 {
		return token{}, 0, false, nil
	}
	label := strings.ReplaceAll(unescape(s[i+1:closeLabel]), "\n", " ")
	if strings.TrimSpace(label) == "" {
		return token{}, 0, false, errEmptyLabel
	}
	return token{kind: tokLink, text: label, url: unescape(s[closeLabel+2 : closeParen]), pair: -1}, closeParen + 1, true, nil
}

// unescape drops the backslash of each escape.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && escapable(s[i+1]) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// pairStars matches each closing run of stars with the nearest opening run of
// the same length. Matching never crosses: once a pair is made, every opener
// between its two runs is dropped, and an opener and closer of different
// lengths never match. Each run is pushed and popped at most once, so this is
// linear however the stars fall.
func pairStars(tokens []token) {
	var open [4][]int // open[n] holds the unmatched openers of n stars, oldest first
	for j := range tokens {
		t := &tokens[j]
		if t.kind != tokStar {
			continue
		}
		if stack := open[t.n]; t.canClose && len(stack) > 0 {
			i := stack[len(stack)-1]
			open[t.n] = stack[:len(stack)-1]
			tokens[i].pair, tokens[i].opens = int32(j), true
			t.pair = int32(i)
			for n := 1; n <= 3; n++ {
				for len(open[n]) > 0 && open[n][len(open[n])-1] > i {
					open[n] = open[n][:len(open[n])-1]
				}
			}
			continue
		}
		if t.canOpen {
			open[t.n] = append(open[t.n], j)
		}
	}
}

// emitInline turns the tokens into runs, giving each piece of text the style
// of the emphasis around it. Stars that found no partner are text.
func emitInline(tokens []token) []mdInline {
	var (
		out      []mdInline
		run      []byte
		runKind  mdInlineKind
		bold     int
		italic   int
		endOfRun = func() {
			if runKind != 0 {
				out = append(out, mdInline{Kind: runKind, Text: string(run)})
				run, runKind = run[:0], 0
			}
		}
		write = func(text string) {
			kind := mdText
			switch {
			case bold > 0 && italic > 0:
				kind = mdBoldItalic
			case bold > 0:
				kind = mdBold
			case italic > 0:
				kind = mdItalic
			}
			if kind != runKind {
				endOfRun()
				runKind = kind
			}
			run = append(run, text...)
		}
	)
	for _, t := range tokens {
		switch t.kind {
		case tokText:
			write(t.text)
		case tokStar:
			if t.pair < 0 {
				write(t.text)
				continue
			}
			delta := -1
			if t.opens {
				delta = 1
			}
			switch t.n {
			case 1:
				italic += delta
			case 2:
				bold += delta
			case 3:
				bold += delta
				italic += delta
			}
		case tokLink:
			endOfRun()
			out = append(out, mdInline{Kind: mdLink, Text: t.text, URL: t.url})
		case tokBreak:
			endOfRun()
			out = append(out, mdInline{Kind: mdBreak})
		}
	}
	endOfRun()
	return out
}
