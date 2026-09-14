package dslgate

// retired_operators.go is the #977 lock-in, moved to load time by memql#3629:
// filter clauses use one operator grammar, and every other spelling is refused
// with its replacement.
//
// Edition 2026 made that grammar the v1 expression parser's (epic memql#5363):
// a filter is a lambda, `filter row => ...`, and the parser refuses every
// retired spelling inside it -- `;` and `,` as connectives, `has`, `not in`,
// `when(...) { }`, `?.` -- with a RetiredFormError naming the rule and the
// replacement. The parser, not a second list of spellings here, is the
// authority on what a clause may say, which is what lets `(a, b) => ...` and a
// ternary `?` through where a text check would read a comma and a `?.`.
//
// The `,` case is the one that was an authorization bypass rather than a style
// drift, which is why this gate belongs at load time: the engine read `,` as a
// pure alias for `||`, so an ownership conjunct written with it became a
// DISJUNCT and any row matching the other side was returned (memql#3612).

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslclause"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// scanRetiredOperators reports every filter clause that spells a retired form.
//
// THE WHOLE CLAUSE IS READ, continuation lines included: memql#4123 made the
// normaliser fold continuation lines, and a gate that read the line opening
// the clause let a retired `,` through one line break away -- the memql#3612
// bypass with a newline in it. Comments are blanked by the lexer's own
// blanker, so a `//` inside a URL literal does not truncate the clause.
//
// A lambda clause goes to the v1 parser (v1RetiredFormViolation), and a
// finding is the parser's refusal, attributed to the line it names. A clause
// with no lambda header is itself the retired `filter <predicate>` form.
func scanRetiredOperators(path, src string) []Violation {
	var out []Violation
	lines := strings.Split(BlankComments(src), "\n")
	for i := 0; i < len(lines); i++ {
		trim := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trim, "filter ") && !strings.HasPrefix(trim, "filter\t") {
			continue
		}
		parts, last := filterClauseLines(lines, i)
		line := i + 1
		i = last
		multiline := strings.Join(parts, "\n")
		if dslclause.OpensLambda(multiline) {
			if v, ok := v1RetiredFormViolation(path, line, multiline); ok {
				out = append(out, v)
			}
			continue
		}
		if f, ok := retiredFilterForm(); ok {
			out = append(out, Violation{
				Gate:   GateRetiredOperator,
				File:   path,
				Line:   line,
				Detail: fmt.Sprintf("retired filter form (%s): %s is retired in edition 2026 -- write %s: filter  %s", f.Rule, f.Spelling, f.Replacement, joinClauseLines(parts)),
			})
		}
	}
	return out
}

// retiredFilterForm is the parser's table entry for a filter with no lambda
// header.
func retiredFilterForm() (languageParser.RetiredForm, bool) {
	for _, f := range languageParser.V1RetiredForms() {
		if f.Rule == "retired_filter_without_lambda" {
			return f, true
		}
	}
	return languageParser.RetiredForm{}, false
}
