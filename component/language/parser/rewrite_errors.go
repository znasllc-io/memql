package parser

// rewrite_errors.go -- a refusal the struct-form rewriter makes names the
// author's line and column (memql#5364).
//
// The rewriter reads text, before any token exists, so its refusals used to
// carry no position at all: "`refine` requires `paginate`" named the query and
// left the author to find the clause, and an editor could only anchor the
// squiggle on the construct's name. A refusal now records the extent of the
// author's text it refuses -- a clause keyword, a step's first word, a field
// line, an annotation -- in the text the refusing stage read (RewriteError),
// and PositionRewriteError places that extent in the author's source through
// the same position markers a parse error's position comes through
// (PositionLowering). That is what makes the position right even when the
// stage read a text earlier stages had already rewritten, or that
// StripNonProceduralBlocks had shortened.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// RewriteError is a refusal by the struct-form rewriter of text the author
// wrote, with that text's extent in the text the refusing stage read. Its
// message is the rewriter's; PositionRewriteError adds the position.
type RewriteError struct {
	err        error
	input      string // the text the refusing stage read
	start, end int    // byte offsets in input of the refused text
}

func (e *RewriteError) Error() string { return e.err.Error() }
func (e *RewriteError) Unwrap() error { return e.err }

// PositionedRewriteError is a rewriter refusal placed in the author's source.
// Parse carries the refused text's extent there -- AuthoredLine and friends,
// what Position and EndPosition read -- so every consumer that places a
// ParseError places this one too; Error prints the position ahead of the
// rewriter's message.
type PositionedRewriteError struct {
	Parse *ParseError
	err   error
}

func (e *PositionedRewriteError) Error() string {
	line, col := e.Parse.Position()
	return fmt.Sprintf("rewrite error at line %d, column %d: %s", line, col, e.err.Error())
}

// Unwrap reaches both the position and the rewriter's error chain, so
// errors.As finds a *ParseError and the *RewriteError alike.
func (e *PositionedRewriteError) Unwrap() []error { return []error{e.Parse, e.err} }

// PositionRewriteError returns err placed in authored, the source the
// rewrite read (before StripNonProceduralBlocks, or with a slice's
// AnchorSource marker -- the placement composes), when err carries a
// RewriteError; otherwise err unchanged. Call it where the rewrite failed,
// with the text handed to it.
func PositionRewriteError(authored string, err error) error {
	var re *RewriteError
	if err == nil || !errors.As(err, &re) {
		return err
	}
	var placed *PositionedRewriteError
	if errors.As(err, &placed) {
		return err
	}
	line, col, endLine, endCol, ok := re.extentIn(authored)
	if !ok {
		return err
	}
	pe := &ParseError{
		Message: err.Error(),
		Line:    line, Column: col, EndLine: endLine, EndColumn: endCol,
		AuthoredLine: line, AuthoredColumn: col, AuthoredEndLine: endLine, AuthoredEndColumn: endCol,
	}
	return &PositionedRewriteError{Parse: pe, err: err}
}

// extentIn is the refused text's extent in authored: the tokens of the
// stage's input that the extent covers, placed by marking that input against
// authored.
func (e *RewriteError) extentIn(authored string) (line, col, endLine, endCol int, ok bool) {
	if e.start < 0 || e.start > len(e.input) {
		return 0, 0, 0, 0, false
	}
	end := max(e.end, e.start)
	toks, err := NewLexer(e.input).Tokenize()
	if err != nil {
		return 0, 0, 0, 0, false
	}
	startRune := utf8.RuneCountInString(e.input[:e.start])
	endRune := utf8.RuneCountInString(e.input[:min(end, len(e.input))])
	first, last := -1, -1
	for i, t := range toks {
		if t.Type == TokenEOF {
			break
		}
		if t.Pos < startRune {
			continue
		}
		if first < 0 {
			first = i
		}
		if t.Pos >= endRune && i > first {
			break
		}
		last = i
	}
	if first < 0 {
		return 0, 0, 0, 0, false
	}
	placed := toks
	if marked := PositionLowering(authored, e.input); marked != e.input {
		if mt, err := NewLexer(marked).Tokenize(); err == nil && len(mt) == len(toks) {
			placed = mt
		}
	}
	line, col = placed[first].At()
	endLine, endCol = placed[last].EndAt()
	return line, col, endLine, endCol, true
}

// refusal is a rewriter error that names the author's text it refuses, for
// rewriteEachBlock to turn into a RewriteError: at is a byte offset into the
// body of the construct being rewritten (width bytes long), or -- at < 0 --
// the text is found in the construct, preamble included: by the nth match
// (nth 0 is the first) of pattern, or of text verbatim, searched after the
// first match of after when after is set. Unlocatable, the refusal falls
// back to the construct's name.
type refusal struct {
	err     error
	at      int
	width   int
	pattern *regexp.Regexp
	text    string
	nth     int
	after   *regexp.Regexp
}

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

// refuseAtBody marks err as a refusal of the body text at [at, at+width).
func refuseAtBody(at, width int, err error) error {
	return &refusal{err: err, at: at, width: width}
}

// refuseText marks err as a refusal of the first occurrence of text in the
// construct.
func refuseText(text string, err error) error {
	return &refusal{err: err, at: -1, text: strings.TrimSpace(text)}
}

// refuseClause marks err as a refusal of the construct's first line that
// opens with keyword, as a word.
func refuseClause(keyword string, err error) error {
	return &refusal{err: err, at: -1, pattern: clauseLine(keyword)}
}

// refuseNthClause is refuseClause for the nth (0-based) such line: the
// second write block of a mutation that may have one.
func refuseNthClause(keyword string, nth int, err error) error {
	return &refusal{err: err, at: -1, pattern: clauseLine(keyword), nth: nth}
}

// refuseTextAfter marks err as a refusal of the first occurrence of text as
// a word after the construct's first line opening with keyword.
func refuseTextAfter(keyword, text string, err error) error {
	return &refusal{err: err, at: -1, pattern: regexp.MustCompile(`\b(` + regexp.QuoteMeta(text) + `)\b`), after: clauseLine(keyword)}
}

// shiftRefusal moves a body-offset refusal found in a slice of the body that
// starts at delta, so its offset indexes the body itself.
func shiftRefusal(err error, delta int) error {
	var r *refusal
	if errors.As(err, &r) && r.at >= 0 {
		r.at += delta
	}
	return err
}

// clauseLine matches a line whose first word is keyword; group 1 is the
// keyword.
func clauseLine(keyword string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*(` + regexp.QuoteMeta(keyword) + `)\b`)
}

// locate is where the refused text sits in src, a construct's text from its
// preamble on, whose body starts at bodyOff; scan is src with comments
// blanked, which is where patterns and text are looked for.
func (r *refusal) locate(scan string, bodyOff int) (start, end int, ok bool) {
	if r.at >= 0 {
		if bodyOff+r.at > len(scan) {
			return 0, 0, false
		}
		return bodyOff + r.at, bodyOff + r.at + max(r.width, 1), true
	}
	from := 0
	if r.after != nil {
		m := r.after.FindStringIndex(scan)
		if m == nil {
			return 0, 0, false
		}
		from = m[1]
	}
	switch {
	case r.pattern != nil:
		ms := r.pattern.FindAllStringSubmatchIndex(scan[from:], r.nth+1)
		if len(ms) <= r.nth {
			return 0, 0, false
		}
		m := ms[r.nth]
		if len(m) >= 4 && m[2] >= 0 {
			return from + m[2], from + m[3], true
		}
		return from + m[0], from + m[1], true
	case r.text != "":
		i := from
		for n := 0; ; n++ {
			j := strings.Index(scan[i:], r.text)
			if j < 0 {
				return 0, 0, false
			}
			if n == r.nth {
				return i + j, i + j + len(r.text), true
			}
			i += j + 1
		}
	}
	return 0, 0, false
}

// firstWordAt is the extent of the first word of s at or after offset i:
// where a refusal of a clause or a step points.
func firstWordAt(s string, i int) (start, width int) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	j := i
	for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' && s[j] != '{' && s[j] != '(' {
		j++
	}
	return i, j - i
}
