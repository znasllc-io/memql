package memql

// collection_scan_lint.go -- the in-memory-vs-SQL guardrail lint
// (Story 6 / memql#2304 / ADR docs/internal/design/core-builtins-and-collections-adr.md
// §2.2 guardrail note).
//
// The Story 4 collection/lambda surface (`.where()/.select()/.count()/...`)
// runs IN-MEMORY, post-fetch. When the base receiver of a collection chain is
// an UNFILTERED FULL-CONCEPT query read, the chain pulls every row of a concept
// into memory just to filter/aggregate it in Go -- work that belongs in a query
// `filter` / SQL pushdown. The anti-pattern:
//
//	allUsers().where(u => u.active).count()
//
// where `allUsers()` reads an entire concept with no filter. The fix is a
// filtered query (`activeUsers()` whose body carries `filter active==true`).
//
// This lint is a WARNING, never a load error -- it mirrors the dead-logic lint
// (#2216, dead_logic_lint.go): a standalone pass that surfaces findings the
// engine logs at boot. It must NOT break DSL load.
//
// CONSERVATISM (avoid false positives): a finding is emitted only when the
// chain's base receiver is a query FUNCTION CALL whose compiled body, after
// peeling directive wrappers (shape/sort/paginate/select/timestamp/depth/
// count/relationship), reduces to a BARE `concept == X` equality and nothing
// else. Any additional predicate (a `&&`/`||` chain, a non-concept comparison,
// a spec reference) means the query is already filtered -> no warning. A base
// receiver of `args.X` is a caller-supplied in-memory list -> legitimately
// in-memory, never warned. Any other base receiver shape is left alone.

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// InMemoryScanFinding records one in-memory-over-unfiltered-full-concept
// collection scan: the logic that hosts the chain, the unfiltered query it
// scans, the concept being scanned, and the collection method applied.
type InMemoryScanFinding struct {
	Logic   string // logic/function name whose body contains the chain
	Query   string // the unfiltered full-concept query function name
	Concept string // the concept id the query scans (the `concept==X` value)
	Method  string // the collection method applied to the scan (chain-head method)
}

// Message renders the finding as the author-facing warning string.
func (f InMemoryScanFinding) Message() string {
	return fmt.Sprintf(
		"logic %q runs an in-memory .%s() over unfiltered full-concept query %q (concept %q) -- "+
			"push the predicate into a query `filter` / SQL pushdown instead of scanning the whole concept in memory (ADR §2.2 / #2304)",
		f.Logic, f.Method, f.Query, f.Concept,
	)
}

// InMemoryCollectionScanFindings walks every loaded function body for Story 4
// collection chains (#2302) whose base receiver is an unfiltered full-concept
// query read and returns one finding per such chain. The result is sorted for
// deterministic reporting. Pure (no logging, no mutation) so it is testable in
// isolation, mirroring DeadLogicNames / enforceLambdaPurity.
func InMemoryCollectionScanFindings(functions map[string]*Function) []InMemoryScanFinding {
	var findings []InMemoryScanFinding
	for name, fn := range functions {
		if fn == nil || fn.Expr == nil {
			continue
		}
		walkForInMemoryScans(fn.Expr, functions, func(f InMemoryScanFinding) {
			f.Logic = name
			findings = append(findings, f)
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Logic != findings[j].Logic {
			return findings[i].Logic < findings[j].Logic
		}
		return findings[i].Query < findings[j].Query
	})
	return findings
}

// warnInMemoryCollectionScans runs the lint and logs each finding as a WARNING.
// Called from the engine boot validation pass (engine_bootstrap.go). A nil
// logger or an empty registry is a no-op.
func warnInMemoryCollectionScans(logger *slog.Logger, functions map[string]*Function) {
	if logger == nil || len(functions) == 0 {
		return
	}
	for _, f := range InMemoryCollectionScanFindings(functions) {
		logger.Warn("in-memory collection scan over unfiltered full-concept query",
			"component", ComponentName,
			"logic", f.Logic,
			"query", f.Query,
			"concept", f.Concept,
			"method", f.Method,
			"lint", "collection-scan",
			"adr", "§2.2",
			"issue", "#2304",
		)
	}
}

// walkForInMemoryScans descends an expression tree, classifying every outermost
// collection chain it finds. On a CollectionMethodExpression it classifies the
// chain's base receiver once, then continues only into the chain's method ARGS
// (lambda bodies may host independent nested chains) -- never re-descending the
// receiver chain, so a single chain is classified exactly once.
func walkForInMemoryScans(expr ExpressionNode, functions map[string]*Function, emit func(InMemoryScanFinding)) {
	switch node := expr.(type) {
	case nil:
		return
	case *CollectionMethodExpression:
		classifyChainBase(node, functions, emit)
		// Walk the args of every node in this chain (nested chains in
		// lambda bodies / value args), but not the receiver chain itself.
		for cur := node; cur != nil; {
			for _, a := range cur.Args {
				walkForInMemoryScans(a, functions, emit)
			}
			inner, ok := cur.Receiver.(*CollectionMethodExpression)
			if !ok {
				break
			}
			cur = inner
		}
	case *LambdaExpression:
		walkForInMemoryScans(node.Body, functions, emit)
	case *LogicalExpression:
		walkForInMemoryScans(node.Left, functions, emit)
		walkForInMemoryScans(node.Right, functions, emit)
	case *ComparisonExpression:
		if v, ok := node.Value.(ExpressionNode); ok {
			walkForInMemoryScans(v, functions, emit)
		}
	case *RelationshipExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *SortExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *PaginateExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *SelectExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *TimestampExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *DepthExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *CountExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *ShapeExpression:
		walkForInMemoryScans(node.Target, functions, emit)
	case *ConditionalFilterExpression:
		walkForInMemoryScans(node.Filter, functions, emit)
	}
}

// classifyChainBase inspects the base receiver of a collection chain and emits
// a finding when it is an unfiltered full-concept query read. A query CALL is
// the only receiver shape that warns; an `args.X` list and every other shape
// are left untouched (conservative).
func classifyChainBase(chain *CollectionMethodExpression, functions map[string]*Function, emit func(InMemoryScanFinding)) {
	call, ok := baseReceiver(chain).(*FunctionCallExpression)
	if !ok {
		// args.X list, literal, spec ref, etc. -- not a full-concept query read.
		return
	}
	fn, ok := functions[call.Name]
	if !ok || fn == nil || !isQueryKind(fn) {
		return
	}
	concept, isScan := unfilteredFullConceptScan(fn.Expr)
	if !isScan {
		return
	}
	emit(InMemoryScanFinding{
		Query:   call.Name,
		Concept: concept,
		Method:  strings.ToLower(strings.TrimSpace(chainHeadMethod(chain))),
	})
}

// chainHeadMethod returns the method name of the outermost node in a chain
// (the operator applied last, e.g. `count` in `q().where(...).count()`).
func chainHeadMethod(chain *CollectionMethodExpression) string {
	return chain.Method
}

// isQueryKind reports whether a function is a read query (the only kind whose
// unfiltered body the lint cares about).
func isQueryKind(fn *Function) bool {
	return strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "query")
}

// unfilteredFullConceptScan reports whether a query's compiled body is a bare
// full-concept scan -- a single `concept == X` equality with no other
// predicate -- and returns the scanned concept id. Directive wrappers
// (shape/sort/paginate/select/timestamp/depth/count/relationship) are peeled
// first; they shape/limit the projection but do not constrain WHICH rows match,
// so a wrapped bare scan is still a full scan. Anything other than a lone
// `concept==X` comparison (a `&&`/`||` chain, a non-concept comparison, a spec
// reference, nil) is treated as filtered -> not a full scan.
func unfilteredFullConceptScan(expr ExpressionNode) (string, bool) {
	cmp, ok := peelDirectiveWrappers(expr).(*ComparisonExpression)
	if !ok || cmp == nil {
		return "", false
	}
	if cmp.Operator != OpEq || cmp.Field.Raw != "concept" {
		return "", false
	}
	concept, _ := cmp.Value.(string)
	return concept, true
}

// peelDirectiveWrappers descends through the projection/ordering/limit wrapper
// nodes to the underlying filter expression. These wrappers must be the
// outermost nodes of a query body (parser.planQuery enforces it), so peeling
// them lands on the row-selecting predicate.
func peelDirectiveWrappers(expr ExpressionNode) ExpressionNode {
	for {
		switch n := expr.(type) {
		case *ShapeExpression:
			expr = n.Target
		case *SortExpression:
			expr = n.Target
		case *PaginateExpression:
			expr = n.Target
		case *SelectExpression:
			expr = n.Target
		case *TimestampExpression:
			expr = n.Target
		case *DepthExpression:
			expr = n.Target
		case *CountExpression:
			expr = n.Target
		case *RelationshipExpression:
			expr = n.Target
		default:
			return expr
		}
	}
}
