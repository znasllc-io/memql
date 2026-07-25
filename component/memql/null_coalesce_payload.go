package memql

import (
	"strconv"
	"strings"
)

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

// malformedNullCoalesce marks a payload value whose `??` could not be
// lowered because an operand is missing (`a ??`, `a ?? ?? b`).
//
// It is a typed sentinel rather than a re-scan of the finished template
// because only EXPRESSION text reaches the lowering: a quoted literal is
// decoded by scanQuotedString and never classified, so re-scanning the
// decoded map cannot tell `note: "pass ?? for a default"` -- legitimate
// prose -- from a real typo, and would refuse the construct at boot.
type malformedNullCoalesce struct{ raw string }

// lowerNullCoalesceValue lowers one payload value expression, reporting a
// sentinel when the operator is malformed. This is the entry point the
// value classifier uses.
func lowerNullCoalesceValue(expr string) any {
	lowered, malformed := lowerNullCoalesce(expr)
	if malformed {
		return malformedNullCoalesce{raw: expr}
	}
	return lowered
}

// lowerNullCoalesceExpr is the string-only form, for callers that have
// already established the expression is well formed (and for tests).
func lowerNullCoalesceExpr(expr string) string {
	lowered, _ := lowerNullCoalesce(expr)
	return lowered
}

// lowerNullCoalesce rewrites the `??` operators in one expression into the
// equivalent coalesce() call, honouring the precedence #2611 chose:
// tighter than comparison, looser than additive, looser than the boolean
// connectives.
//
// Operators are therefore split LOOSEST-FIRST, so each recursion lowers a
// strictly tighter slice than its caller and the arms that finally reach
// the `??` split are exactly the operands the parser cascade would fold.
// `a ?? "" == "x"` becomes `coalesce(a, "") == "x"` rather than
// `coalesce(a, "" == "x")`; `a + 1 ?? 0` has the single arm `a + 1`, so it
// lowers to `coalesce(a + 1, 0)` for free.
//
// The second return reports a MISSING OPERAND -- the only condition that
// is a genuine authoring error. A well-formed `??` that this function
// cannot place (inside an object or array literal arm, whose arms the
// payload evaluator does not evaluate anyway) is left alone and is NOT
// reported: that is the pre-#2772 status quo, not a typo.
//
// Text carrying no live `??` is returned unchanged, so the function is a
// no-op on already-lowered templates and on ordinary values.
func lowerNullCoalesce(expr string) (string, bool) {
	if !strings.Contains(expr, "??") {
		return expr, false
	}
	// Lower the core and give the caller back its own padding, so the
	// splits below rejoin around their operator exactly as authored.
	core := strings.TrimSpace(expr)
	if core != expr {
		lead := expr[:strings.Index(expr, core)]
		lowered, bad := lowerNullCoalesce(core)
		return lead + lowered + expr[len(lead)+len(core):], bad
	}
	if op, at := topLevelBinary(expr, boolConnectives); at >= 0 {
		l, lbad := lowerNullCoalesce(expr[:at])
		r, rbad := lowerNullCoalesce(expr[at+len(op):])
		return l + op + r, lbad || rbad
	}
	if op, at := topLevelComparison(expr); at >= 0 {
		l, lbad := lowerNullCoalesce(expr[:at])
		r, rbad := lowerNullCoalesce(expr[at+len(op):])
		return l + op + r, lbad || rbad
	}
	arms := splitTopLevelNullCoalesce(expr)
	if len(arms) < 2 {
		// No live top-level `??` -- it sits inside a nested group.
		return lowerNullCoalesceInGroups(expr)
	}
	bad := false
	for i, arm := range arms {
		trimmed := strings.TrimSpace(arm)
		if trimmed == "" {
			// A dangling or doubled operator. Leave the text ALONE so
			// nothing downstream silently reads a coalesce() with an
			// empty argument, and report it.
			return expr, true
		}
		lowered, armBad := lowerNullCoalesce(trimmed)
		bad = bad || armBad
		arms[i] = lowered
	}
	if bad {
		return expr, true
	}
	return "coalesce(" + strings.Join(arms, ", ") + ")", false
}

// boolConnectives bind LOOSER than `??` in the parser cascade
// (parseLogicalOr -> parseLogicalAnd -> ... -> parseNullCoalesce). `;` is
// the retired separator spelling of AND, still accepted by parseLogicalAnd.
var boolConnectives = []string{"&&", "||", ";"}

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

// topLevelBinary returns the first unparenthesised occurrence of any of
// the given operators, or ("", -1).
func topLevelBinary(expr string, ops []string) (string, int) {
	sc := &exprScanner{s: expr}
	for sc.i < len(expr) {
		at := sc.i
		_, live, top := sc.next()
		if !live || !top {
			continue
		}
		for _, op := range ops {
			if strings.HasPrefix(expr[at:], op) {
				return op, at
			}
		}
	}
	return "", -1
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

// lowerNullCoalesceInGroups lowers `??` nested inside parenthesised
// groups, e.g. `concat("P-", w ?? "30", "D")` and `f(a ?? 1) + g(2)`.
// Each group's comma-separated members are lowered independently.
//
// Object and array literal groups are deliberately NOT descended into:
// their members are `key: value` pairs (splitting those on `??` would
// straddle the colon) and the payload evaluator does not evaluate a
// literal arm anyway, so leaving them is the pre-existing behaviour.
func lowerNullCoalesceInGroups(expr string) (string, bool) {
	var out strings.Builder
	bad := false
	last := 0
	depth := 0
	inStr := false
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '(':
			if depth > 0 {
				depth++
				continue
			}
			closeAt := matchingCloseParen(expr, i)
			if closeAt < 0 {
				// Unbalanced -- leave the remainder untouched.
				out.WriteString(expr[last:])
				return out.String(), bad
			}
			lowered, memberBad := lowerGroupMembers(expr[i+1 : closeAt])
			bad = bad || memberBad
			out.WriteString(expr[last : i+1])
			out.WriteString(lowered)
			out.WriteString(")")
			last = closeAt + 1
			i = closeAt
		}
	}
	if last == 0 {
		return expr, bad
	}
	out.WriteString(expr[last:])
	return out.String(), bad
}

// lowerGroupMembers lowers each top-level comma-separated member of a
// parenthesised group, preserving the authored spacing.
func lowerGroupMembers(inner string) (string, bool) {
	var members []string
	prev := 0
	bad := false
	sc := &exprScanner{s: inner}
	for sc.i < len(inner) {
		at := sc.i
		b, live, top := sc.next()
		if live && top && b == ',' {
			members = append(members, inner[prev:at])
			prev = at + 1
		}
	}
	members = append(members, inner[prev:])
	for i, m := range members {
		trimmed := strings.TrimSpace(m)
		if trimmed == "" {
			continue
		}
		lowered, memberBad := lowerNullCoalesce(trimmed)
		bad = bad || memberBad
		if lowered == trimmed {
			continue // preserve the original spacing
		}
		members[i] = strings.Replace(m, trimmed, lowered, 1)
	}
	return strings.Join(members, ","), bad
}

// matchingCloseParen returns the offset of the ')' closing the '(' at
// open, ignoring parens inside string literals, or -1 when unbalanced.
func matchingCloseParen(expr string, open int) int {
	sc := &exprScanner{s: expr, i: open}
	depth := 0
	for sc.i < len(expr) {
		at := sc.i
		b, live, _ := sc.next()
		if !live {
			continue
		}
		switch b {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return at
			}
		}
	}
	return -1
}

// firstMalformedNullCoalesce finds a payload field carrying the
// malformed-operator sentinel, walking nested objects and arrays.
func firstMalformedNullCoalesce(v any) (string, string, bool) {
	switch t := v.(type) {
	case malformedNullCoalesce:
		return "", t.raw, true
	case map[string]any:
		for k, nested := range t {
			if path, raw, bad := firstMalformedNullCoalesce(nested); bad {
				if path == "" {
					return k, raw, true
				}
				return k + "." + path, raw, true
			}
		}
	case []any:
		for i, nested := range t {
			if path, raw, bad := firstMalformedNullCoalesce(nested); bad {
				idx := "[" + strconv.Itoa(i) + "]"
				if path == "" {
					return idx, raw, true
				}
				return idx + "." + path, raw, true
			}
		}
	}
	return "", "", false
}
