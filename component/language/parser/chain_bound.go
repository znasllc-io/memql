package parser

// chain_bound.go -- the bound on how TALL an expression tree may be.
//
// v1MaxDepth (v1_expr.go) bounds how deeply the parser RECURSES, and that is
// only half a bound. A chain is built in a LOOP -- `a.b.c...`, `a && b && ...`,
// `x.m().m()...` -- so the parser never recurses while reading one, and
// nothing refuses it. The TREE it builds is as tall as the chain, and
// everything that reads a tree recurses over it: the statement parser's own
// check of the expression it just read (checkV1BodyExpression -> ast.WalkV1),
// the printer, the lowering, the evaluator, the cost estimate, the call graph,
// the import integrity walk. A goroutine stack is not something Go lets a
// program run out of gracefully: past 1 GB the runtime prints "goroutine stack
// exceeds 1000000000-byte limit" and ends the process, and recover() never
// sees it. So a chain nothing bounds is a fatal error, in a process that also
// serves every other client.
//
// Bounding the WALKS instead would mean bounding each of them, and there are
// at least eight callers of ast.WalkV1 outside this package; the next one
// added would be unguarded. Bounding what the PARSER BUILDS holds for every
// reader at once, including the ones not written yet.
//
// # How the count works
//
// A LINK is one binary operator fold, one member read, or one method call --
// the three shapes a loop builds without recursing. MaxExpressionChain bounds
// how many links lie on any one path from a node to a leaf.
//
//   - The edition-2026 grammar carries that count with every node it builds
//     (v1Expr.links) and refuses the link that would make it one too many.
//     Every node kind PROPAGATES its operands' count -- a group, a list
//     element, a call argument, a lambda body -- so a chain continued inside
//     another chain's operand is one chain, and 256 groups of 4096 links each
//     cannot add up to a million.
//   - The procedural grammar (the internal query form an SDK sends to Execute,
//     and what the struct-form rewriter lowers a query and a mutation into)
//     builds nodes with no room to carry a count, so it counts every link of a
//     declaration, or of one standalone expression, against the same number
//     (chainLink). A path holds no more links than the whole expression does,
//     so the bound holds there too, and a single chain meets it at exactly the
//     same link.
//
// A tree that survives is therefore at most MaxExpressionChain links plus
// v1MaxDepth nesting levels tall -- a few thousand frames, not a few million.
//
// # Why this is not a wall in front of real code
//
// The longest chain in the 1,151 .memql files of this repository is 8 links.
// The bound is three orders of magnitude above that, so it is reached only by
// generated or hostile input; chain_bound_test.go parses a chain AT the bound
// of every kind, so the bound is a bound rather than a wall.

import "fmt"

// MaxExpressionChain is how many links one path of an expression tree may
// hold: a binary operator fold, a member read or a method call is one link.
const MaxExpressionChain = 4096

// RuleExpressionChainTooLong is the stable code of the refusal of a chain past
// MaxExpressionChain: the last thing its message prints, in brackets, and the
// code a load report records (baseloader.CodedRefusal).
const RuleExpressionChainTooLong = "expression_chain_too_long"

// ChainTooLongError is the refusal of an expression chained past
// MaxExpressionChain. It unwraps to the positioned *ParseError, so every
// consumer that reads a parse error's line and column reads this one's; the
// declaration it sits in is named by the wrap the construct parsers put around
// every refusal of theirs.
type ChainTooLongError struct {
	Parse *ParseError
}

// Error is the positioned message with the code last, in brackets (D24).
func (e *ChainTooLongError) Error() string {
	return e.Parse.Error() + " [" + RuleExpressionChainTooLong + "]"
}

// RuleCode is RuleExpressionChainTooLong (baseloader.CodedRefusal).
func (e *ChainTooLongError) RuleCode() string { return RuleExpressionChainTooLong }

// Unwrap returns the positioned parse error, which unwraps to ErrInvalidSyntax.
func (e *ChainTooLongError) Unwrap() error { return e.Parse }

// chainTooLong refuses, at the token that made the link one too many, an
// expression chained past MaxExpressionChain. The remedy is chain-specific:
// a long chain does not nest, so "name its parts as separate statements or
// constructs" (what v1TooDeep says) is the wrong instruction -- what splits a
// chain is binding its prefix to a name.
func chainTooLong(tok Token) error {
	return &ChainTooLongError{Parse: v1ParseErrorAt(tok,
		fmt.Sprintf("the expression chain is too long (more than %d links): bind its parts to names and combine them in separate statements",
			MaxExpressionChain))}
}

// maxLinks is the most links any of counts holds. It is what a node that is
// not itself a link reports: a group, a list, a map value, a call argument, a
// lambda body and a prefix all carry their operand's count onward, so a chain
// that continues inside one is still one chain.
func maxLinks(counts ...int) int {
	most := 0
	for _, n := range counts {
		most = max(most, n)
	}
	return most
}

// v1ChainLink is the link count of a node that IS a link, over operands
// holding the counts given: the most of them, plus one. It refuses at tok --
// the operator, member name or method name that made the link -- when the
// result would be past MaxExpressionChain.
func v1ChainLink(tok Token, operands ...int) (int, error) {
	links := maxLinks(operands...) + 1
	if links > MaxExpressionChain {
		return 0, chainTooLong(tok)
	}
	return links, nil
}

// chainLink counts one link of the PROCEDURAL grammar at tok against the
// declaration's MaxExpressionChain, and refuses the link past it. That
// grammar's nodes carry no count of their own, so the count is cumulative over
// the declaration (parseDefinition resets it) rather than per path -- an
// over-approximation, which is the safe direction: a path holds no more links
// than the whole expression does.
func (p *Parser) chainLink(tok Token) error {
	if p.chainLinks >= MaxExpressionChain {
		return chainTooLong(tok)
	}
	p.chainLinks++
	return nil
}
