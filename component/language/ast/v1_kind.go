package ast

// NodeKind names what an edition-2026 expression node is, in the vocabulary
// the tier manifest (component/language/tiers) states its rules in: which kinds
// each position admits. It is finer than the Go type where the tiers need it to
// be -- a `+` and an `==` are both BinaryExpr, and only one of them pushes down
// -- and no finer.
type NodeKind string

const (
	KindUnknown        NodeKind = ""
	KindIdent          NodeKind = "ident"
	KindMember         NodeKind = "member"
	KindOptionalMember NodeKind = "optionalMember"
	KindCall           NodeKind = "call"
	KindMethodCall     NodeKind = "methodCall"
	KindConstructCall  NodeKind = "constructCall"
	KindNot            NodeKind = "not"
	KindNegate         NodeKind = "negate"
	KindArithmetic     NodeKind = "arithmetic"
	KindCoalesce       NodeKind = "coalesce"
	KindComparison     NodeKind = "comparison"
	KindIn             NodeKind = "in"
	KindStartsWith     NodeKind = "startsWith"
	KindAnd            NodeKind = "and"
	KindOr             NodeKind = "or"
	KindTernary        NodeKind = "ternary"
	KindLambda         NodeKind = "lambda"
	KindList           NodeKind = "list"
	KindMap            NodeKind = "map"
	KindLiteral        NodeKind = "literal"
	KindNil            NodeKind = "nil"
	KindParen          NodeKind = "paren"
)

// AllNodeKinds lists every kind, in a stable order. The tier manifest's
// completeness test reads it: a kind no position admits is a kind no author can
// write, and the test fails naming it.
func AllNodeKinds() []NodeKind {
	return []NodeKind{
		KindIdent, KindMember, KindOptionalMember,
		KindCall, KindMethodCall, KindConstructCall,
		KindNot, KindNegate,
		KindArithmetic, KindCoalesce, KindComparison, KindIn, KindStartsWith,
		KindAnd, KindOr, KindTernary, KindLambda,
		KindList, KindMap, KindLiteral, KindNil, KindParen,
	}
}

// ArithmeticOps, ComparisonOps are the binary operators each kind covers.
var (
	arithmeticOps = map[string]bool{"+": true, "-": true, "*": true, "/": true, "%": true}
	comparisonOps = map[string]bool{"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}
)

// KindOf returns the kind of an edition-2026 node, or KindUnknown for a node
// outside that set (a node of the internal query form).
func KindOf(n ExpressionNode) NodeKind {
	switch e := n.(type) {
	case *IdentExpr:
		return KindIdent
	case *MemberExpr:
		if e.Optional {
			return KindOptionalMember
		}
		return KindMember
	case *CallExpr:
		switch {
		case e.Kind != "":
			return KindConstructCall
		case e.Receiver != nil:
			return KindMethodCall
		default:
			return KindCall
		}
	case *UnaryExpr:
		if e.Op == "!" {
			return KindNot
		}
		return KindNegate
	case *BinaryExpr:
		switch {
		case arithmeticOps[e.Op]:
			return KindArithmetic
		case comparisonOps[e.Op]:
			return KindComparison
		case e.Op == "??":
			return KindCoalesce
		case e.Op == "in":
			return KindIn
		case e.Op == "startsWith":
			return KindStartsWith
		case e.Op == "&&":
			return KindAnd
		case e.Op == "||":
			return KindOr
		}
	case *TernaryExpr:
		return KindTernary
	case *LambdaExpr:
		return KindLambda
	case *ListExpr:
		return KindList
	case *MapExpr:
		return KindMap
	case *LiteralExpr:
		return KindLiteral
	case *NilExpr:
		return KindNil
	case *ParenExpr:
		return KindParen
	}
	return KindUnknown
}
