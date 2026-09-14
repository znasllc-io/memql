package parser

// v1_precedence_test.go -- the precedence table of D9 is published once, and
// three things must agree about it: V1PrecedenceTable() (the data Sense and
// dslspec read), the parser's own grouping, and the table in the language
// reference.

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestV1PrecedenceTableIsPublished: the reference's `### Operator precedence`
// table is the parser's table, row for row.
func TestV1PrecedenceTableIsPublished(t *testing.T) {
	t.Skip("the published table lands with the docs task (Task 13)")

	published := readPublishedPrecedence(t, "../../../docs/public/language/memql.md")
	want := V1PrecedenceTable()
	if len(published) != len(want) {
		t.Fatalf("the published table has %d rows, V1PrecedenceTable has %d", len(published), len(want))
	}
	for i := range want {
		got, w := published[i], want[i]
		if got.Level != w.Level {
			t.Errorf("row %d: level %d, want %d", i+1, got.Level, w.Level)
		}
		if strings.Join(got.Operators, " ") != strings.Join(w.Operators, " ") {
			t.Errorf("level %d: published operators %q, want %q", w.Level, got.Operators, w.Operators)
		}
		if got.Assoc != w.Assoc {
			t.Errorf("level %d: published associativity %q, want %q", w.Level, got.Assoc, w.Assoc)
		}
	}
}

// readPublishedPrecedence reads the first markdown table under the heading
// `### Operator precedence`: the level from the first column, the operators
// from the code spans of the second (a table cell escapes `|` as `\|`), and
// the associativity from the first word of the third.
func readPublishedPrecedence(t *testing.T, path string) []PrecedenceLevel {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "### Operator precedence" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s has no `### Operator precedence` heading", path)
	}
	var rows [][]string
	for _, l := range lines[start+1:] {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "#") {
			break
		}
		if strings.HasPrefix(trimmed, "|") {
			rows = append(rows, splitMarkdownRow(trimmed))
			continue
		}
		if len(rows) > 0 {
			break
		}
	}
	if len(rows) < 3 {
		t.Fatalf("the table under `### Operator precedence` has %d rows; want a header, a separator and the levels", len(rows))
	}
	span := regexp.MustCompile("`([^`]+)`")
	var out []PrecedenceLevel
	for _, cells := range rows[2:] {
		if len(cells) < 3 {
			t.Fatalf("a precedence row has %d cells, want 3: %q", len(cells), cells)
		}
		level, err := strconv.Atoi(cells[0])
		if err != nil {
			t.Fatalf("level cell %q is not a number", cells[0])
		}
		var ops []string
		for _, m := range span.FindAllStringSubmatch(cells[1], -1) {
			ops = append(ops, m[1])
		}
		assoc := strings.ToLower(strings.Fields(cells[2] + " ?")[0])
		out = append(out, PrecedenceLevel{Level: level, Operators: ops, Assoc: assoc})
	}
	return out
}

// splitMarkdownRow splits one table row on its unescaped pipes.
func splitMarkdownRow(row string) []string {
	const pipe = "<ESCAPED-PIPE>"
	row = strings.ReplaceAll(row, `\|`, pipe)
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	cells := strings.Split(row, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(strings.ReplaceAll(cells[i], pipe, "|"))
	}
	return cells
}

// TestV1PrecedenceTableShape: ten levels, numbered in order, each with
// operators and one of the three associativities.
func TestV1PrecedenceTableShape(t *testing.T) {
	table := V1PrecedenceTable()
	if len(table) != 10 {
		t.Fatalf("V1PrecedenceTable has %d levels, want 10", len(table))
	}
	for i, lv := range table {
		if lv.Level != i+1 {
			t.Errorf("row %d has level %d", i, lv.Level)
		}
		if len(lv.Operators) == 0 {
			t.Errorf("level %d lists no operators", lv.Level)
		}
		switch lv.Assoc {
		case "left", "right", "none":
		default:
			t.Errorf("level %d has associativity %q", lv.Level, lv.Assoc)
		}
	}
	// A caller editing its copy must not edit the parser's.
	table[0].Operators[0] = "changed"
	if V1PrecedenceTable()[0].Operators[0] == "changed" {
		t.Error("V1PrecedenceTable shares its slices with its caller")
	}
}

// v1BinaryLevels are the levels whose operators are all infix binary
// operators, so the templates below can be generated from the table.
var v1BinaryLevels = []int{3, 4, 5, 6, 7, 8}

// TestV1PrecedenceTableMatchesTheParser: for each pair of adjacent levels the
// tighter operator groups first, and each level associates the way the table
// says. The binary cases are generated from V1PrecedenceTable, so a table row
// the parser disagrees with fails here.
func TestV1PrecedenceTableMatchesTheParser(t *testing.T) {
	table := V1PrecedenceTable()
	level := func(n int) PrecedenceLevel { return table[n-1] }
	check := func(t *testing.T, src, want string) {
		t.Helper()
		n, err := ParseV1Expression(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			return
		}
		if got := fullyParen(n); got != want {
			t.Errorf("%s:\n got %s\nwant %s", src, got, want)
		}
	}

	// Adjacent binary levels, every operator pair.
	for i := 0; i+1 < len(v1BinaryLevels); i++ {
		tight, loose := level(v1BinaryLevels[i]), level(v1BinaryLevels[i+1])
		for _, tOp := range tight.Operators {
			for _, lOp := range loose.Operators {
				check(t, "a "+lOp+" b "+tOp+" c", "(a "+lOp+" (b "+tOp+" c))")
				check(t, "a "+tOp+" b "+lOp+" c", "((a "+tOp+" b) "+lOp+" c)")
			}
		}
	}

	// Associativity of the binary levels.
	for _, n := range v1BinaryLevels {
		lv := level(n)
		for _, op := range lv.Operators {
			src := "a " + op + " b " + op + " c"
			switch lv.Assoc {
			case "left":
				check(t, src, "((a "+op+" b) "+op+" c)")
			case "none":
				if _, err := ParseV1Expression(src); err == nil || !strings.Contains(err.Error(), "comparisons do not chain") {
					t.Errorf("%s: a non-associative level must refuse a chain, got %v", src, err)
				}
			default:
				t.Errorf("level %d: no template for associativity %q", n, lv.Assoc)
			}
		}
	}

	// Level 2 (unary) against level 3 and level 1 (postfix) against level 2.
	unary := level(2)
	if strings.Join(unary.Operators, " ") != "! -" || unary.Assoc != "right" {
		t.Fatalf("level 2 is %+v; the unary cases below assume `! -`, right-associative", unary)
	}
	for _, u := range unary.Operators {
		for _, op := range level(3).Operators {
			check(t, u+"a "+op+" b", "(("+u+"a) "+op+" b)")
			check(t, "a "+op+" "+u+"b", "(a "+op+" ("+u+"b))")
		}
		check(t, u+"a.b", "("+u+"(a.b))")
		check(t, u+"a.?b", "("+u+"(a.?b))")
		check(t, u+"f(x)", "("+u+"f(x))")
		check(t, u+"a.m(x)", "("+u+"(a.m(x)))")
		check(t, u+" "+u+"a", "("+u+"("+u+"a))")
	}

	// Level 8 against level 9 (the conditional) and its right associativity.
	if level(9).Assoc != "right" || level(10).Assoc != "right" {
		t.Fatalf("levels 9 and 10 must be right-associative: %+v %+v", level(9), level(10))
	}
	check(t, "a || b ? c : d", "((a || b) ? c : d)")
	check(t, "a ? b : c || d", "(a ? b : (c || d))")
	check(t, "a ? b || c : d", "(a ? (b || c) : d)")
	check(t, "a ? b : c ? d : e", "(a ? b : (c ? d : e))")
	// Level 9 against level 10 (the lambda), whose body extends as far as it
	// can, and its right associativity.
	check(t, "x => a ? b : c", "(x => (a ? b : c))")
	check(t, "x => a || b", "(x => (a || b))")
	check(t, "x => y => z", "(x => (y => z))")
}
