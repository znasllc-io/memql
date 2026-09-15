package parser

// loop_mode.go -- @loop and @mode, the parse half (epic memql#5380, #5381).
//
// The annotation registry decides that @loop and @mode sit on an automation,
// that each takes keyword arguments, and which keys. What a legal key's VALUE
// means is the parser's, and it refuses here what the source alone decides:
//
//   - @loop's until= that is not a lambda of one parameter, or is left out
//     (loop_until_form);
//   - @loop's maxDepth= that is not a whole number, or is left out
//     (loop_max_depth_range);
//   - @mode's max= that is not a whole number of at least 1 (mode_max_range).
//     A written 0 is refused here rather than at load because nothing later
//     can see it: in the compiled form, a max of 0 reads as "the default".
//
// The load decides the rest (component/automations, loop_prepare.go):
// maxDepth against the depth cap, which is an env value; until against the
// automation's @filter; that a @loop's automation is event-triggered; and
// the mode's flags, which it refuses by the names written. Every refusal, the
// load's included, ends with its rule id in brackets, and the ids are the
// constants below -- one spelling for the two halves.
//
// A parse refusal carries its rule id on an *annotations.Refusal, the cause
// the registry's own refusals carry: the editor labels it an invalid
// annotation, and a load report reads the id (baseloader.RuleCode).

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/num"
)

// The @loop and @mode refusal ids (D-I of the loop protection plan).
const (
	// RuleLoopUntilForm: until= is not a lambda of one parameter.
	RuleLoopUntilForm = "loop_until_form"
	// RuleLoopMaxDepthRange: maxDepth= is not a whole number from 1 to the
	// depth cap.
	RuleLoopMaxDepthRange = "loop_max_depth_range"
	// RuleLoopNotEventTriggered: @loop on an automation no event triggers.
	RuleLoopNotEventTriggered = "loop_not_event_triggered"
	// RuleLoopUntilNotInFilter: the @filter does not exclude the rows until
	// holds on.
	RuleLoopUntilNotInFilter = "loop_until_not_in_filter"
	// RuleModeFlags: @mode names no mode, or more than one.
	RuleModeFlags = "mode_flags"
	// RuleModeMaxNotAllowed: max= on single or restart, which admit no
	// second run to bound.
	RuleModeMaxNotAllowed = "mode_max_not_allowed"
	// RuleModeMaxRange: max= is not a whole number of at least 1.
	RuleModeMaxRange = "mode_max_range"
)

// LoopExample is @loop written in full, as a refusal shows it.
const LoopExample = `@loop(maxDepth=4, until=row => row.status == "done")`

// loopUntilFix is what to write for until=, as a refusal says it.
const loopUntilFix = "write until=row => <the state that ends the loop>, as in " + LoopExample

// parseLoopUntil parses @loop's until= value: a lambda of one parameter over
// the triggering row, kept as the node. Anything else is refused at the value
// the author wrote (loop_until_form). A lambda's own syntax errors are the
// grammar's, reported as they are everywhere else.
func (p *Parser) parseLoopUntil() (*ast.LambdaExpr, error) {
	start := p.current
	if !p.v1FilterLambdaAhead() {
		return nil, loopModeRefusal(start, "", RuleLoopUntilForm,
			"@loop's until= takes a lambda of one parameter over the triggering row -- "+loopUntilFix+"; got "+v1Describe(start))
	}
	n, err := p.parseV1Expression()
	if err != nil {
		return nil, err
	}
	lam, ok := n.(*ast.LambdaExpr)
	if !ok {
		return nil, loopModeRefusal(start, "", RuleLoopUntilForm,
			"@loop's until= takes a lambda of one parameter over the triggering row -- "+loopUntilFix+"; got `"+ast.FormatExpr(n)+"`")
	}
	if len(lam.Params) != 1 {
		return nil, loopModeRefusal(start, "", RuleLoopUntilForm, fmt.Sprintf(
			"@loop's until= takes a lambda of one parameter (the triggering row), got %d -- %s", len(lam.Params), loopUntilFix))
	}
	return lam, nil
}

// foldLoop reads a checked @loop onto a LoopDef, refusing a key left out and a
// depth that is not a whole number. The registry has already refused an
// unknown key and a key written in the wrong shape.
func (p *Parser) foldLoop(automation string, attr *Attribute) (*LoopDef, error) {
	at := p.attrTokens[attr]
	subject := fmt.Sprintf("automation %q", automation)
	loop := &LoopDef{}
	raw, written := attr.Args["maxDepth"]
	if !written {
		return nil, loopModeRefusal(at, subject, RuleLoopMaxDepthRange,
			"@loop names no maxDepth= -- write how many runs of this automation one causal chain may hold, a whole number from 1 to the depth cap, as in "+LoopExample)
	}
	n, whole := wholeAttrNumber(raw)
	if !whole {
		return nil, loopModeRefusal(at, subject, RuleLoopMaxDepthRange,
			"@loop's maxDepth= takes a whole number from 1 to the depth cap, as in "+LoopExample+"; it was written "+attrValueText(raw))
	}
	loop.MaxDepth, loop.MaxDepthSet = n, true
	until, ok := attr.Args["until"].(*LambdaExpr)
	if !ok {
		return nil, loopModeRefusal(at, subject, RuleLoopUntilForm,
			"@loop names no until= -- the predicate that ends the loop, which the @filter must exclude: "+loopUntilFix)
	}
	loop.Until = until
	return loop, nil
}

// foldMode reads a checked @mode onto a ModeDef: its bare keys are its flags,
// in the order written -- the registry has refused every bare key but the
// four modes -- and max= its bound. How many flags were written is the load's
// to judge, so its refusal can name them.
func (p *Parser) foldMode(automation string, attr *Attribute) (*ModeDef, error) {
	mode := &ModeDef{}
	for _, k := range writtenKeys(attr) {
		if k.Bare {
			mode.Flags = append(mode.Flags, k.Name)
		}
	}
	raw, written := attr.Args["max"]
	if !written {
		return mode, nil
	}
	n, whole := wholeAttrNumber(raw)
	if !whole || n < 1 {
		return nil, loopModeRefusal(p.attrTokens[attr], fmt.Sprintf("automation %q", automation), RuleModeMaxRange,
			"@mode's max= takes a whole number of at least 1, as in @mode(queued, max=10); it was written max="+attrValueText(raw))
	}
	mode.Max, mode.MaxSet = n, true
	return mode, nil
}

// wholeAttrNumber reads an attribute value written as a whole number: the
// parser stores an integer literal as int64, and anything else -- a decimal,
// a quoted number, a word -- is not one.
func wholeAttrNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		// narrowing: SATURATE -- a depth or a bound is an ORDERING: a value
		// past the platform int still has to read as past every limit, and
		// the load refuses it there.
		return num.ClampInt64(n), true
	case int:
		return n, true
	}
	return 0, false
}

// attrValueText renders an attribute value the way the author wrote it.
func attrValueText(v any) string {
	switch x := v.(type) {
	case string:
		return ast.QuoteString(x)
	case float64, int64, int, bool:
		return ast.FormatLiteral(x)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// loopModeRefusal renders an @loop or @mode refusal as a parse error at tok,
// its rule id last and on the cause.
func loopModeRefusal(tok Token, subject, code, msg string) *ParseError {
	return annotationRefusalError(tok, subject, &annotations.Refusal{Code: code, Message: msg})
}
