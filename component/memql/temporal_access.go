package memql

// temporal_access.go is the engine-side support for the temporal-access
// (`asOf`) visibility rule (core-builtins ADR §2.3, story memql#2305).
//
// `asOf` is a QUERY-ONLY clause: it compiles to a time-travel read
// against the graph and is rejected anywhere but a query (the parser's
// parseAsOfFunction enforces this for logic / automation / mutation
// bodies via the receiver context; the spec loader uses
// findTemporalAccess below because spec bodies are parsed through the
// context-free ParseExpression entry).
//
// For now-parity visibility, a query whose `asOf` is `latest` returns
// CLOCK-DEPENDENT data (the live tip of the append-only stream), so the
// loaded query metadata is marked time-dependent (Function.LatestMode)
// and consumers can see the result is not reproducible. `asOf <explicit
// timestamp>` is DETERMINISTIC (historical state is immutable) and is
// NOT marked.

// findTemporalAccess walks an engine expression tree and returns the
// first TimestampExpression (the compiled `asOf` clause) it finds, or
// nil. It descends the directive wrappers, boolean composition, and
// builtin call arguments so an `asOf` nested under a logic body's
// coalesce / cond / && is still found. It backs both the query-only
// load gate (reject `asOf` outside a query) and the latest-mode
// contract marker (UseLatest -> time-dependent).
func findTemporalAccess(expr ExpressionNode) *TimestampExpression {
	switch n := expr.(type) {
	case nil:
		return nil
	case *TimestampExpression:
		return n
	case *SortExpression:
		return findTemporalAccess(n.Target)
	case *PaginateExpression:
		return findTemporalAccess(n.Target)
	case *SelectExpression:
		return findTemporalAccess(n.Target)
	case *DepthExpression:
		return findTemporalAccess(n.Target)
	case *CountExpression:
		return findTemporalAccess(n.Target)
	case *ShapeExpression:
		return findTemporalAccess(n.Target)
	case *RelationshipExpression:
		return findTemporalAccess(n.Target)
	case *LogicalExpression:
		if t := findTemporalAccess(n.Left); t != nil {
			return t
		}
		return findTemporalAccess(n.Right)
	case *FunctionCallExpression:
		// coalesce / cond / concat / ... carry their (positionally
		// indexed) argument expressions in the Args map; descend into
		// any value that is itself an expression node.
		for _, v := range n.Args {
			if inner, ok := v.(ExpressionNode); ok {
				if t := findTemporalAccess(inner); t != nil {
					return t
				}
			}
		}
		return nil
	default:
		return nil
	}
}

// queryLatestMode reports whether a query expression CAN read the live tip
// -- i.e. it may return clock-dependent (non-reproducible) data, so its
// contract must be marked time-dependent. An `asOf <explicit timestamp>` is
// deterministic and returns false.
//
// FallbackLatest counts, and missing that was a real regression (memql#2992
// landing review). `asOf args.asOf ?? latest` reads the live tip for every
// caller who omits the argument -- which is every caller of
// deploymentsForCluster today, the whole component/deploycontrol path
// included. This runs at LOAD time on the UNEXPANDED node, long before
// resolveAsOfArg decides which branch a given call takes, so the loaded
// contract has to describe what the query MAY do, not what one invocation
// did. Reading only UseLatest silently flipped that query's marker from
// time-dependent to deterministic while its behaviour was unchanged: the
// ruling's stated safety property was that adopting the form is
// behaviour-identical, and the contract marker is part of the behaviour a
// consumer sees.
//
// The cost of getting this wrong scales with adoption -- the five remaining
// `asOf latest` queries would each have lost the marker as they adopted the
// form.
func queryLatestMode(expr ExpressionNode) bool {
	ts := findTemporalAccess(expr)
	return ts != nil && (ts.UseLatest || ts.FallbackLatest)
}
