package automations

// args_resolution.go implements G2 of the event-payload-binding epic
// (memql#2352, story #2364; ADR docs/internal/design/event-payload-binding-adr.md
// Decision 3): bare-field reads inside automations that declare an
// `args { }` block, plus the load-time rules that keep them unambiguous.
//
// Runtime -- for an args-block automation, a BARE identifier in a step arg,
// condition, switch subject, or forEach source/filter resolves in tier
// order: loop variable in scope -> recorded step result -> args field.
// A declared-but-absent optional field resolves to nil (never to the
// identifier's own text -- the literal-string fallback is the silent-wrong-
// value class this story kills). Automations WITHOUT an args block keep
// their exact prior behavior: every new path is gated on the `args`
// binding G1 seeds, so the existing tree is untouched.
//
// Load time -- validateArgsResolution rejects, for args-block automations
// only:
//   - an args field shadowing a reserved engine name;
//   - a step id or forEach loop variable shadowing an args field;
//   - a bare identifier in an authored expression that is not a reserved
//     name, loop variable in scope, step name, or args field (a typo'd
//     field would otherwise be a runtime nil/literal).
// Explicit `args.X` remains valid everywhere as the disambiguator.

import (
	"fmt"
	"regexp"
	"strings"
)

// reservedAutomationRoots are the engine-provided bare roots an automation
// expression may reference. An args field may not shadow any of these
// (mirrors the reserved-name rule for function args). The set is the ADR
// Decision 3 reserved list plus the evaluator's long-standing ambient roots
// (conditionRootSegment) and the `payload.X` filter shorthand.
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

// bareExprLiterals are tokens that look like identifiers but are literal /
// operator vocabulary in the evaluator's expression dialect -- never
// resolution candidates.
var bareExprLiterals = map[string]bool{
	"true": true, "false": true, "null": true, "nil": true,
	"and": true, "or": true, "not": true, "in": true,
	"exists": true, "len": true, "cond": true, "coalesce": true, "concat": true,
}

// resolveBareForArgsAutomation implements the runtime tier order for a bare
// identifier inside an args-block automation:
//
//	loop variable -> step result -> args field (bound, or declared -> nil).
//
// Returns (nil, false) when the automation carries no args binding, so
// args-less automations keep their exact prior behavior (G2 is additive).
func (e *Evaluator) resolveBareForArgsAutomation(name string) (any, bool) {
	boundArgs, gated := e.custom["args"].(map[string]any)
	if !gated {
		return nil, false
	}
	// Tier 2: loop variable in scope (tier 1, reserved names, never reaches
	// the bare-identifier fallback -- they resolve via their existing forms).
	if name == e.itemName && e.item != nil {
		return e.item, true
	}
	// Tier 3: recorded step result.
	if v, ok := e.StepResultValue(name); ok {
		return v, true
	}
	// Tier 4: args field. A declared-but-absent optional field is nil --
	// never the literal fallback.
	if v, ok := boundArgs[name]; ok {
		return v, true
	}
	if declared, ok := e.custom["argsDeclared"].(map[string]bool); ok && declared[name] {
		return nil, true
	}
	return nil, false
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

// bareTokenPattern matches identifier-ish tokens (with optional dotted
// tails) in the evaluator's expression dialect. Quoted strings and
// $-prefixed references are stripped/skipped before matching.
var bareTokenPattern = regexp.MustCompile(`[$]?[A-Za-z_][A-Za-z0-9_.]*[(]?`)

// quotedSegmentPattern strips single- and double-quoted string literals so
// their content never yields candidate tokens.
var quotedSegmentPattern = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)

// validateArgsResolution enforces the ADR Decision 3 load-time rules on an
// args-block automation. Automations without an args block are exempt --
// their bare-identifier semantics (including the literal-string fallback)
// are unchanged, so the existing tree cannot regress.
func validateArgsResolution(a *Automation) error {
	fields := declaredArgsSet(a)
	if fields == nil {
		return nil
	}

	// Rule 1: an args field may not shadow a reserved engine name.
	for name := range fields {
		if reservedAutomationRoots[name] {
			return fmt.Errorf("automation %q: args field %q shadows a reserved engine name -- rename the field (reserved: now, actor, partition, config, trace, event, args, steps, ...)", a.Name, name)
		}
	}

	// Rules 2+3 walk the step tree with loop-variable scope.
	stepIDs := map[string]bool{}
	collectStepIDs(a.Steps, stepIDs)

	// Rule 2: a step id may not shadow an args field.
	for id := range stepIDs {
		if fields[id] {
			return fmt.Errorf("automation %q: step %q shadows the args field of the same name -- rename one of them (bare %q must resolve unambiguously; explicit args.%s always works)", a.Name, id, id, id)
		}
	}

	return walkStepsForResolution(a, a.Steps, fields, stepIDs, map[string]bool{})
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

// walkStepsForResolution validates loop-var shadowing plus every authored
// expression surface (condition, forEach source/filter, switch subject,
// function/action/automation step args, brace-form mutation payloads).
// Query strings are deliberately EXCLUDED: bare identifiers there are the
// bound concept's payload properties (SQL pushdown), a different namespace.
func walkStepsForResolution(a *Automation, steps []*Step, fields, stepIDs, loopVars map[string]bool) error {
	for _, s := range steps {
		if s == nil {
			continue
		}
		if err := checkExprIdentifiers(a, "condition of step "+s.ID, s.Condition, fields, stepIDs, loopVars); err != nil {
			return err
		}
		if s.Function != nil {
			if err := checkArgMapIdentifiers(a, "args of step "+s.ID, s.Function.Args, fields, stepIDs, loopVars); err != nil {
				return err
			}
		}
		if s.Action != nil {
			if err := checkArgMapIdentifiers(a, "args of action step "+s.ID, s.Action.Args, fields, stepIDs, loopVars); err != nil {
				return err
			}
		}
		if s.Automation != nil {
			if err := checkArgMapIdentifiers(a, "args of sub-automation step "+s.ID, s.Automation.Args, fields, stepIDs, loopVars); err != nil {
				return err
			}
		}
		if s.Mutation != nil {
			if err := checkArgMapIdentifiers(a, "payload of mutation step "+s.ID, s.Mutation.Payload, fields, stepIDs, loopVars); err != nil {
				return err
			}
			if err := checkExprIdentifiers(a, "id of mutation step "+s.ID, s.Mutation.ID, fields, stepIDs, loopVars); err != nil {
				return err
			}
		}
		if s.ForEach != nil {
			loopVar := s.ForEach.As
			if loopVar == "" {
				loopVar = "item"
			}
			if fields[loopVar] {
				return fmt.Errorf("automation %q: forEach loop variable %q (step %q) shadows the args field of the same name -- rename one of them", a.Name, loopVar, s.ID)
			}
			if err := checkExprIdentifiers(a, "forEach source of step "+s.ID, s.ForEach.Source, fields, stepIDs, loopVars); err != nil {
				return err
			}
			inner := cloneSet(loopVars)
			inner[loopVar] = true
			if err := checkExprIdentifiers(a, "forEach filter of step "+s.ID, s.ForEach.Filter, fields, stepIDs, inner); err != nil {
				return err
			}
			if err := walkStepsForResolution(a, s.ForEach.Do, fields, stepIDs, inner); err != nil {
				return err
			}
		}
		if s.Parallel != nil {
			if err := walkStepsForResolution(a, s.Parallel.Branches, fields, stepIDs, loopVars); err != nil {
				return err
			}
		}
		if s.Switch != nil {
			if err := checkExprIdentifiers(a, "switch subject of step "+s.ID, s.Switch.Expression, fields, stepIDs, loopVars); err != nil {
				return err
			}
			for _, c := range s.Switch.Cases {
				if err := walkStepsForResolution(a, caseSteps(c), fields, stepIDs, loopVars); err != nil {
					return err
				}
			}
			if err := walkStepsForResolution(a, caseSteps(s.Switch.Default), fields, stepIDs, loopVars); err != nil {
				return err
			}
		}
	}
	return nil
}

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+1)
	for k := range in {
		out[k] = true
	}
	return out
}

// checkArgMapIdentifiers recurses string leaves of an authored arg/payload
// map (nested maps + slices included).
func checkArgMapIdentifiers(a *Automation, where string, m map[string]any, fields, stepIDs, loopVars map[string]bool) error {
	for _, v := range m {
		if err := checkValueIdentifiers(a, where, v, fields, stepIDs, loopVars); err != nil {
			return err
		}
	}
	return nil
}

// checkValueIdentifiers is deliberately a NO-OP for string leaves: parseValue
// strips quotes from string values at parse time, so a literal
// `reason: "system.shutdown"` and a dotted reference arrive as IDENTICAL
// strings -- static identifier validation on values is unsound. The runtime
// resolves value strings ref-first (loop var / step / args field, G2) with
// the literal as fallback, exactly as before. Strict typo rejection holds on
// the surfaces where every token IS an expression: step conditions, forEach
// source/filter (filters keep their quotes since #2367), and switch subjects.
func checkValueIdentifiers(a *Automation, where string, v any, fields, stepIDs, loopVars map[string]bool) error {
	return nil
}

// checkExprIdentifiers rejects a bare identifier that resolves to nothing:
// not reserved, not a loop variable in scope, not a step name, not an args
// field. Only the HEAD segment of a dotted path is classified; call names
// (identifier immediately followed by '(') and $-references are skipped.
func checkExprIdentifiers(a *Automation, where, expr string, fields, stepIDs, loopVars map[string]bool) error {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	scrubbed := quotedSegmentPattern.ReplaceAllString(expr, `""`)
	for _, tok := range bareTokenPattern.FindAllString(scrubbed, -1) {
		if strings.HasPrefix(tok, "$") {
			continue // $-reference: the evaluator's explicit form
		}
		if strings.HasSuffix(tok, "(") {
			continue // call name (builtin/operator), not a value read
		}
		head := tok
		if i := strings.IndexByte(head, '.'); i >= 0 {
			head = head[:i]
		}
		if head == "" || bareExprLiterals[head] || reservedAutomationRoots[head] {
			continue
		}
		if loopVars[head] || stepIDs[head] || fields[head] {
			continue
		}
		return fmt.Errorf("automation %q: unknown bare identifier %q in %s -- not a reserved name, loop variable, step name, or args field. Declare it in the args { } block, or reference it explicitly (args.X / steps.X / event.payload.X)", a.Name, head, where)
	}
	return nil
}

// ---------------------------------------------------------------------------
// G5 (memql#2367): retired event.payload reads -- source-level scan
// ---------------------------------------------------------------------------

// eventPayloadReadPattern matches a live `event.payload.<field>` (or the
// `$event.payload.<field>` dollar form) read in authored automation source.
var eventPayloadReadPattern = regexp.MustCompile(`[$]?\bevent\.payload\.[A-Za-z_]`)

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

// ResolveBareArg exposes the G2 bare-resolution tiers (loop var -> step ->
// args field, declared-absent -> nil) to the arg-time evaluators in the steps
// package (G5, #2367): without it a bare/punned args-field VALUE falls through
// to the literal-name fallback -- `coalesce(source, "uploaded")` returned the
// string "source". Gated on the args binding; args-less automations get
// (nil, false) and keep prior behavior.
func (e *Evaluator) ResolveBareArg(name string) (any, bool) {
	return e.resolveBareForArgsAutomation(name)
}

// ResolveArgsPath resolves a dotted path whose HEAD is an ARGS FIELD carrying
// an object (e.g. `node.id` from cluster registerNode). A declared head with
// a missing tail yields (nil, true) so coalesce defaults apply -- never the
// literal path text. Heads that are NOT args fields return (_, false) so the
// legacy resolvers keep owning them: in particular `decide.result` (a STEP
// head) must fall through to the strict bare-path resolver, which knows the
// `.result` segment is already unwrapped -- routing it through this walker
// silently nil'd every step-rooted read in args-block automations (#2368
// conformance fallout).
func (e *Evaluator) ResolveArgsPath(path string) (any, bool) {
	bound, gated := e.custom["args"].(map[string]any)
	if !gated {
		return nil, false
	}
	head, rest, _ := strings.Cut(path, ".")
	declared, _ := e.custom["argsDeclared"].(map[string]bool)
	if !declared[head] {
		return nil, false
	}
	root, ok := bound[head]
	if !ok {
		return nil, true // declared-but-absent: nil, coalesce defaults apply
	}
	cur := root
	for rest != "" {
		var seg string
		seg, rest, _ = strings.Cut(rest, ".")
		m, isMap := cur.(map[string]any)
		if !isMap {
			return nil, true
		}
		cur = m[seg]
	}
	return cur, true
}
