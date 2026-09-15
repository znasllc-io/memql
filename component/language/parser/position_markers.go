package parser

// position_markers.go -- the author's line and column through the struct-form
// lowering (memql#5364).
//
// The parser never reads the text an author wrote. NormaliseAll lowers every
// struct-form construct to procedural text first, and in doing so it MOVES the
// author's clauses: a filter lands inside `return concept==X && (...), nil`, a
// terse automation's annotations are hoisted onto lines of their own, a step's
// condition moves into `name := if cond { ... }`, and every line below a
// rewritten construct shifts. A position the lexer counts is a position in
// that lowered text, so a refusal inside a v1 expression -- the place an
// author most needs a squiggle -- was reported at a column the author never
// wrote (a `null` at column 31 of a filter reported at column 60 of the
// synthesized return).
//
// PositionLowering puts the author's coordinates back, IN the lowered text,
// as position markers: block comments the lexer reads the way a C compiler
// reads `#line`.
//
//	/*@at L:C*/   the token after the marker starts at the author's line L,
//	              column C, and the text after it advances from there
//	/*@pin L:C*/  every token up to the next marker is attributed to L:C --
//	              scaffolding the rewriter synthesized, pinned to the author
//	              text it stands in for
//
// Carrying the map in the text rather than beside it is what lets it survive
// the text transforms a consumer applies between the lowering and the lexer
// (the engine loader's payload-path translation, its canonical-id
// resolution): a marker states an absolute position, so a copied or edited
// neighbourhood cannot make it lie about the text after it. And a consumer
// that does not read markers loses nothing -- they are comments.
//
// The lexer stamps every token of a marked text with its authored extent
// (Token.Authored*), a ParseError built from a token carries it and prints it,
// and a v1 node's Span is authored. Token.Line and Column stay the lexed
// text's own coordinates: the parser's line-sensitive rules must read the text
// they are parsing.
//
// How the map is derived. A lowering keeps every line outside a rewritten
// construct byte-identical and in order, so a longest common subsequence over
// the two line sequences pairs those lines exactly; a token on one of them is
// at the same column on its authored line. What is left is one hunk per
// rewritten construct: its authored lines against the lines it was lowered
// to. Inside a hunk the lowered tokens are aligned with the authored ones by
// a second LCS, over token type and literal. The rewriter copies every piece
// of author text it keeps -- a filter, a refine lambda, a mutation value, a
// step condition, an annotation -- verbatim and in order, so each copied token
// aligns with the author's token and maps to it exactly. A token that aligns
// with nothing is scaffolding, and is pinned to the end of the author token
// before it (or, at the head of a construct, to the construct's first token):
// a `)` the lowering closed around a filter that never closed is reported
// where the filter ends.

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The two marker kinds, as the text after a marker comment's `/*`.
const (
	positionMarkerAt  = "@at "
	positionMarkerPin = "@pin "
)

// positionLCSMaxCells caps both alignments' dynamic-programming matrices. The
// line alignment of the largest DSL file in the tree is well under it; past
// the cap a text is left unmarked, which leaves every consumer exactly where
// it was before markers existed.
const positionLCSMaxCells = 12_000_000

// lexOrigin is the lexer's position-marker state: how a position in the text
// being lexed maps back to the author's source.
type lexOrigin struct {
	marked      bool // a marker has been read: tokens carry authored positions
	pinned      bool
	aLine, aCol int // the authored position the last marker named
	rLine, rCol int // the lexed position just after that marker
}

// at maps a lexed position to the author's source. Before the first marker it
// is the identity -- which PositionLowering keeps true, by marking the first
// token the identity would place wrongly.
func (o *lexOrigin) at(line, col int) (int, int) {
	switch {
	case !o.marked:
		return line, col
	case o.pinned:
		return o.aLine, o.aCol
	case line == o.rLine:
		return o.aLine, o.aCol + col - o.rCol
	default:
		return o.aLine + line - o.rLine, col
	}
}

// stamp records tok's authored extent. A pinned token is one column wide at
// its pin, and the EOF token is zero-width.
func (o *lexOrigin) stamp(tok *Token) {
	tok.AuthoredLine, tok.AuthoredCol = o.at(tok.Line, tok.Column)
	switch {
	case tok.Type == TokenEOF:
		tok.AuthoredEndLine, tok.AuthoredEndCol = tok.AuthoredLine, tok.AuthoredCol
	case o.marked && o.pinned:
		tok.AuthoredEndLine, tok.AuthoredEndCol = tok.AuthoredLine, tok.AuthoredCol+1
	default:
		tok.AuthoredEndLine, tok.AuthoredEndCol = o.at(tok.EndLine, tok.EndCol)
	}
}

// read takes the body of a block comment the lexer just consumed; a position
// marker moves the origin to the position it names. (line, col) is the lexed
// position just after the comment.
func (o *lexOrigin) read(body string, line, col int) {
	aLine, aCol, pinned, ok := parsePositionMarker(body)
	if !ok {
		return
	}
	*o = lexOrigin{marked: true, pinned: pinned, aLine: aLine, aCol: aCol, rLine: line, rCol: col}
}

// parsePositionMarker reads a block comment's body as a position marker. Any
// other comment -- `/*@todo*/` included -- is not one.
func parsePositionMarker(body string) (line, col int, pinned, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(body, positionMarkerAt):
		rest = body[len(positionMarkerAt):]
	case strings.HasPrefix(body, positionMarkerPin):
		rest, pinned = body[len(positionMarkerPin):], true
	default:
		return 0, 0, false, false
	}
	ls, cs, found := strings.Cut(rest, ":")
	if !found {
		return 0, 0, false, false
	}
	l, lerr := strconv.Atoi(ls)
	c, cerr := strconv.Atoi(cs)
	// Line 0 is the line above the first: AnchorSource's marker names it so
	// that the newline after the marker starts line 1.
	if lerr != nil || cerr != nil || l < 0 || c < 1 {
		return 0, 0, false, false
	}
	return l, c, pinned, true
}

// positionMarker spells a marker.
func positionMarker(line, col int, pinned bool) string {
	kind := positionMarkerAt
	if pinned {
		kind = positionMarkerPin
	}
	return "/*" + kind + strconv.Itoa(line) + ":" + strconv.Itoa(col) + "*/"
}

// PositionLowering returns lowered carrying the position markers that map each
// of its tokens back to authored, the source it was lowered from (by
// NormaliseAll, or any chain of the struct-form rewriters, with
// StripNonProceduralBlocks before them or not). Lex the result instead of
// lowered and every token, every ParseError and every v1 Span reports the
// author's line and column.
//
// Either text failing to lex, or a pair too large to align, returns lowered
// unmarked: positions then fall back to the lowered text's, which is what
// every consumer read before this existed.
//
// authored may itself carry markers -- a construct slice AnchorSource placed
// at its line in the file, say -- and the positions then compose: the
// lowering's tokens map to the file, not to the slice. Markers already in
// lowered (copied through by the rewriter) are replaced, not stacked.
func PositionLowering(authored, lowered string) string {
	if authored == lowered {
		return lowered
	}
	lowered = StripPositionMarkers(lowered)
	plain := StripPositionMarkers(authored)
	low, err := NewLexer(lowered).Tokenize()
	if err != nil {
		return lowered
	}
	// The alignment reads the authored text's own geometry; the positions
	// it hands out come from the authored text as written, markers and all.
	// Markers are comments, so the two token streams are the same tokens.
	geom, err := NewLexer(plain).Tokenize()
	if err != nil {
		return lowered
	}
	pos := geom
	if plain != authored {
		if pos, err = NewLexer(authored).Tokenize(); err != nil || len(pos) != len(geom) {
			return lowered
		}
	}
	want, ok := authoredTokenPositions(plain, lowered, geom, pos, low)
	if !ok {
		return lowered
	}
	return insertPositionMarkers(lowered, low, want)
}

// AnchorSource marks src, a slice cut from a file at the start of a line, as
// starting on that line of the file: lexed after PositionLowering, or on its
// own, every position in the slice is the file's. The marker goes on a line of
// its own above src, so nothing that reads src's first line as TEXT -- the
// rewriter's header patterns -- sees it; the slice gains that one line.
func AnchorSource(src string, line int) string {
	if line < 1 {
		return src
	}
	return positionMarker(line-1, 1, false) + "\n" + src
}

// StripPositionMarkers removes every position marker from s -- for a span of
// marked text that leaves the parser as TEXT (a step's verbatim source), where
// a marker would be noise. Other comments are kept.
func StripPositionMarkers(s string) string {
	if !strings.Contains(s, "/*@") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "/*@")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := strings.Index(s[i:], "*/")
		if end < 0 {
			b.WriteString(s)
			return b.String()
		}
		end += i
		if _, _, _, ok := parsePositionMarker(s[i+2 : end]); ok {
			b.WriteString(s[:i])
		} else {
			b.WriteString(s[:end+2])
		}
		s = s[end+2:]
	}
}

// wantPos is where a lowered token belongs in the author's source.
type wantPos struct {
	line, col int
	pinned    bool
}

// authoredTokenPositions computes, for each lowered token, its position in
// the author's source (see the file comment for the method). auth is the
// authored text's tokens as that text lays them out, for the alignment; pos
// is the same tokens with the positions to hand out (At, EndAt), which differ
// only when the authored text carries markers of its own. ok is false when
// the texts are too large to align.
func authoredTokenPositions(authored, lowered string, auth, pos, low []Token) ([]wantPos, bool) {
	aLines := strings.Split(authored, "\n")
	lLines := strings.Split(lowered, "\n")
	pairs, ok := lcsPairs(len(aLines), len(lLines), func(a, r int) bool { return aLines[a] == lLines[r] })
	if !ok {
		return nil, false
	}

	// Number the hunks: the lowered lines between two matched lines, against
	// the authored lines between their partners.
	lowMatch := make([]int, len(lLines))
	lowHunk := make([]int, len(lLines))
	authHunk := make([]int, len(aLines))
	for i := range lowMatch {
		lowMatch[i], lowHunk[i] = -1, -1
	}
	for i := range authHunk {
		authHunk[i] = -1
	}
	hunks := 0
	prevA, prevR := -1, -1
	closeHunk := func(nextA, nextR int) {
		if nextR-prevR > 1 || nextA-prevA > 1 {
			for r := prevR + 1; r < nextR; r++ {
				lowHunk[r] = hunks
			}
			for a := prevA + 1; a < nextA; a++ {
				authHunk[a] = hunks
			}
			hunks++
		}
	}
	for _, p := range pairs {
		closeHunk(p.a, p.b)
		lowMatch[p.b] = p.a
		prevA, prevR = p.a, p.b
	}
	closeHunk(len(aLines), len(lLines))

	lowIn := make([][]int, hunks)
	authIn := make([][]int, hunks)
	for i, t := range low {
		if t.Type != TokenEOF {
			if h := lowHunk[t.Line-1]; h >= 0 {
				lowIn[h] = append(lowIn[h], i)
			}
		}
	}
	for j, t := range auth {
		if t.Type != TokenEOF {
			if h := authHunk[t.Line-1]; h >= 0 {
				authIn[h] = append(authIn[h], j)
			}
		}
	}

	aligned := make([]int, len(low))
	for i := range aligned {
		aligned[i] = -1
	}
	for h := 0; h < hunks; h++ {
		li, ai := lowIn[h], authIn[h]
		pairs, ok := lcsPairs(len(ai), len(li), func(a, r int) bool {
			at, lt := auth[ai[a]], low[li[r]]
			return at.Type == lt.Type && at.Literal == lt.Literal
		})
		if !ok {
			continue // pinned wholesale: positions within the construct, never outside it
		}
		for _, p := range pairs {
			aligned[li[p.b]] = ai[p.a]
		}
	}

	// A token on a matched line is at the same column of the partner line, so
	// it IS the authored token found there.
	authAt := make(map[[2]int]int, len(auth))
	for j, t := range auth {
		authAt[[2]int{t.Line, t.Column}] = j
	}
	start := func(j int) wantPos {
		line, col := pos[j].At()
		return wantPos{line: line, col: col}
	}
	end := func(j int) wantPos {
		line, col := pos[j].EndAt()
		return wantPos{line: line, col: col}
	}

	want := make([]wantPos, len(low))
	lastEnd := wantPos{line: 1, col: 1} // the authored end of the previous placed token
	curHunk, placedInHunk := -1, false
	for i, t := range low {
		if t.Type == TokenEOF {
			continue
		}
		if m := lowMatch[t.Line-1]; m >= 0 {
			curHunk = -1
			if j, found := authAt[[2]int{m + 1, t.Column}]; found {
				want[i] = start(j)
				lastEnd = end(j)
				continue
			}
			// Unreachable while a matched line lexes the same on both sides;
			// if a construct spanning lines ever makes it differ, pinning
			// keeps the position on the author's text rather than guessing.
			want[i] = wantPos{line: lastEnd.line, col: lastEnd.col, pinned: true}
			continue
		}
		if h := lowHunk[t.Line-1]; h != curHunk {
			curHunk, placedInHunk = h, false
		}
		if j := aligned[i]; j >= 0 {
			want[i] = start(j)
			lastEnd = end(j)
			placedInHunk = true
			continue
		}
		pin := lastEnd
		if !placedInHunk && curHunk >= 0 && len(authIn[curHunk]) > 0 {
			pin = start(authIn[curHunk][0])
		}
		want[i] = wantPos{line: pin.line, col: pin.col, pinned: true}
	}
	return want, true
}

// insertPositionMarkers writes lowered with a marker before each token whose
// authored position the lexer would otherwise get wrong. It simulates the
// lexer's origin over the text it is writing, so a marker is emitted only
// where the mapping breaks: a run of tokens copied verbatim needs one.
//
// A marker only ever goes immediately before a token, which is never inside a
// string or a comment, and no token's scan reads past its own end into the
// next token -- so the marked text lexes to the same tokens. The one
// character a comment cannot follow is '/', which would make `//*`; a space
// goes between them.
func insertPositionMarkers(lowered string, low []Token, want []wantPos) string {
	runes := []rune(lowered)
	var b strings.Builder
	b.Grow(len(lowered) + len(lowered)/4)
	sim := lexOrigin{}
	copied := 0
	shiftLine, shift := 0, 0
	for i, t := range low {
		if t.Type == TokenEOF {
			break
		}
		if t.Line != shiftLine {
			shiftLine, shift = t.Line, 0
		}
		w := want[i]
		var need bool
		if w.pinned {
			need = !sim.marked || !sim.pinned || sim.aLine != w.line || sim.aCol != w.col
		} else {
			line, col := sim.at(t.Line, t.Column+shift)
			need = (sim.marked && sim.pinned) || line != w.line || col != w.col
		}
		if !need {
			continue
		}
		marker := positionMarker(w.line, w.col, w.pinned)
		if t.Pos > 0 && runes[t.Pos-1] == '/' {
			marker = " " + marker
		}
		b.WriteString(string(runes[copied:t.Pos]))
		b.WriteString(marker)
		copied = t.Pos
		shift += utf8.RuneCountInString(marker)
		sim = lexOrigin{marked: true, pinned: w.pinned, aLine: w.line, aCol: w.col, rLine: t.Line, rCol: t.Column + shift}
	}
	b.WriteString(string(runes[copied:]))
	return b.String()
}

// lcsPair is one matched index pair of a longest common subsequence: a into
// the first sequence, b into the second.
type lcsPair struct{ a, b int }

// lcsPairs returns a longest common subsequence of two sequences of lengths n
// and m under eq, as matched index pairs in increasing order. A common prefix
// and suffix are matched without the matrix, which is what keeps a lowering's
// line alignment cheap: most of a file is outside any construct. ok is false
// when the remaining middle exceeds positionLCSMaxCells.
func lcsPairs(n, m int, eq func(a, b int) bool) ([]lcsPair, bool) {
	var pairs []lcsPair
	lo := 0
	for lo < n && lo < m && eq(lo, lo) {
		pairs = append(pairs, lcsPair{lo, lo})
		lo++
	}
	hiA, hiB := n, m
	var tail []lcsPair
	for hiA > lo && hiB > lo && eq(hiA-1, hiB-1) {
		hiA--
		hiB--
		tail = append(tail, lcsPair{hiA, hiB})
	}
	rows, cols := hiA-lo, hiB-lo
	if rows > 0 && cols > 0 {
		if rows > positionLCSMaxCells || cols > positionLCSMaxCells || rows > positionLCSMaxCells/cols {
			return nil, false
		}
		// dp[i][j] = LCS length of the middles' suffixes from i and j.
		dp := make([][]int32, rows+1)
		for i := range dp {
			dp[i] = make([]int32, cols+1)
		}
		for i := rows - 1; i >= 0; i-- {
			for j := cols - 1; j >= 0; j-- {
				switch {
				case eq(lo+i, lo+j):
					dp[i][j] = dp[i+1][j+1] + 1
				case dp[i+1][j] >= dp[i][j+1]:
					dp[i][j] = dp[i+1][j]
				default:
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		for i, j := 0, 0; i < rows && j < cols; {
			switch {
			case eq(lo+i, lo+j) && dp[i][j] == dp[i+1][j+1]+1:
				pairs = append(pairs, lcsPair{lo + i, lo + j})
				i++
				j++
			case dp[i+1][j] >= dp[i][j+1]:
				i++
			default:
				j++
			}
		}
	}
	for k := len(tail) - 1; k >= 0; k-- {
		pairs = append(pairs, tail[k])
	}
	return pairs, true
}
