package parser

// nesting_bound.go -- the bound on how DEEP the parser recurses.
//
// This is the mirror of v1Depth (v1_expr.go), which bounds the edition-2026
// expression grammar only. Everything else the parser reads recursed without a
// bound: the PROCEDURAL expression grammar -- the internal query form an SDK
// sends to Execute, what the HTTP gateway builds from a POST body, the MCP
// `query` argument, and what the struct-form rewriter lowers a query and a
// mutation into -- and the declaration parsers and the statement-block parser
// beside it. A goroutine's stack is not something Go lets a program run out of
// gracefully: past the process's maximum stack the runtime prints "goroutine
// stack exceeds ...-byte limit" and ends the process, and recover() never sees
// it.
//
// The DEPTH at which that happens is a property of the STACK CEILING, not of
// the source's SIZE: the same number of levels costs the same number of frames
// however few bytes they are written in, and the ceiling differs between one
// machine and the next. So no byte cap on the source is a bound on this at
// all; counting the levels is the only thing that is.
//
// # What a level is
//
// A function that recurses into a deeper level enters it through
// enterNesting, and every recursive cycle of the parser's call graph passes
// one -- so no input makes the parser recurse further than MaxNestingDepth
// levels. The cycles of the edition-2026 expression grammar pass v1Depth's own
// count instead; the two counts never interleave without bound, because the
// procedural grammar reads a v1 lambda (tryParseV1LambdaOperand) and the v1
// grammar never reads its way back.
//
//   - In the procedural expression grammar: each parenthesised group, each
//     `[...]` array and `{...}` object value, each call argument list, each
//     collection-method argument list, each `when` guard, each `??` chain
//     operand, each prefix `!` or `-`, and each conditional, from its `?`.
//   - In a declaration: each property (one inside a nested object, an @open
//     body or a variant is a level below the property that holds it, and a
//     concept's own properties are at level 1), each type that holds an
//     element type (`[]T`, `array(T)`, `map[K]V`), and each seed block.
//   - In a statement body: each block -- the body of an if, else, for, case,
//     default or branch (parseV1Block). A logic's own body is not a level, so
//     a body nests MaxNestingDepth blocks and is refused at the next one.
//
// # Why 256
//
// It is v1MaxDepth, deliberately: one language, one answer to "how deeply may
// this nest", whichever grammar reads the text. What real input NEEDS was
// measured by lowering the constant and re-running the gates -- memqllint over
// dsl/, the conformance corpus, the SDK, the language tree, grpc and mcp:
//
//	bound  memqllint dsl/    conformance   sdk + language + grpc + mcp
//	  256  clean             pass          pass
//	   16  clean             pass*         pass
//	    8  clean             pass*         pass
//	    7  clean             pass*         pass
//	    6  clean             pass*         2 refused
//	    4  clean             pass          2 refused
//	    3  clean             --            --
//	    2  92 files refused  --            --
//
//	* bracketed by the runs at 4 and at 256 rather than measured directly.
//
// So the deepest nesting anything real writes is SEVEN levels, and it is not
// hand-written DSL: the shipped tree needs only three, and the two cases that
// need seven are both GENERATED -- the Go SDK rendering a mutation call whose
// argument is a nested document, and the lambda serializer's
// `groupBy(...).select(...)` with arithmetic in the projection. Generated
// callers are exactly the ones that would nest deeper tomorrow without anybody
// noticing, which is the argument for headroom rather than for a snug bound.
//
// 256 is thirty-six times the measured floor, and a parse that survives it
// costs a few hundred levels of frames rather than a few hundred thousand.

import "fmt"

// MaxNestingDepth is how many levels deep the parser reads before it refuses.
// It is v1MaxDepth, so that one number answers the question for every grammar
// the parser holds.
const MaxNestingDepth = v1MaxDepth

// RuleNestingTooDeep is the stable code of the refusal of a source nested
// deeper than MaxNestingDepth: the last thing its message prints, in brackets,
// and the code a load report records (baseloader.CodedRefusal).
const RuleNestingTooDeep = "nesting_too_deep"

// nestingSite is where a level is entered, which decides what the refusal
// calls the thing that nests and what remedy it names. A remedy has to fit
// where the bound was crossed: "name its parts as separate statements" is the
// wrong instruction for a concept's property tree.
type nestingSite int

const (
	siteExpression  nestingSite = iota // a group, list, object, argument list, guard, chain, prefix or conditional
	siteDeclaration                    // a property, a container type, a seed block
	siteBody                           // a statement block
)

func (s nestingSite) subject() string {
	switch s {
	case siteDeclaration:
		return "the declaration"
	case siteBody:
		return "the body"
	}
	return "the expression"
}

func (s nestingSite) remedy() string {
	switch s {
	case siteDeclaration:
		return "flatten the nesting into separate fields or concepts"
	case siteBody:
		return "split the body into smaller logic calls"
	}
	// The same remedy, in the same words, the edition-2026 grammar's own
	// depth refusal gives (v1TooDeep): one rule, one sentence, whichever
	// grammar read the text.
	return "name its parts as separate statements or constructs"
}

// NestingTooDeepError is the refusal of a source nested deeper than
// MaxNestingDepth. It unwraps to the positioned *ParseError, so every consumer
// that reads a parse error's line and column reads this one's; the declaration
// it sits in is named by the wrap the construct parsers put around their
// refusals.
type NestingTooDeepError struct {
	Parse *ParseError
}

// Error is what nests, how deep the bound is, where, the remedy, and the code
// last, in brackets (D24).
func (e *NestingTooDeepError) Error() string {
	return e.Parse.Error() + " [" + RuleNestingTooDeep + "]"
}

// RuleCode is RuleNestingTooDeep (baseloader.CodedRefusal).
func (e *NestingTooDeepError) RuleCode() string { return RuleNestingTooDeep }

// Unwrap returns the positioned parse error, which unwraps to ErrInvalidSyntax.
func (e *NestingTooDeepError) Unwrap() error { return e.Parse }

// enterNesting enters the level the current token opens, at site, and refuses
// at that token when the level would be past MaxNestingDepth. A nil return is
// paired with a leaveNesting deferred at the call site; a refusal enters
// nothing, so it has nothing to leave.
func (p *Parser) enterNesting(site nestingSite) error {
	if p.nesting >= MaxNestingDepth {
		return &NestingTooDeepError{Parse: v1ParseErrorAt(p.current,
			fmt.Sprintf("%s nests too deeply (more than %d levels): %s",
				site.subject(), MaxNestingDepth, site.remedy()))}
	}
	p.nesting++
	return nil
}

// leaveNesting leaves the level enterNesting entered.
func (p *Parser) leaveNesting() { p.nesting-- }
