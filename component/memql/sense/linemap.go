package sense

// linemap.go maps positions in the LOWERED (rewritten) MemQL source that
// Diagnose feeds to the lexer/parser back to positions in the AUTHORED
// source the user is editing.
//
// Why a map is needed: Diagnose runs the same lowering the runtime loaders
// run (parser.StripNonProceduralBlocks + parser.NormaliseAll, mirrored by
// compiler.ParseFileSource) so struct-form query / mutation / logic /
// automation files parse instead of false-erroring on the contextual
// keyword (memql#2359). That lowering replaces each struct-form construct
// block with procedural `func (...) { ... }` text of a DIFFERENT line
// count, so a lexer/parser position in the lowered source no longer lines
// up with the authored line the author sees.
//
// The line map itself is the parser's (parser.LineMap), which
// compiler.ParseFileSource maps its parse errors through too, so a Sense
// squiggle and a lint diagnostic name the same authored line. This file
// adapts it to Sense's positions.

import "github.com/znasllc-io/memql/component/language/parser"

// lineMap resolves rewritten positions to authored ones. It is built once
// per Diagnose call.
type lineMap struct {
	lines *parser.LineMap
}

// newLineMap builds the rewritten->authored line map for one lowering.
func newLineMap(authored, rewritten string) *lineMap {
	return &lineMap{lines: parser.NewLineMap(authored, rewritten)}
}

// remap rewrites a diagnostic's start/end positions from rewritten
// coordinates to authored coordinates.
func (lm *lineMap) remap(d Diagnostic) Diagnostic {
	d.Range.Start = lm.pos(d.Range.Start)
	d.Range.End = lm.pos(d.Range.End)
	// A remap can invert the span (e.g. start/end fell on the same lowered
	// line but different authored anchors); normalise so End >= Start.
	if d.Range.End.Line < d.Range.Start.Line ||
		(d.Range.End.Line == d.Range.Start.Line && d.Range.End.Column < d.Range.Start.Column) {
		d.Range.End = Position{Line: d.Range.Start.Line, Column: d.Range.Start.Column + 1}
	}
	return d
}

// pos maps a single rewritten position to authored coordinates. For a line
// that was preserved verbatim the column is meaningful and kept; for a
// synthesized line the column no longer corresponds to anything the author
// wrote, so it collapses to column 1 (point at the line, not a bogus
// offset).
func (lm *lineMap) pos(p Position) Position {
	line, exact := lm.lines.Authored(p.Line)
	col := p.Column
	if !exact {
		col = 1
	}
	if col < 1 {
		col = 1
	}
	return Position{Line: line, Column: col}
}
