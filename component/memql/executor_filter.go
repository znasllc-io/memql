package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	coreid "github.com/znasllc-io/memql/core/id"
)

// constantBoolExpression is an internal post-filter node carrying a
// pre-resolved boolean. `actor.<field>` comparisons are query-time constants
// (the resolved auth envelope) and are already enforced once in the SQL WHERE
// clause; resolveActorComparisonsToConstants folds them to a constant here so
// the ctx-less post-filter evaluator preserves the AND/OR truth value without
// re-resolving actor state it cannot see (#1659). It is never produced by the
// parser -- only by the pre-post-filter rewrite, the caller-flag fold
// (memql#4814), the row-authz lowering, and plan-constant replacement.
type constantBoolExpression struct {
	value bool

	// planConstant marks a constant that a plan constant produced
	// (PlanConstExpression, expr_plan_const.go). Only these fold the logical
	// operators around them at expansion (TRUE && x -> x, FALSE || x -> x,
	// ...), and only these let a deciding left operand skip expanding its
	// right one. The older producers keep the tree shape they have always had
	// -- TestFilterArgReferenceOnTheLeftHandSideIsBound pins the caller-flag
	// constant sitting inside its disjunction -- which is behaviourally
	// identical, so folding them too would be churn rather than a fix.
	//
	// NOT part of the node's identity: canonicalExpression renders the value
	// alone, because two plans that differ only in where a constant came from
	// select the same rows.
	planConstant bool
}

func (*constantBoolExpression) isExpressionNode() {}

// nodeMatchesExpression evaluates a predicate tree against one row: the
// in-process twin of tryCompileCombinedFilter, which executeCombinedFilterQuery
// re-runs on every candidate the SQL scan returned.
func nodeMatchesExpression(node memorynodes.MemoryNode, expr ExpressionNode, payloadCache map[string]map[string]any) (bool, error) {
	return nodeMatchesExpressionIn(node, expr, nil, payloadCache)
}

// nodeMatchesExpressionIn is nodeMatchesExpression with the collection element
// in scope (nil at row level): the one evaluator for both the row and an
// ArrayPredicateExpression's element predicate, so the two cannot drift into
// different comparison rules. A comparison on arrayElementRoot reads the
// element; every other node reads the row exactly as it does at the top.
func nodeMatchesExpressionIn(node memorynodes.MemoryNode, expr ExpressionNode, elem *arrayElementFrame, payloadCache map[string]map[string]any) (bool, error) {
	if expr == nil {
		return true, nil
	}
	switch n := expr.(type) {
	case *constantBoolExpression:
		return n.value, nil
	case *accountScopeMatch:
		// The post-filter twin of the SQL arm above. It must exist and it
		// must AGREE: a tree the combined compiler refuses falls to the
		// split evaluator, and a node the split evaluator does not know
		// fails the whole read with "unsupported expression node" -- which
		// on an authorization disjunct would present as "my client's work
		// disappeared" rather than as an error anybody could act on.
		return accountScopeMatchesNode(node, n, payloadCache), nil
	case *LiteralValueNode:
		// A literal in predicate position acts as a boolean constant via
		// truthiness -- the post-filter counterpart to constantBoolExpression
		// (#1705). Mirrors how the spec evaluator treats a literal (specTruthy)
		// so the SQL-compile path and the in-process post-filter agree.
		return literalTruthy(n.Value), nil
	case *ComparisonExpression:
		if isArrayElementField(n.Field) {
			return elementMatchesComparison(elem, n)
		}
		return nodeMatchesComparison(node, n, payloadCache)
	case *LogicalExpression:
		left, err := nodeMatchesExpressionIn(node, n.Left, elem, payloadCache)
		if err != nil {
			return false, err
		}
		right, err := nodeMatchesExpressionIn(node, n.Right, elem, payloadCache)
		if err != nil {
			return false, err
		}
		switch n.Op {
		case LogicalAnd:
			return left && right, nil
		case LogicalOr:
			return left || right, nil
		default:
			return false, fmt.Errorf("unsupported logical operator %q", n.Op)
		}
	case *NotExpression:
		// The twin of compileNotSQL. Exact two-valued negation: every arm of
		// this evaluator already answers false wherever the SQL answers NULL
		// (the absence rules), so negating the answer here is what NOT
		// COALESCE(<sql>, FALSE) computes there.
		if n.Target == nil {
			return false, fmt.Errorf("`!` has no operand")
		}
		match, err := nodeMatchesExpressionIn(node, n.Target, elem, payloadCache)
		if err != nil {
			return false, err
		}
		return !match, nil
	case *ArrayPredicateExpression:
		return nodeMatchesArrayPredicate(node, n, elem, payloadCache)
	case *PlanConstExpression:
		return false, errPlanConstantUnevaluated(n)
	default:
		return false, fmt.Errorf("unsupported expression node %T", expr)
	}
}

// tryCompileCombinedFilter attempts to compile an entire expression tree into a single SQL filter.
// This is an optimization for AND expressions - instead of running separate queries and intersecting
// results in Go, we combine the WHERE clauses into a single database query.
// Returns (compiledExpression, true) if successful, or (empty, false) if the expression contains
// nodes that cannot be combined (e.g., RelationshipExpression, BuiltinFunctionExpression).
func (e *MemQLEngine) tryCompileCombinedFilter(ctx context.Context, expr ExpressionNode, conceptContext string) (compiledExpression, bool) {
	return e.tryCompileCombinedFilterIn(ctx, expr, conceptContext, nil)
}

// tryCompileCombinedFilterIn is tryCompileCombinedFilter with the collection
// element in scope (nil at row level) -- the SQL half of
// nodeMatchesExpressionIn. An ArrayPredicateExpression compiles its element
// predicate through this same function in a child scope, so a comparison on
// the element gets exactly the compiler a payload field gets.
func (e *MemQLEngine) tryCompileCombinedFilterIn(ctx context.Context, expr ExpressionNode, conceptContext string, scope *sqlElementScope) (compiledExpression, bool) {
	if expr == nil {
		return compiledExpression{}, false
	}

	switch node := expr.(type) {
	case *LogicalExpression:
		leftFilter, leftOk := e.tryCompileCombinedFilterIn(ctx, node.Left, conceptContext, scope)
		if !leftOk {
			return compiledExpression{}, false
		}
		rightFilter, rightOk := e.tryCompileCombinedFilterIn(ctx, node.Right, conceptContext, scope)
		if !rightOk {
			return compiledExpression{}, false
		}

		// Combine the filters with the appropriate SQL operator
		var sqlOp string
		switch node.Op {
		case LogicalAnd:
			sqlOp = "AND"
		case LogicalOr:
			sqlOp = "OR"
		default:
			return compiledExpression{}, false
		}

		combinedSQL := fmt.Sprintf("(%s %s %s)", leftFilter.sql, sqlOp, rightFilter.sql)
		combinedArgs := append(leftFilter.args, rightFilter.args...)
		return compiledExpression{sql: combinedSQL, args: combinedArgs}, true

	case *ComparisonExpression:
		// A comparison on the collection element in scope (expr_collection_sql.go).
		// Outside a collection predicate there is no element to read, and the
		// compile refuses rather than reading one.
		if isArrayElementField(node.Field) {
			filter, err := compileElementComparison(node, scope)
			if err != nil {
				return compiledExpression{}, false
			}
			return filter, true
		}
		// An `actor.<field>` comparison (e.g. `actor.role == "admin"`) is a
		// constant at query time -- the resolved auth envelope. Bind the
		// actor value + the comparison value as SQL parameters so the term
		// can sit inside an OR/AND with row predicates and push down in one
		// query (#974). This is what lets a mixed predicate like
		// `payload.stage=="won" || actor.role=="admin"` compile to a single
		// `(... OR ? = ?)` instead of needing a split evaluation.
		if isActorFieldComparison(node) {
			filter, err := compileActorFieldComparison(ctx, node)
			if err != nil {
				return compiledExpression{}, false
			}
			return filter, true
		}
		filter, err := e.compileComparisonExpressionWithContext(node, conceptContext)
		if err != nil {
			return compiledExpression{}, false
		}
		return filter, true

	case *constantBoolExpression:
		// A term already decided for the whole scan -- a folded caller-flag
		// comparison (`args.includeArchived==true`, memql#4814). It has to
		// compile, not merely post-filter: the shape that produces it is a
		// DISJUNCT beside row predicates, and a tree the combined compiler
		// refuses falls to the split evaluator, which has no notion of a
		// node-set for a constant and fails the read with "unsupported
		// expression node". TRUE / FALSE keeps the whole filter in one query
		// and lets Postgres fold the term away.
		if node.value {
			return compiledExpression{sql: "TRUE"}, true
		}
		return compiledExpression{sql: "FALSE"}, true

	case *accountScopeMatch:
		// The account grant, lowered for this request (epic memql#5165).
		//
		// ONE EXPRESSION FOR BOTH FIELD SHAPES. `jsonb_exists_any` is true
		// when any of the given strings is a top-level key, an array
		// element, OR the jsonb value itself as a string -- so one call
		// answers for the scalar declaration (`accountId`) and for the list
		// one (`accountIds`), with nothing here needing to know which the
		// concept declared. That is what lets `account="<field>"` be one
		// argument rather than two.
		//
		// The FUNCTION form rather than the `?|` operator, and that is not
		// style: bun renders `?` as its own placeholder, so an operator
		// spelled `?|` in a query string is consumed as a parameter marker
		// and the statement no longer means what it reads as. (The payload
		// `in` compilation carried the same reasoning until memql#5366 took
		// its array-overlap disjunct out: `in` is typed equality against
		// each element now, and a scalar field never overlaps a list.)
		//
		// The "top-level key" half of the behaviour is also why the
		// LOAD-TIME type check is load-bearing rather than tidy: a field
		// declared `object` would admit any row whose map happened to carry
		// an account id as a key. validateRowAuthzAccount refuses that
		// declaration, and this arm relies on it having done so.
		jsonbExpr, err := buildJSONBPathExpression([]string{node.field})
		if err != nil {
			return compiledExpression{}, false
		}
		if node.everyAccount {
			// Staff (D6): every TIED row, and no untied one. Spelled as
			// "an array with elements, or a non-empty string" rather than
			// "not null", because a row carrying `""` or `[]` is untied and
			// admitting it would hand staff rows that belong to no client.
			// Type names are bound params (not adjacent SQL string literals) so
			// a path expression cannot be misread as breaking out of quotes.
			return compiledExpression{
				sql: fmt.Sprintf(
					"((jsonb_typeof(%s) = ? AND jsonb_array_length(%s) > 0) OR "+
						"(jsonb_typeof(%s) = ? AND (%s #>> '{}') <> ''))",
					jsonbExpr, jsonbExpr, jsonbExpr, jsonbExpr),
				args: []any{"array", "string"},
			}, true
		}
		if len(node.accounts) == 0 {
			return compiledExpression{sql: "FALSE"}, true
		}
		return compiledExpression{
			sql:  fmt.Sprintf("jsonb_exists_any(%s, ?::text[])", jsonbExpr),
			args: []any{pq.Array(node.accounts)},
		}, true

	case *SpecReferenceExpression:
		// Expand the spec inline and try to compile its expression.
		// This allows specs like specIsActiveRecord (payload.active==true) to be
		// combined with concept filters into a single SQL query, avoiding separate
		// queries whose results are intersected with limited row counts.
		if e.specs == nil {
			return compiledExpression{}, false
		}
		spec, err := e.specs.Get(node.Name)
		if err != nil {
			return compiledExpression{}, false
		}
		return e.tryCompileCombinedFilterIn(ctx, spec.Expr, conceptContext, scope)

	case *NotExpression:
		// Compiles exactly when its operand does (expr_not.go). A NOT over an
		// operand this function refuses -- a relationship traversal -- is
		// refused with it, and evaluateExpressionSetWithContext then names it
		// rather than computing a set complement.
		if node.Target == nil {
			return compiledExpression{}, false
		}
		inner, ok := e.tryCompileCombinedFilterIn(ctx, node.Target, conceptContext, scope)
		if !ok {
			return compiledExpression{}, false
		}
		return compileNotSQL(inner), true

	case *ArrayPredicateExpression:
		filter, err := e.compileArrayPredicate(ctx, node, conceptContext, scope)
		if err != nil {
			return compiledExpression{}, false
		}
		return filter, true

	case *PlanConstExpression:
		// Never compiled: argument expansion replaces every plan constant
		// before a tree reaches SQL. One that got here was never expanded,
		// and there is no value to bind for it.
		return compiledExpression{}, false

	default:
		// RelationshipExpression, BuiltinFunctionExpression, etc. cannot be combined
		return compiledExpression{}, false
	}
}

// isActorFieldComparison reports whether a comparison's left-hand side is an
// `actor.<field>` accessor (the resolved auth envelope), e.g. `actor.role`.
func isActorFieldComparison(node *ComparisonExpression) bool {
	if node == nil {
		return false
	}
	return len(node.Field.Parts) >= 2 && strings.EqualFold(strings.TrimSpace(node.Field.Parts[0]), "actor")
}

// compileActorFieldComparison compiles an `actor.<field> <op> <value>` term to
// a bound-parameter SQL fragment. The actor field is resolved from the request's
// AccessContext (a query-time constant), so both operands bind as parameters and
// the DB evaluates the constant comparison -- correct inside any OR/AND nesting.
func compileActorFieldComparison(ctx context.Context, node *ComparisonExpression) (compiledExpression, error) {
	subPath := strings.Join(node.Field.Parts[1:], ".")
	actorValue, err := resolveActorPath(ctx, subPath, node.Operator)
	if err != nil {
		return compiledExpression{}, err
	}
	sqlOp, err := sqlOperatorForComparison(node.Operator)
	if err != nil {
		return compiledExpression{}, err
	}
	return compiledExpression{
		sql:  fmt.Sprintf("? %s ?", sqlOp),
		args: []any{actorValue, node.Value},
	}, nil
}

// resolveActorComparisonsToConstants walks an expression tree and replaces
// every `actor.<field> <op> <value>` comparison with a constantBoolExpression
// carrying the comparison's query-time result. The actor value is resolved
// from the request's AccessContext via resolveActorPath -- the SAME resolver
// the SQL-compile path (compileActorFieldComparison) uses -- so the post-filter
// evaluates the gate identically to the WHERE clause (no divergence, gate
// enforced exactly once). Non-actor nodes pass through unchanged. Returns a NEW
// tree; the input AST may be cached upstream and must not be mutated (#1659).
func resolveActorComparisonsToConstants(ctx context.Context, expr ExpressionNode) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch n := expr.(type) {
	case *ComparisonExpression:
		if !isActorFieldComparison(n) {
			return expr, nil
		}
		subPath := strings.Join(n.Field.Parts[1:], ".")
		actorValue, err := resolveActorPath(ctx, subPath, n.Operator)
		if err != nil {
			return nil, err
		}
		match, err := compareScalarValues(actorValue, n.Operator, n.Value)
		if err != nil {
			return nil, err
		}
		return &constantBoolExpression{value: match}, nil
	case *LogicalExpression:
		left, err := resolveActorComparisonsToConstants(ctx, n.Left)
		if err != nil {
			return nil, err
		}
		right, err := resolveActorComparisonsToConstants(ctx, n.Right)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{Op: n.Op, Left: left, Right: right}, nil
	case *NotExpression:
		// `!(actor.role == "admin")` is as constant as the comparison it
		// negates, and nodeMatchesComparison has no actor arm to fall back on
		// -- an unfolded actor term under a NOT fails the read the way #1659's
		// did at the top level.
		target, err := resolveActorComparisonsToConstants(ctx, n.Target)
		if err != nil {
			return nil, err
		}
		return &NotExpression{Target: target}, nil
	case *ArrayPredicateExpression:
		// An element predicate may carry an actor term beside its element
		// comparisons; the in-process twin reads it through
		// nodeMatchesComparison, which needs it folded exactly as above.
		pred, err := resolveActorComparisonsToConstants(ctx, n.Pred)
		if err != nil {
			return nil, err
		}
		copied := *n
		copied.Pred = pred
		return &copied, nil
	default:
		return expr, nil
	}
}

// combinedFilterOrderExprs returns the SQL ORDER BY expressions for the
// combined-filter scan. The result MUST end with `id ASC`: the keyset cursor
// predicate's tie-breaker is `id > ?`, which is only correct when rows sharing
// an identical createdAt are ordered by `id ASC` — otherwise two equal-timestamp
// rows straddling a page boundary can be skipped or duplicated. A compiled
// sorter always appends the `createdAt DESC, id ASC` fallback (compileSortFields),
// so its orderExpressions() are airtight; the nil-sorter fallback below must
// match that same contract, not just `createdAt DESC`. Centralizing the order
// list here keeps the keyset predicate and the SQL row order from drifting apart.
func combinedFilterOrderExprs(sorter *compiledSort) []string {
	if sorter != nil {
		return sorter.orderExpressions()
	}
	return []string{`"createdAt" DESC`, `id ASC`}
}

// executeCombinedFilterQuery executes a query using a combined SQL filter.
// This is similar to executeFilterQuery but doesn't require a ComparisonExpression node.
// staged-data: GATE -- and the gate for this seam is memql#3983's, not this
// file's (epic memql#3974; the direct-SQL inventory is memql#3984).
//
// This is one of the two ENGINE SEAMS, so unlike the hand-rolled reads in
// integrations/ it does go through the parser: memql#3983 ANDs a
// `row.concept != <staged>` conjunct into plan.Root before the filter is
// compiled, and separately admits every emitted row against its own concept.
// The `filter` argument arrives here already carrying that term.
//
// A second, local check here would be a SECOND source of truth over the same
// rows -- and a per-seam one, which is worse than a per-module one, because the
// two would be edited by different changes. Adjudicated as GATE, enforced one
// layer up, deliberately not enforced twice.
//
// The load-bearing detail, recorded because an SQL-only reading of this seam is
// wrong: latestMatchingNodes reloads each scanned candidate's true latest
// version through loadLatestNodes, which filters on id and an optional
// createdAt and NOTHING ELSE, and then INSTALLS that version as the result. So
// the predicate has to survive into the in-process re-check as well, which is
// why memql#3983 spells it as a comparison Go can evaluate and co-gates the
// swap directly. See the note on loadLatestNodes.
func (e *MemQLEngine) executeCombinedFilterQuery(ctx context.Context, expr ExpressionNode, filter compiledExpression, timestamp *time.Time, target int, sorter *compiledSort) ([]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	// buildScan constructs the latest-per-id query at a given row limit.
	//
	// THE SQL ENUMERATION HAS TO BE THE RESULT SET (memql#3388).
	//
	// MemoryNodes is append-only: one id carries one row per version, and this
	// read returns the LATEST row per id. Scanning RAW rows and collapsing them
	// afterwards puts a single id at MANY positions in the scanned order, and
	// keyset pagination needs a total order over the rows it actually returns.
	// With the two orders apart, no cursor can be right: minted from the
	// collapsed row it resumes at that row's latest timestamp and skips
	// everything the scan had not reached (silent loss, ascending); minted from
	// the raw scan position it reaches the same id again further down (silent
	// duplication, descending -- the walk that never terminated).
	//
	// So the collapse happens IN SQL -- `DISTINCT ON (id) ... ORDER BY id,
	// "createdAt" DESC` in a subquery, re-sorted by the declared ordering
	// outside it. Each id then occupies exactly one position, the LIMIT bounds
	// DISTINCT rows instead of raw versions, and the `(createdAt, id)` keyset
	// predicate is correct over the enumeration it pages.
	//
	// A closure rather than one mutated builder: the window below can widen and
	// re-scan, and a bun query carries both its LIMIT and the destination slice,
	// so each attempt needs a fresh builder and a fresh slice.
	buildScan := func(rows *[]memorynodes.MemoryNode, limit int) *bun.SelectQuery {
		// The collapse. `memory_nodes_id_created_at_desc_idx` is `(id,
		// "createdAt" DESC)`, exactly this ordering, so the subquery reads the
		// index rather than sorting.
		//
		// The filter and the asOf timestamp ride INSIDE it: a bare collapse
		// would read every row in the table. For an id whose latest version
		// still matches the filter -- every id this query can return -- the
		// subquery yields that latest version, so its enumeration position is
		// its own (createdAt, id) and the cursor is exact. For an id whose
		// latest version no longer matches, the subquery yields the newest
		// version that does, and the post-filter below drops the id either way;
		// only the position of an already-discarded candidate differs.
		latest := db.NewSelect().
			Model((*memorynodes.MemoryNode)(nil)).
			DistinctOn("id").
			OrderExpr(`id ASC, "createdAt" DESC`)

		if filter.sql != "" {
			latest = latest.Where(filter.sql, filter.args...)
		}
		if timestamp != nil {
			latest = latest.Where(`"createdAt" <= ?`, timestamp.UTC())
		}

		q := db.NewSelect().Model(rows).ModelTableExpr("(?) AS mn", latest)

		// Keyset cursor (5.12): when a continuation cursor is present, push the
		// keyset predicate `(createdAt, id) <keyset> (?, ?)` into SQL so deep
		// pages continue from the encoded position instead of scanning +
		// discarding an offset window. It rides OUTSIDE the collapse, over the
		// latest-per-id rows -- the only order the cursor position is meaningful
		// in. The filter + asOf WHERE (and the per-row authz the filter carries)
		// are already applied inside; ordered by the declared sort below.
		if keyset, hasKeyset := keysetFromContext(ctx); hasKeyset {
			if eligible, createdAtDesc := keysetEligibleSort(sorter); eligible {
				predicate, args := keysetWhere(keyset, createdAtDesc)
				q = q.Where(predicate, args...)
			}
			// A non-keyset-eligible ordering reaching the SQL path with a cursor
			// set pushes no predicate. The engine gates this upstream, so it is
			// a defensive no-op.
		}

		for _, expr := range combinedFilterOrderExprs(sorter) {
			if strings.TrimSpace(expr) == "" {
				continue
			}
			q = q.OrderExpr(expr)
		}

		return q.Limit(limit)
	}

	fetchTarget := target
	if fetchTarget <= 0 {
		fetchTarget = e.config.MaxWindow
	}
	if fetchTarget <= 0 {
		fetchTarget = e.config.MaxResults
	}

	maxWindow := e.config.MaxWindow
	if maxWindow < fetchTarget {
		maxWindow = fetchTarget
	}

	// Expand all spec references in the expression tree before post-filtering.
	// This is necessary because nodeMatchesExpression doesn't have access to the specs registry.
	expandedExpr, err := e.expandSpecReferences(expr)
	if err != nil {
		return nil, err
	}

	// Fold `actor.<field>` comparisons to their query-time boolean constant
	// before post-filtering (#1659). The actor gate is already enforced once in
	// the SQL WHERE clause (compileActorFieldComparison binds the resolved actor
	// value as a parameter); the ctx-less nodeMatchesExpression has no actor.*
	// handling, so without this fold an `actor.isClusterOwner==true` term blows
	// up with "field \"actor.isClusterOwner\" is not supported in queries"
	// whenever the scan returns a candidate row. Folding to a constant preserves
	// AND/OR truth values exactly while keeping the gate enforced exactly once.
	expandedExpr, err = resolveActorComparisonsToConstants(ctx, expandedExpr)
	if err != nil {
		return nil, err
	}

	// THE WINDOW STILL WIDENS, FOR A NARROWER REASON (memql#3388).
	//
	// The DISTINCT ON collapse makes the LIMIT bound DISTINCT ids, so the raw /
	// collapsed mismatch #3390 widened against is gone: `window` rows in means
	// `window` candidates out. What remains is the in-memory post-filter below,
	// which re-evaluates the predicate against each candidate's TRUE latest
	// version. An id whose latest version no longer matches (it was renamed,
	// archived, reassigned) is dropped there, and a page short of its limit is
	// read as exhaustion by the nextCursor block in engine.go -- the cursor is
	// withdrawn and everything past it becomes unreachable.
	//
	// So the loop now measures what the caller actually receives: widen until
	// the window yields `fetchTarget` RESULTS, or the database returns a short
	// scan (genuine exhaustion), or the ceiling is reached. For data whose rows
	// keep matching -- the overwhelming common case -- the first attempt at
	// exactly `fetchTarget` fills the page and nothing widens.
	window := fetchTarget
	if window > maxWindow {
		window = maxWindow
	}

	var result []memorynodes.MemoryNode
	for {
		var nodes []memorynodes.MemoryNode
		if err := buildScan(&nodes, window).Scan(ctx); err != nil {
			return nil, err
		}

		result, err = e.latestMatchingNodes(ctx, nodes, expandedExpr, timestamp, target)
		if err != nil {
			return nil, err
		}

		if len(result) >= fetchTarget || len(nodes) < window || window >= maxWindow {
			break
		}
		next := window * 4
		if next <= window || next > maxWindow {
			next = maxWindow
		}
		window = next
	}

	return result, nil
}

// latestMatchingNodes resolves each scanned candidate to its TRUE latest
// version (respecting the asOf timestamp) and keeps the ones the expression
// still matches, preserving the scan order and stopping at target.
//
// The scan already collapsed to one row per id, but that row is the newest
// version matching the SQL filter, which is the latest version only while the
// predicate still holds on it. Reloading is what makes "the latest row
// satisfies the predicate" the semantics of the read rather than "some version
// did".
func (e *MemQLEngine) latestMatchingNodes(
	ctx context.Context,
	nodes []memorynodes.MemoryNode,
	expr ExpressionNode,
	timestamp *time.Time,
	target int,
) ([]memorynodes.MemoryNode, error) {
	uniqueIds := make([]string, 0, len(nodes))
	idSet := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		if _, seen := idSet[id]; !seen {
			idSet[id] = struct{}{}
			uniqueIds = append(uniqueIds, id)
		}
	}

	var latest map[string]memorynodes.MemoryNode
	if len(uniqueIds) > 0 {
		loaded, err := e.loadLatestNodes(ctx, uniqueIds, timestamp)
		if err != nil {
			return nil, err
		}
		latest = loaded
	}

	payloadCache := make(map[string]map[string]any)
	seen := make(map[string]struct{})
	result := make([]memorynodes.MemoryNode, 0, len(uniqueIds))

	// Resolved ONCE, outside the loop (memql#4040). Per-candidate resolution
	// would repeat an actor-envelope read for every row and, worse, would let
	// two rows of one read be decided against two different answers if the
	// context were ever swapped mid-scan.
	stagedScope := e.stagedScopeFor(ctx)

	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}

		candidate := node
		if latestNode, ok := latest[id]; ok {
			candidate = latestNode
		}

		// THE SWAP CO-GATE (staged data, memql#3983).
		//
		// The line above is the one that makes an SQL-only visibility gate a
		// lie. loadLatestNodes filters on `id IN (?)` and an optional
		// `createdAt <= ?` and NOTHING else -- no concept, no filter, no authz
		// -- so it can hand back a version the scan's WHERE clause would never
		// have returned, and the assignment above installs it as the result.
		//
		// nodeMatchesExpression below re-runs the full expression and would
		// reject a staged candidate on its own, because the injected conjunct
		// compares `concept` and that is evaluable in process. This gate does
		// not rely on that: `expr` only carries the conjunct when something
		// injected one, and the unbound plans (memql#3981 measured 115 of 619
		// constructs binding no concept) are exactly the ones where nothing
		// did. Staging is a pure function of the row's concept, so asking the
		// row directly is both cheaper and complete.
		if !e.admitStagedRow(stagedScope, candidate) {
			continue
		}

		match, err := nodeMatchesExpression(candidate, expr, payloadCache)
		if err != nil {
			return nil, err
		}
		if !match {
			continue
		}

		seen[id] = struct{}{}
		result = append(result, candidate)
		if target > 0 && len(result) >= target {
			break
		}
	}

	return result, nil
}

func (e *MemQLEngine) compileComparisonExpressionWithContext(expr *ComparisonExpression, conceptContext string) (compiledExpression, error) {
	if expr == nil {
		return compiledExpression{}, fmt.Errorf("comparison expression is nil")
	}

	if len(expr.Field.Parts) == 0 {
		return compiledExpression{}, fmt.Errorf("comparison field is missing")
	}

	field := strings.TrimSpace(expr.Field.Parts[0])

	if info, ok := resolveIntrinsicField(field); ok && len(expr.Field.Parts) == 1 {
		switch info.kind {
		case intrinsicFieldConcept:
			return e.compileConceptComparison(expr.Operator, expr.Value)
		case intrinsicFieldId:
			return compileIdComparison(expr.Operator, expr.Value, conceptContext)
		case intrinsicFieldType:
			return compileTypeComparison(expr.Operator, expr.Value)
		case intrinsicFieldCreatedAt:
			return compileCreatedAtComparison(expr.Operator, expr.Value)
		case intrinsicFieldCreatedBy:
			return compileCreatedByComparison(expr.Operator, expr.Value)
		default:
			return compiledExpression{}, fmt.Errorf("field %q is not supported", expr.Field.Raw)
		}
	}

	if strings.EqualFold(field, "provenance") {
		// `provenance` (bare) compares the entire JSON object — not
		// supported as a filter target. Authors compare on a leaf
		// (provenance.kind, .name, .trigger, .via).
		if len(expr.Field.Parts) < 2 {
			return compiledExpression{}, fmt.Errorf("provenance filters require a leaf path (e.g. provenance.kind)")
		}
		return compileProvenanceComparison(expr.Field.Parts[1], expr.Operator, expr.Value)
	}

	if strings.EqualFold(field, "payload") {
		if len(expr.Field.Parts) < 2 {
			return compiledExpression{}, fmt.Errorf("payload field must include a property path (e.g., payload.status)")
		}
		return compilePayloadComparison(expr.Field.Parts[1:], expr.Operator, expr.Value)
	}

	return compiledExpression{}, fmt.Errorf("field %q is not supported", expr.Field.Raw)
}

// canonicalizeRelationshipComparisons walks an expression tree and
// rewrites every payload-field comparison RHS to canonical id form
// when the LHS is an outgoing @relationship on the conceptContext
// concept. Returns a NEW tree (mutating the caller's tree would
// corrupt cached query plans).
//
// Symmetric counterpart to canonicalizeRelationshipFields (which
// runs on insert): inserts store canonical, queries should compare
// canonical. Without this pass, a bare-slug RHS like
// `payload.partitionId == "daily-..."` misses canonical-stored rows.
//
// The pre-walk runs once before SQL compile + post-filter, so both
// paths see the same canonical RHS -- avoids a class of "SQL returns
// 1 row, post-filter rejects it" bugs that would arise if only one
// side rewrote the value.
//
// No concept context (top-level expressions with no `concept==X`
// constraint), no relationship match, or non-string RHS: pass
// through unchanged.
//
// ROW-AUTHZ (memql#3172). A node carrying RowAuthzConcept names its own
// concept and is canonicalized from THAT, whatever conceptContext says
// -- including when conceptContext is empty. The injected owner term is
// exactly the node whose concept cannot be read off the filter: the
// tier is resolved from the construct's declared binding, so a filter
// spelled `a || b`, `row.id == ...` or `concept != ...` produces no
// concept context at all, and an owner field is an @relationship, so an
// uncanonicalized RHS compares the bare `actor.userId` against a stored
// `v1:identity:user:<id>` and matches NOTHING -- the owner's own rows
// included. Routed through this existing pass rather than a second
// canonicalizer so read and write cannot drift about what "canonical"
// means.
func (e *MemQLEngine) canonicalizeRelationshipComparisons(ctx context.Context, expr ExpressionNode, conceptContext string) ExpressionNode {
	if expr == nil || e == nil || e.concepts == nil {
		return expr
	}
	switch n := expr.(type) {
	case *ComparisonExpression:
		if n == nil {
			return expr
		}
		// ROW-AUTHZ, self-owned form (`@rowAuthz(owner="id")`,
		// memql#3029). That tier's term names the ROW'S OWN identity
		// rather than a payload field, so it is `row.id == actor.userId`
		// and not a `payload.<field>` comparison -- but it has the same
		// spelling problem: the id column stores canonical ids and
		// actor.userId resolves to the bare form. The generic id
		// short->full resolution downstream keys off the filter-derived
		// concept context, which is "" for exactly the spellings
		// enforcement is resolved from the declaration for. Canonicalize
		// it here, against the concept the DECLARATION names, so the two
		// forms of one tier cannot disagree.
		//
		// Inert today (no concept declares the self-owned form; #3029 is
		// what unblocks it on v1:identity:user) and covered by
		// TestSelfOwnedInjectedPredicateIsCanonicalised, which declares
		// one synthetically rather than waiting for the day it bites.
		if concept := strings.TrimSpace(n.RowAuthzConcept); concept != "" && isRowIdName(n.Field) {
			str, isStr := n.Value.(string)
			if !isStr || strings.TrimSpace(str) == "" {
				return expr
			}
			canon, cerr := e.canonicalizeIdValue(ctx, str, concept)
			if cerr != nil || canon == "" || canon == str {
				return expr
			}
			rewritten := *n
			rewritten.Value = canon
			return &rewritten
		}
		// payload.<field> == <value>  -- the only other shape we rewrite.
		// A `startsWith` over a relationship field is NOT one: its value is a
		// prefix, and composing a prefix against the target concept would
		// manufacture an id that matches nothing (memql#4208).
		if n.Operator == OpStartsWith || len(n.Field.Parts) != 2 {
			return expr
		}
		if !strings.EqualFold(strings.TrimSpace(n.Field.Parts[0]), "payload") {
			return expr
		}
		// The node's own declaration wins over the filter-derived
		// context; see the row-authz note above.
		concept := strings.TrimSpace(n.RowAuthzConcept)
		if concept == "" {
			concept = conceptContext
		}
		if concept == "" {
			return expr
		}
		fieldName := strings.TrimSpace(n.Field.Parts[1])
		canon, ok := e.canonicalizeRelationshipFieldValue(ctx, concept, fieldName, n.Value)
		if !ok {
			return expr
		}
		// Build a copy with the rewritten value. Don't touch the
		// original -- the AST may be cached upstream.
		rewritten := *n
		rewritten.Value = canon
		return &rewritten
	case *LogicalExpression:
		if n == nil {
			return expr
		}
		left := e.canonicalizeRelationshipComparisons(ctx, n.Left, conceptContext)
		right := e.canonicalizeRelationshipComparisons(ctx, n.Right, conceptContext)
		if left == n.Left && right == n.Right {
			return expr
		}
		copy := *n
		copy.Left = left
		copy.Right = right
		return &copy
	case *NotExpression:
		if n == nil {
			return expr
		}
		target := e.canonicalizeRelationshipComparisons(ctx, n.Target, conceptContext)
		if target == n.Target {
			return expr
		}
		return &NotExpression{Target: target}
	case *ArrayPredicateExpression:
		if n == nil {
			return expr
		}
		// Row comparisons inside the element predicate get the ordinary
		// treatment above. The element comparisons get the one this pass
		// already gives `args.x in row.<field>` (OpHas, the payload arm):
		// when the array is an @relationship field its elements are stored
		// canonical, so `row.memberIds.any(m => m == args.userId)` compares
		// against the canonical id too. Without it the two spellings of one
		// question would disagree -- the membership form matching and the
		// lambda form silently matching nothing for a bare id.
		pred := e.canonicalizeRelationshipComparisons(ctx, n.Pred, conceptContext)
		if concept := strings.TrimSpace(conceptContext); concept != "" && len(n.Field.Parts) == 2 &&
			strings.EqualFold(strings.TrimSpace(n.Field.Parts[0]), "payload") {
			pred = e.canonicalizeElementComparisons(ctx, pred, concept, strings.TrimSpace(n.Field.Parts[1]))
		}
		if pred == n.Pred {
			return expr
		}
		copied := *n
		copied.Pred = pred
		return &copied
	default:
		// RelationshipExpression, SpecReferenceExpression,
		// FunctionCallExpression, etc. don't carry comparison values
		// directly; pass through. (Spec bodies get canonicalized
		// when they're expanded inline in the comparable arm above.)
		return expr
	}
}

// canonicalizeElementComparisons rewrites the value of every comparison on the
// BARE element (`$elem`, not a path under it) of a relationship array field to
// its canonical id, walking the connectives of one element predicate. A nested
// collection predicate is a different array and is left to its own pass; a
// `startsWith` value is a prefix, which composing against a concept would turn
// into an id that matches nothing (memql#4208's rule for the payload arm).
func (e *MemQLEngine) canonicalizeElementComparisons(ctx context.Context, expr ExpressionNode, conceptName, fieldName string) ExpressionNode {
	switch n := expr.(type) {
	case *ComparisonExpression:
		if n == nil || n.Operator == OpStartsWith || len(n.Field.Parts) != 1 || !isArrayElementField(n.Field) {
			return expr
		}
		canon, ok := e.canonicalizeRelationshipFieldValue(ctx, conceptName, fieldName, n.Value)
		if !ok {
			return expr
		}
		rewritten := *n
		rewritten.Value = canon
		return &rewritten
	case *LogicalExpression:
		if n == nil {
			return expr
		}
		left := e.canonicalizeElementComparisons(ctx, n.Left, conceptName, fieldName)
		right := e.canonicalizeElementComparisons(ctx, n.Right, conceptName, fieldName)
		if left == n.Left && right == n.Right {
			return expr
		}
		return &LogicalExpression{Op: n.Op, Left: left, Right: right}
	case *NotExpression:
		if n == nil {
			return expr
		}
		target := e.canonicalizeElementComparisons(ctx, n.Target, conceptName, fieldName)
		if target == n.Target {
			return expr
		}
		return &NotExpression{Target: target}
	default:
		return expr
	}
}

// resolveCanonicalIdComparisons walks an expression tree and replaces
// any comparison RHS that is still an unresolved `*ast.CanonicalIdExpr`
// (i.e. an inlined `canonicalId(<value>, "<concept>")` in a query
// filter) with its resolved canonical-id string. Returns a NEW tree
// when anything was rewritten (mutating the caller's tree would corrupt
// cached query plans); otherwise the original node is returned.
//
// Why this pass exists (#1109): when a `.memql` query filter inlines
// `canonicalId(...)` on a comparison RHS and the WHERE chain also
// includes a bool comparison, the typed `*ast.CanonicalIdExpr` survives
// all the way to the literal evaluator (normalizeScalarValue /
// compareEquality), which has no case for it and fails the whole query
// with `unsupported literal type *ast.CanonicalIdExpr`. The mutation /
// automation runtime already resolves the same node via
// canonicalizeIdValue (mutation_templates.go), and the `now()` /
// `*ast.TimestampExprFunc` literal gets the analogous lazy substitution
// in the three comparison branches -- this is the query-filter
// counterpart for canonicalId. Runs before SQL compile + post-filter so
// both paths see the same resolved string.
//
// A node whose inner value can't be resolved to a non-empty string is
// passed through unchanged so the downstream evaluator surfaces the
// original (well-understood) error instead of a partially-rewritten tree.
func (e *MemQLEngine) resolveCanonicalIdComparisons(ctx context.Context, expr ExpressionNode) ExpressionNode {
	if expr == nil || e == nil {
		return expr
	}
	switch n := expr.(type) {
	case *ComparisonExpression:
		if n == nil {
			return expr
		}
		cid, ok := n.Value.(*ast.CanonicalIdExpr)
		if !ok {
			return expr
		}
		canon, ok := e.resolveCanonicalIdExprValue(ctx, cid)
		if !ok {
			return expr
		}
		rewritten := *n
		rewritten.Value = canon
		return &rewritten
	case *LogicalExpression:
		if n == nil {
			return expr
		}
		left := e.resolveCanonicalIdComparisons(ctx, n.Left)
		right := e.resolveCanonicalIdComparisons(ctx, n.Right)
		if left == n.Left && right == n.Right {
			return expr
		}
		clone := *n
		clone.Left = left
		clone.Right = right
		return &clone
	case *RelationshipExpression:
		if n == nil {
			return expr
		}
		target := e.resolveCanonicalIdComparisons(ctx, n.Target)
		if target == n.Target {
			return expr
		}
		return &RelationshipExpression{Function: n.Function, Target: target, Label: n.Label}
	case *NotExpression:
		if n == nil {
			return expr
		}
		target := e.resolveCanonicalIdComparisons(ctx, n.Target)
		if target == n.Target {
			return expr
		}
		return &NotExpression{Target: target}
	case *ArrayPredicateExpression:
		if n == nil {
			return expr
		}
		pred := e.resolveCanonicalIdComparisons(ctx, n.Pred)
		if pred == n.Pred {
			return expr
		}
		copied := *n
		copied.Pred = pred
		return &copied
	default:
		return expr
	}
}

// resolveCanonicalIdExprValue evaluates a `*ast.CanonicalIdExpr` node to
// its canonical-id string. Returns (value, true) on success; (_, false)
// when the inner value expression isn't a plain literal we can resolve
// here or canonicalization fails, so the caller leaves the tree alone.
func (e *MemQLEngine) resolveCanonicalIdExprValue(ctx context.Context, cid *ast.CanonicalIdExpr) (string, bool) {
	if cid == nil {
		return "", false
	}
	concept := strings.TrimSpace(cid.Concept)
	if concept == "" {
		return "", false
	}
	// In a query filter the inner Value arg is an already-inlined
	// literal (query construction substitutes concrete arg values
	// before the filter is built), so a *ast.LiteralExpr covers the
	// live shapes. Anything more exotic (a nested expression) is left
	// for the existing evaluator/error path rather than re-implementing
	// the full expression interpreter in this pre-walk.
	lit, ok := cid.Value.(*ast.LiteralExpr)
	if !ok {
		return "", false
	}
	str := strings.TrimSpace(fmt.Sprintf("%v", lit.Value))
	if str == "" {
		return "", false
	}
	canon, err := e.canonicalizeIdValue(ctx, str, concept)
	if err != nil || canon == "" {
		return "", false
	}
	return canon, true
}

// canonicalizeRelationshipFieldValue is the leaf helper for
// canonicalizeRelationshipComparisons. Returns (newValue, true) when
// the field has an outgoing @relationship and the value can be
// canonicalized, otherwise (originalValue, false) so callers can
// short-circuit AST cloning.
func (e *MemQLEngine) canonicalizeRelationshipFieldValue(ctx context.Context, conceptName, fieldName string, value any) (any, bool) {
	if conceptName == "" || fieldName == "" {
		return value, false
	}
	str, ok := value.(string)
	if !ok || strings.TrimSpace(str) == "" {
		return value, false
	}
	c, err := e.concepts.Get(conceptName)
	if err != nil || c == nil {
		return value, false
	}
	for _, rel := range c.Relationships {
		if !strings.EqualFold(strings.TrimSpace(rel.Direction), relationshipDirectionOutgoing) {
			continue
		}
		// EXACT, not EqualFold (memql#3654). The write path looks the field up
		// with an exact `payload[field]` map lookup, so a case-insensitive match
		// here made a mismatched field canonicalize on filter but NOT on write
		// -- writes landing non-canonical while reads looked correct. The load
		// gate now rejects a field whose case does not match the concept's
		// declaration, so the two sides agree on one spelling.
		//
		// The direction comparison above stays EqualFold: normalization
		// lowercases it, but fixtures that construct definitions directly
		// without going through Init rely on the fold.
		if strings.TrimSpace(rel.Field) != fieldName {
			continue
		}
		target := strings.TrimSpace(rel.TargetConcept)
		if target == "" {
			return value, false
		}
		canon, cerr := e.canonicalizeIdValue(ctx, str, target)
		if cerr != nil || canon == "" || canon == str {
			return value, false
		}
		return canon, true
	}
	return value, false
}

// extractConceptFromExpression walks an expression tree and extracts any concept equality constraint.
// Returns the concept name if found, or empty string if no concept constraint exists.
// Only extracts from concept==value expressions (not !=, IN, etc.) since those provide
// unambiguous context for short ID resolution.
//
// Each case guards against a typed-nil pointer inside a non-nil interface
// (e.g. a `var foo *ComparisonExpression = nil` stashed into an
// ExpressionNode): the outer `expr == nil` check returns false for that
// shape, the type switch still matches, and then `node.Field` dereferences
// nil. Seen in the wild on spaceParticipants when a compiled sub-
// expression had a nil branch -- panic'd the BFF.
func extractConceptFromExpression(expr ExpressionNode) string {
	if expr == nil {
		return ""
	}

	switch node := expr.(type) {
	case *ComparisonExpression:
		if node == nil {
			return ""
		}
		if len(node.Field.Parts) == 1 {
			field := strings.TrimSpace(node.Field.Parts[0])
			if info, ok := resolveIntrinsicField(field); ok && info.kind == intrinsicFieldConcept {
				// Only extract from equality comparisons
				if node.Operator == OpEq {
					if conceptName, ok := node.Value.(string); ok {
						return strings.TrimSpace(conceptName)
					}
				}
			}
		}
		return ""
	case *LogicalExpression:
		if node == nil {
			return ""
		}
		// For AND expressions, check both sides
		// For OR expressions, we can't reliably use concept context since
		// different branches might have different concepts
		if node.Op == LogicalAnd {
			if concept := extractConceptFromExpression(node.Left); concept != "" {
				return concept
			}
			return extractConceptFromExpression(node.Right)
		}
		return ""
	case *RelationshipExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *NotExpression, *ArrayPredicateExpression:
		// Deliberately NOT descended. `!(concept == X)` names the one concept
		// the rows are NOT of, and taking X as the context would compose a
		// bare short id against the wrong concept -- a filter that matches
		// nothing, or worse, the row it was written to exclude. An element
		// predicate reads an array, not the row's concept.
		return ""
	// Directive wrappers carry their filter on .Target. They normally
	// get peeled off into plan fields by planQuery before the filter
	// reaches this extractor -- but resolvePlanFunctions expands
	// FunctionCallExpression nodes AFTER planQuery has run, so a
	// function whose fn.Expr is shape(...) / sort(...) / paginate(...)
	// lands as plan.Root with the wrapper still on top. Descending
	// here lets the concept filter inside the wrapper still drive
	// partitionForConcept's _system routing for global concepts.
	case *ShapeExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *SortExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *PaginateExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *SelectExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *TimestampExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *DepthExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	case *CountExpression:
		if node == nil {
			return ""
		}
		return extractConceptFromExpression(node.Target)
	default:
		return ""
	}
}

func (e *MemQLEngine) compileConceptComparison(op ComparisonOperator, value any) (compiledExpression, error) {
	switch op {
	case OpEq, OpNe:
		conceptName, err := ensureString(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("concept comparison requires a string: %w", err)
		}
		sqlOp, err := sqlOperatorForComparison(op)
		if err != nil {
			return compiledExpression{}, err
		}
		conceptName = strings.TrimSpace(conceptName)

		// Read isolation: reject queries for concepts not in the
		// embedded registry. This mirrors the mutation path
		// (executeInsert calls concepts.Get and rejects unknown
		// concepts) and ensures a binary that doesn't embed a concept
		// schema can neither read nor write that concept's data.
		// Part of the per-binary security model for BFF nodes.
		if op == OpEq && e.concepts != nil {
			if _, err := e.concepts.Get(conceptName); err != nil {
				return compiledExpression{}, fmt.Errorf("concept %q not found in registry", conceptName)
			}
		}

		// NULL-safe inequality, matching the payload rule at
		// compilePayloadComparison (memql#1685): `!=` must MATCH a row whose
		// value is NULL, because NULL is logically DISTINCT FROM any concrete
		// value, and plain SQL `<>` yields NULL rather than true -- which
		// silently DROPS those rows. That is the isNotDeleted bug, and the
		// staged-data read gate (memql#3983) injects `row.concept!=<staged>`
		// as an AND-ed conjunct on every read, which is precisely the shape
		// that turns a dropped row into an emptied installation.
		//
		// `concept` is typed Go `string` and tagged `bun:",notnull"`, so no
		// real row can reach this with a NULL, and the two forms are identical
		// in practice today. That is what makes the change safe; it is not
		// what makes it worthwhile. The point is that the gate's safety stops
		// depending on a schema annotation staying true, and the cost of being
		// wrong about that annotation is silent data loss.
		//
		// No `!= ""` exception here, unlike the payload rule: that exception
		// exists because an ABSENT string field is logically EQUAL to "", and
		// a NOT NULL column has no absent case to reconcile.
		if op == OpNe {
			return compiledExpression{
				sql:  "(concept IS DISTINCT FROM ?)",
				args: []any{conceptName},
			}, nil
		}

		baseSql := fmt.Sprintf("(concept %s ?)", sqlOp)
		args := []any{conceptName}

		return compiledExpression{
			sql:  baseSql,
			args: args,
		}, nil
	case OpIn, OpOut:
		values, err := ensureStringSlice(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("concept comparison requires string values: %w", err)
		}
		if len(values) == 0 {
			return compiledExpression{}, fmt.Errorf("concept list must include at least one value")
		}

		placeholders := make([]string, 0, len(values))
		args := make([]any, 0, len(values))
		for _, name := range values {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				continue
			}
			// Read isolation: validate each concept in the list.
			if e.concepts != nil {
				if _, err := e.concepts.Get(trimmed); err != nil {
					return compiledExpression{}, fmt.Errorf("concept %q not found in registry", trimmed)
				}
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) == 0 {
			return compiledExpression{}, fmt.Errorf("concept list must include at least one value")
		}
		operator := "IN"
		if op == OpOut {
			operator = "NOT IN"
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(concept %s (%s))", operator, strings.Join(placeholders, ",")),
			args: args,
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for concept filters", op)
	}
}

func compileIdComparison(op ComparisonOperator, value any, conceptContext string) (compiledExpression, error) {
	switch op {
	case OpEq, OpNe:
		id, err := ensureString(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("id comparison requires a string: %w", err)
		}
		fullId, err := resolveFullId(strings.TrimSpace(id), conceptContext)
		if err != nil {
			return compiledExpression{}, err
		}
		if op == OpEq {
			return compiledExpression{sql: "(id = ?)", args: []any{fullId}}, nil
		}
		return compiledExpression{sql: "(id <> ?)", args: []any{fullId}}, nil
	case OpIn, OpOut:
		values, err := ensureStringSlice(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("id comparison requires string collection: %w", err)
		}
		if len(values) == 0 {
			return compiledExpression{}, fmt.Errorf("id list must include at least one value")
		}
		fullIds := make([]string, 0, len(values))
		for _, raw := range values {
			fullId, err := resolveFullId(strings.TrimSpace(raw), conceptContext)
			if err != nil {
				return compiledExpression{}, err
			}
			fullIds = append(fullIds, fullId)
		}
		if len(fullIds) == 0 {
			return compiledExpression{}, fmt.Errorf("id list must include at least one value")
		}
		if op == OpIn {
			return compiledExpression{sql: "(id IN (?))", args: []any{bun.In(fullIds)}}, nil
		}
		return compiledExpression{sql: "(id NOT IN (?))", args: []any{bun.In(fullIds)}}, nil
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for id filters", op)
	}
}

// resolveFullId resolves a potentially short ID to a full storage ID.
// A bare (colon-free) value is composed against the query's bound
// concept. A colon-bearing value must already BE a well-formed
// canonical id under the bound concept -- #2440 hardening: a
// wrong-concept id or a legacy prefixed form (e.g. the retired
// "<partition>:v1:..." shape) errors LOUDLY instead of silently
// matching zero rows. Mirrors the semantics of the engine's
// canonicalizeIdValue (partition_context.go) structurally, without
// needing the concept registry.
func resolveFullId(rawId string, conceptContext string) (string, error) {
	rawId = strings.TrimSpace(rawId)
	if rawId == "" {
		return "", fmt.Errorf("id cannot be empty")
	}

	if strings.Contains(rawId, ":") {
		gotConcept, shortId, err := coreid.ParseNodeId(rawId)
		if err != nil {
			return "", fmt.Errorf("malformed id %q in filter: %w", rawId, err)
		}
		// ParseNodeId tolerates (and drops) prefix segments before the
		// version segment -- the retired partition-prefixed form. A
		// stored id never carries such a prefix, so an exact-reassembly
		// mismatch means the caller holds a legacy/malformed id that
		// would silently match nothing.
		if coreid.BuildNodeId(gotConcept, shortId) != rawId {
			return "", fmt.Errorf("id %q is not a canonical {concept}:{shortId} id (legacy prefixed form?); pass the bare shortId or the canonical id", rawId)
		}
		// Wrong concept tag: the caller passed an id of a different
		// concept than the one this filter is bound to.
		if conceptContext != "" && gotConcept != "" && gotConcept != conceptContext {
			return "", fmt.Errorf("id %q is under concept %q, expected %q (caller passed the wrong id type)", rawId, gotConcept, conceptContext)
		}
		return rawId, nil
	}

	// Short ID requires concept context to resolve
	if conceptContext == "" {
		return "", fmt.Errorf("short ID %q requires concept context; either use full ID (e.g., \"v1:concept:name:%s\") or include concept filter in query (e.g., concept==\"v1:your:concept\";id==\"%s\")", rawId, rawId, rawId)
	}

	// Construct full ID: concept:shortId
	return conceptContext + ":" + rawId, nil
}

// resolveComparisonForExecution creates a copy of the comparison expression with any
// short IDs resolved to full IDs. This ensures that post-filter comparisons in
// executeFilterQuery use full IDs, maintaining the principle that short IDs are
// syntactic sugar at the query boundary while all internal operations use full IDs.
func resolveComparisonForExecution(cmp *ComparisonExpression, conceptContext string) (*ComparisonExpression, error) {
	if cmp == nil {
		return nil, nil
	}

	// Only resolve ID field comparisons
	if len(cmp.Field.Parts) != 1 {
		return cmp, nil
	}

	field := strings.TrimSpace(cmp.Field.Parts[0])
	info, ok := resolveIntrinsicField(field)
	if !ok || info.kind != intrinsicFieldId {
		return cmp, nil
	}

	// Create a copy of the comparison expression
	resolved := &ComparisonExpression{
		Field:            cmp.Field,
		Operator:         cmp.Operator,
		CacheHintSeconds: cmp.CacheHintSeconds,
		FieldSelections:  cmp.FieldSelections,
	}

	switch cmp.Operator {
	case OpEq, OpNe:
		id, err := ensureString(cmp.Value)
		if err != nil {
			return nil, fmt.Errorf("id comparison requires a string: %w", err)
		}
		fullId, err := resolveFullId(strings.TrimSpace(id), conceptContext)
		if err != nil {
			return nil, err
		}
		resolved.Value = fullId
	case OpIn, OpOut:
		values, err := ensureStringSlice(cmp.Value)
		if err != nil {
			return nil, fmt.Errorf("id comparison requires string collection: %w", err)
		}
		fullIds := make([]any, 0, len(values))
		for _, raw := range values {
			fullId, err := resolveFullId(strings.TrimSpace(raw), conceptContext)
			if err != nil {
				return nil, err
			}
			fullIds = append(fullIds, fullId)
		}
		resolved.Value = fullIds
	default:
		// For other operators (missing, etc.), no ID resolution needed
		resolved.Value = cmp.Value
	}

	return resolved, nil
}

func compileTypeComparison(op ComparisonOperator, value any) (compiledExpression, error) {
	switch op {
	case OpEq, OpNe:
		typeValue, err := ensureString(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("type comparison requires a string: %w", err)
		}
		typeValue = strings.ToLower(strings.TrimSpace(typeValue))
		if typeValue == "" {
			return compiledExpression{}, fmt.Errorf("type comparison value cannot be empty")
		}
		sqlOp, err := sqlOperatorForComparison(op)
		if err != nil {
			return compiledExpression{}, err
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(type %s ?)", sqlOp),
			args: []any{typeValue},
		}, nil
	case OpIn, OpOut:
		values, err := ensureStringSlice(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("type comparison requires string collection: %w", err)
		}
		if len(values) == 0 {
			return compiledExpression{}, fmt.Errorf("type list must include at least one value")
		}
		for i := range values {
			values[i] = strings.ToLower(strings.TrimSpace(values[i]))
		}
		operator := "IN"
		if op == OpOut {
			operator = "NOT IN"
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(type %s (?))", operator),
			args: []any{bun.In(values)},
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for type filters", op)
	}
}

func compileCreatedByComparison(op ComparisonOperator, value any) (compiledExpression, error) {
	switch op {
	case OpEq, OpNe:
		name, err := ensureString(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("createdBy comparison requires a string: %w", err)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return compiledExpression{}, fmt.Errorf("createdBy comparison value cannot be empty")
		}
		sqlOp, err := sqlOperatorForComparison(op)
		if err != nil {
			return compiledExpression{}, err
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(\"createdBy\" %s ?)", sqlOp),
			args: []any{name},
		}, nil
	case OpIn, OpOut:
		values, err := ensureStringSlice(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("createdBy comparison requires string collection: %w", err)
		}
		if len(values) == 0 {
			return compiledExpression{}, fmt.Errorf("createdBy list must include at least one value")
		}
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
		}
		operator := "IN"
		if op == OpOut {
			operator = "NOT IN"
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(\"createdBy\" %s (?))", operator),
			args: []any{bun.In(values)},
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for createdBy filters", op)
	}
}

// compileProvenanceComparison pushes a `provenance.<leaf> <op> <value>`
// filter down to a JSONB text-extract over the row's provenance
// intrinsic. Only the four engine-defined leaves are admitted:
// kind, name, trigger, via. All string-valued.
func compileProvenanceComparison(leaf string, op ComparisonOperator, value any) (compiledExpression, error) {
	leaf = strings.ToLower(strings.TrimSpace(leaf))
	// Constant extracts only: interpolating the leaf into quotes is what
	// CodeQL flags as unsafe quoting, even though the switch below is a
	// closed allowlist. Keep the SQL identical for callers/tests.
	var extract string
	switch leaf {
	case "kind":
		extract = "provenance->>'kind'"
	case "name":
		extract = "provenance->>'name'"
	case "trigger":
		extract = "provenance->>'trigger'"
	case "via":
		extract = "provenance->>'via'"
	default:
		return compiledExpression{}, fmt.Errorf("provenance.%s is not a supported leaf (kind|name|trigger|via)", leaf)
	}
	switch op {
	case OpEq, OpNe:
		s, err := ensureString(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("provenance.%s comparison requires a string: %w", leaf, err)
		}
		sqlOp, err := sqlOperatorForComparison(op)
		if err != nil {
			return compiledExpression{}, err
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(%s %s ?)", extract, sqlOp),
			args: []any{s},
		}, nil
	case OpIn, OpOut:
		values, err := ensureStringSlice(value)
		if err != nil {
			return compiledExpression{}, fmt.Errorf("provenance.%s comparison requires string collection: %w", leaf, err)
		}
		if len(values) == 0 {
			return compiledExpression{}, fmt.Errorf("provenance.%s list must include at least one value", leaf)
		}
		operator := "IN"
		if op == OpOut {
			operator = "NOT IN"
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(%s %s (?))", extract, operator),
			args: []any{bun.In(values)},
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for provenance filters", op)
	}
}

func compileCreatedAtComparison(op ComparisonOperator, value any) (compiledExpression, error) {
	timestampLiteral, err := ensureString(value)
	if err != nil {
		return compiledExpression{}, fmt.Errorf("createdAt comparison requires a timestamp string: %w", err)
	}

	ts, err := time.Parse(time.RFC3339, timestampLiteral)
	if err != nil {
		return compiledExpression{}, fmt.Errorf("invalid createdAt timestamp %q: %w", timestampLiteral, err)
	}

	sqlOp, err := sqlOperatorForComparison(op)
	if err != nil {
		return compiledExpression{}, err
	}

	return compiledExpression{
		sql:  fmt.Sprintf("(\"createdAt\" %s ?)", sqlOp),
		args: []any{ts.UTC()},
	}, nil
}

func compilePayloadComparison(path []string, op ComparisonOperator, value any) (compiledExpression, error) {
	textExpr, err := buildJSONPathExpression(path)
	if err != nil {
		return compiledExpression{}, err
	}

	// Also build JSONB path expression for array operations (OpIn, OpOut)
	jsonbExpr, err := buildJSONBPathExpression(path)
	if err != nil {
		return compiledExpression{}, err
	}

	return compileJSONValueComparison(textExpr, jsonbExpr, op, value)
}

// compileJSONValueComparison is the comparison half of compilePayloadComparison,
// parameterised by the operand's two spellings: textExpr, the TEXT extraction
// (`payload #>> '{f}'`) a scalar comparison reads, and jsonbExpr, the jsonb
// navigation (`payload->'f'`) that knows the stored value's JSON TYPE.
//
// Split out so a collection predicate's element (`e.v #>> '{}'`,
// expr_collection_sql.go) compiles through EXACTLY this function. A second copy
// of these rules for elements would be a second place for them to drift, and
// the rows of a filter would then depend on whether the author reached a value
// through a field or through an element.
//
// # Two rules decide every comparison (epic memql#5363, task memql#5366)
//
// ONE NOTION OF UNSET. An absent key, a JSON null, the `nil` literal and the
// empty string are ONE value to `==`, `!=` and `in`. SQL spells "unset" as the
// COALESCE below, and it means exactly that: the `#>>` extraction is NULL for a
// missing key and for JSON null, and the empty text only for a JSON string ""
// -- a number, a boolean, an object or an array never extracts as the empty
// text. A whitespace-only string is a VALUE: a single space is not empty. So
// `== nil` and `== ""` compile to the same fragment, and so do `!= nil` and
// `!= ""`:
//
//	x == nil, x == ""   (COALESCE(x, '') = '')
//	x != nil, x != ""   (COALESCE(x, '') <> '')
//
// This subsumes both carve-outs authoring rule 27 accumulated: #1708/#1714's
// `!= ""` (the "is set" idiom must exclude an absent field) and edition 2026's
// `== ""` (the "is not set" idiom must include one).
//
// TYPED COMPARISONS. A literal compares only with a stored value of its own
// JSON type, which is the in-process evaluator's typed equality (`1 == "1"` is
// false) and also what makes a malformed stored value harmless. This replaces
// memql#3628's model, which cast the extracted TEXT by the literal's type: under
// it a stored string "5" equalled the number 5, and a stored "abc" compared with
// a number was a Postgres ERROR (`invalid input syntax for type numeric`) that
// failed the whole read. Each comparison is guarded by the stored type:
//
//	string   (jsonb_typeof(p) = 'string' AND x <op> ?)
//	number   (CASE WHEN jsonb_typeof(p) = 'number' THEN (x)::numeric <op> ? ELSE FALSE END)
//	boolean  (CASE WHEN jsonb_typeof(p) = 'boolean' THEN (x)::boolean <op> ? ELSE FALSE END)
//
// The string form needs no cast, so its guard can be a plain AND. The casts sit
// behind CASE, not AND, for the reason the Postgres manual gives in "Expression
// Evaluation Rules": the order in which a WHERE clause evaluates the operands
// of AND is not defined -- the planner flattens nested ANDs into the qual list
// and may reorder them by cost -- so `jsonb_typeof(p) = 'number' AND
// (x)::numeric > 1` can still run the cast on a string and raise. CASE is the
// construct the manual names for forcing the order, and it answers FALSE where
// the AND answers NULL, which is the same verdict in a WHERE clause and under
// the two-valued negation below.
//
// `!=` IS EXACTLY `NOT (==)`, taken two-valued (compileNotSQL). That is what
// keeps #1685's null-safety -- an absent field is not equal to 1, so `!= 1` is
// true -- and what makes a stored "1" `!= 1` true under the typed rule.
func compileJSONValueComparison(textExpr, jsonbExpr string, op ComparisonOperator, value any) (compiledExpression, error) {
	switch op {
	case OpMissing:
		return compileUnsetSQL(textExpr, true), nil
	case OpNotMissing:
		return compileUnsetSQL(textExpr, false), nil
	case OpEq, OpNe:
		if isUnsetValue(value) {
			return compileUnsetSQL(textExpr, op == OpEq), nil
		}
		eq, err := compileTypedComparison(textExpr, jsonbExpr, OpEq, value)
		if err != nil {
			return compiledExpression{}, err
		}
		if op == OpEq {
			return eq, nil
		}
		return compileNotSQL(eq), nil
	case OpGt, OpGe, OpLt, OpLe:
		// All four orderings are valid on strings, which is the right
		// semantics for RFC 3339 datetime fields (`expiresAt > now`, the
		// delegation / invitation sweeps). An ABSENT field orders as nothing:
		// the typed guard is NULL for it and the row is not returned. A stored
		// "" is a string like any other here -- the one notion of unset is a
		// statement about equality, not about ordering.
		return compileTypedComparison(textExpr, jsonbExpr, op, value)
	case OpIn, OpOut:
		// Owner bypass: caller-reference resolver may have substituted
		// the value with ownerWildcardSentinel. For OpIn this means
		// "match every row"; for OpOut it means "match no row".
		if _, ok := value.(ownerWildcardSentinel); ok {
			if op == OpIn {
				return compiledExpression{sql: "TRUE"}, nil
			}
			return compiledExpression{sql: "FALSE"}, nil
		}
		// `in` is `==` against each member, so an unset member (nil or "")
		// admits the unset rows and the remaining members are compared typed.
		// There is no array-field form any more: the old string-collection
		// SQL also admitted a field that was an ARRAY overlapping the list, a
		// row the in-process twin always rejected -- so it was only ever
		// scanned to be dropped, and under `!` the drop would have flipped
		// into a lost row. Membership of an array's elements is `v in
		// row.<field>` (OpHas) or a collection predicate.
		members, hasUnset, err := splitUnsetMembers(value)
		if err != nil {
			return compiledExpression{}, err
		}
		if len(members) == 0 && !hasUnset {
			return compiledExpression{}, fmt.Errorf("collection literal cannot be empty")
		}
		var membership compiledExpression
		if len(members) > 0 {
			membership, err = compileTypedMembership(textExpr, jsonbExpr, members)
			if err != nil {
				return compiledExpression{}, err
			}
		}
		if op == OpIn {
			unset := compileUnsetSQL(textExpr, true)
			switch {
			case hasUnset && len(members) > 0:
				return compiledExpression{
					sql:  fmt.Sprintf("(%s OR %s)", unset.sql, membership.sql),
					args: membership.args,
				}, nil
			case hasUnset:
				return unset, nil
			default:
				return membership, nil
			}
		}
		// OpOut, the legacy `not in` (edition 2026 spells it `!(x in list)`,
		// which is NotExpression's two-valued negation). It keeps the rule it
		// has always had: an unset field is NOT a match -- `deleted not in
		// [true]` does not behave like `deleted != true` (authoring rule 27)
		// -- and a set value matches when it is not a member.
		set := compileUnsetSQL(textExpr, false)
		if len(members) == 0 {
			return set, nil
		}
		negated := compileNotSQL(membership)
		return compiledExpression{
			sql:  fmt.Sprintf("(%s AND %s)", set.sql, negated.sql),
			args: negated.args,
		}, nil
	case OpStartsWith:
		// `<field> startsWith <prefix>` (memql#4208). One bound text[]
		// parameter whatever the right-hand shape was -- a single prefix is a
		// one-element array -- compared with `^@ ANY(...)`: `^@` is
		// starts_with() as an operator, a byte-prefix test with no pattern
		// language, so a `%` or `_` in a prefix is literal and nothing here
		// needs escaping. ANY over an empty array is FALSE; the constant is
		// emitted instead so the emitted SQL says what it means and binds no
		// parameter for it.
		//
		// A prefix test is a question about a STRING, so the typed guard
		// applies: a stored number is not tested by its digits. NULL (field
		// absent) is never admitted, which matches the in-process evaluator.
		prefixes, err := normalizePrefixValues(value)
		if err != nil {
			return compiledExpression{}, err
		}
		if len(prefixes) == 0 {
			return compiledExpression{sql: "FALSE"}, nil
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(jsonb_typeof(%s) = 'string' AND (%s) ^@ ANY(?::text[]))", jsonbExpr, textExpr),
			args: []any{pq.Array(prefixes)},
		}, nil
	case OpHas:
		// has checks if a JSONB array field contains a scalar value.
		// Uses the @> containment operator: payload->'field' @> to_jsonb('value'::text)
		//
		// GUARDED TO AN ARRAY (memql#5366). jsonb containment is also true
		// between two EQUAL SCALARS -- `'"a"'::jsonb @> '"a"'::jsonb` -- so a
		// field holding the string "a" where an array was declared answered
		// `"a" in row.tags` TRUE in SQL, while the in-process twin
		// (compareScalarValues' OpHas arm) answers false for anything that is
		// not an array. The combined path intersects the two, so the row was
		// dropped either way -- until `!` arrived: under a negation the two
		// flip, and the intersection would then drop a row the in-process
		// side says matches. Membership is a question about an array, and
		// both halves now say so.
		//
		// Containment is typed already (`[1] @> '"1"'` is false), which is the
		// typed equality this whole function applies. An UNSET needle is `==`
		// to an unset ELEMENT, so it looks for a "" element or a null one.
		if isUnsetValue(value) {
			return compiledExpression{
				sql: fmt.Sprintf("(jsonb_typeof(%[1]s) = 'array' AND (%[1]s @> '[\"\"]'::jsonb OR %[1]s @> '[null]'::jsonb))", jsonbExpr),
			}, nil
		}
		kind, normalized, err := normalizeScalarValue(value)
		if err != nil {
			return compiledExpression{}, err
		}

		switch kind {
		case valueKindString:
			return compiledExpression{
				sql:  fmt.Sprintf("(jsonb_typeof(%s) = 'array' AND %s @> to_jsonb(?::text))", jsonbExpr, jsonbExpr),
				args: []any{normalized},
			}, nil
		case valueKindNumber:
			return compiledExpression{
				sql:  fmt.Sprintf("(jsonb_typeof(%s) = 'array' AND %s @> to_jsonb(?::numeric))", jsonbExpr, jsonbExpr),
				args: []any{normalized},
			}, nil
		case valueKindBool:
			return compiledExpression{
				sql:  fmt.Sprintf("(jsonb_typeof(%s) = 'array' AND %s @> to_jsonb(?::boolean))", jsonbExpr, jsonbExpr),
				args: []any{normalized},
			}, nil
		default:
			return compiledExpression{}, fmt.Errorf("unsupported value type for has operator")
		}
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for payload filters", op)
	}
}

// compileUnsetSQL is the one spelling of "unset" (unset=true) and "set"
// (unset=false). See compileJSONValueComparison.
func compileUnsetSQL(textExpr string, unset bool) compiledExpression {
	op := "="
	if !unset {
		op = "<>"
	}
	return compiledExpression{sql: fmt.Sprintf("(COALESCE(%s, '') %s '')", textExpr, op)}
}

// compileTypedComparison compiles one typed comparison against a SET literal:
// the stored value must be of the literal's JSON type, and then compares.
func compileTypedComparison(textExpr, jsonbExpr string, op ComparisonOperator, value any) (compiledExpression, error) {
	kind, normalized, err := normalizeScalarValue(value)
	if err != nil {
		return compiledExpression{}, err
	}
	sqlOp, err := sqlOperatorForComparison(op)
	if err != nil {
		return compiledExpression{}, err
	}
	switch kind {
	case valueKindString:
		// BYTE ORDER for the orderings, and the collation is what says so
		// (edition 2026's absence table: "strings order by byte"). An
		// uncollated `<` orders by the DATABASE's default collation, which is
		// a locale's -- en_US puts `a` before `E` and weighs accents and
		// punctuation differently from the bytes -- while the in-process twin
		// compares Go strings, which is byte order. The combined path
		// intersects the two, so under a locale collation a string range
		// filter silently dropped every row the two orders disagreed about.
		// `COLLATE "C"` makes the database compare bytes: for UTF-8, "C" is
		// memcmp, which is exactly Go's order.
		//
		// The extraction is parenthesised because COLLATE binds tighter than
		// `#>>`: `payload #>> '{f}' COLLATE "C"` would collate the path
		// LITERAL and leave the comparison under the default. Equality stays
		// uncollated -- a deterministic collation's `=` is already byte
		// equality.
		operand := textExpr
		if isOrderingOperator(op) {
			operand = fmt.Sprintf("(%s) COLLATE \"C\"", textExpr)
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(jsonb_typeof(%s) = 'string' AND %s %s ?)", jsonbExpr, operand, sqlOp),
			args: []any{normalized},
		}, nil
	case valueKindNumber:
		return compiledExpression{
			sql:  fmt.Sprintf("(CASE WHEN jsonb_typeof(%s) = 'number' THEN (%s)::numeric %s ? ELSE FALSE END)", jsonbExpr, textExpr, sqlOp),
			args: []any{normalized},
		}, nil
	case valueKindBool:
		if isOrderingOperator(op) {
			return compiledExpression{}, fmt.Errorf("boolean payload comparisons only support == or !=")
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(CASE WHEN jsonb_typeof(%s) = 'boolean' THEN (%s)::boolean %s ? ELSE FALSE END)", jsonbExpr, textExpr, sqlOp),
			args: []any{normalized},
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("unsupported payload value type")
	}
}

// compileTypedMembership compiles `x in members` over SET members (no nil or
// ""): typed, like compileTypedComparison, with the collection's one kind
// choosing the guard.
func compileTypedMembership(textExpr, jsonbExpr string, members []any) (compiledExpression, error) {
	collection, err := normalizeCollectionValues(members)
	if err != nil {
		return compiledExpression{}, err
	}
	switch collection.kind {
	case valueKindString:
		return compiledExpression{
			sql:  fmt.Sprintf("(jsonb_typeof(%s) = 'string' AND %s IN (?))", jsonbExpr, textExpr),
			args: []any{bun.In(collection.strings)},
		}, nil
	case valueKindNumber:
		return compiledExpression{
			sql:  fmt.Sprintf("(CASE WHEN jsonb_typeof(%s) = 'number' THEN (%s)::numeric IN (?) ELSE FALSE END)", jsonbExpr, textExpr),
			args: []any{bun.In(collection.numbers)},
		}, nil
	case valueKindBool:
		return compiledExpression{
			sql:  fmt.Sprintf("(CASE WHEN jsonb_typeof(%s) = 'boolean' THEN (%s)::boolean IN (?) ELSE FALSE END)", jsonbExpr, textExpr),
			args: []any{bun.In(collection.bools)},
		}, nil
	default:
		return compiledExpression{}, fmt.Errorf("unsupported payload collection type")
	}
}

// isUnsetValue reports whether a value is one of the unset spellings a
// comparison literal or a decoded stored value can take: nil (a missing key,
// JSON null or the `nil` literal) or the empty string. A whitespace-only
// string is a value.
func isUnsetValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	default:
		return false
	}
}

// isOrderingOperator reports whether op orders rather than equates.
func isOrderingOperator(op ComparisonOperator) bool {
	switch op {
	case OpGt, OpGe, OpLt, OpLe:
		return true
	default:
		return false
	}
}

// splitUnsetMembers separates a membership list's unset members (nil, "")
// from its set ones, which are the only ones a typed comparison can take.
func splitUnsetMembers(value any) ([]any, bool, error) {
	var raw []any
	switch v := value.(type) {
	case []any:
		raw = v
	case []string:
		raw = make([]any, len(v))
		for i := range v {
			raw[i] = v[i]
		}
	default:
		return nil, false, fmt.Errorf("expected collection literal")
	}
	members := make([]any, 0, len(raw))
	hasUnset := false
	for _, item := range raw {
		if isUnsetValue(item) {
			hasUnset = true
			continue
		}
		members = append(members, item)
	}
	return members, hasUnset, nil
}

func buildJSONPathExpression(path []string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("payload field path cannot be empty")
	}

	segments := make([]string, len(path))
	for i, segment := range path {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" || !isSafePathSegment(trimmed) {
			return "", fmt.Errorf("payload path segment %q is invalid", segment)
		}
		// Escape even though isSafePathSegment rejects quotes: CodeQL tracks
		// path text into the quoted #>> literal and wants a sanitizer.
		segments[i] = strings.ReplaceAll(trimmed, "'", "''")
	}

	return fmt.Sprintf("payload #>> '{%s}'", strings.Join(segments, ",")), nil
}

// buildJSONBPathExpression builds a JSONB path expression using the -> operator chain.
// For path ["topics"], returns: payload->'topics'
// For path ["profile", "tags"], returns: payload->'profile'->'tags'
// This returns JSONB (not text) which is needed for array operations like ?|.
func buildJSONBPathExpression(path []string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("payload field path cannot be empty")
	}

	segments := make([]string, len(path))
	for i, segment := range path {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" || !isSafePathSegment(trimmed) {
			return "", fmt.Errorf("payload path segment %q is invalid", segment)
		}
		segments[i] = sqlStringLiteral(trimmed)
	}

	return fmt.Sprintf("payload->%s", strings.Join(segments, "->")), nil
}

// sqlStringLiteral quotes s as a PostgreSQL string literal. Callers still gate
// with isSafePathSegment; the escape is what CodeQL recognizes as sanitizing
// a value embedded between single quotes.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func isSafePathSegment(segment string) bool {
	for _, r := range segment {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return len(segment) > 0
}

func sqlOperatorForComparison(op ComparisonOperator) (string, error) {
	switch op {
	case OpEq:
		return "=", nil
	case OpNe:
		return "<>", nil
	case OpGt:
		return ">", nil
	case OpGe:
		return ">=", nil
	case OpLt:
		return "<", nil
	case OpLe:
		return "<=", nil
	default:
		return "", fmt.Errorf("operator %q is not supported in this context", op)
	}
}

func ensureString(value any) (string, error) {
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("expected string, received %T", value)
	}
	return str, nil
}

func ensureStringSlice(value any) ([]string, error) {
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("expected collection literal")
	}

	result := make([]string, 0, len(raw))
	for _, item := range raw {
		str, err := ensureString(item)
		if err != nil {
			return nil, err
		}
		result = append(result, str)
	}

	return result, nil
}

func normalizeScalarValue(value any) (valueKind, any, error) {
	switch v := value.(type) {
	case string:
		return valueKindString, v, nil
	case int:
		return valueKindNumber, float64(v), nil
	case int8:
		return valueKindNumber, float64(v), nil
	case int16:
		return valueKindNumber, float64(v), nil
	case int32:
		return valueKindNumber, float64(v), nil
	case int64:
		return valueKindNumber, float64(v), nil
	case float32:
		return valueKindNumber, float64(v), nil
	case float64:
		return valueKindNumber, v, nil
	case bool:
		return valueKindBool, v, nil
	case *ast.TimestampExprFunc:
		// `now()` / `timestamp()` in a comparison RHS, e.g.
		// `payload.expiresAt < now()`. The mutation evaluator
		// (mutation_templates.go) already substitutes these at
		// runtime; do the same for query comparisons. Resolved at
		// compile-time per query execution, so each query run sees
		// "now" at its own dispatch time -- not at function-load time.
		return valueKindString, time.Now().UTC().Format(time.RFC3339Nano), nil
	case *PlanConstExpression:
		// A comparison value that is still a plan constant was never
		// expanded; name it rather than report "unsupported literal type
		// *memql.PlanConstExpression", which reads like a type-system bug.
		return 0, nil, errPlanConstantUnevaluated(v)
	default:
		return 0, nil, fmt.Errorf("unsupported literal type %T", value)
	}
}

// normalizePrefixValues turns the right-hand side of a `startsWith`
// comparison into the prefix list both evaluators test against: a string, a
// []string, or a []any of strings (the call-site parser's list shape).
//
// Blank prefixes are DROPPED. strings.HasPrefix(s, "") and Postgres
// starts_with(s, ”) are both true for every s, and a selection that admits
// every row on a blank input is the fail-open shape this codebase keeps
// filing issues about (`!= ""` as the is-set idiom, `??` blank-coalescing).
// `codeReference startsWith args.prefixes` is safe to hand whatever list the
// caller holds precisely because neither an empty list nor a list of blanks
// can widen it: both match nothing. An author who wants "no constraint when
// the arg is absent" has `when(args.x) { ... }` for that; a blank is a value,
// and as a prefix it is not one.
func normalizePrefixValues(value any) ([]string, error) {
	var raw []string
	switch v := value.(type) {
	case string:
		raw = []string{v}
	case []string:
		raw = v
	case []any:
		raw = make([]string, 0, len(v))
		for _, item := range v {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("startsWith requires string prefixes, got %T in the list", item)
			}
			raw = append(raw, str)
		}
	default:
		return nil, fmt.Errorf("startsWith requires a string or a list of strings, got %T", value)
	}
	out := make([]string, 0, len(raw))
	for _, prefix := range raw {
		if strings.TrimSpace(prefix) == "" {
			continue
		}
		out = append(out, prefix)
	}
	return out, nil
}

// startsWithAny reports whether s begins with any of the (already
// normalized, non-blank) prefixes. An empty list matches nothing.
func startsWithAny(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func normalizeCollectionValues(value any) (*normalizedCollection, error) {
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("expected collection literal")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("collection literal cannot be empty")
	}

	firstKind, firstValue, err := normalizeScalarValue(raw[0])
	if err != nil {
		return nil, err
	}

	collection := &normalizedCollection{kind: firstKind}

	switch firstKind {
	case valueKindString:
		collection.strings = make([]string, 0, len(raw))
		collection.strings = append(collection.strings, firstValue.(string))
	case valueKindNumber:
		collection.numbers = make([]float64, 0, len(raw))
		collection.numbers = append(collection.numbers, firstValue.(float64))
	case valueKindBool:
		collection.bools = make([]bool, 0, len(raw))
		collection.bools = append(collection.bools, firstValue.(bool))
	default:
		return nil, fmt.Errorf("unsupported collection type")
	}

	for _, item := range raw[1:] {
		kind, normalized, err := normalizeScalarValue(item)
		if err != nil {
			return nil, err
		}
		if kind != firstKind {
			return nil, fmt.Errorf("collection values must be of the same type")
		}
		switch kind {
		case valueKindString:
			collection.strings = append(collection.strings, normalized.(string))
		case valueKindNumber:
			collection.numbers = append(collection.numbers, normalized.(float64))
		case valueKindBool:
			collection.bools = append(collection.bools, normalized.(bool))
		}
	}

	return collection, nil
}

func nodeMatchesComparison(node memorynodes.MemoryNode, cmp *ComparisonExpression, payloadCache map[string]map[string]any) (bool, error) {
	if cmp == nil {
		return true, nil
	}
	if len(cmp.Field.Parts) == 0 {
		return false, fmt.Errorf("comparison field is missing")
	}

	field := strings.TrimSpace(cmp.Field.Parts[0])

	if info, ok := resolveIntrinsicField(field); ok {
		// Each arm below normalises the LITERAL exactly as its compile
		// counterpart does, and reads the STORED column verbatim -- because
		// that is what the emitted SQL compares (memql#3628). These used to
		// strings.TrimSpace both sides, which no `=` in Postgres does: the
		// compile functions normalise only the bound parameter
		// (compileIdComparison / compileCreatedByComparison trim,
		// compileTypeComparison lowercases and trims, compileConceptComparison
		// trims, compileProvenanceComparison does neither) and leave the
		// column alone.
		//
		// KNOWN RESIDUAL, out of scope here: compileIdComparison also runs
		// resolveFullId, which expands a BARE shortId to `{concept}:{shortId}`
		// against the query's concept context. This evaluator has no concept
		// context to expand with, so `row.id=="abc"` still matches in SQL and
		// misses in process. Fail-closed like the rest of this class, and
		// fixing it means threading conceptContext through nodeMatches ->
		// nodeMatchesComparison rather than changing a comparison rule.
		switch info.kind {
		case intrinsicFieldConcept:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			return compareStringValues(node.Concept, strings.TrimSpace(want), cmp.Operator)
		case intrinsicFieldId:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			return compareStringValues(node.ID, strings.TrimSpace(want), cmp.Operator)
		case intrinsicFieldType:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			return compareStringValues(node.Type, strings.ToLower(strings.TrimSpace(want)), cmp.Operator)
		case intrinsicFieldCreatedBy:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			return compareStringValues(node.CreatedBy, strings.TrimSpace(want), cmp.Operator)
		case intrinsicFieldCreatedAt:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			return compareTimestamp(node.CreatedAt, want, cmp.Operator)
		case intrinsicFieldProvenance:
			// provenance.<leaf> post-filter: unmarshal the node's Provenance JSONB
			// and compare the named leaf. Leaf allowlist mirrors compileProvenanceComparison
			// (kind|name|trigger|via). This is the post-filter counterpart to the SQL
			// path; both must handle the same leaf set so the combined-filter scan and
			// the post-filter agree on what is filterable.
			// #1670: the missing post-filter case caused every per-user seed dedup-lookup
			// to fail with "field \"provenance.name\" is not supported in queries" even
			// though the SQL path ran and returned the right row, because executeCombined-
			// FilterQuery re-evaluates the full expression tree in-process on every
			// candidate after the DB scan -- and the in-process evaluator had no case here.
			if len(cmp.Field.Parts) < 2 {
				return false, fmt.Errorf("provenance filters require a leaf path (e.g. provenance.kind)")
			}
			leaf := strings.ToLower(strings.TrimSpace(cmp.Field.Parts[1]))
			switch leaf {
			case "kind", "name", "trigger", "via":
			default:
				return false, fmt.Errorf("provenance.%s is not a supported leaf (kind|name|trigger|via)", leaf)
			}
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, fmt.Errorf("provenance.%s comparison requires a string: %w", leaf, err)
			}
			// compileProvenanceComparison binds the literal untouched, so
			// this must not trim it either (memql#3628).
			return compareStringValues(provenanceLeafFromJSON(node.Provenance, leaf), want, cmp.Operator)
		default:
			return false, fmt.Errorf("field %q is not supported in queries", cmp.Field.Raw)
		}
	}

	if strings.EqualFold(field, "payload") {
		if len(cmp.Field.Parts) < 2 {
			return false, fmt.Errorf("payload comparisons must specify a property path (e.g., payload.active)")
		}
		payloadMap, err := cachedPayloadMap(node, payloadCache)
		if err != nil {
			return false, err
		}
		if payloadMap == nil {
			// A payload that decodes to no object (a JSON-null payload) has no
			// fields, so every field of it is ABSENT -- which is also what the
			// SQL half sees, since `#>>` into a JSON-null payload is NULL. This
			// arm used to answer false for every operator but `== nil`, so it
			// disagreed with the absence rules below on `!=` (and now on
			// `== ""`). Stored rows cannot reach it -- the payload column is
			// NOT NULL and every write validates an object -- but a disagreement
			// between the halves is not made acceptable by being rare.
			return matchJSONValue(nil, false, cmp.Operator, cmp.Value)
		}
		value, exists := valueAtPath(payloadMap, cmp.Field.Parts[1:])
		return matchJSONValue(value, exists, cmp.Operator, cmp.Value)
	}

	return false, fmt.Errorf("field %q is not supported in queries", cmp.Field.Raw)
}

// matchJSONValue decides one comparison against a decoded JSON value read at a
// path -- a payload field, or a collection element -- in process: the twin of
// compileJSONValueComparison. A missing key reads as nil, exactly like JSON
// null, because the `#>>` extraction the SQL reads is NULL for both; from there
// compareScalarValues applies the same two rules the SQL does (one notion of
// unset, typed comparisons).
func matchJSONValue(value any, exists bool, op ComparisonOperator, expected any) (bool, error) {
	if !exists {
		value = nil
	}
	return compareScalarValues(value, op, expected)
}

func cachedPayloadMap(node memorynodes.MemoryNode, cache map[string]map[string]any) (map[string]any, error) {
	key := payloadCacheKey(node)
	if payload, ok := cache[key]; ok {
		return payload, nil
	}
	if len(node.Payload) == 0 {
		cache[key] = nil
		return nil, nil
	}
	payload, err := payloadToMap(node.Payload)
	if err != nil {
		return nil, err
	}
	cache[key] = payload
	return payload, nil
}

func payloadCacheKey(node memorynodes.MemoryNode) string {
	return fmt.Sprintf("%s|%s", strings.TrimSpace(node.ID), node.CreatedAt.UTC().Format(time.RFC3339Nano))
}

func compareStringValues(actual, expected string, op ComparisonOperator) (bool, error) {
	switch op {
	case OpEq:
		return actual == expected, nil
	case OpNe:
		return actual != expected, nil
	default:
		return false, fmt.Errorf("operator %q is not supported for string comparisons", op)
	}
}

func compareTimestamp(actual time.Time, expected string, op ComparisonOperator) (bool, error) {
	ts, err := time.Parse(time.RFC3339, expected)
	if err != nil {
		return false, fmt.Errorf("invalid timestamp %q: %w", expected, err)
	}

	switch op {
	case OpEq:
		return actual.Equal(ts), nil
	case OpNe:
		return !actual.Equal(ts), nil
	case OpGt:
		return actual.After(ts), nil
	case OpGe:
		return actual.After(ts) || actual.Equal(ts), nil
	case OpLt:
		return actual.Before(ts), nil
	case OpLe:
		return actual.Before(ts) || actual.Equal(ts), nil
	default:
		return false, fmt.Errorf("operator %q is not supported for createdAt comparisons", op)
	}
}

// compareScalarValues decides one comparison over a decoded value (nil when
// absent) in process. It is the twin of compileJSONValueComparison and carries
// the same two rules (see that function):
//
//   - ONE NOTION OF UNSET: nil and "" are one value to ==, != and in; a
//     whitespace-only string is a value.
//   - TYPED COMPARISONS: a literal compares only with a value of its own JSON
//     type -- a Go string for a string literal, a JSON number for a number
//     literal, a bool for a boolean literal -- and anything else is not equal,
//     not ordered and not a member. Nothing is cast.
//
// Besides the post-filter (matchJSONValue), resolveActorComparisonsToConstants
// folds `actor.<field>` comparisons through it and the caller-flag fold
// (memql#4814) folds `args.<flag>` comparisons, so all three read a comparison
// the same way.
func compareScalarValues(actual any, op ComparisonOperator, expected any) (bool, error) {
	// `now()` / `timestamp()` -> RFC3339Nano string at eval time, the same
	// lazy substitution normalizeScalarValue makes on the SQL side.
	if _, ok := expected.(*ast.TimestampExprFunc); ok {
		expected = time.Now().UTC().Format(time.RFC3339Nano)
	}
	switch op {
	case OpMissing:
		return isUnsetValue(actual), nil
	case OpNotMissing:
		return !isUnsetValue(actual), nil
	case OpEq, OpNe:
		return compareEquality(actual, expected, op)
	case OpGt, OpGe, OpLt, OpLe:
		return compareTypedOrdering(actual, op, expected)
	case OpIn, OpOut:
		// Owner-bypass: caller-reference resolver may have substituted
		// the collection with ownerWildcardSentinel. OpIn means "match
		// everything"; OpOut means "match nothing".
		if _, ok := expected.(ownerWildcardSentinel); ok {
			return op == OpIn, nil
		}
		members, hasUnset, err := splitUnsetMembers(expected)
		if err != nil {
			return false, err
		}
		if len(members) == 0 && !hasUnset {
			return false, fmt.Errorf("collection literal cannot be empty")
		}
		// Normalised whether or not this row needs it, so a list the SQL
		// refuses (mixed member types) is refused here too.
		var collection *normalizedCollection
		if len(members) > 0 {
			collection, err = normalizeCollectionValues(members)
			if err != nil {
				return false, err
			}
		}
		unset := isUnsetValue(actual)
		member := unset && hasUnset
		if !unset && collection != nil {
			member = typedValueInCollection(actual, collection)
		}
		if op == OpIn {
			return member, nil
		}
		// OpOut, the legacy `not in`: an unset value is never a match, and a
		// set one matches when it is not a member (compileJSONValueComparison).
		return !unset && !member, nil
	case OpStartsWith:
		// The in-process mirror of the typed `^@ ANY` SQL: a prefix test is a
		// question about a STRING, so a number is not tested by its digits.
		// Every candidate the SQL scan returns is re-evaluated here by
		// executeCombinedFilterQuery, so the two must agree on every case
		// (empty list, blank prefix, absent field, non-string field).
		prefixes, err := normalizePrefixValues(expected)
		if err != nil {
			return false, err
		}
		actualStr, ok := actual.(string)
		if !ok {
			return false, nil
		}
		return startsWithAny(actualStr, prefixes), nil
	case OpHas:
		// OpHas tests array containment: does the array field `actual`
		// contain the scalar `expected`. The parser desugars the
		// canonical membership form `<scalar> in payload.<arrayField>`
		// to `payload.<arrayField> has <scalar>` (#976), so by the time
		// it reaches the in-memory evaluator the array field is `actual`
		// (LHS) and the membership scalar is `expected` (RHS). The SQL
		// fast-path compiles this via the @> containment operator; this
		// branch is the post-filter / non-pushdown mirror (#1674). Element
		// equality is compareEquality's, so it is typed like containment is,
		// and an unset needle finds an unset element.
		items, ok := payloadArrayElements(actual)
		if !ok {
			// Field is absent or not an array -> contains nothing.
			return false, nil
		}
		for _, item := range items {
			match, err := compareEquality(item, expected, OpEq)
			if err != nil {
				return false, err
			}
			if match {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("operator %q is not supported for payload comparisons", op)
	}
}

// compareTypedOrdering is the ordering half of compareScalarValues. An absent
// value orders as nothing (every ordering is false), and the typed rule decides
// the rest: a string literal orders only strings, by byte -- Go's string order,
// which is what the SQL's `COLLATE "C"` reproduces -- and a number literal
// orders only JSON numbers.
func compareTypedOrdering(actual any, op ComparisonOperator, expected any) (bool, error) {
	if actual == nil {
		return false, nil
	}
	if want, ok := expected.(string); ok {
		have, ok := actual.(string)
		if !ok {
			return false, nil
		}
		switch op {
		case OpGt:
			return have > want, nil
		case OpGe:
			return have >= want, nil
		case OpLt:
			return have < want, nil
		default:
			return have <= want, nil
		}
	}
	if _, isBool := expected.(bool); isBool {
		return false, fmt.Errorf("boolean payload comparisons only support == or !=")
	}
	want, ok := numericValue(expected)
	if !ok {
		return false, fmt.Errorf("numeric comparison requires a number literal, got %T", expected)
	}
	have, ok := typedNumber(actual)
	if !ok {
		return false, nil
	}
	switch op {
	case OpGt:
		return have > want, nil
	case OpGe:
		return have >= want, nil
	case OpLt:
		return have < want, nil
	default:
		return have <= want, nil
	}
}

// payloadArrayElements normalizes a payload field value into a []any
// for in-memory array operations (OpHas). JSONB arrays decode to []any,
// but defensively handle the typed-slice shapes the payload cache can
// surface too.
func payloadArrayElements(value any) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		return v, true
	case []string:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, true
	default:
		return nil, false
	}
}

// compareEquality is typed, unset-aware equality: the in-process statement of
// the SQL's COALESCE unset test (compileUnsetSQL) for an unset literal, and of
// its jsonb_typeof-guarded comparison for a set one. `!=` is its exact
// negation.
func compareEquality(actual any, expected any, op ComparisonOperator) (bool, error) {
	// Lazily evaluate `now()` / `timestamp()` AST nodes. They show up
	// when the SQL fast-path didn't compile the comparison (e.g. JSON
	// path comparisons that fall back to in-memory filtering). Same
	// resolution semantics as the SQL path: substitute the current
	// time at evaluation time.
	if _, ok := expected.(*ast.TimestampExprFunc); ok {
		expected = time.Now().UTC().Format(time.RFC3339Nano)
	}
	var equal bool
	switch {
	case isUnsetValue(expected):
		equal = isUnsetValue(actual)
	case isUnsetValue(actual):
		equal = false
	default:
		switch want := expected.(type) {
		case bool:
			have, ok := actual.(bool)
			equal = ok && have == want
		case string:
			have, ok := actual.(string)
			equal = ok && have == want
		default:
			wantNum, ok := numericValue(expected)
			if !ok {
				return false, fmt.Errorf("unsupported literal type %T in comparison", expected)
			}
			haveNum, ok := typedNumber(actual)
			equal = ok && haveNum == wantNum
		}
	}
	if op == OpEq {
		return equal, nil
	}
	return !equal, nil
}

// typedNumber reads a decoded JSON NUMBER: every Go numeric type the payload
// cache or a caller can produce, plus json.Number for a decoder that kept the
// source text. A string of digits is NOT a number -- that is the typed rule --
// and neither is a boolean.
func typedNumber(value any) (float64, bool) {
	if n, ok := value.(json.Number); ok {
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return numericValue(value)
}

// typedValueInCollection reports whether a SET value is a member of a
// normalised collection under the typed rule.
func typedValueInCollection(actual any, collection *normalizedCollection) bool {
	switch collection.kind {
	case valueKindString:
		have, ok := actual.(string)
		if !ok {
			return false
		}
		for _, candidate := range collection.strings {
			if have == candidate {
				return true
			}
		}
	case valueKindNumber:
		have, ok := typedNumber(actual)
		if !ok {
			return false
		}
		for _, candidate := range collection.numbers {
			if have == candidate {
				return true
			}
		}
	case valueKindBool:
		have, ok := actual.(bool)
		if !ok {
			return false
		}
		for _, candidate := range collection.bools {
			if have == candidate {
				return true
			}
		}
	}
	return false
}

// payloadText renders a decoded payload value the way `payload #>> '{path}'`
// renders the stored JSONB: a string as itself, a boolean as `true`/`false`, a
// number in numeric's plain (never exponent) notation.
//
// It was the post-filter's half of memql#3628's model, in which the SQL cast
// the extracted text by the literal's type and this file reproduced the cast.
// Edition 2026 replaced that model with typed comparisons (compareScalarValues:
// a literal compares only with a value of its own type), so the post-filter no
// longer calls it. It survives for the two callers that keep their own
// comparison rules and read a value AS TEXT on purpose: the legacy
// collection-lambda evaluator's startsWith (evalCollComparison) and shape
// match() conditions (compareValues). Objects and arrays report themselves
// un-renderable, as before.
func payloadText(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), true
	case int:
		return strconv.Itoa(v), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	default:
		return "", false
	}
}

// provenanceLeafFromJSON extracts a named leaf from a raw provenance JSONB value.
// Returns empty string when raw is nil/empty, the JSON is malformed, or the
// leaf is absent. The leaf set matches compileProvenanceComparison:
// kind, name, trigger, via.
func provenanceLeafFromJSON(raw json.RawMessage, leaf string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, _ := m[leaf].(string)
	return v
}

// executeFilterQuery resolves a pre-compiled SQL filter to the latest row per
// matching id. It is the RELATIONSHIP path's lookup: fetchNodesByIds /
// fetchNodesByJSONFieldValues / fetchNodesByNodeFieldValues
// (executor_mutation.go) each call it with an enumerated `id IN (...)` or
// `<field> IN (...)` set on behalf of parentOf / childOf / contains / owns /
// references / createdBy. Those three are its only live callers, and all
// three pass a nil `cmp`.
//
// WHO DOES NOT CALL IT (memql#3397, worth stating because the investigation
// turned on it): the read path does not. evaluateExpressionSetWithContext has
// a *ComparisonExpression branch that calls this with a non-nil `cmp`, but
// tryCompileCombinedFilter runs first and succeeds for every comparison whose
// compile succeeds -- and when the compile fails, the branch's own
// compileComparisonExpressionWithContext call fails identically and returns the
// error before reaching here. So filter reads land in
// executeCombinedFilterQuery, which is why memql#3388 lost an hour patching
// this function on the assumption it was the live path. Confirmed empirically:
// an stderr probe on `cmp != nil` fired zero times across the whole db-gated
// suite (2874 tests, six trees, against a live Postgres).
//
// The consequence for pagination: nothing paginates this path, no keyset cursor
// reaches it, and no caller reads a short result as exhaustion. What its
// callers DO read is an id's absence, as a dangling reference -- see the scan
// comment below.
// staged-data: GATE -- the second engine seam, enforced by memql#3983's plan
// injection plus its per-row gate, exactly as executeCombinedFilterQuery is.
// See the note there for why this file adds no second check of its own.
func (e *MemQLEngine) executeFilterQuery(ctx context.Context, cmp *ComparisonExpression, filter compiledExpression, timestamp *time.Time, target int, sorter *compiledSort) ([]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	var nodes []memorynodes.MemoryNode

	// THE SQL ENUMERATION HAS TO BE THE RESULT SET (memql#3397), the same way
	// executeCombinedFilterQuery's does since memql#3388.
	//
	// MemoryNodes is append-only: one id carries one row per version, and this
	// read returns the LATEST row per id. Scanning RAW rows and collapsing them
	// afterwards makes the LIMIT bound VERSIONS rather than ids, so a window
	// whose consecutive rows share an id yields fewer rows than were asked for.
	// The old sizing guessed at `target * 2` -- an assumption of ~2 versions per
	// id, against real concepts that run to 15.
	//
	// That matters more here than a short page does, because this path is what
	// the RELATIONSHIP resolvers look ids up through (fetchNodesByIds /
	// fetchNodesByJSONFieldValues / fetchNodesByNodeFieldValues, on behalf of
	// parentOf / childOf / contains / owns / references / createdBy). Those
	// callers enumerate the ids they want and read an id's ABSENCE from the
	// result as a dangling reference -- fetchNodesByIds logs "memql reference
	// missing; skipping node" and hands back a smaller graph bundle. So a
	// collapsed window turned into a traversal answering "no parent" about a
	// parent that exists: measured at 2 of 10 before this change.
	//
	// So the collapse happens IN SQL -- `DISTINCT ON (id) ... ORDER BY id,
	// "createdAt" DESC` in a subquery, re-sorted by the declared ordering
	// outside it. The LIMIT then bounds DISTINCT ids and the `* 2` guess is
	// gone rather than retuned.
	//
	// THE PLAN, measured rather than assumed. memql#3388 recorded the combined
	// path's collapse riding a SkipScan over `memory_nodes_id_created_at_desc_idx`;
	// against a chunked hypertable neither path does. Both plan it as Sort +
	// Unique above a per-chunk index scan on whatever index the FILTER selects,
	// because `(id, "createdAt" DESC)` holds per chunk and the chunks still have
	// to be merged. So #3388's other observation stands -- the outer sort differs
	// from the inner ordering, and the LIMIT cannot short-circuit the inner scan
	// -- but it is bounded differently here: every filter that reaches this
	// function is an enumerated `id IN (...)` / `<field> IN (...)` set, so the
	// inner scan reads the versions of rows the caller NAMED rather than a whole
	// concept's. Measured on the 8-ids x 12-versions fixture in
	// relationship_versioned_ids_3397_db_test.go: 96 rows scanned, 82 shared
	// buffer hits, against 158 for the same lookup under the old raw window.
	//
	// The filter and the asOf timestamp ride INSIDE the collapse: a bare
	// collapse would read every row in the table, and asOf has to bound the
	// versions the collapse picks FROM or it would resolve rows to versions
	// that did not exist yet. The filter carries the per-row authz it already
	// carried, unchanged.
	//
	// NO WIDENING LOOP, unlike the combined path. The loop that survives there
	// exists for one reason: engine.go's nextCursor block reads a short page as
	// exhaustion, so a row dropped by the in-memory post-filter costs the rest
	// of the set. Nothing paginates this path -- it takes no cursor and its
	// callers ask for a bounded, enumerated id set -- so with the LIMIT now
	// bounding ids, one scan returns every id that exists.
	latest := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		DistinctOn("id").
		OrderExpr(`id ASC, "createdAt" DESC`)

	if filter.sql != "" {
		latest = latest.Where(filter.sql, filter.args...)
	}
	if timestamp != nil {
		latest = latest.Where(`"createdAt" <= ?`, timestamp.UTC())
	}

	query := db.NewSelect().Model(&nodes).ModelTableExpr("(?) AS mn", latest)

	// Shared with the combined path so the two orderings cannot drift; the
	// nil-sorter fallback there also carries the `id ASC` tie-break, which
	// makes equal-createdAt rows deterministic here instead of arbitrary.
	for _, expr := range combinedFilterOrderExprs(sorter) {
		if strings.TrimSpace(expr) == "" {
			continue
		}
		query = query.OrderExpr(expr)
	}

	fetchTarget := target
	if fetchTarget <= 0 {
		fetchTarget = e.config.MaxWindow
	}
	if fetchTarget <= 0 {
		fetchTarget = e.config.MaxResults
	}

	query = query.Limit(fetchTarget)

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	// The same post-filter the combined path runs, through the same helper:
	// resolve each scanned candidate to its TRUE latest version and keep the
	// ones the predicate still matches. A typed-nil *ComparisonExpression would
	// make a non-nil ExpressionNode interface, so the nil case is spelled out --
	// nil there means "no predicate", which is what every live caller passes.
	var predicate ExpressionNode
	if cmp != nil {
		predicate = cmp
	}
	return e.latestMatchingNodes(ctx, nodes, predicate, timestamp, target)
}

// evaluateShapeTemplatesExpression lists available shape templates.
