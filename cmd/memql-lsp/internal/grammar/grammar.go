// Package grammar generates the baseline TextMate grammar (memql.tmLanguage.json)
// for .memql files from the machine-readable DSL spec (component/language/dslspec)
// and the v1 function catalog (component/language/functions) -- the same sources
// of truth MemQL Sense projects from. The grammar colors a file the instant it
// opens (before the language server attaches, and in diffs / on GitHub); the
// server's semantic tokens then refine it.
//
// The grammar is GENERATED, never hand-written, so it cannot drift from the
// language: a construct/keyword/operator/function a future grammar epic adds
// flows in on the next regeneration, and a drift test (grammar_test.go) fails
// the build if the checked-in file falls behind. Regenerate on every
// GrammarVersion bump:
//
//	memql-lsp gen-grammar editors/vscode/syntaxes/memql.tmLanguage.json
//
// # Rule order is precedence
//
// TextMate tries every pattern at the current position and takes the match that
// STARTS earliest, breaking a tie by the order the patterns are listed. The v1
// forms lean on that instead of lookaround, because the emitted regexes must
// also compile under Go's RE2 (TestGrammarPatternsCompile) and RE2 has none:
//
//   - operators-symbol comes before unary-not and the ternary, so `!=` and `??`
//     win over the `!` and the `?` they start with;
//   - lambda-parameters comes before the keyword rules, so the `actor` of
//     `actor => ...` is a parameter while every other `actor` is the reserved
//     word;
//   - optional-accessor and method-calls come before accessor, so `.?` and
//     `.any(` win over the plain `.` they start with.
package grammar

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslspec"
	"github.com/znasllc-io/memql/component/language/functions"
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
	Name          string               `json:"name,omitempty"`
	Match         string               `json:"match,omitempty"`
	Begin         string               `json:"begin,omitempty"`
	End           string               `json:"end,omitempty"`
	Include       string               `json:"include,omitempty"`
	Captures      map[string]tmCapture `json:"captures,omitempty"`
	BeginCaptures map[string]tmCapture `json:"beginCaptures,omitempty"`
	EndCaptures   map[string]tmCapture `json:"endCaptures,omitempty"`
	Patterns      []tmRule             `json:"patterns,omitempty"`
}

// tmCapture names the scope of one capture group.
type tmCapture struct {
	Name string `json:"name"`
}

// The scopes of the v1 expression forms (memql#5365).
const (
	scopeArrow            = "keyword.operator.arrow.memql"
	scopeOptionalAccessor = "punctuation.accessor.optional.memql"
	scopeAccessor         = "punctuation.accessor.memql"
	scopeLogicalNot       = "keyword.operator.logical.memql"
	scopeTernary          = "keyword.operator.ternary.memql"
	scopeParameter        = "variable.parameter.memql"
	scopeFunction         = "support.function.memql"
	scopeMethod           = "entity.name.function.member.memql"
)

// dedicatedOperators are the operator symbols a rule of their own scopes, and
// so are kept out of the generic operators-symbol alternation.
var dedicatedOperators = map[string]string{
	"=>":  "arrow",
	".?":  "optional-accessor",
	".":   "accessor",
	"!":   "unary-not",
	"? :": "ternary",
}

// Generate returns the pretty-printed memql.tmLanguage.json bytes for the
// current dslspec, function catalog and GrammarVersion. Deterministic: every
// word and symbol set is sorted and encoding/json sorts the repository map, so
// identical inputs yield byte-identical output (what the drift test relies on).
func Generate() ([]byte, error) {
	g := build(dslspec.Build(), functions.Catalog(), parser.GrammarVersion)
	out, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func build(spec *dslspec.Spec, catalog []functions.Function, grammarVersion string) tmGrammar {
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
		switch {
		case isWord(o.Symbol):
			wordOps = append(wordOps, o.Symbol)
		case dedicatedOperators[o.Symbol] == "":
			symbolOps = append(symbolOps, o.Symbol)
		}
	}

	var fnNames, methodNames []string
	for _, f := range catalog {
		if f.Receiver == "" {
			fnNames = append(fnNames, f.Name)
		} else {
			methodNames = append(methodNames, f.Name)
		}
	}

	const ident = `[A-Za-z_][A-Za-z0-9_]*`
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

		// ---- The v1 expression forms (memql#5365) ----

		// A lambda's parameters: `row => ...`, `(acc, x) => ...`. The two
		// forms a v1 lambda takes; the arrow is captured with its parameter so
		// the pair scopes as one.
		"lambda-parameters": {Patterns: []tmRule{
			{
				Match: `\(\s*(` + ident + `)\s*,\s*(` + ident + `)\s*\)\s*(=>)`,
				Captures: map[string]tmCapture{
					"1": {Name: scopeParameter}, "2": {Name: scopeParameter}, "3": {Name: scopeArrow},
				},
			},
			{
				Match:    `\b(` + ident + `)\s*(=>)`,
				Captures: map[string]tmCapture{"1": {Name: scopeParameter}, "2": {Name: scopeArrow}},
			},
		}},
		// An arrow no parameter rule claimed: a longer parameter list, or the
		// terse automation header's target.
		"arrow":             {Name: scopeArrow, Match: `=>`},
		"optional-accessor": {Name: scopeOptionalAccessor, Match: `\.\?`},
		"accessor":          {Name: scopeAccessor, Match: `\.`},
		// A catalog method named after a dot, in call position: `.any(`.
		"method-calls": {
			Match:    `(\.)(` + strings.Join(escapedSortedSet(methodNames), "|") + `)\s*(\()`,
			Captures: map[string]tmCapture{"1": {Name: scopeAccessor}, "2": {Name: scopeMethod}},
		},
		// A catalog function in call position: `lower(`, `childOf(`.
		"function-calls": {
			Match:    `\b(` + strings.Join(escapedSortedSet(fnNames), "|") + `)\s*(\()`,
			Captures: map[string]tmCapture{"1": {Name: scopeFunction}},
		},
		// Unary `!`. Listed after operators-symbol, which claims `!=` first.
		"unary-not": {Name: scopeLogicalNot, Match: `!`},
		// The ternary `p ? a : b`, as a region from its `?` to its `:` so the
		// colon is told from a map key's. Listed after operators-symbol, which
		// claims `??` first; `.?` starts a character earlier and needs no help.
		// Brackets inside a branch are their own groups, so a map key's or a
		// named argument's colon inside them does not end the region.
		"ternary": {
			Begin:         `\?`,
			BeginCaptures: map[string]tmCapture{"0": {Name: scopeTernary}},
			End:           `:`,
			EndCaptures:   map[string]tmCapture{"0": {Name: scopeTernary}},
			Patterns:      []tmRule{{Include: "#ternary-groups"}, {Include: "$self"}},
		},
		"ternary-groups": {Patterns: []tmRule{
			{Begin: `\(`, End: `\)`, Patterns: []tmRule{{Include: "$self"}}},
			{Begin: `\[`, End: `\]`, Patterns: []tmRule{{Include: "$self"}}},
			{Begin: `\{`, End: `\}`, Patterns: []tmRule{{Include: "$self"}}},
		}},
	}

	patterns := []tmInclude{
		{Include: "#comments"},
		{Include: "#strings"},
		{Include: "#annotations"},
		{Include: "#concept-ids"},
		{Include: "#numbers"},
		{Include: "#lambda-parameters"},
		{Include: "#arrow"},
		{Include: "#optional-accessor"},
		{Include: "#method-calls"},
		{Include: "#accessor"},
		{Include: "#function-calls"},
		{Include: "#construct-keywords"},
		{Include: "#control-keywords"},
		{Include: "#import-keywords"},
		{Include: "#reserved-words"},
		{Include: "#operators-word"},
		{Include: "#operators-symbol"},
		{Include: "#unary-not"},
		{Include: "#ternary"},
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
