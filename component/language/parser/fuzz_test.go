package parser

// fuzz_test.go -- native Go fuzz targets for the lexer and the edition-2026
// expression parser (epic memql#5363, D5/D23: "fuzz targets for lexer, parser
// and lowering"). A failure the fuzzer finds is kept as a seed under
// testdata/fuzz/<Target>/, so `go test` replays it on every run.
//
//	go test -run='^$' -fuzz=FuzzParseV1Expression -fuzztime=60s github.com/znasllc-io/memql/component/language/parser/
//	go test -run='^$' -fuzz=FuzzLexer -fuzztime=60s github.com/znasllc-io/memql/component/language/parser/

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// v1FuzzSeeds starts both targets from the shapes the language has: every
// canonical form, the retired spellings, and the lexical traps around `-`,
// `:`, `.` and `?`.
var v1FuzzSeeds = []string{
	`row => row.status == args.status && isActiveRecord(row)`,
	`(args.x == nil || row.f == args.x)`,
	`row.?lineage.planId`,
	`row.tags.any(t => t == "urgent")`,
	`p ? a : q ? b : c`,
	`"a" + (args.x ?? "b")`,
	`query activeUsers(status: "active")`,
	`publishEvent(topic: "x", payload: {a: [1, 2.5, -3], b: nil})`,
	`xs.reduce(0, (acc, x) => acc + x)`,
	`-5.x`,
	`a - -5`,
	`!(row.a == 1) && row.b startsWith "x"`,
	"row.a == 1\n  && row.b == 2 // comment",
	`when(args.x) { row.f == args.x }`,
	`?.status == args.status`,
	`row.a == 1; row.b == 2`,
	`row.a == 1, row.b == 2`,
	`tags has "x"`,
	`status not in ["a"]`,
	`cond(p, a, b)`,
	`concat(a, b)`,
	`coalesce(a, b)`,
	`exists(x) && len(x) > count(y)`,
	`contains(s, "sub")`,
	`$args.x == null`,
	`spec isActive`,
	`row.concept == v1:crm:lead`,
	`remaining == total-used`,
	`a < b < c`,
	`a = 1`,
	`{"a": 1}`,
	`"esc \" \\ \n \t"`,
	`x.?0 . b[0]`,
	`p?a:b`,
}

// FuzzLexer: tokenising any input returns tokens or an error; it never
// panics.
func FuzzLexer(f *testing.F) {
	for _, s := range v1FuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		_, _ = NewLexer(src).Tokenize()
	})
}

// FuzzParseV1Expression: parsing any input returns a tree or an error and
// never panics; and a tree it returns prints (FormatExpr) to text that parses
// back to the same grouping, and that text is a fixed point of parse+print.
//
// The comparison is modulo parentheses: the printer adds parentheses the
// source did not have in exactly two places (a conditional in a then-branch,
// and a lambda used as an operand), and the fixed-point check is what keeps
// that honest -- the printed text must print to itself.
func FuzzParseV1Expression(f *testing.F) {
	for _, s := range v1FuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		first, err := ParseV1Expression(src)
		if err != nil {
			return
		}
		printed := ast.FormatExpr(first)
		second, err := ParseV1Expression(printed)
		if err != nil {
			t.Fatalf("the printed form does not parse:\n source  %q\n printed %q\n error   %v", src, printed, err)
		}
		if a, b := v1Dump(first, false), v1Dump(second, false); a != b {
			t.Fatalf("the printed form parses to a different tree:\n source  %q\n printed %q\n first   %s\n second  %s", src, printed, a, b)
		}
		if again := ast.FormatExpr(second); again != printed {
			t.Fatalf("printing is not a fixed point:\n source  %q\n printed %q\n again   %q", src, printed, again)
		}
	})
}
