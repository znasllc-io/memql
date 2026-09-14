package grammar

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// checkedInGrammar is the grammar the extension bundles, relative to this
// package directory (cmd/memql-lsp/internal/grammar -> repo root).
const checkedInGrammar = "../../../../editors/vscode/syntaxes/memql.tmLanguage.json"

type parsedGrammar struct {
	Comment  string `json:"comment"`
	Patterns []struct {
		Include string `json:"include"`
	} `json:"patterns"`
	Repository map[string]parsedRule `json:"repository"`
}

type parsedRule struct {
	Name          string                 `json:"name"`
	Match         string                 `json:"match"`
	Begin         string                 `json:"begin"`
	End           string                 `json:"end"`
	Captures      map[string]parsedScope `json:"captures"`
	BeginCaptures map[string]parsedScope `json:"beginCaptures"`
	EndCaptures   map[string]parsedScope `json:"endCaptures"`
	Patterns      []parsedRule           `json:"patterns"`
}

type parsedScope struct {
	Name string `json:"name"`
}

func generated(t *testing.T) []byte {
	t.Helper()
	data, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return data
}

func parse(t *testing.T, data []byte) parsedGrammar {
	t.Helper()
	var g parsedGrammar
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("unmarshal grammar: %v", err)
	}
	return g
}

// TestGrammarIsUpToDate is the drift guard: the checked-in grammar must be
// byte-identical to what the generator currently produces. It fails the build
// if dslspec / GrammarVersion moved ahead of the committed file.
func TestGrammarIsUpToDate(t *testing.T) {
	want := generated(t)
	got, err := os.ReadFile(checkedInGrammar)
	if err != nil {
		t.Fatalf("read checked-in grammar: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("checked-in grammar is stale; regenerate with:\n  memql-lsp gen-grammar %s", checkedInGrammar)
	}
}

// TestGrammarCoversConstructKeywords: every dslspec construct keyword is colored.
func TestGrammarCoversConstructKeywords(t *testing.T) {
	g := parse(t, generated(t))
	re := regexp.MustCompile(g.Repository["construct-keywords"].Match)
	for _, c := range dslspec.Build().Constructs {
		if !re.MatchString(c.Keyword) {
			t.Errorf("construct-keywords pattern does not cover %q", c.Keyword)
		}
	}
}

// TestGrammarCoversKeywordsAndOperators: every control/reserved keyword and
// operator is covered by some rule.
func TestGrammarCoversKeywordsAndOperators(t *testing.T) {
	g := parse(t, generated(t))
	control := regexp.MustCompile(g.Repository["control-keywords"].Match)
	reserved := regexp.MustCompile(g.Repository["reserved-words"].Match)
	imports := regexp.MustCompile(g.Repository["import-keywords"].Match)
	for _, k := range dslspec.Build().Keywords {
		switch k.Kind {
		case "control", "clause":
			if !control.MatchString(k.Name) {
				t.Errorf("control-keywords missing %q", k.Name)
			}
		case "reserved":
			if !reserved.MatchString(k.Name) {
				t.Errorf("reserved-words missing %q", k.Name)
			}
		case "import":
			if !imports.MatchString(k.Name) {
				t.Errorf("import-keywords missing %q", k.Name)
			}
		}
	}
	word := regexp.MustCompile(g.Repository["operators-word"].Match)
	symbol := regexp.MustCompile(g.Repository["operators-symbol"].Match)
	for _, o := range dslspec.Build().Operators {
		switch rule := dedicatedOperators[o.Symbol]; {
		case isWord(o.Symbol):
			if !word.MatchString(o.Symbol) {
				t.Errorf("operators-word missing %q", o.Symbol)
			}
		case rule == "ternary":
			// The ternary's `?` has a rule of its own; its `:` is shared with a
			// map entry and a named argument, so no rule claims it.
			r := g.Repository[rule]
			if !regexp.MustCompile(`^(?:` + r.Match + `)$`).MatchString("?") {
				t.Errorf("the ternary rule does not match ?, got %q", r.Match)
			}
		case rule != "":
			// A symbol with a rule of its own is covered by that rule, whole.
			if !regexp.MustCompile(`^(?:` + g.Repository[rule].Match + `)$`).MatchString(o.Symbol) {
				t.Errorf("rule %q does not match its operator %q", rule, o.Symbol)
			}
		case !symbol.MatchString(o.Symbol):
			t.Errorf("operators-symbol missing %q", o.Symbol)
		}
	}
}

// TestGrammarScopesTheV1Forms pins the scope of every v1 expression form
// (memql#5365) and the rule order the scoping depends on: the grammar relies
// on "earliest match, then listed order" rather than lookaround, so the order
// IS the precedence.
func TestGrammarScopesTheV1Forms(t *testing.T) {
	g := parse(t, generated(t))
	rule := func(name string) parsedRule {
		t.Helper()
		r, ok := g.Repository[name]
		if !ok {
			t.Fatalf("the grammar has no %q rule", name)
		}
		return r
	}
	captures := func(re string, text string) []string {
		t.Helper()
		m := regexp.MustCompile(re).FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("pattern %q does not match %q", re, text)
		}
		return m
	}

	// Plain scopes.
	for name, want := range map[string]string{
		"arrow":             "keyword.operator.arrow.memql",
		"optional-accessor": "punctuation.accessor.optional.memql",
		"unary-not":         "keyword.operator.logical.memql",
		"ternary":           "keyword.operator.ternary.memql",
	} {
		if got := rule(name).Name; got != want {
			t.Errorf("rule %s scopes %q, want %q", name, got, want)
		}
	}
	if m := captures(rule("optional-accessor").Match, "row.?lineage"); m[0] != ".?" {
		t.Errorf("optional-accessor matched %q", m[0])
	}

	// The ternary's `?` is one mark, not a region: a region opened on `?`
	// would run to the next `:` anywhere below it -- a map key's, after a
	// `?` the author is still typing -- and the `:` is left unscoped because
	// a map entry and a named argument write it too.
	tern := rule("ternary")
	if tern.Begin != "" || tern.End != "" {
		t.Errorf("the ternary is a region (begin %q, end %q); it should match its `?` alone", tern.Begin, tern.End)
	}
	if m := captures(tern.Match, "p ? a : b"); m[0] != "?" {
		t.Errorf("ternary matched %q in `p ? a : b`, want the `?` alone", m[0])
	}
	for name, r := range g.Repository {
		if r.Match != "" && regexp.MustCompile(`^(?:`+r.Match+`)$`).MatchString(":") {
			t.Errorf("rule %s scopes a lone `:`, which a map entry and a named argument share with the ternary", name)
		}
	}

	// Lambda parameters, one and two of them.
	params := rule("lambda-parameters").Patterns
	if len(params) != 2 {
		t.Fatalf("lambda-parameters has %d patterns, want the two-parameter and the one-parameter form", len(params))
	}
	if m := captures(params[0].Match, "(acc, x) => acc + x"); m[1] != "acc" || m[2] != "x" {
		t.Errorf("two-parameter lambda captured %v", m)
	}
	if m := captures(params[1].Match, "row => row.status"); m[1] != "row" {
		t.Errorf("one-parameter lambda captured %v", m)
	}
	for i, p := range params {
		for group, scope := range p.Captures {
			if scope.Name != "variable.parameter.memql" && scope.Name != "keyword.operator.arrow.memql" {
				t.Errorf("lambda-parameters pattern %d group %s scopes %q", i, group, scope.Name)
			}
		}
	}

	// Every catalog function in call position, and every method after a dot.
	fnRule, methodRule := rule("function-calls"), rule("method-calls")
	if fnRule.Captures["1"].Name != "support.function.memql" || methodRule.Captures["2"].Name != "entity.name.function.member.memql" {
		t.Errorf("call scopes: function %+v, method %+v", fnRule.Captures, methodRule.Captures)
	}
	for _, f := range functions.Catalog() {
		if f.Receiver == "" {
			if m := captures(fnRule.Match, f.Name+"(x)"); m[1] != f.Name {
				t.Errorf("function-calls captured %q for %s", m[1], f.Name)
			}
		} else if m := captures(methodRule.Match, "xs."+f.Name+"()"); m[2] != f.Name {
			t.Errorf("method-calls captured %q for %s", m[2], f.Name)
		}
	}
	if regexp.MustCompile(fnRule.Match).MatchString("lowerCase(x)") {
		t.Error("function-calls must match a whole name, not a prefix of one")
	}

	// The order is the precedence.
	order := map[string]int{}
	for i, p := range g.Patterns {
		order[strings.TrimPrefix(p.Include, "#")] = i
	}
	for _, pair := range [][2]string{
		{"operators-symbol", "unary-not"},       // `!=` before `!`
		{"operators-symbol", "ternary"},         // `??` before `?`
		{"optional-accessor", "ternary"},        // `.?` before its `?`
		{"lambda-parameters", "reserved-words"}, // the `actor` of `actor =>` is a parameter
		{"lambda-parameters", "arrow"},          // a parameter takes its arrow with it
		{"optional-accessor", "accessor"},       // `.?` before `.`
		{"method-calls", "accessor"},            // `.any(` before `.`
		{"strings", "ternary"},                  // a `?` inside a string is text
		{"comments", "ternary"},                 // and so is one in a comment
	} {
		first, okA := order[pair[0]]
		second, okB := order[pair[1]]
		if !okA || !okB || first > second {
			t.Errorf("rule %s must be listed before %s", pair[0], pair[1])
		}
	}
}

func TestGrammarAnnotationAndConceptPatterns(t *testing.T) {
	g := parse(t, generated(t))
	ann := regexp.MustCompile(g.Repository["annotations"].Match)
	if !ann.MatchString("@description") || !ann.MatchString("@trigger") {
		t.Error("annotations pattern should match @description / @trigger")
	}
	concept := regexp.MustCompile(g.Repository["concept-ids"].Match)
	if !concept.MatchString("v1:cognition:space") {
		t.Error("concept-ids pattern should match v1:cognition:space")
	}
}

// TestGrammarCarriesGrammarVersion ties regeneration to GrammarVersion.
func TestGrammarCarriesGrammarVersion(t *testing.T) {
	if !strings.Contains(string(generated(t)), parser.GrammarVersion) {
		t.Errorf("grammar comment must carry GrammarVersion %q", parser.GrammarVersion)
	}
}

// TestGrammarPatternsCompile sanity-checks every emitted regex (Go's engine is a
// reasonable proxy for the subset of Oniguruma the grammar uses).
func TestGrammarPatternsCompile(t *testing.T) {
	g := parse(t, generated(t))
	for name, rule := range g.Repository {
		for _, m := range []string{rule.Match, rule.Begin, rule.End} {
			if m == "" {
				continue
			}
			if _, err := regexp.Compile(m); err != nil {
				t.Errorf("rule %q regex %q does not compile: %v", name, m, err)
			}
		}
		for _, p := range rule.Patterns {
			for _, m := range []string{p.Match, p.Begin, p.End} {
				if m == "" {
					continue
				}
				if _, err := regexp.Compile(m); err != nil {
					t.Errorf("rule %q nested regex %q does not compile: %v", name, m, err)
				}
			}
		}
	}
}

// TestGenerateIsDeterministic: two generations are byte-identical.
func TestGenerateIsDeterministic(t *testing.T) {
	a := generated(t)
	b := generated(t)
	if string(a) != string(b) {
		t.Error("Generate is not deterministic")
	}
}
