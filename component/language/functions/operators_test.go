package functions

import (
	"strings"
	"testing"
	"unicode"
)

// The operator table is what Sense's hover, dslspec's operator list and the
// generated docs all print, so every row has to be complete: a form to show,
// one or two sentences of prose, a precedence level inside the ten the record
// defines (D9), and a node kind to look admission up by.
func TestEveryOperatorIsDocumented(t *testing.T) {
	ops := Operators()
	if len(ops) == 0 {
		t.Fatal("Operators() is empty: this test examined nothing, which is not a pass")
	}
	names := map[string]bool{}
	for _, op := range ops {
		if op.Symbol == "" || op.Name == "" || op.Kind == "" || op.Form == "" {
			t.Errorf("operator %+v is missing its symbol, name, kind or form", op)
			continue
		}
		if names[op.Name] {
			t.Errorf("operator name %q is used twice", op.Name)
		}
		names[op.Name] = true
		if !identifier.MatchString(op.Name) {
			t.Errorf("operator name %q is not a lowerCamel identifier", op.Name)
		}
		if op.Level < 1 || op.Level > 10 {
			t.Errorf("%s: level %d is outside the record's ten precedence levels", op.Name, op.Level)
		}
		// The form shows the operator in use, so it must contain the symbol
		// (both halves of the ternary).
		for _, part := range strings.Fields(op.Symbol) {
			if !strings.Contains(op.Form, part) {
				t.Errorf("%s: form %q does not show the symbol %q", op.Name, op.Form, part)
			}
		}
		checkProse(t, op.Name+" doc", op.Doc, 2)
		if op.Absence != "" {
			checkProse(t, op.Name+" absence", op.Absence, 2)
		}
	}
}

// checkProse holds a Doc or an Absence rule to the table's prose rule: at most
// max sentences, a leading capital, a closing period.
func checkProse(t *testing.T, what, text string, max int) {
	t.Helper()
	if strings.TrimSpace(text) == "" {
		t.Errorf("%s is empty", what)
		return
	}
	if !strings.HasSuffix(text, ".") {
		t.Errorf("%s does not end with a period: %q", what, text)
	}
	if first := []rune(text)[0]; !unicode.IsUpper(first) && first != '`' {
		t.Errorf("%s does not start with a capital or a code span: %q", what, text)
	}
	if n := strings.Count(text, ". ") + 1; n > max {
		t.Errorf("%s has %d sentences; the rule is at most %d", what, n, max)
	}
}

// Operators() is in precedence order, tightest first, so a docs table printed
// straight from it reads like the record's.
func TestOperatorsAreInPrecedenceOrder(t *testing.T) {
	ops := Operators()
	for i := 1; i < len(ops); i++ {
		if ops[i].Level < ops[i-1].Level {
			t.Errorf("%s (level %d) is listed after %s (level %d)", ops[i].Name, ops[i].Level, ops[i-1].Name, ops[i-1].Level)
		}
	}
}

// The levels are pinned against the parser's own table by
// TestOperatorLevelsAreTheParsersPrecedence (precedence_test.go).

// The absence rules are the record's D8 table. Pinned where a row exists for
// the operator, because an absence rule that drifts from the table is exactly
// the disagreement the differential lane exists to catch -- in the prose,
// before either evaluator is written against it.
func TestOperatorAbsenceRulesFollowTheTable(t *testing.T) {
	byName := map[string]Operator{}
	for _, op := range Operators() {
		byName[op.Name] = op
	}
	for name, needles := range map[string][]string{
		// One notion of unset in == and != (D8, settled 2026-09-13): a missing
		// field, JSON null, nil and "" are one value.
		"notEqual":       {"unset", `""`, "nil"},
		"equal":          {"unset", `""`, "nil"},
		"less":           {"false"},
		"in":             {"unset", `""`, "nil"},
		"startsWith":     {"false"},
		"coalesce":       {"blank"},
		"not":            {"!="},
		"optionalMember": {"absent"},
	} {
		op, ok := byName[name]
		if !ok {
			t.Errorf("operator %s is missing", name)
			continue
		}
		for _, needle := range needles {
			if !strings.Contains(op.Absence, needle) {
				t.Errorf("%s absence rule %q should mention %q", name, op.Absence, needle)
			}
		}
	}
}

func TestParamStringIsTheSignatureSpelling(t *testing.T) {
	for p, want := range map[Param]string{
		{Name: "value", Type: TypeString}:                 "value string",
		{Name: "label", Type: TypeString, Optional: true}: "label? string",
		{Name: "parts", Type: TypeString, Variadic: true}: "parts ...string",
	} {
		if got := p.String(); got != want {
			t.Errorf("Param%+v.String() = %q, want %q", p, got, want)
		}
	}
	// Signature is built from the same spelling, so the two cannot disagree
	// about what a parameter looks like.
	f, _ := Lookup("parentOf")
	for _, p := range f.Params {
		if !strings.Contains(f.Signature(), p.String()) {
			t.Errorf("Signature %q does not contain its parameter %q", f.Signature(), p.String())
		}
	}
}
