package grammar

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// checkedInLanguageConfiguration is the language configuration the extension
// bundles, relative to this package directory.
const checkedInLanguageConfiguration = "../../../../editors/vscode/language-configuration.json"

func generatedLanguageConfiguration(t *testing.T) []byte {
	t.Helper()
	data, err := GenerateLanguageConfiguration()
	if err != nil {
		t.Fatalf("GenerateLanguageConfiguration: %v", err)
	}
	return data
}

// TestLanguageConfigurationIsUpToDate is the staleness gate: the checked-in
// language configuration must be byte-identical to what the generator
// produces from dslspec. It fails when the punctuation table moved ahead of
// the committed file -- or when the committed file was edited by hand, which
// the next regeneration would silently undo.
func TestLanguageConfigurationIsUpToDate(t *testing.T) {
	want := generatedLanguageConfiguration(t)
	got, err := os.ReadFile(checkedInLanguageConfiguration)
	if err != nil {
		t.Fatalf("read checked-in language configuration: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("editors/vscode/language-configuration.json is stale or was edited by hand; it is generated " +
			"from component/language/dslspec/punctuation.go and the rules in " +
			"cmd/memql-lsp/internal/grammar/languageconfig.go. From the repository root, regenerate it with:\n" +
			"  make vscode-grammar\n" +
			"or\n" +
			"  go run ./cmd/memql-lsp gen-language-config editors/vscode/language-configuration.json\n" +
			"A hand edit belongs in one of those two files instead; the next regeneration would undo it.")
	}
}

// TestLanguageConfigurationIsValidJSON: the hand-shaped rendering is still
// JSON, and every key the extension relies on is present.
func TestLanguageConfigurationIsValidJSON(t *testing.T) {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(generatedLanguageConfiguration(t), &cfg); err != nil {
		t.Fatalf("the generated language configuration is not JSON: %v", err)
	}
	for _, key := range []string{"comments", "brackets", "autoClosingPairs", "surroundingPairs", "onEnterRules", "indentationRules"} {
		if _, ok := cfg[key]; !ok {
			t.Errorf("the generated language configuration has no %q", key)
		}
	}
}

// TestLanguageConfigurationDerivesFromThePunctuationTable: the comment tokens,
// the bracket pairs, the auto-closing and surrounding pairs are dslspec's, not
// a second copy of them.
func TestLanguageConfigurationDerivesFromThePunctuationTable(t *testing.T) {
	var cfg struct {
		Comments struct {
			LineComment  string    `json:"lineComment"`
			BlockComment [2]string `json:"blockComment"`
		} `json:"comments"`
		Brackets         [][2]string `json:"brackets"`
		AutoClosingPairs []struct {
			Open  string   `json:"open"`
			Close string   `json:"close"`
			NotIn []string `json:"notIn"`
		} `json:"autoClosingPairs"`
		SurroundingPairs [][2]string `json:"surroundingPairs"`
	}
	if err := json.Unmarshal(generatedLanguageConfiguration(t), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := dslspec.LexicalPunctuation()

	if cfg.Comments.LineComment != p.LineComment {
		t.Errorf("lineComment = %q, want dslspec's %q", cfg.Comments.LineComment, p.LineComment)
	}
	if cfg.Comments.BlockComment != [2]string{p.BlockComment.Open, p.BlockComment.Close} {
		t.Errorf("blockComment = %q, want dslspec's %q", cfg.Comments.BlockComment, p.BlockComment)
	}

	var brackets [][2]string
	for _, b := range p.Brackets {
		brackets = append(brackets, [2]string{b.Open, b.Close})
	}
	if !equalPairs(cfg.Brackets, brackets) {
		t.Errorf("brackets = %q, want dslspec's %q", cfg.Brackets, brackets)
	}

	// Every bracket pair auto-closes and surrounds, and so does the string
	// quote -- which alone must not auto-close inside a string or a comment:
	// typing the quote that ends a string would otherwise insert a second one.
	quote := [2]string{p.StringQuote, p.StringQuote}
	if !equalPairs(cfg.SurroundingPairs, append(append([][2]string{}, brackets...), quote)) {
		t.Errorf("surroundingPairs = %q, want the brackets then the string quote", cfg.SurroundingPairs)
	}
	if len(cfg.AutoClosingPairs) != len(brackets)+1 {
		t.Fatalf("autoClosingPairs has %d entries, want %d (the brackets and the string quote)", len(cfg.AutoClosingPairs), len(brackets)+1)
	}
	for i, b := range brackets {
		got := cfg.AutoClosingPairs[i]
		if got.Open != b[0] || got.Close != b[1] || len(got.NotIn) != 0 {
			t.Errorf("autoClosingPairs[%d] = %+v, want %q with no notIn", i, got, b)
		}
	}
	last := cfg.AutoClosingPairs[len(brackets)]
	if last.Open != p.StringQuote || last.Close != p.StringQuote || strings.Join(last.NotIn, ",") != "string,comment" {
		t.Errorf("the string quote's auto-closing pair = %+v, want %q not in string or comment", last, quote)
	}
}

// TestLanguageConfigurationPatternsFollowTheBrackets: the Enter and indent
// patterns are composed from the bracket table, so a pair added to dslspec
// reaches them with no edit here. Proved with a synthetic fourth pair.
func TestLanguageConfigurationPatternsFollowTheBrackets(t *testing.T) {
	p := dslspec.LexicalPunctuation()
	p.Brackets = append(p.Brackets, dslspec.DelimiterPair{Open: "<", Close: ">"})
	cfg := buildLanguageConfiguration(p)

	if !strings.Contains(cfg.blockOpener, `<[^>"']*`) {
		t.Errorf("the opener pattern does not carry the added pair: %s", cfg.blockOpener)
	}
	if !strings.Contains(cfg.closerLead, `>`) {
		t.Errorf("the closer pattern does not carry the added pair: %s", cfg.closerLead)
	}
}

// TestGenerateLanguageConfigurationIsDeterministic: two generations agree.
func TestGenerateLanguageConfigurationIsDeterministic(t *testing.T) {
	if string(generatedLanguageConfiguration(t)) != string(generatedLanguageConfiguration(t)) {
		t.Error("GenerateLanguageConfiguration is not deterministic")
	}
}

func equalPairs(a, b [][2]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
