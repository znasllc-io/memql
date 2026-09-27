package memql

import (
	"fmt"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// prompt_level_required.go -- every prompt declares its level (epic
// memql#5127, D3; enforced by memql#5426).
//
// @level is how much intelligence a prompt's call needs -- fast, strong,
// reasoning or embeddings -- and it is what the router's rules branch on, which
// is why a prompt never names a model. The parser validates a PRESENT level
// against the closed four and leaves an absent one empty; requiring it is the
// loader's, so a prompt without one is a skip on the load report that strict
// boot refuses, with MEMQL_DSL_ALLOW_SKIPS as the operator break-glass, and
// the same rule reaches a bundle mounted at MEMQL_DSL_PATH. Before this, a
// prompt with no @level loaded, and every call to it resolved at no declared
// level -- the root CLAUDE.md and the language reference both said it was
// refused.
//
// The rule holds for a @disabled prompt too: disabled means "not loaded right
// now", not exempt from the language (ast.AttrDisabled).

// CodePromptLevelMissing is the stable rule id of a prompt with no @level.
const CodePromptLevelMissing = "prompt_level_missing"

// PromptLevelMissingError is the refusal of a prompt that declares no @level.
type PromptLevelMissingError struct {
	// Line is the line of the `prompt` keyword in its file, 0 when unknown.
	Line int
}

// Error names the position, the rule and the fix, with the rule id last in
// brackets (D24). The prompt itself is named by the load report's skip, which
// prints `prompt "<name>" (level): ` before this, so it is not repeated here.
func (e *PromptLevelMissingError) Error() string {
	at := ""
	if e.Line > 0 {
		at = fmt.Sprintf("line %d: ", e.Line)
	}
	return fmt.Sprintf("%sno @level declared: every prompt names how much intelligence its call needs -- "+
		"one of %s -- because the router's rules branch on it and a prompt never names a model; add one above the "+
		"declaration, as in @level(\"fast\") [%s]",
		at, RuleLevelNames(), CodePromptLevelMissing)
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *PromptLevelMissingError) RuleCode() string { return CodePromptLevelMissing }

// promptHeaderRe finds the `prompt` keyword that opens a declaration slice.
var promptHeaderRe = regexp.MustCompile(`(?m)^[ \t]*prompt[ \t]`)

// promptDeclarationLine is the 1-based line of the `prompt` keyword of the
// declaration slice s within content, or 0 when it cannot be found.
func promptDeclarationLine(content string, s languageParser.DeclarationSlice) int {
	if s.Start < 0 || s.Start > len(content) {
		return 0
	}
	loc := promptHeaderRe.FindStringIndex(languageParser.BlankComments(s.Source))
	if loc == nil {
		return 0
	}
	return 1 + strings.Count(content[:s.Start], "\n") + strings.Count(s.Source[:loc[0]], "\n")
}
