package sense

import (
	"context"
	"sort"
)

// diagnose_load.go -- the load's refusals as editor diagnostics (memql#5434).
//
// Diagnose reports what lexing, parsing and rewriting find. What the load's
// lowering refuses -- a field the concept does not declare, a context spec
// applied to the row, a read that needs `.?`, a cost over budget -- needs the
// engine's registries, so it is a second pass with a price of its own: the
// load of the document's constructs, 10-100 ms on the tree's largest files.
// An editor runs it beside Diagnose rather than inside it, so a keystroke's
// syntax error is never kept waiting on it, and merges the two with
// MergeLoadDiagnostics.

// LoadPass is a RegistryProvider that can run the engine's load over one
// document: the engine's adapter implements it, a registry-less or fake
// provider need not.
type LoadPass interface {
	// LoadDiagnostics returns the load's refusals of source, were it the file
	// at filePath -- a path RELATIVE TO THE DSL ROOT ("planner/queries.memql"),
	// which is what the load derives namespaces from. Each is an Error carrying
	// its rule code. A path that places the document in no loaded domain
	// yields nothing, and so does a pass ctx cancels.
	LoadDiagnostics(ctx context.Context, source, filePath string) []Diagnostic
}

// CanLoad reports whether the service has an engine behind it that can run the
// load -- a caller that has to schedule the pass asks before it does.
func (s *Service) CanLoad() bool {
	if s == nil {
		return false
	}
	_, ok := s.registries.(LoadPass)
	return ok
}

// DiagnoseLoad runs the engine's load over a document and returns its
// refusals, with their rule codes (`lower_unknown_field`, ...) and the
// positions of the nodes they refuse. filePath is the document's path relative
// to the DSL root: without it a name two domains declare cannot be resolved as
// the load resolves it, so an empty path yields nothing rather than a guess.
// Nil too when the service has no engine behind it, and when ctx is cancelled
// before the pass finishes -- the caller has gone or moved on.
func (s *Service) DiagnoseLoad(ctx context.Context, source, filePath string) []Diagnostic {
	if s == nil || filePath == "" || ctx.Err() != nil {
		return nil
	}
	lp, ok := s.registries.(LoadPass)
	if !ok || lp == nil {
		return nil
	}
	return lp.LoadDiagnostics(ctx, source, filePath)
}

// MergeLoadDiagnostics adds the load's diagnostics to Diagnose's, one squiggle
// per fault. A load refusal whose range overlaps a diagnostic Diagnose already
// reports is the same fault seen twice -- `actor.displayName` is both the
// closed-envelope rule's error and Lower's unknown field -- and one of the two
// is kept: the more severe, and on a tie Diagnose's, which the editor has
// already drawn. A load refusal that is more severe than every diagnostic it
// overlaps replaces them: a bare `id` in a filter is Diagnose's warning and the
// load's error, and the load is the one that refuses it. The result is ordered
// by position.
func MergeLoadDiagnostics(fast, load []Diagnostic) []Diagnostic {
	if len(load) == 0 {
		return fast
	}
	dropped := make([]bool, len(fast))
	out := make([]Diagnostic, 0, len(fast)+len(load))
	for _, l := range load {
		duplicate := false
		var weaker []int
		for i, f := range fast {
			if !rangesOverlap(f.Range, l.Range) {
				continue
			}
			if f.Severity <= l.Severity {
				duplicate = true
				break
			}
			weaker = append(weaker, i)
		}
		if duplicate {
			continue
		}
		for _, i := range weaker {
			dropped[i] = true
		}
		out = append(out, l)
	}
	for i, f := range fast {
		if !dropped[i] {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return positionBefore(out[i].Range.Start, out[j].Range.Start)
	})
	return out
}

// rangesOverlap reports whether two ranges share a character. An empty range
// covers the character it starts at, so a caret-wide diagnostic still meets the
// token it sits on.
func rangesOverlap(a, b Range) bool {
	a, b = widenEmpty(a), widenEmpty(b)
	return positionBefore(a.Start, b.End) && positionBefore(b.Start, a.End)
}

// widenEmpty gives an empty or inverted range the one character at its start.
func widenEmpty(r Range) Range {
	if !positionBefore(r.Start, r.End) {
		r.End = Position{Line: r.Start.Line, Column: r.Start.Column + 1}
	}
	return r
}

// positionBefore orders positions by line, then column.
func positionBefore(a, b Position) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Column < b.Column
}
