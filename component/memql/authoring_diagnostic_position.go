package memql

// authoring_diagnostic_position.go -- maps a Gate-1 sandbox parse failure back
// to the AUTHORED bundle-file line/column (epic memql#2354 E4, issue #2375).
//
// A bundle construct is validated by slicing it out of the bundle, prepending
// the shared `use ...{ ... }` import preamble (function / action / capability
// kinds), and lowering it (NormaliseAll rewrites struct-form query / mutation /
// logic / automation into procedural `func (...)` of a DIFFERENT line count)
// before the parser sees it. So a parser.ParseError.Line refers to the LOWERED
// SLICE, not the line the author is looking at. Two hops recover the authored
// line:
//
//   hop A (lowered slice -> authored slice): reproduce the exact transform the
//     kind's parser applied to SandboxConstruct.Source, then run an LCS line
//     map (adapted from component/memql/sense/linemap.go, S4 #2359) rewritten
//     -> authored. Lines the lowering preserved BYTE-IDENTICALLY (comments,
//     `use` imports, blank lines, the @-annotation preamble, args-block fields,
//     filter / insert bodies) map EXACTLY; synthesized scaffolding anchors
//     within the construct's authored span, never onto an unrelated line.
//
//   hop B (authored slice -> bundle file): subtract the prepended import
//     preamble (BundlePreambleLines), then offset by the bundle line where the
//     construct body begins (BundleLine). A slice line inside the prepended
//     preamble has no authored-body counterpart, so its position is omitted.
//
// The rule throughout: emit a position ONLY when it can be computed reliably.
// A missing BundleLine, an unrecoverable parser position, or a hit inside the
// shared preamble yields zero (absent) -- never a guessed line.

import (
	"errors"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// authoredPosition is the bundle-file position of a failing token. A zero Line
// means "no reliable position" -- consumers must treat it as absent.
type authoredPosition struct {
	Line, Column, EndLine, EndColumn int
}

// bundleAnchorFor computes the position-mapping breadcrumbs for one bundle
// slice: the 1-based bundle line where the slice BODY begins, and the number of
// leading lines the splitter prepended (the shared import preamble). The body
// -- the slice with any prepended `use` preamble removed -- is a byte-identical
// contiguous region of the bundle, so strings.Index locates it exactly. A body
// that is NOT found verbatim (e.g. a terse automation the slicer lowered before
// slicing) yields bundleLine == 0, which downstream omits the position rather
// than guessing.
func bundleAnchorFor(bundle, src, usePreamble string) (bundleLine, preambleLines int) {
	body := src
	if usePreamble != "" && strings.HasPrefix(src, usePreamble) {
		preambleLines = strings.Count(usePreamble, "\n")
		body = src[len(usePreamble):]
	}
	idx := strings.Index(bundle, body)
	if idx < 0 {
		return 0, preambleLines
	}
	return 1 + strings.Count(bundle[:idx], "\n"), preambleLines
}

// resolveAuthoredPosition maps a compile/bind error for construct c back to the
// authored bundle-file position. It returns the zero authoredPosition when no
// reliable position exists: the error carries no recoverable parser position,
// the construct has no bundle anchor (BundleLine == 0, e.g. the DB-row path),
// or the failing token maps into the shared import preamble rather than the
// construct body.
func resolveAuthoredPosition(c SandboxConstruct, err error) authoredPosition {
	var pe *languageParser.ParseError
	if !errors.As(err, &pe) || pe.Line <= 0 {
		return authoredPosition{}
	}
	// Without a verbatim bundle anchor there is nothing to offset into.
	if c.BundleLine <= 0 {
		return authoredPosition{}
	}

	lm := newAuthoredLineMap(c.Source, rewrittenForKind(c))

	line, col, ok := c.bundlePos(lm, pe.Line, pe.Column)
	if !ok {
		return authoredPosition{}
	}
	out := authoredPosition{Line: line, Column: col}

	// End anchor (best-effort): the failing token carries its own end line/col.
	if pe.Token != nil && pe.Token.EndLine > 0 {
		if eLine, eCol, eok := c.bundlePos(lm, pe.Token.EndLine, pe.Token.EndCol); eok {
			out.EndLine, out.EndColumn = eLine, eCol
		}
	}
	return out
}

// bundlePos runs both hops for a single (rewritten line, column): hop A through
// the line map to the authored slice line, hop B off the bundle anchor. It
// returns ok=false when the slice line falls inside the prepended import
// preamble (no authored-body counterpart). The column is preserved only for a
// line the lowering kept byte-identical; a synthesized line collapses to
// column 1 (point at the line, not a bogus offset).
func (c SandboxConstruct) bundlePos(lm *authoredLineMap, rewrittenLine, rewrittenCol int) (line, col int, ok bool) {
	sliceLine, exact := lm.authoredLine(rewrittenLine)
	// Convert the authored-SLICE line to an authored-BODY line by removing the
	// prepended import preamble. A hit inside the preamble is not attributable
	// to this construct's body.
	bodyLine := sliceLine - c.BundlePreambleLines
	if bodyLine < 1 {
		return 0, 0, false
	}
	line = c.BundleLine + bodyLine - 1
	col = 0
	if exact && rewrittenCol > 0 {
		col = rewrittenCol
	}
	return line, col, true
}

// rewrittenForKind reproduces the exact source transform the per-construct
// parser applied to c.Source before lexing, so the LCS line map can align the
// parser's (lowered) line numbers with the authored slice. It mirrors the
// transforms in the real compile paths:
//
//   - query / mutation / logic / automation: NormaliseAll lowers struct-form to
//     procedural `func (...)`, changing line count. (The subsequent within-line
//     rewrites -- signature-concept path translation, canonical-id resolution --
//     do not change line COUNT, so they do not affect the mapping.)
//   - spec / trait / shape / action / capability: the parser sees the source
//     with its file-top `use` imports stripped (stripUseDeclarations); struct-
//     form, no lowering.
//   - concept + anything else: parsed as-is (identity).
//
// A NormaliseAll error falls back to the identity source -- the map then treats
// every line as its own, which is the safe degrade (no synthesized offset).
func rewrittenForKind(c SandboxConstruct) string {
	switch c.Kind {
	case "query", "mutation", "logic", "automation":
		if rewritten, err := languageParser.NormaliseAll(c.Source); err == nil {
			return rewritten
		}
		return c.Source
	case "spec", "trait", "shape", "action", "capability":
		return stripUseDeclarations(c.Source)
	default:
		return c.Source
	}
}

// authoredLineMap resolves a 1-based line in the LOWERED (parser-visible) source
// back to a 1-based line in the AUTHORED source. Adapted from S4's
// component/memql/sense/linemap.go: a longest-common-subsequence over the two
// line-text sequences recovers an EXACT mapping for every preserved line;
// rewritten-only (synthesized) lines anchor by relative offset within the
// authored span of the hunk they were synthesized into. Ported here (rather
// than imported) because that mapper is unexported and tied to the sense
// package's Diagnostic/Position types; this is the same algorithm over bare
// line indices.
type authoredLineMap struct {
	authored []int  // authored[i] = authored line (1-based) for rewritten line i+1
	exact    []bool // exact[i] = rewritten line i+1 was preserved verbatim
}

// authoredLineMapMaxCells caps the LCS matrix (12M int32 ~= 48MB transient),
// matching the sense mapper. Beyond it, a clamped identity map is used -- never
// worse than emitting no position.
const authoredLineMapMaxCells = 12_000_000

// newAuthoredLineMap builds the rewritten->authored line map for one lowering.
func newAuthoredLineMap(authored, rewritten string) *authoredLineMap {
	aLines := strings.Split(authored, "\n")
	rLines := strings.Split(rewritten, "\n")

	lm := &authoredLineMap{
		authored: make([]int, len(rLines)),
		exact:    make([]bool, len(rLines)),
	}

	// Degenerate / oversized inputs: clamped identity.
	if len(aLines) == 0 || len(aLines)*len(rLines) > authoredLineMapMaxCells {
		for i := range rLines {
			lm.authored[i] = clampAuthoredLine(i+1, len(aLines))
			lm.exact[i] = false
		}
		return lm
	}

	pairs := lcsAuthoredPairs(aLines, rLines)

	prevA, prevR := -1, -1
	fillHunk := func(nextA, nextR int) {
		aStart := prevA + 1
		aEnd := nextA - 1
		for r := prevR + 1; r < nextR; r++ {
			k := r - (prevR + 1)
			var a int
			switch {
			case aEnd >= aStart:
				a = aStart + k
				if a > aEnd {
					a = aEnd
				}
			case prevA >= 0:
				a = prevA
			default:
				a = 0
			}
			lm.authored[r] = clampAuthoredLine(a+1, len(aLines))
			lm.exact[r] = false
		}
	}
	for _, p := range pairs {
		fillHunk(p.a, p.r)
		lm.authored[p.r] = clampAuthoredLine(p.a+1, len(aLines))
		lm.exact[p.r] = true
		prevA, prevR = p.a, p.r
	}
	fillHunk(len(aLines), len(rLines))

	return lm
}

// authoredLine maps a 1-based rewritten line to its authored line plus whether
// that line was preserved verbatim (so its column is meaningful).
func (lm *authoredLineMap) authoredLine(rewrittenLine int) (line int, exact bool) {
	idx := rewrittenLine - 1
	if len(lm.authored) == 0 {
		return 1, false
	}
	if idx < 0 {
		idx = 0
	} else if idx >= len(lm.authored) {
		idx = len(lm.authored) - 1
	}
	return lm.authored[idx], lm.exact[idx]
}

func clampAuthoredLine(line, n int) int {
	if n <= 0 || line < 1 {
		return 1
	}
	if line > n {
		return n
	}
	return line
}

// authoredPair is one matched (authored, rewritten) line index, both 0-based.
type authoredPair struct{ a, r int }

// lcsAuthoredPairs returns the longest common subsequence of the two line slices
// as matched index pairs in increasing order. Standard O(n*m) DP; the caller
// caps n*m via authoredLineMapMaxCells.
func lcsAuthoredPairs(a, b []string) []authoredPair {
	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		row, next := dp[i], dp[i+1]
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				row[j] = next[j+1] + 1
			} else if next[j] >= row[j+1] {
				row[j] = next[j]
			} else {
				row[j] = row[j+1]
			}
		}
	}

	var pairs []authoredPair
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			pairs = append(pairs, authoredPair{a: i, r: j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pairs
}
