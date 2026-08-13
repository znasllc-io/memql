package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

var hasOperatorRe = regexp.MustCompile(`\bhas\b`)

// hasTopLevelComma reports whether s contains the retired `,`-as-OR separator:
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
// `clauseGuaranteesAt` splits on '|' and '&' only, so with no ',' case it fell
// through to a leaf check on the whole joined text, found the `actor.userId`
// substring, and reported the clause owner-scoped. The engine meanwhile
// computes `,` as a pure alias for `||` -- the owner conjunct becomes a
// DISJUNCT and any row matching the other side is returned.
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
// The corpus is clean under this rule; what it protects is future authoring,
// and product DSL bundles, which no test-time gate reaches at all (memql#3629).
func hasTopLevelComma(s string) bool {
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

// TestNoRetiredOperatorForms is the #977 lock-in: filter clauses must use the
// single unified operator grammar. The retired forms are rejected tree-wide:
//   - `;` AND separator   -> use `&&`
//   - `,` OR separator     -> use `||`
//   - `has` membership     -> use `in`
//   - `?.` optional-chain  -> use `when(args.x) { ... }`
func TestNoRetiredOperatorForms(t *testing.T) {
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			trim := strings.TrimSpace(line)
			if !strings.HasPrefix(trim, "filter ") && !strings.HasPrefix(trim, "filter\t") {
				continue
			}
			clause := strings.TrimSpace(strings.TrimPrefix(trim, "filter"))
			ref := func(msg string) {
				t.Errorf("%s:%d retired filter operator (%s):\n  %s", p, i+1, msg, trim)
			}
			if strings.Contains(clause, "?.") {
				ref("`?.` is retired -- use `when(args.x) { ... }`")
			}
			if hasOperatorRe.MatchString(clause) {
				ref("`has` is retired -- use `<scalar> in <collection>`")
			}
			if strings.Contains(clause, ";") {
				ref("`;` AND separator is retired -- use `&&`")
			}
			if hasTopLevelComma(clause) {
				ref("`,` OR separator is retired -- use `||`")
			}
		}
	}
}

// TestRetiredCommaIsFoundAtEveryDepth is the memql#3612 lock.
//
// `hasTopLevelComma` reported only depth 0, so the retired OR separator passed
// unnoticed inside parentheses -- which is exactly where an author reaches for
// it. The corpus was clean, so nothing failed; what was broken was the gate.
//
// The cases below are the whole distinction the detector has to make, because
// depth alone cannot make it: a comma inside `[ ... ]` or inside a CALL's
// parens is a separator, and a comma inside GROUPING parens is the retired OR.
func TestRetiredCommaIsFoundAtEveryDepth(t *testing.T) {
	retired := []string{
		`ownerUserId==actor.userId, title==args.v`,          // depth 0, always caught
		`(ownerUserId==actor.userId, visibility=="public")`, // the authz bypass
		`a && (b, c)`,
		`((x, y))`,
	}
	for _, s := range retired {
		if !hasTopLevelComma(s) {
			t.Errorf("hasTopLevelComma(%q) = false; the retired ',' OR separator must be "+
				"found at any depth -- inside parens it is an authorization bypass, because "+
				"the engine reads it as || and the per-row classifier read it as a conjunction", s)
		}
	}

	separators := []string{
		`isActiveRecord && name in ["MEMQL_A", "MEMQL_B"]`, // list literal
		`status in ["superseded", "failed"]`,               // list literal
		`concat(a, b) == "x"`,                              // call arguments
		`name in ["a,b", "c"]`,                             // comma inside a string
		`ownerUserId==actor.userId && title==args.v`,       // no comma at all
	}
	for _, s := range separators {
		if hasTopLevelComma(s) {
			t.Errorf("hasTopLevelComma(%q) = true; a comma separating list elements or call "+
				"arguments is not the retired operator, and flagging it would refuse the "+
				"three `in [...]` filters the tree ships today", s)
		}
	}
}
