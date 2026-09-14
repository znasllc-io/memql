package functions_test

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The operator table's levels are the parser's precedence table read a second
// time. Sense's hover and the generated docs print Operator.Level; the parser
// binds by parser.V1PrecedenceTable, which the language reference publishes.
// So every operator row must sit at the level the parser gives its spelling,
// and every operator the parser ranks must have a row.
//
// It is an external test package so the pin survives the parser coming to
// read this catalog: an internal test importing the parser would then be an
// import cycle.
func TestOperatorLevelsAreTheParsersPrecedence(t *testing.T) {
	table := parser.V1PrecedenceTable()
	if len(table) == 0 {
		t.Fatal("parser.V1PrecedenceTable() is empty: this test examined nothing, which is not a pass")
	}
	// rows[spelling] lists the levels a spelling appears at: `-` is at two,
	// once prefix (negate) and once infix (subtract).
	type row struct {
		level int
		assoc string
	}
	rows := map[string][]row{}
	for _, lv := range table {
		for _, op := range lv.Operators {
			rows[op] = append(rows[op], row{lv.Level, lv.Assoc})
		}
	}

	covered := map[string]map[int]bool{}
	for _, op := range functions.Operators() {
		spelling := tableSpelling(op)
		candidates := rows[spelling]
		if len(candidates) == 0 {
			t.Errorf("operator %s (%q) is not in the parser's precedence table under %q", op.Name, op.Symbol, spelling)
			continue
		}
		// A spelling ranked twice is a prefix operator at the right-associative
		// (unary) level and an infix one elsewhere.
		prefix := op.Kind == "negate" || op.Kind == "not"
		level := candidates[0].level
		if len(candidates) > 1 {
			for _, c := range candidates {
				if (c.assoc == "right") == prefix {
					level = c.level
				}
			}
		}
		if op.Level != level {
			t.Errorf("operator %s (%q): level %d, but the parser ranks it at level %d", op.Name, op.Symbol, op.Level, level)
		}
		if covered[spelling] == nil {
			covered[spelling] = map[int]bool{}
		}
		covered[spelling][level] = true
	}

	// The call forms are ranked by the parser and are not operators: a
	// function call, a method call and the two-parameter lambda (the one-
	// parameter form carries the lambda's row).
	notOperators := map[string]bool{"f(...)": true, ".m(...)": true, "(x, y) => body": true}
	for spelling, rs := range rows {
		if notOperators[spelling] {
			continue
		}
		for _, r := range rs {
			if !covered[spelling][r.level] {
				t.Errorf("the parser ranks %q at level %d and functions.Operators() has no row for it", spelling, r.level)
			}
		}
	}
}

// tableSpelling is how the parser's precedence table writes an operator: the
// member forms with a placeholder field, the ternary and the lambda in use,
// every other operator as its symbol.
func tableSpelling(op functions.Operator) string {
	switch op.Name {
	case "member":
		return ".f"
	case "optionalMember":
		return ".?f"
	case "ternary":
		return "c ? a : b"
	case "lambda":
		return "x => body"
	}
	return op.Symbol
}
