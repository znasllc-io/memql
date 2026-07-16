// Package grammar generates the baseline TextMate grammar (memql.tmLanguage.json)
// for .memql files from the machine-readable DSL spec (component/language/dslspec)
// -- the same source of truth MemQL Sense projects from. The grammar colors a
// file the instant it opens (before the language server attaches, and in diffs /
// on GitHub); the server's semantic tokens then refine it.
//
// The grammar is GENERATED, never hand-written, so it cannot drift from the
// language: a construct/keyword/operator a future grammar epic adds flows in on
// the next regeneration, and a drift test (grammar_test.go) fails the build if
// the checked-in file falls behind. Regenerate on every GrammarVersion bump:
//
//	memql-lsp gen-grammar editors/vscode/syntaxes/memql.tmLanguage.json
package grammar

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslspec"
	"github.com/znasllc-io/memql/component/language/parser"
)

// tmGrammar is the subset of the TextMate grammar schema this generator emits.
type tmGrammar struct {
	Schema     string            `json:"$schema,omitempty"`
	Name       string            `json:"name"`
	ScopeName  string            `json:"scopeName"`
	Comment    string            `json:"comment"`
	Patterns   []tmInclude       `json:"patterns"`
	Repository map[string]tmRule `json:"repository"`
}

type tmInclude struct {
	Include string `json:"include"`
}

type tmRule struct {
	Name     string   `json:"name,omitempty"`
	Match    string   `json:"match,omitempty"`
	Begin    string   `json:"begin,omitempty"`
	End      string   `json:"end,omitempty"`
	Patterns []tmRule `json:"patterns,omitempty"`
}

// Generate returns the pretty-printed memql.tmLanguage.json bytes for the
// current dslspec + GrammarVersion. Deterministic: keyword/operator sets are
// sorted and encoding/json sorts the repository map, so identical inputs yield
// byte-identical output (what the drift test relies on).
func Generate() ([]byte, error) {
	g := build(dslspec.Build(), parser.GrammarVersion)
	out, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func build(spec *dslspec.Spec, grammarVersion string) tmGrammar {
	var constructKw, controlKw, importKw, reservedKw []string
	for _, c := range spec.Constructs {
		constructKw = append(constructKw, c.Keyword)
	}
	for _, k := range spec.Keywords {
		switch k.Kind {
		case "control", "clause":
			controlKw = append(controlKw, k.Name)
		case "import":
			importKw = append(importKw, k.Name)
		case "reserved":
			reservedKw = append(reservedKw, k.Name)
		}
	}

	var wordOps, symbolOps []string
	for _, o := range spec.Operators {
		if isWord(o.Symbol) {
			wordOps = append(wordOps, o.Symbol)
		} else {
			symbolOps = append(symbolOps, o.Symbol)
		}
	}

	repo := map[string]tmRule{
		"comments": {Patterns: []tmRule{
			{Name: "comment.line.double-slash.memql", Match: `//.*$`},
			{Name: "comment.block.memql", Begin: `/\*`, End: `\*/`},
		}},
		"strings": {
			Name:  "string.quoted.double.memql",
			Begin: `"`,
			End:   `"`,
			Patterns: []tmRule{
				{Name: "constant.character.escape.memql", Match: `\\.`},
			},
		},
		// Concept ids (v1:cognition:space) must precede numbers so the leading
		// version digits are not mis-scoped as a number.
		"concept-ids":        {Name: "entity.name.type.memql", Match: `\bv?\d+:[\w:]+`},
		"numbers":            {Name: "constant.numeric.memql", Match: `\b\d+(?:\.\d+)?\b`},
		"annotations":        {Name: "storage.modifier.memql", Match: `@\w+`},
		"construct-keywords": {Name: "storage.type.memql", Match: wordAlternation(constructKw)},
		"control-keywords":   {Name: "keyword.control.memql", Match: wordAlternation(controlKw)},
		"import-keywords":    {Name: "keyword.control.import.memql", Match: wordAlternation(importKw)},
		"reserved-words":     {Name: "variable.language.memql", Match: wordAlternation(reservedKw)},
		"operators-word":     {Name: "keyword.operator.word.memql", Match: wordAlternation(wordOps)},
		"operators-symbol":   {Name: "keyword.operator.memql", Match: symbolAlternation(symbolOps)},
	}

	patterns := []tmInclude{
		{Include: "#comments"},
		{Include: "#strings"},
		{Include: "#annotations"},
		{Include: "#concept-ids"},
		{Include: "#numbers"},
		{Include: "#construct-keywords"},
		{Include: "#control-keywords"},
		{Include: "#import-keywords"},
		{Include: "#reserved-words"},
		{Include: "#operators-word"},
		{Include: "#operators-symbol"},
	}

	return tmGrammar{
		Schema:     "https://raw.githubusercontent.com/martinring/tmlanguage/master/tmlanguage.json",
		Name:       "MemQL",
		ScopeName:  "source.memql",
		Comment:    "Generated from dslspec by 'memql-lsp gen-grammar'. Do not edit by hand. GrammarVersion: " + grammarVersion,
		Patterns:   patterns,
		Repository: repo,
	}
}

// wordAlternation builds \b(?:a|b|...)\b over the sorted, deduped, regex-escaped
// words. Empty input yields a never-matching pattern.
func wordAlternation(words []string) string {
	esc := escapedSortedSet(words)
	if len(esc) == 0 {
		return `\b(?!x)x` // never matches
	}
	return `\b(?:` + strings.Join(esc, "|") + `)\b`
}

// symbolAlternation builds (?:...|...) over regex-escaped symbols, longest first
// so a longer operator (==) wins over its prefix (=).
func symbolAlternation(symbols []string) string {
	uniq := dedupe(symbols)
	sort.Slice(uniq, func(i, j int) bool {
		if len(uniq[i]) != len(uniq[j]) {
			return len(uniq[i]) > len(uniq[j])
		}
		return uniq[i] < uniq[j]
	})
	if len(uniq) == 0 {
		return `(?!x)x`
	}
	esc := make([]string, len(uniq))
	for i, s := range uniq {
		esc[i] = regexp.QuoteMeta(s)
	}
	return `(?:` + strings.Join(esc, "|") + `)`
}

func escapedSortedSet(words []string) []string {
	uniq := dedupe(words)
	sort.Strings(uniq)
	esc := make([]string, len(uniq))
	for i, w := range uniq {
		esc[i] = regexp.QuoteMeta(w)
	}
	return esc
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

var wordRe = regexp.MustCompile(`^\w+$`)

func isWord(s string) bool { return wordRe.MatchString(s) }
