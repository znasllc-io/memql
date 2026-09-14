package parser

// v1_refusals.go -- the spellings edition 2026 retires from expressions, and
// the errors that refuse them (epic memql#5363; D9, D10 and D24 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A retired form is refused, never accepted with a warning: there is no
// compatibility shim, and the tree is migrated in the same change that retires
// the form. What a refusal owes its reader is the way out, so every message
// has one shape, pinned by the conformance corpus:
//
//	<spelling> is retired in edition 2026: write <replacement> (memqlmigrate --rewrite=expressions rewrites it)
//
// and every refusal carries its Rule, a stable code the corpus and Sense key
// on rather than on the wording. The table is data (V1RetiredForms) so the
// language server can offer the replacement and the docs can list the forms
// without restating them.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// v1Migrator is the codemod every retired-form refusal names.
const v1Migrator = "memqlmigrate --rewrite=expressions"

// RetiredForm is one spelling edition 2026 refuses in an expression.
type RetiredForm struct {
	// Spelling is the retired form as an author wrote it, with placeholders
	// (`x`, `<name>`) for the parts that vary.
	Spelling string
	// Replacement is the edition-2026 form that says the same thing.
	Replacement string
	// Rule is the stable refusal code. Messages may be reworded; a rule id,
	// once written into the corpus, is not.
	Rule string
}

// The rule ids, one per retired form. Named constants so a misspelled id is a
// compile error rather than a refusal that silently names no table entry.
const (
	ruleWhenGuard           = "retired_when_guard"
	ruleConditionalPrefix   = "retired_conditional_prefix"
	ruleSemicolonConnective = "retired_semicolon_connective"
	ruleCommaConnective     = "retired_comma_connective"
	ruleHas                 = "retired_has"
	ruleNotIn               = "retired_not_in"
	ruleAndCall             = "retired_and_call"
	ruleOrCall              = "retired_or_call"
	ruleNotCall             = "retired_not_call"
	ruleLtCall              = "retired_lt_call"
	ruleGtCall              = "retired_gt_call"
	ruleLteCall             = "retired_lte_call"
	ruleGteCall             = "retired_gte_call"
	ruleCondCall            = "retired_cond_call"
	ruleConcatCall          = "retired_concat_call"
	ruleCoalesceCall        = "retired_coalesce_call"
	ruleExistsCall          = "retired_exists_call"
	ruleLenCall             = "retired_len_call"
	ruleCountCall           = "retired_count_call"
	ruleContainsCall        = "retired_contains_call"
	ruleMeanCall            = "retired_mean_call"
	ruleFirstCall           = "retired_first_call"
	ruleLastCall            = "retired_last_call"
	ruleTimestampCall       = "retired_timestamp_call"
	ruleNowCall             = "retired_now_call"
	ruleNull                = "retired_null"
	ruleDollarArgs          = "retired_dollar_args"
	ruleSpecReference       = "retired_spec_reference"
	ruleTraitReference      = "retired_trait_reference"
	ruleContainsMethod      = "retired_contains_method"
)

// v1RetiredFormTable is the one list. Order is the order the docs print.
var v1RetiredFormTable = []RetiredForm{
	// The optional-argument guards. Both were a syntactic drop of the guarded
	// predicate when the argument is absent; the v1 spelling says the same
	// thing as an ordinary predicate, and the lowering recognises the pattern.
	{Spelling: "when(args.x) { ... }", Replacement: "(args.x == nil || <predicate>)", Rule: ruleWhenGuard},
	{Spelling: "?.<predicate>", Replacement: "(args.x == nil || <predicate>)", Rule: ruleConditionalPrefix},

	// The connectives. One spelling each: symbols for the two common ones.
	{Spelling: "; as a connective", Replacement: "&&", Rule: ruleSemicolonConnective},
	{Spelling: ", as a connective", Replacement: "||", Rule: ruleCommaConnective},
	{Spelling: "has", Replacement: "<value> in <list>", Rule: ruleHas},
	{Spelling: "not in", Replacement: "!(<value> in <list>)", Rule: ruleNotIn},
	{Spelling: "and(...)", Replacement: "&&", Rule: ruleAndCall},
	{Spelling: "or(...)", Replacement: "||", Rule: ruleOrCall},
	{Spelling: "not(...)", Replacement: "!", Rule: ruleNotCall},
	{Spelling: "lt(a, b)", Replacement: "a < b", Rule: ruleLtCall},
	{Spelling: "gt(a, b)", Replacement: "a > b", Rule: ruleGtCall},
	{Spelling: "lte(a, b)", Replacement: "a <= b", Rule: ruleLteCall},
	{Spelling: "gte(a, b)", Replacement: "a >= b", Rule: ruleGteCall},

	// The functions an operator or a method replaces (D10: one spelling each).
	{Spelling: "cond(p, a, b)", Replacement: "p ? a : b", Rule: ruleCondCall},
	{Spelling: "concat(a, b)", Replacement: "a + b", Rule: ruleConcatCall},
	{Spelling: "coalesce(a, b)", Replacement: "a ?? b", Rule: ruleCoalesceCall},
	{Spelling: "exists(x)", Replacement: "x != nil", Rule: ruleExistsCall},
	{Spelling: "len(x)", Replacement: "x.count()", Rule: ruleLenCall},
	{Spelling: "count(x)", Replacement: "x.count()", Rule: ruleCountCall},
	{Spelling: "contains(s, sub)", Replacement: "s.includes(sub)", Rule: ruleContainsCall},
	{Spelling: "mean(x)", Replacement: "x.avg()", Rule: ruleMeanCall},
	{Spelling: "first(x)", Replacement: "x.first()", Rule: ruleFirstCall},
	{Spelling: "last(x)", Replacement: "x.last()", Rule: ruleLastCall},
	{Spelling: "timestamp()", Replacement: "now", Rule: ruleTimestampCall},
	{Spelling: "now()", Replacement: "now", Rule: ruleNowCall},

	// The values and references with a second spelling.
	{Spelling: "null", Replacement: "nil", Rule: ruleNull},
	{Spelling: "$args.x", Replacement: "args.x", Rule: ruleDollarArgs},
	{Spelling: "spec <name>", Replacement: "<name>(row)", Rule: ruleSpecReference},
	{Spelling: "trait <name>", Replacement: "<name>(row)", Rule: ruleTraitReference},
	// The parser cannot know the receiver's type, so the method's refusal
	// names both of the forms it used to stand for.
	{Spelling: ".contains(...)", Replacement: "v in <list> for membership or s.includes(sub) for a substring", Rule: ruleContainsMethod},
}

// V1RetiredForms returns every spelling the edition-2026 expression parser
// refuses, in the order the docs list them. The slice is the caller's.
func V1RetiredForms() []RetiredForm {
	return append([]RetiredForm(nil), v1RetiredFormTable...)
}

// v1RetiredCalls maps a retired bare-call name, LOWERCASED, to its rule. The
// legacy dispatch matched builtin names case-insensitively, so `Coalesce(a, b)`
// was the same call as `coalesce(a, b)` and is refused the same way.
//
// `contains` is absent on purpose: it is retired only as the two-argument
// substring test, and the graph traversal keeps the name
// (`contains(<lambda>)`, `contains("label", <lambda>)`), so the call is
// judged after its arguments are parsed (parseV1FunctionCall).
var v1RetiredCalls = map[string]string{
	"and":       ruleAndCall,
	"or":        ruleOrCall,
	"lt":        ruleLtCall,
	"gt":        ruleGtCall,
	"lte":       ruleLteCall,
	"gte":       ruleGteCall,
	"cond":      ruleCondCall,
	"concat":    ruleConcatCall,
	"coalesce":  ruleCoalesceCall,
	"exists":    ruleExistsCall,
	"len":       ruleLenCall,
	"count":     ruleCountCall,
	"mean":      ruleMeanCall,
	"first":     ruleFirstCall,
	"last":      ruleLastCall,
	"timestamp": ruleTimestampCall,
	"now":       ruleNowCall,
}

// RetiredFormError is the refusal of a retired spelling. It unwraps to the
// positioned *ParseError, so every caller that reads a parse error's line and
// column reads this one's too; Form carries the rule and the replacement for
// the callers that act on them (Sense's quick fix, the corpus runner).
type RetiredFormError struct {
	Form  RetiredForm
	Parse *ParseError
}

func (e *RetiredFormError) Error() string { return e.Parse.Error() }

// Unwrap returns the positioned parse error, which itself unwraps to
// ErrInvalidSyntax.
func (e *RetiredFormError) Unwrap() error { return e.Parse }

// v1Retired refuses the retired form named by rule, positioned at tok.
func v1Retired(tok Token, rule string) error {
	form, ok := v1FormByRule(rule)
	if !ok {
		// Unreachable while every rule constant has a table row, which
		// TestV1EveryRetiredFormHasASample holds. Refuse rather than panic:
		// a parser that crashes on an input is worse than a vague message.
		form = RetiredForm{Spelling: rule, Replacement: "the edition-2026 form", Rule: rule}
	}
	msg := fmt.Sprintf("%s is retired in edition 2026: write %s (%s rewrites it)", form.Spelling, form.Replacement, v1Migrator)
	return &RetiredFormError{Form: form, Parse: v1ParseErrorAt(tok, msg)}
}

func v1FormByRule(rule string) (RetiredForm, bool) {
	for _, f := range v1RetiredFormTable {
		if f.Rule == rule {
			return f, true
		}
	}
	return RetiredForm{}, false
}

// v1ParseErrorAt positions msg at tok. Token is deliberately left nil:
// ParseError.Error appends a `(got "...")` suffix when it is set, and every v1
// message already says what it found, so the suffix would only repeat it.
func v1ParseErrorAt(tok Token, msg string) *ParseError {
	return &ParseError{Message: msg, Pos: tok.Pos, Line: tok.Line, Column: tok.Column}
}

// v1Errorf is v1ParseErrorAt with a format.
func v1Errorf(tok Token, format string, args ...any) error {
	return v1ParseErrorAt(tok, fmt.Sprintf(format, args...))
}

// v1Describe names a token the way a message quotes it.
func v1Describe(tok Token) string {
	switch tok.Type {
	case TokenEOF:
		return "end of input"
	case TokenString:
		return "`" + ast.QuoteString(tok.Literal) + "`"
	}
	return "`" + tok.Literal + "`"
}

// v1RefuseMistake refuses the token at the cursor when it can never continue
// an expression in ANY construct: a retired connective, a misspelled operator,
// an optional-access or member spelling with the wrong glyphs, a number the
// lexer glued its '-' onto. nil means the token is an ordinary place to stop,
// which the caller judges.
//
// Mid-stream callers get this too. Stopping at `has` and letting the enclosing
// statement parser report "unexpected has" would name the symptom; refusing
// here names the fix, at the token that needs it.
func (p *Parser) v1RefuseMistake() error {
	tok := p.current
	switch tok.Type {
	case TokenSemicolon:
		switch p.peekAhead(1).Type {
		case TokenEOF, TokenBraceClose, TokenParenClose, TokenBracketClose:
			// The legacy internal query form tolerated a trailing `;`, so a
			// C habit is more likely here than a connective.
			return v1Errorf(tok, "a trailing `;` is not part of an expression: remove it")
		}
		return v1Retired(tok, ruleSemicolonConnective)
	case TokenKeywordHas:
		return v1Retired(tok, ruleHas)
	case TokenKeywordNot:
		switch p.peekAhead(1).Type {
		case TokenKeywordIn:
			return v1Retired(tok, ruleNotIn)
		case TokenKeywordStartsWith:
			return v1Errorf(tok, "`not startsWith` is not a form: negate the comparison instead, as in !(x startsWith p)")
		}
		return v1Errorf(tok, "`not` is not an operator: negation is written `!`, as in !(a == b)")
	case TokenQuestionDot:
		return v1Errorf(tok, "optional member access is written `.?` after the object, as in x.?field")
	case TokenDot:
		return v1Errorf(tok, "a member is written with no space around `.`, as in x.field")
	case TokenBracketOpen:
		return v1Errorf(tok, "indexing is not an operator: read an element with a member path, as in args.items.0")
	case TokenNumber:
		if mag, ok := strings.CutPrefix(tok.Literal, "-"); ok {
			// The lexer reads a '-' glued to a digit as the number's sign
			// (it cannot know position), so `a -5` is two operands.
			return v1Errorf(tok, "`%s` directly after an operand reads as a second number: write `- %s` (a space after `-`) for subtraction", tok.Literal, mag)
		}
	case TokenOperator:
		switch tok.Literal {
		case "=":
			return v1Errorf(tok, "`=` is not a comparison: write `==`")
		case "=>":
			return v1Errorf(tok, "`=>` follows a lambda's parameters, which are simple names: x => body, or (x, y) => body")
		case "$":
			return v1Retired(tok, ruleDollarArgs)
		}
	}
	return nil
}

// v1Expected refuses the token at the cursor where `what` was required,
// naming a known mistake when the token is one.
func (p *Parser) v1Expected(what string) error {
	if err := p.v1RefuseMistake(); err != nil {
		return err
	}
	return v1Errorf(p.current, "expected %s, got %s", what, v1Describe(p.current))
}

// v1ExpectEnd requires the whole input to have been one expression. A comma
// here is the retired `,` connective: the only commas an expression owns are
// inside the brackets that separate its elements.
func (p *Parser) v1ExpectEnd() error {
	switch p.current.Type {
	case TokenEOF:
		return nil
	case TokenComma:
		return v1Retired(p.current, ruleCommaConnective)
	}
	if err := p.v1RefuseMistake(); err != nil {
		return err
	}
	return v1Errorf(p.current, "unexpected %s after the expression", v1Describe(p.current))
}

// v1ColonError refuses an identifier the lexer glued across a ':'. The lexer
// joins ':' into a name so a canonical id scans whole (v1:crm:lead), which in
// an expression is never a name: it is a string, or -- when there is one colon
// -- a `key: value` or a conditional that lost the space after its ':'.
func v1ColonError(tok Token) error {
	lit := strings.TrimPrefix(tok.Literal, ".")
	msg := "a canonical id in an expression is written as a string: " + ast.QuoteString(lit)
	if strings.Count(lit, ":") == 1 {
		key, value, _ := strings.Cut(lit, ":")
		msg += fmt.Sprintf(" -- or, if the ':' separates a name from a value, put a space after it: %s: %s", key, value)
	}
	return v1Errorf(tok, "%s", msg)
}

// v1KebabError refuses an identifier the lexer glued across a '-'. A hyphen is
// an identifier character (seed slugs, capability argument names), so
// `total-used` is ONE name that quietly compares against nothing -- the silent
// hazard memql#3624 documented at the lexer and left for a positional rule.
// This is that rule: in expression position a hyphenated name is refused; in
// the two KEY positions (a named argument, a map key) it stays legal, because
// a key is never an expression and cannot be read as a subtraction.
func v1KebabError(tok Token) error {
	fix := strings.TrimSpace(strings.ReplaceAll(tok.Literal, "-", " - "))
	return v1Errorf(tok, "`%s` reads as one name; write `%s` (spaces) for subtraction", tok.Literal, fix)
}
