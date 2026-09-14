package automations

// args_resolution.go -- the args contract of an automation's expressions
// (G2 of the event-payload-binding epic, memql#2352, story #2364; ADR
// docs/internal/design/event-payload-binding-adr.md Decision 3), and the G5
// source scan (memql#2367).
//
// At run time a bare name in an args-block automation resolves in RunScope's
// order -- loop variable, step, args field, a declared-but-absent optional
// field as nil (run_scope.go). At load, validateArgsResolution checks the
// parsed expressions against the same order (args_resolution_v1.go):
//   - an args field may not shadow a reserved engine name;
//   - a step id or forEach loop variable may not shadow an args field;
//   - a free name must be a reserved root, a loop variable in scope, a step
//     or an args field -- a typo'd field is a load error, not a run-time nil.
// Explicit `args.X` remains valid everywhere as the disambiguator.

import (
	"regexp"
	"strings"
)

// reservedAutomationRoots are the engine-provided bare roots an automation
// expression may reference. An args field may not shadow any of these
// (mirrors the reserved-name rule for function args). The set is the ADR
// Decision 3 reserved list plus the roots a run seeds (RunScope) and the
// `payload` envelope shorthand.
var reservedAutomationRoots = map[string]bool{
	// ADR Decision 3 reserved names.
	"now": true, "actor": true, "partition": true, "config": true,
	"trace": true, "event": true, "args": true, "steps": true,
	// Evaluator ambient roots + shorthands (conditionRootSegment,
	// EvaluateFilterValue).
	"timestamp": true, "ctx": true, "input": true, "item": true,
	"var": true, "systemVar": true, "secret": true, "systemSecret": true,
	"automation": true, "payload": true, "error": true,
}

// declaredArgsSet projects an automation's args-field names for the
// evaluator seed (SetCustom("argsDeclared", ...)) so optional declared
// fields resolve to nil instead of the literal fallback.
func declaredArgsSet(a *Automation) map[string]bool {
	if a == nil || a.Args == nil || len(a.Args.Fields) == 0 {
		return nil
	}
	set := make(map[string]bool, len(a.Args.Fields))
	for _, f := range a.Args.Fields {
		set[f.Name] = true
	}
	return set
}

// ---------------------------------------------------------------------------
// Load-time validation
// ---------------------------------------------------------------------------

// validateArgsResolution enforces the ADR Decision 3 load-time rules on an
// automation's parsed expressions (args_resolution_v1.go).
func validateArgsResolution(a *Automation) error {
	// A statement body's names were checked when it compiled, by
	// compiler.CheckBody (epic memql#5370): the scope rules are the gate
	// there, and the G2 bare-args tier this function polices does not exist
	// in it -- an argument is read args.x.
	if a.IsStatementBody() {
		return nil
	}
	return validateArgsResolutionV1(a)
}

func collectStepIDs(steps []*Step, into map[string]bool) {
	for _, s := range steps {
		if s == nil {
			continue
		}
		if s.ID != "" {
			into[s.ID] = true
		}
		if s.ForEach != nil {
			collectStepIDs(s.ForEach.Do, into)
		}
		if s.Parallel != nil {
			collectStepIDs(s.Parallel.Branches, into)
		}
		if s.Switch != nil {
			for _, c := range s.Switch.Cases {
				collectStepIDs(caseSteps(c), into)
			}
			collectStepIDs(caseSteps(s.Switch.Default), into)
		}
	}
}

func caseSteps(c *SwitchCase) []*Step {
	if c == nil {
		return nil
	}
	if c.Step != nil {
		return append(append([]*Step(nil), c.Steps...), c.Step)
	}
	return c.Steps
}

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+1)
	for k := range in {
		out[k] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// G5 (memql#2367): retired event.payload reads -- source-level scan
// ---------------------------------------------------------------------------

// eventPayloadReadPattern matches a live `event.<anything>` (or the
// `$event.<anything>` dollar form) read in authored automation source.
//
// DELIBERATELY WIDER THAN `event.payload.` (memql#3610). It used to match that
// prefix alone, which made it blind to the one spelling authors actually
// reached for: `event.node.payload.X` reads exactly like the shape of a
// graph-node event, and there is no `node` key anywhere in the envelope the CDC
// publisher builds -- so it resolved to nothing, the filter decided false, and
// the automation never fired. That is how the computer-use kill switch came to
// be inert, and how per-Plan workbench directories stopped being torn down.
//
// The narrow pattern could not have caught it: the broken spelling was not the
// retired one. Any dotted read off `event` is now refused, because in an
// automation body the payload binds to the args { } contract and is read bare.
// A bare `event` passed along as a step argument (`logic f ( event: event )`)
// carries no dot and is unaffected, as are logic bodies, which read
// `args.event.payload.X` on a different surface.
var eventPayloadReadPattern = regexp.MustCompile(`[$]?\bevent\.[A-Za-z_]`)

// scrubSourceForPayloadScan blanks string literals AND both comment forms so
// the retirement scan never fires on prose (@description text, header
// comments).
//
// The `/*` arm was missing (memql#2872 review). It went unreachable-from-above
// until the preamble walk started carrying block-comment bodies into the slice;
// after that, an ordinary note like
//
//	/* before #2367 this read event.payload.status directly */
//
// above an automation refused the whole tree -- and the diagnostic blamed the
// automation for a read that exists only in a comment. Byte-for-byte the class
// the $steps. gate fix closed.
//
// NOT replaced with BlankComments: this scrubber deliberately also blanks
// STRING LITERALS, which BlankComments leaves intact by design (it exists so
// header detectors see a comment-free view, not a literal-free one). The two
// answer different questions, so this keeps its own scan and just grows the
// missing arm.
func scrubSourceForPayloadScan(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '"':
			// Escape state is TRACKED, not inferred from the preceding byte
			// (memql#2949). A one-byte lookback cannot tell an escaped quote
			// from a quote that follows a COMPLETED `\\` escape, so a literal
			// ending in a backslash pair read its own closing quote as escaped
			// and consumed on to the next quote -- blanking source that was
			// never inside a literal and failing the G5 scan OPEN.
			//
			// A literal MAY span lines, so this does NOT stop at a newline.
			// The lexer's scanString (component/language/parser/lexer.go) has
			// no newline case: it writes `\n` into the literal like any other
			// byte, so `"one<NL>two"` is ONE valid string token. An earlier
			// version of this arm stopped at `\n` on the belief that the
			// grammar forbade multi-line literals; it does not, and that guard
			// broke the gate in BOTH directions -- prose in a wrapped
			// @description was scanned as code (false refusal), and the
			// literal's real closing quote was re-read as an OPENING quote,
			// blanking a genuine retired read after it (the very fail-open
			// this function exists to prevent). memql#2949 review.
			//
			// An UNBALANCED quote -- no closing quote anywhere before EOF --
			// still costs only its own line rather than the rest of the file,
			// which is what memql#2949 asked for. Note the lexer refuses such
			// source outright ("unterminated string"), so compileMemQL aborts
			// before this scan ever runs; the fallback is belt-and-braces for
			// callers that scan source the parser has not accepted.
			//
			// Newlines inside the consumed span are PRESERVED, exactly as the
			// `/*` arm below does, so line accounting is unchanged.
			j := i + 1
			escaped := false
			closed := false
			firstNL := -1
			for j < len(s) {
				c := s[j]
				if c == '\n' && firstNL < 0 {
					firstNL = j
				}
				j++
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == '"' {
					closed = true
					break // closing quote, consumed above
				}
			}
			if !closed && firstNL >= 0 {
				j = firstNL // unbalanced: stop at the newline, leave it to the default arm
			}
			for k := i; k < j; k++ {
				if s[k] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
			}
			i = j
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			out.WriteString(strings.Repeat(" ", j))
			i += j
		case strings.HasPrefix(s[i:], "/*"):
			// Newlines are preserved so the scan's line accounting is
			// unchanged; everything else in the span becomes a space. An
			// unterminated block comment runs to EOF, matching the lexer.
			end := strings.Index(s[i+2:], "*/")
			j := len(s) - i
			if end >= 0 {
				j = end + 4
			}
			for k := i; k < i+j; k++ {
				if s[k] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
			}
			i += j
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}
