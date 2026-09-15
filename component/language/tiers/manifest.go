package tiers

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
)

// manifest.go is the tier manifest proper: for every position, the tier it
// evaluates in, the node kinds and catalog functions it admits, and whether a
// spec or trait may be applied there. It is data: for the lowering to read at
// load (a node a position does not admit is a load refusal naming the node and
// the position), and for Sense's completion and hover, the generated docs table
// and the conformance corpus's completeness gate.

// Tier is the tier a position evaluates in -- the same word a catalog function
// carries: P, pushed down to SQL, or M, evaluated in process.
type Tier = functions.Tier

// The two tiers, re-exported so a caller that only asks about positions needs
// one import.
const (
	TierP = functions.TierP
	TierM = functions.TierM
)

// Admission is what a position does with a node kind, a catalog function or a
// predicate application.
type Admission int

const (
	// Refused: the load refuses the expression, naming the node and the
	// position.
	Refused Admission = iota
	// PlanConstantOnly: admitted only where the subexpression does not read the
	// row parameter. It exists for the pushdown positions. There, a
	// subexpression that does not read the row is a plan constant: EvalExpr
	// evaluates it once per call, before the query runs, and the lowering binds
	// the value as a parameter. So `row.expiresAt < addDuration(now, "P1D")` and
	// `row.rank > args.floor * 2` are legal filters, and
	// `addDuration(row.createdAt, "P1D") < now` is refused with the nearest
	// pushdown spelling (`row.createdAt < addDuration(now, "-P1D")`).
	//
	// Why not Refused: whether a subexpression reads the row is a property of
	// the node, not of its kind, so the manifest cannot tell the legal use from
	// the illegal one. Refusing the kind would refuse the legal use too, and
	// send every time-window filter through a logic body to compute its bound
	// first -- for no gain, since a folded plan constant costs the query exactly
	// what the literal it replaces costs. The manifest answers
	// PlanConstantOnly; Lower decides per node.
	PlanConstantOnly
	// Admitted: admitted wherever the grammar allows it.
	Admitted
)

// String names the admission in the manifest's own vocabulary, the spelling
// the docs table and refusal messages print.
func (a Admission) String() string {
	switch a {
	case Refused:
		return "refused"
	case PlanConstantOnly:
		return "planConstantOnly"
	case Admitted:
		return "admitted"
	}
	return fmt.Sprintf("Admission(%d)", int(a))
}

// Rule is one row of the manifest: a position and EITHER a node kind or a
// catalog function key (Function.Key: "lower", "list.any"), never both, with
// what the position does with it.
type Rule struct {
	Position  Position
	Kind      ast.NodeKind
	Function  string
	Admission Admission
}

// The rule sets. A kind a set does not name is Refused. Every set names each
// kind it admits EXPLICITLY -- none is derived from ast.AllNodeKinds() -- so a
// kind added to the AST is refused everywhere until somebody decides where it
// belongs, and TestEveryNodeKindIsInATier fails naming it, rather than the new
// kind riding into every in-process position by default.
var (
	// pushdownKinds is the rule set of the two pushdown positions that are
	// predicates over the row: a query filter and a spec or trait body.
	pushdownKinds = map[ast.NodeKind]Admission{
		ast.KindIdent:          Admitted, // the lambda parameter, a reserved root, a predicate's name
		ast.KindMember:         Admitted, // row.status is a payload path; actor.userId the actor operand
		ast.KindOptionalMember: Admitted, // row.?lineage.planId: the same path, absent-safe
		ast.KindCall:           Admitted, // a P function or a predicate; an M function narrows to PlanConstantOnly
		ast.KindMethodCall:     Admitted, // row.tags.any(t => ...); an M method narrows the same way
		ast.KindNot:            Admitted, // every lowered predicate is two-valued, so `!e` is NOT COALESCE((e), FALSE)
		ast.KindComparison:     Admitted,
		ast.KindIn:             Admitted,
		ast.KindStartsWith:     Admitted,
		ast.KindAnd:            Admitted,
		ast.KindOr:             Admitted,
		ast.KindTernary:        Admitted, // a boolean ternary over the row lowers to (c && p) || (!c && q); Lower refuses a value ternary over the row
		ast.KindLambda:         Admitted, // the filter's own header, a traversal's match, a method's predicate
		ast.KindList:           Admitted, // row.status in ["a", "b"] lowers to an SQL IN list
		ast.KindLiteral:        Admitted,
		ast.KindNil:            Admitted, // == nil and != nil are IS NULL and IS NOT NULL
		ast.KindParen:          Admitted,

		// No SQL form over the row, and legal on plan constants
		// (`args.limit * 2`, `args.stage ?? "active"`). The two evaluators
		// must agree on every expression (D7; the differential lane holds them
		// to it), and these kinds mean something in process that depends on
		// the RUNTIME type of their operands: `+` concatenates two strings and
		// adds two numbers -- the meaning that keeps the `concat` migration
		// exact -- and a payload field's JSON type is only known per row, so
		// there is no one SQL operator both evaluators agree on. `-`, `*`, `/`
		// and `%` share the arithmetic kind with `+` and go with it rather than
		// split the kind in two, and unary minus follows them; the record puts
		// arithmetic in the M tier in any case. `??` is blank-coalescing
		// (authoring rule 30: it falls through on an empty or whitespace-only
		// string as well as on absent), defined once in process and not
		// restated per operand type in SQL. A map literal has no SQL value in
		// a predicate at all.
		ast.KindArithmetic: PlanConstantOnly,
		ast.KindNegate:     PlanConstantOnly,
		ast.KindCoalesce:   PlanConstantOnly,
		ast.KindMap:        PlanConstantOnly,

		// A construct call has no place in a predicate. Over the row it would
		// run a query, a mutation or a logic for every row the predicate is
		// asked about. As a plan constant it would still bring a whole query
		// result into the filter -- an unbounded source, which D11 refuses in
		// a pushdown position -- or put a write inside a read.
		ast.KindConstructCall: Refused,
	}

	// literalKinds is the rule set of the three positions whose syntax the
	// v1 grammar does not change: a query's sort key (`sort "row.createdAt",
	// "desc"`), an @rowAuthz argument (`owner="ownerUserId"`) and a tool
	// field's @default. Each is a literal and nothing else. They are in the
	// manifest so that it covers every position the record names, and so the
	// corpus asks each of them for a refused case like any other position.
	literalKinds = map[ast.NodeKind]Admission{
		ast.KindLiteral: Admitted,
	}

	// inProcessKinds is the rule set of the in-process positions that compute
	// a condition or a value for ANOTHER construct: an automation condition, a
	// trigger filter, a mutation value and a prompt input. Every kind is
	// admitted except the construct call. A construct call is the work of a
	// statement -- it reads or writes through the engine, and a run journals
	// it as a step (D14) -- and these positions are not statements: a call in
	// a condition or a value would read, or write, with no step of its own.
	inProcessKinds = map[ast.NodeKind]Admission{
		ast.KindIdent:          Admitted,
		ast.KindMember:         Admitted,
		ast.KindOptionalMember: Admitted,
		ast.KindCall:           Admitted,
		ast.KindMethodCall:     Admitted,
		ast.KindNot:            Admitted,
		ast.KindNegate:         Admitted,
		ast.KindArithmetic:     Admitted,
		ast.KindCoalesce:       Admitted,
		ast.KindComparison:     Admitted,
		ast.KindIn:             Admitted,
		ast.KindStartsWith:     Admitted,
		ast.KindAnd:            Admitted,
		ast.KindOr:             Admitted,
		ast.KindTernary:        Admitted,
		ast.KindLambda:         Admitted,
		ast.KindList:           Admitted,
		ast.KindMap:            Admitted,
		ast.KindLiteral:        Admitted,
		ast.KindNil:            Admitted,
		ast.KindParen:          Admitted,
		ast.KindConstructCall:  Refused,
	}

	// bodyKinds is inProcessKinds plus the construct call, for a logic body
	// and a step argument: the two positions where an expression sits in the
	// statement that journals it, so `rows := query activeUsers(...)` is the
	// unit of work rather than a side effect of computing a value.
	bodyKinds = func() map[ast.NodeKind]Admission {
		out := make(map[ast.NodeKind]Admission, len(inProcessKinds))
		for k, a := range inProcessKinds {
			out[k] = a
		}
		out[ast.KindConstructCall] = Admitted
		return out
	}()
)

// functionRule is how a position admits catalog functions.
type functionRule int

const (
	// noFunctions: the position is a literal, so it calls nothing.
	noFunctions functionRule = iota
	// pushdownFunctions: a P function is Admitted; an M function is
	// PlanConstantOnly (see PlanConstantOnly for why not Refused).
	pushdownFunctions
	// allFunctions: every catalog function is Admitted.
	allFunctions
)

// positionRule is one position's row of the manifest.
type positionRule struct {
	tier      Tier
	kinds     map[ast.NodeKind]Admission
	functions functionRule
	// predicates is whether a spec or trait may be applied here
	// (`isActiveRecord(row)`, `requiresOwner(actor)`): predicates are
	// constructs, not catalog functions, so they have their own answer.
	predicates Admission
}

// manifest is the manifest: one row per position, exactly the positions
// Positions() lists (TestTierOfEveryPosition holds the two together).
//
// A position's tier and its rule set are separate answers: a tool @default is
// an M position (a default is resolved in process, when the tool is called)
// whose syntax is a literal. A spec or trait may be applied in the two
// predicate positions and in every in-process position except a prompt
// input; the three literal positions call nothing at all.
var manifest = map[Position]positionRule{
	PositionQueryFilter:         {tier: TierP, kinds: pushdownKinds, functions: pushdownFunctions, predicates: Admitted},
	PositionSort:                {tier: TierP, kinds: literalKinds, functions: noFunctions, predicates: Refused},
	PositionSpecBody:            {tier: TierP, kinds: pushdownKinds, functions: pushdownFunctions, predicates: Admitted},
	PositionRowAuthzArgument:    {tier: TierP, kinds: literalKinds, functions: noFunctions, predicates: Refused},
	PositionAutomationCondition: {tier: TierM, kinds: inProcessKinds, functions: allFunctions, predicates: Admitted},
	PositionTriggerFilter:       {tier: TierM, kinds: inProcessKinds, functions: allFunctions, predicates: Admitted},
	PositionLogicBody:           {tier: TierM, kinds: bodyKinds, functions: allFunctions, predicates: Admitted},
	PositionMutationValue:       {tier: TierM, kinds: inProcessKinds, functions: allFunctions, predicates: Admitted},
	PositionBeforeWriteValue:    {tier: TierM, kinds: inProcessKinds, functions: allFunctions, predicates: Admitted},
	PositionStepArgument:        {tier: TierM, kinds: bodyKinds, functions: allFunctions, predicates: Admitted},
	PositionToolDefault:         {tier: TierM, kinds: literalKinds, functions: noFunctions, predicates: Refused},
	PositionPromptInput:         {tier: TierM, kinds: inProcessKinds, functions: allFunctions, predicates: Refused},
	PositionQueryRefine:         {tier: TierM, kinds: inProcessKinds, functions: allFunctions, predicates: Admitted},
}

// catalogByKey indexes the function catalog by Function.Key, the key
// FunctionAdmission is asked about.
var catalogByKey = func() map[string]functions.Function {
	fns := functions.Catalog()
	out := make(map[string]functions.Function, len(fns))
	for _, f := range fns {
		out[f.Key()] = f
	}
	return out
}()

// TierOf returns the tier a position evaluates in, or "" for a string that is
// not a position.
func TierOf(p Position) Tier {
	return manifest[p].tier
}

// KindAdmission returns what position p does with a node of kind k. An unknown
// position or kind is Refused.
func KindAdmission(p Position, k ast.NodeKind) Admission {
	r, ok := manifest[p]
	if !ok {
		return Refused
	}
	return r.kinds[k]
}

// FunctionAdmission returns what position p does with a call to the catalog
// function whose Function.Key is key. A key the catalog does not hold -- a
// retired spelling, a misspelling, a name in the wrong case -- is Refused.
// Applying a spec or a trait is not a catalog call: see PredicateAdmission.
func FunctionAdmission(p Position, key string) Admission {
	f, ok := catalogByKey[key]
	if !ok {
		return Refused
	}
	return functionAdmission(p, f)
}

// functionAdmission decides for a function value, so a test can ask about an
// entry the catalog does not hold.
func functionAdmission(p Position, f functions.Function) Admission {
	r, ok := manifest[p]
	if !ok {
		return Refused
	}
	// A function with no tier is in no tier: refused everywhere, which is how
	// TestEveryFunctionIsInATier sees it.
	if f.Tier != TierP && f.Tier != TierM {
		return Refused
	}
	switch r.functions {
	case pushdownFunctions:
		if f.Tier == TierP {
			return Admitted
		}
		return PlanConstantOnly
	case allFunctions:
		return Admitted
	}
	return Refused
}

// PredicateAdmission returns whether a spec or trait may be applied in
// position p (`isActiveRecord(row)`). An unknown position is Refused.
func PredicateAdmission(p Position) Admission {
	return manifest[p].predicates
}

// Allows reports whether position p admits kind k at all, as a plan constant
// included.
func Allows(p Position, k ast.NodeKind) bool {
	return KindAdmission(p, k) != Refused
}

// AllowsFunction reports whether position p admits a call to the catalog
// function key at all, as a plan constant included.
func AllowsFunction(p Position, key string) bool {
	return FunctionAdmission(p, key) != Refused
}

// NodeKinds lists the kinds position p admits, as a plan constant included, in
// ast.AllNodeKinds() order.
func NodeKinds(p Position) []ast.NodeKind {
	var out []ast.NodeKind
	for _, k := range ast.AllNodeKinds() {
		if Allows(p, k) {
			out = append(out, k)
		}
	}
	return out
}

// Rules returns the whole manifest as rows: for each position in Positions()
// order, one row per node kind in ast.AllNodeKinds() order, then one row per
// catalog function in functions.Catalog() order. Refused rows are included --
// the corpus asks each position for a refused case, and the docs table shows
// what a position does not take as well as what it does.
func Rules() []Rule {
	kinds := ast.AllNodeKinds()
	fns := functions.Catalog()
	out := make([]Rule, 0, len(Positions())*(len(kinds)+len(fns)))
	for _, p := range Positions() {
		for _, k := range kinds {
			out = append(out, Rule{Position: p, Kind: k, Admission: KindAdmission(p, k)})
		}
		for _, f := range fns {
			out = append(out, Rule{Position: p, Function: f.Key(), Admission: functionAdmission(p, f)})
		}
	}
	return out
}
