package grammar

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
	"github.com/znasllc-io/memql/component/language/parser"
)

// checkedInGrammar is the grammar the extension bundles, relative to this
// package directory (cmd/memql-lsp/internal/grammar -> repo root).
const checkedInGrammar = "../../../../editors/vscode/syntaxes/memql.tmLanguage.json"

type parsedGrammar struct {
	Comment    string `json:"comment"`
	Repository map[string]struct {
		Match    string `json:"match"`
		Begin    string `json:"begin"`
		End      string `json:"end"`
		Patterns []struct {
			Match string `json:"match"`
		} `json:"patterns"`
	} `json:"repository"`
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
		if isWord(o.Symbol) {
			if !word.MatchString(o.Symbol) {
				t.Errorf("operators-word missing %q", o.Symbol)
			}
		} else if !symbol.MatchString(o.Symbol) {
			t.Errorf("operators-symbol missing %q", o.Symbol)
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
			if p.Match == "" {
				continue
			}
			if _, err := regexp.Compile(p.Match); err != nil {
				t.Errorf("rule %q nested regex %q does not compile: %v", name, p.Match, err)
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
