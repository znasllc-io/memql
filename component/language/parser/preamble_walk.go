package parser

import (
	"sort"
	"strings"
)

// preamble_walk.go -- a declaration's preamble includes a MULTI-LINE annotation
// whole (memql#5426 review).
//
// Every preamble walk stepped backwards from a header over lines that start
// with `@` or `//` and stopped at the first line that did not. A multi-line
// annotation's continuation lines start with neither:
//
//	@level("fast")
//	@description(
//	  "Summarise a ticket."
//	)
//	@templateFile("summ.tmpl")
//	prompt acmeSumm { ... }
//
// so the walk stopped at `)` and everything above it -- @level here, a
// @rowAuthz above a concept, a @trigger above an automation -- was cut out of
// the slice every loader parses. The slice still parsed (the annotations below
// the cut were whole), so nothing said anything: the prompt was refused as
// having no level, the concept loaded on the UNDECLARED row-authz tier, and an
// automation lost the annotations that made it fire.
//
// A continuation line is one that begins inside an annotation's argument list:
// after `@name(` and before the `)` that closes it, brackets balanced and
// strings and comments skipped. PreambleWalker finds those lines with one
// forward pass over the source (done only when a walk first needs it), and
// every walk treats one as part of the annotation that opened above it.
//
// Comments keep the rules each walk already had. PreambleStartOf does NOT carry
// a preamble across a block comment (memql#2965 chose to report that as an
// orphan rather than guess which declaration the annotations were for); the
// automation slicer does (memql#2872). A multi-line annotation is neither: it
// is one annotation, and only a walk that reads it whole agrees with the parser.

// PreambleWalker answers preamble questions for many headers of one source,
// computing what they share at most once.
type PreambleWalker struct {
	source string
	// continuation maps the start of every line that begins inside an
	// annotation's argument list to the start of the line holding that
	// annotation's `@`. Built on first use.
	continuation map[int]int
	built        bool
}

// NewPreambleWalker returns a walker over source.
func NewPreambleWalker(source string) *PreambleWalker {
	return &PreambleWalker{source: source}
}

// StartOf is PreambleStartOf for one header of the walker's source.
func (w *PreambleWalker) StartOf(headerStart int) int {
	src := w.source
	if headerStart > len(src) {
		headerStart = len(src)
	}
	preambleStart := headerStart
	for k := headerStart - 1; k >= 0; k-- {
		lineStart := strings.LastIndexByte(src[:k], '\n') + 1
		trimmed := strings.TrimSpace(strings.TrimRight(src[lineStart:k+1], "\r\n"))
		if strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "//") {
			preambleStart = lineStart
			k = lineStart - 1
			continue
		}
		// A line inside a multi-line annotation's argument list belongs to
		// the annotation, and the walk resumes above the line its `@` is on.
		if opener, ok := w.ContinuationStart(lineStart); ok {
			preambleStart = opener
			k = opener - 1
			continue
		}
		break
	}
	return preambleStart
}

// ContinuationStart reports whether the line starting at lineStart begins
// inside a multi-line annotation's argument list, and if so the start of the
// line holding that annotation's `@` -- where a preamble that reaches the line
// must continue from.
func (w *PreambleWalker) ContinuationStart(lineStart int) (int, bool) {
	if !w.built {
		w.continuation = annotationContinuations(w.source)
		w.built = true
	}
	opener, ok := w.continuation[lineStart]
	return opener, ok
}

// ContinuationLines is ContinuationStart for a walk over the lines of
// strings.Split(source, "\n"): it maps the 0-based index of every line that
// begins inside a multi-line annotation's argument list to the index of the
// line holding that annotation's `@`. A source with none maps nothing.
func ContinuationLines(source string) map[int]int {
	byOffset := annotationContinuations(source)
	if len(byOffset) == 0 {
		return nil
	}
	starts := []int{0}
	for i := 0; i < len(source); i++ {
		if source[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	// Both offsets are line starts, so the search lands exactly.
	out := make(map[int]int, len(byOffset))
	for line, opener := range byOffset {
		out[sort.SearchInts(starts, line)] = sort.SearchInts(starts, opener)
	}
	return out
}

// annotationContinuations maps the start of every line that begins inside an
// annotation's argument list to the start of the line holding the annotation's
// `@`, over one forward pass that shares the lexer's view of strings (double
// quoted with escapes, backtick raw) and comments (line and block, which do
// not nest). An annotation is `@name` followed, after spaces or tabs, by `(`;
// its list closes at the matching bracket. A source with no multi-line
// annotation maps nothing.
//
// Two lists record nothing. One that never closes: its `(` would otherwise make
// every line below it a "continuation" -- declarations included -- and the next
// header's preamble would swallow them, turning one malformed annotation into a
// second broken declaration. And one whose line does not START with an
// annotation (`title string @description(` inside a concept body): that line is
// not a preamble line, so a walk that reached it would leave the preamble.
func annotationContinuations(src string) map[int]int {
	out := map[int]int{}
	var pending []int // line starts inside the open list, recorded once it closes
	record := false   // whether the open list's line starts with an annotation
	const (
		stateCode = iota
		stateString
		stateBacktick
		stateLineComment
		stateBlockComment
	)
	state := stateCode
	depth := 0     // bracket depth inside an open annotation list; 0 outside one
	opener := -1   // start of the line holding the open annotation's `@`
	lineStart := 0 // start of the current line
	n := len(src)
	for i := 0; i < n; i++ {
		c := src[i]
		if c == '\n' {
			if state == stateLineComment {
				state = stateCode
			}
			lineStart = i + 1
			if depth > 0 {
				pending = append(pending, lineStart)
			}
			continue
		}
		switch state {
		case stateString:
			if c == '\\' && i+1 < n && src[i+1] != '\n' {
				i++
			} else if c == '"' {
				state = stateCode
			}
			continue
		case stateBacktick:
			if c == '`' {
				state = stateCode
			}
			continue
		case stateLineComment:
			continue
		case stateBlockComment:
			if c == '*' && i+1 < n && src[i+1] == '/' {
				i++
				state = stateCode
			}
			continue
		}
		switch {
		case c == '"':
			state = stateString
		case c == '`':
			state = stateBacktick
		case c == '/' && i+1 < n && src[i+1] == '/':
			i++
			state = stateLineComment
		case c == '/' && i+1 < n && src[i+1] == '*':
			i++
			state = stateBlockComment
		case depth > 0 && (c == '(' || c == '[' || c == '{'):
			depth++
		case depth > 0 && (c == ')' || c == ']' || c == '}'):
			depth--
			if depth == 0 {
				if record {
					for _, l := range pending {
						out[l] = opener
					}
				}
				pending = pending[:0]
			}
		case depth == 0 && c == '@':
			j := i + 1
			for j < n && isAnnotationNameByte(src[j]) {
				j++
			}
			if j == i+1 {
				continue // a bare `@`
			}
			k := j
			for k < n && (src[k] == ' ' || src[k] == '\t') {
				k++
			}
			if k < n && src[k] == '(' {
				depth = 1
				opener = lineStart
				record = strings.HasPrefix(strings.TrimLeft(src[lineStart:], " \t"), "@")
				i = k
			} else {
				i = j - 1
			}
		}
	}
	return out
}

// isAnnotationNameByte reports whether b can continue an annotation's name.
func isAnnotationNameByte(b byte) bool {
	return b == '_' || b == '.' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
