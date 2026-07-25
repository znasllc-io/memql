package memql

import "strings"

// memql#2772: bring the `??` operator (#2611) to write-block payloads.
//
// #2611's central claim is that `??` lowers byte-identically to the
// coalesce() spelling, so every downstream evaluator is untouched by
// construction. The compiler honours that by parsing and re-serialising.
// The write-block payload path cannot: it never builds an AST, it carries
// the value as raw text into a string-prefix evaluator whose grammar is a
// closed set of forms (`args.<path>`, `actor.<path>`, `now()`, `var(...)`,
// `coalesce(...)`, ...). Teaching that dispatcher operator precedence
// would be a much larger change than restoring the claim.
//
// So the template stores the LOWERED spelling: `a ?? b` becomes
// `coalesce(a, b)` while the template is built, and the evaluator never
// sees the token at all.

// lowerNullCoalesceExpr rewrites the `??` operators in one payload value
// expression into the equivalent coalesce() call, honouring the
// precedence #2611 chose: tighter than comparison, looser than additive.
//
// A comparison is therefore split FIRST and each side lowered on its own,
// so `a ?? "" == "x"` becomes `coalesce(a, "") == "x"` and not
// `coalesce(a, "" == "x")`. Below a comparison, the top-level `??` arms
// become the coalesce arguments verbatim, which gives the
// looser-than-additive binding for free: `a + 1 ?? 0` has the single arm
// `a + 1`, so it lowers to `coalesce(a + 1, 0)`.
//
// Text carrying no live `??` is returned unchanged, so the function is a
// no-op on already-lowered templates and on ordinary values.
func lowerNullCoalesceExpr(expr string) string {
	if !strings.Contains(expr, "??") {
		return expr
	}
	// Lower the core and give the caller back its own padding, so the
	// comparison recursion below rejoins around the operator exactly as
	// the author spaced it.
	core := strings.TrimSpace(expr)
	if core != expr {
		lead := expr[:strings.Index(expr, core)]
		return lead + lowerNullCoalesceExpr(core) + expr[len(lead)+len(core):]
	}
	// Recurse through a top-level comparison before touching `??`.
	if op, at := topLevelComparison(expr); at >= 0 {
		left := lowerNullCoalesceExpr(expr[:at])
		right := lowerNullCoalesceExpr(expr[at+len(op):])
		return left + op + right
	}
	arms := splitTopLevelNullCoalesce(expr)
	if len(arms) < 2 {
		// No live top-level `??` -- the token was inside a string, or
		// nested in a call argument. Lower each call argument in place.
		return lowerNullCoalesceInCallArgs(expr)
	}
	for i, arm := range arms {
		arms[i] = lowerNullCoalesceExpr(strings.TrimSpace(arm))
	}
	return "coalesce(" + strings.Join(arms, ", ") + ")"
}

// exprScanner walks an expression tracking string literals and bracket
// nesting, so callers only ever act on live, top-level bytes.
type exprScanner struct {
	s     string
	i     int
	depth int
	inStr bool
}

// next advances one byte and reports whether it is live (outside a string
// literal) and at top level (bracket depth zero).
func (sc *exprScanner) next() (b byte, live, top bool) {
	b = sc.s[sc.i]
	if sc.inStr {
		if b == '\\' && sc.i+1 < len(sc.s) {
			sc.i += 2
			return b, false, false
		}
		if b == '"' {
			sc.inStr = false
		}
		sc.i++
		return b, false, false
	}
	switch b {
	case '"':
		sc.inStr = true
	case '(', '[', '{':
		sc.depth++
	case ')', ']', '}':
		if sc.depth > 0 {
			sc.depth--
		}
	}
	atTop := sc.depth == 0
	// An opening bracket is itself top-level; the bytes after it are not.
	if b == '(' || b == '[' || b == '{' {
		atTop = sc.depth == 1
	}
	sc.i++
	return b, true, atTop
}

// topLevelComparison returns the first unparenthesised comparison
// operator and its offset, or ("", -1) when there is none.
func topLevelComparison(expr string) (string, int) {
	sc := &exprScanner{s: expr}
	for sc.i < len(expr) {
		at := sc.i
		b, live, top := sc.next()
		if !live || !top {
			continue
		}
		switch b {
		case '=', '!', '<', '>':
			if at+1 < len(expr) && expr[at+1] == '=' {
				return expr[at : at+2], at
			}
			// A lone `=` is not a comparison, and a lone `!` is
			// negation; only `<` and `>` stand alone.
			if b == '<' || b == '>' {
				return expr[at : at+1], at
			}
		}
	}
	return "", -1
}

// splitTopLevelNullCoalesce splits on unparenthesised `??` operators.
// Returns a single element when there is no live top-level `??`.
func splitTopLevelNullCoalesce(expr string) []string {
	var arms []string
	prev := 0
	sc := &exprScanner{s: expr}
	for sc.i < len(expr) {
		at := sc.i
		b, live, top := sc.next()
		if !live || !top || b != '?' {
			continue
		}
		if at+1 >= len(expr) || expr[at+1] != '?' {
			continue
		}
		arms = append(arms, expr[prev:at])
		sc.i = at + 2 // step over the second '?'
		prev = sc.i
	}
	if len(arms) == 0 {
		return []string{expr}
	}
	return append(arms, expr[prev:])
}

// lowerNullCoalesceInCallArgs lowers `??` that sits inside a call's
// argument list, e.g. `concat("P-", w ?? "30", "D")`. The arguments are
// split at their own top level and lowered independently.
func lowerNullCoalesceInCallArgs(expr string) string {
	open := strings.IndexByte(expr, '(')
	if open < 0 || !strings.HasSuffix(strings.TrimSpace(expr), ")") {
		return expr
	}
	close := strings.LastIndexByte(expr, ')')
	if close <= open {
		return expr
	}
	inner := expr[open+1 : close]
	if !strings.Contains(inner, "??") {
		return expr
	}
	var args []string
	prev := 0
	sc := &exprScanner{s: inner}
	for sc.i < len(inner) {
		at := sc.i
		b, live, top := sc.next()
		if live && top && b == ',' {
			args = append(args, inner[prev:at])
			prev = at + 1
		}
	}
	args = append(args, inner[prev:])
	for i, a := range args {
		trimmed := strings.TrimSpace(a)
		lowered := lowerNullCoalesceExpr(trimmed)
		if lowered == trimmed {
			continue // preserve the original spacing
		}
		args[i] = strings.Replace(a, trimmed, lowered, 1)
	}
	return expr[:open+1] + strings.Join(args, ",") + expr[close:]
}
