package dslgate

// retired_operators.go is the #977 lock-in, moved to load time by memql#3629:
// filter clauses must use the single unified operator grammar.
//
//	`;` AND separator   -> use `&&`
//	`,` OR separator    -> use `||`
//	`has` membership    -> use `in`
//	`?.` optional-chain -> use `when(args.x) { ... }`
//
// The `,` case is the one that is an authorization bypass rather than a style
// drift, which is why this gate belongs at load time: the engine reads `,` as a
// pure alias for `||`, so an ownership conjunct written with it becomes a
// DISJUNCT and any row matching the other side is returned (memql#3612).

import (
	"fmt"
	"regexp"
	"strings"
)

var hasOperatorRe = regexp.MustCompile(`\bhas\b`)

// HasTopLevelComma reports whether s contains the retired `,`-as-OR separator:
// a comma outside string literals that is not separating list elements or call
// arguments.
//
// It used to report a comma only at depth 0, which let the retired OR
// separator through inside parentheses -- and parentheses are exactly where an
// author reaches for it (memql#3612):
//
//	filter (ownerUserId==actor.userId, visibility=="public")
//
// That is an AUTHORIZATION BYPASS, and it escaped the per-row classifier too:
// clauseGuaranteesAt split on '|' and '&' only, so with no ',' case it fell
// through to a leaf check on the whole joined text, found the `actor.userId`
// substring, and reported the clause owner-scoped.
//
// Depth alone cannot tell the two uses of ',' apart, so this tracks WHY it is
// nested:
//
//   - inside `[ ... ]` -- a list literal for `in`. Its commas are element
//     separators. All three nested commas in the corpus today are this.
//   - inside CALL parens (an identifier immediately precedes the '(') --
//     argument separators.
//   - inside GROUPING parens -- nothing else it can be but the retired OR.
//
// The embedded corpus is clean under this rule; what it protects is future
// authoring and -- since this moved to load time (memql#3629) -- product DSL
// bundles, which no test-time gate reached at all.
func HasTopLevelComma(s string) bool {
	// One entry per open bracket: true when commas inside it are separators
	// (a list literal, or a call's argument list) rather than the retired OR.
	var separatorCtx []bool
	inStr := false
	prevSignificant := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '[' || c == '{':
			separatorCtx = append(separatorCtx, true)
		case c == '(':
			// A call has an identifier (or a closing bracket, for a chained
			// call) right before its '('; a grouping paren does not.
			isCall := prevSignificant == '_' ||
				(prevSignificant >= 'a' && prevSignificant <= 'z') ||
				(prevSignificant >= 'A' && prevSignificant <= 'Z') ||
				(prevSignificant >= '0' && prevSignificant <= '9')
			separatorCtx = append(separatorCtx, isCall)
		case c == ')' || c == ']' || c == '}':
			if len(separatorCtx) > 0 {
				separatorCtx = separatorCtx[:len(separatorCtx)-1]
			}
		case c == ',':
			// Innermost enclosing bracket decides. Unenclosed is the
			// long-standing depth-0 case.
			if len(separatorCtx) == 0 || !separatorCtx[len(separatorCtx)-1] {
				return true
			}
		}
		if !inStr && c != ' ' && c != '\t' {
			prevSignificant = c
		}
	}
	return false
}

// scanRetiredOperators reports every filter clause naming a retired operator.
func scanRetiredOperators(path, src string) []Violation {
	var out []Violation
	for i, line := range strings.Split(src, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "filter ") && !strings.HasPrefix(trim, "filter\t") {
			continue
		}
		clause := strings.TrimSpace(strings.TrimPrefix(trim, "filter"))
		add := func(msg string) {
			out = append(out, Violation{
				Gate:   GateRetiredOperator,
				File:   path,
				Line:   i + 1,
				Detail: fmt.Sprintf("retired filter operator (%s): %s", msg, trim),
			})
		}
		if strings.Contains(clause, "?.") {
			add("`?.` is retired -- use `when(args.x) { ... }`")
		}
		if hasOperatorRe.MatchString(clause) {
			add("`has` is retired -- use `<scalar> in <collection>`")
		}
		if strings.Contains(clause, ";") {
			add("`;` AND separator is retired -- use `&&`")
		}
		if HasTopLevelComma(clause) {
			add("`,` OR separator is retired -- use `||`. The engine reads `,` as a pure alias for `||`, " +
				"so an ownership conjunct written with it becomes a disjunct and returns rows the caller does not own (memql#3612)")
		}
	}
	return out
}
