package grammar

// languageconfig.go generates the extension's language configuration
// (editors/vscode/language-configuration.json) from dslspec's punctuation
// table (component/language/dslspec/punctuation.go), beside the TextMate
// grammar grammar.go generates from the keyword tables (memql#5362; D25 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The file used to be written by hand while the grammar next to it was
// generated. Both describe the language to the editor, and only one of them
// could notice the language moving.
//
// WHAT COMES FROM THE TABLE: the comment tokens, the bracket pairs, the
// auto-closing and surrounding pairs, and every character the Enter and
// re-indent patterns match -- the openers, their closers, the comment guard
// and the string quote. A pair added to the table reaches all of them on the
// next regeneration (TestLanguageConfigurationPatternsFollowTheBrackets).
//
// WHAT IS WRITTEN HERE, as data: the SHAPE of those patterns and of the Enter
// rules, which is editor behaviour rather than a fact about MemQL, and which
// cmd/memql-lsp/languageconfig_test.go pins case by case (memql#2602,
// memql#2638). The output reproduces the file those tests were written
// against byte for byte, in its hand-compact layout; TestLanguageConfiguration-
// IsUpToDate holds the committed file to it.
//
// Regenerate with `make vscode-grammar`, which writes both files.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// enterRule is one onEnterRules entry.
type enterRule struct {
	// betweenPair limits the rule to an Enter whose text AFTER the cursor
	// begins with a closer: the cursor sits between an opener and its closer.
	betweenPair bool
	// indent is the action VS Code takes: "indentOutdent" puts the cursor on
	// an indented line of its own and the closer on the line below it;
	// "indent" indents the new line.
	indent string
}

// enterRules, in order. VS Code walks them top down and the FIRST rule whose
// patterns match wins (onEnter.ts), so the order is the behaviour: the
// between-a-pair rule must come first, because the rule after it carries no
// afterText and would otherwise take the adjacent-closer case too, leaving
// `args {|}` to indent without expanding.
var enterRules = []enterRule{
	{betweenPair: true, indent: "indentOutdent"},
	{betweenPair: false, indent: "indent"},
}

// quoteNotIn is where typing the string quote must NOT insert its closer:
// inside a string, where it is the closer, and inside a comment, where it is
// prose.
var quoteNotIn = []string{"string", "comment"}

// literalQuote is the character the indentation patterns treat as quoting a
// bracket, in addition to MemQL's string quote from the table. MemQL has no
// single-quoted string, but prose on a line quotes a bracket with it --
// `prefix: '['`, pinned in cmd/memql-lsp/languageconfig_test.go -- and a
// bracket quoted that way opens no block. The corpus sweep behind these
// patterns (memql#2638) scored zero false positives and zero false negatives
// across dsl/ with it in the set.
const literalQuote = "'"

// languageConfiguration is everything the rendered file says, computed from
// the punctuation table.
type languageConfiguration struct {
	lineComment  string
	blockComment dslspec.DelimiterPair
	brackets     []dslspec.DelimiterPair
	quote        string
	// blockOpener matches a line that leaves a nesting pair open: Enter after
	// it indents, and re-indent increases after it.
	blockOpener string
	// closerLead matches a line that begins with a closer: Enter before it
	// expands, and re-indent decreases on it.
	closerLead string
}

// GenerateLanguageConfiguration returns the language-configuration.json bytes
// for the current punctuation table. Deterministic: identical inputs yield
// byte-identical output, which the staleness gate relies on.
func GenerateLanguageConfiguration() ([]byte, error) {
	return renderLanguageConfiguration(buildLanguageConfiguration(dslspec.LexicalPunctuation()))
}

func buildLanguageConfiguration(p dslspec.Punctuation) languageConfiguration {
	quotes := p.StringQuote + literalQuote

	// One branch per pair: its opener, then -- to the end of the line --
	// neither ITS OWN closer nor a quote. Each pair is cancelled only by its
	// own closer (memql#2638): a single class banning every closer after the
	// last opener stopped `filter { cond(a)` from indenting.
	branches := make([]string, 0, len(p.Brackets))
	closers := make([]string, 0, len(p.Brackets))
	for _, pair := range p.Brackets {
		branches = append(branches, regexp.QuoteMeta(pair.Open)+"[^"+classEscape(pair.Close+quotes)+"]*")
		closers = append(closers, pair.Close)
	}
	anyOpener := "(?:" + strings.Join(branches, "|") + ")"

	// The comment guard: the line's first non-space character is not one a
	// comment starts with, so a commented-out opener never opens a block.
	// Anchored on the FIRST non-space character rather than banning the
	// character anywhere before the opener (memql#2602), so `n/a {` and
	// `total / count > 5 {` still indent.
	guard := classEscape(firstCharacters(p.LineComment, p.BlockComment.Open))

	return languageConfiguration{
		lineComment:  p.LineComment,
		blockComment: p.BlockComment,
		brackets:     p.Brackets,
		quote:        p.StringQuote,
		// Two alternatives. The first is a line with code before its open
		// pair. The second is a line that IS a bare opener (`{`, `  [`): the
		// first alternative's guard class would consume that opener and then
		// demand a second one.
		blockOpener: `^\s*[^` + guard + `\s].*` + anyOpener + `$|^\s*` + anyOpener + `$`,
		// Anchored: an unanchored class would outdent any line merely
		// containing a closer, balanced object literals included.
		closerLead: `^\s*[` + classEscape(strings.Join(closers, "")) + `]`,
	}
}

// firstCharacters returns the distinct first characters of the given tokens,
// in order.
func firstCharacters(tokens ...string) string {
	var out strings.Builder
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		first := tok[:1]
		if !strings.Contains(out.String(), first) {
			out.WriteString(first)
		}
	}
	return out.String()
}

// classEscape escapes the characters that are special inside a regular
// expression character class, in both the RE2 dialect the pin tests compile
// with and the JavaScript dialect VS Code evaluates.
func classEscape(chars string) string {
	var b strings.Builder
	for _, r := range chars {
		switch r {
		case '\\', ']', '^', '-':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renderLanguageConfiguration writes the file in its hand-compact layout: a
// pair on one line, a rule's action on one line. encoding/json's indenter
// would spread every pair over four lines, and the diff of the first
// regeneration would then be the whole file.
func renderLanguageConfiguration(c languageConfiguration) ([]byte, error) {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}
	q := jsonString

	line("{")
	line(`  "comments": {`)
	line(`    "lineComment": %s,`, q(c.lineComment))
	line(`    "blockComment": [%s, %s]`, q(c.blockComment.Open), q(c.blockComment.Close))
	line(`  },`)

	line(`  "brackets": [`)
	for i, pair := range c.brackets {
		line(`    [%s, %s]%s`, q(pair.Open), q(pair.Close), comma(i, len(c.brackets)))
	}
	line(`  ],`)

	notIn := make([]string, len(quoteNotIn))
	for i, scope := range quoteNotIn {
		notIn[i] = q(scope)
	}
	line(`  "autoClosingPairs": [`)
	for _, pair := range c.brackets {
		line(`    { "open": %s, "close": %s },`, q(pair.Open), q(pair.Close))
	}
	line(`    { "open": %s, "close": %s, "notIn": [%s] }`, q(c.quote), q(c.quote), strings.Join(notIn, ", "))
	line(`  ],`)

	line(`  "surroundingPairs": [`)
	for _, pair := range c.brackets {
		line(`    [%s, %s],`, q(pair.Open), q(pair.Close))
	}
	line(`    [%s, %s]`, q(c.quote), q(c.quote))
	line(`  ],`)

	line(`  "onEnterRules": [`)
	for i, rule := range enterRules {
		line(`    {`)
		line(`      "beforeText": %s,`, q(c.blockOpener))
		if rule.betweenPair {
			line(`      "afterText": %s,`, q(c.closerLead))
		}
		line(`      "action": { "indent": %s }`, q(rule.indent))
		line(`    }%s`, comma(i, len(enterRules)))
	}
	line(`  ],`)

	line(`  "indentationRules": {`)
	line(`    "increaseIndentPattern": %s,`, q(c.blockOpener))
	line(`    "decreaseIndentPattern": %s`, q(c.closerLead))
	line(`  }`)
	line("}")
	return []byte(b.String()), nil
}

// jsonString quotes s as a JSON string. HTML escaping is off: `<`, `>` and `&`
// are ordinary characters in a pattern, and < in the file would be a
// spelling nobody wrote.
func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// Encoding a Go string cannot fail; a panic here is a broken
		// encoding/json, not bad input.
		panic(fmt.Sprintf("encode %q: %v", s, err))
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// comma separates list entries: every entry but the last carries one.
func comma(i, n int) string {
	if i < n-1 {
		return ","
	}
	return ""
}
