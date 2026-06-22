package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// constantBoolExpression is an internal post-filter node carrying a
// pre-resolved boolean. `actor.<field>` comparisons are query-time constants
// (the resolved auth envelope) and are already enforced once in the SQL WHERE
// clause; resolveActorComparisonsToConstants folds them to a constant here so
// the ctx-less post-filter evaluator preserves the AND/OR truth value without
// re-resolving actor state it cannot see (#1659). It is never produced by the
// parser -- only by the pre-post-filter rewrite.
type constantBoolExpression struct {
	value bool
}

func (*constantBoolExpression) isExpressionNode() {}

func nodeMatchesExpression(node memorynodes.MemoryNode, expr ExpressionNode, payloadCache map[string]map[string]any) (bool, error) {
	if expr == nil {
		return true, nil
	}
	switch n := expr.(type) {
	case *constantBoolExpression:
		return n.value, nil
	case *LiteralValueNode:
		// A literal in predicate position acts as a boolean constant via
		// truthiness -- the post-filter counterpart to constantBoolExpression
		// (#1705). Mirrors how the spec evaluator treats a literal (specTruthy)
		// so the SQL-compile path and the in-process post-filter agree.
		return literalTruthy(n.Value), nil
	case *ComparisonExpression:
		return nodeMatchesComparison(node, n, payloadCache)
	case *LogicalExpression:
		left, err := nodeMatchesExpression(node, n.Left, payloadCache)
		if err != nil {
			return false, err
		}
		right, err := nodeMatchesExpression(node, n.Right, payloadCache)
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
	if expr == nil {
		return compiledExpression{}, false
	}

	switch node := expr.(type) {
	case *LogicalExpression:
		leftFilter, leftOk := e.tryCompileCombinedFilter(ctx, node.Left, conceptContext)
		if !leftOk {
			return compiledExpression{}, false
		}
		rightFilter, rightOk := e.tryCompileCombinedFilter(ctx, node.Right, conceptContext)
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
		return e.tryCompileCombinedFilter(ctx, spec.Expr, conceptContext)

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
	default:
		return expr, nil
	}
}

// executeCombinedFilterQuery executes a query using a combined SQL filter.
// This is similar to executeFilterQuery but doesn't require a ComparisonExpression node.
func (e *MemQLEngine) executeCombinedFilterQuery(ctx context.Context, expr ExpressionNode, filter compiledExpression, timestamp *time.Time, target int, sorter *compiledSort) ([]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	var nodes []memorynodes.MemoryNode

	query := db.NewSelect().Model(&nodes)

	if filter.sql != "" {
		query = query.Where(filter.sql, filter.args...)
	}
	if timestamp != nil {
		query = query.Where(`"createdAt" <= ?`, timestamp.UTC())
	}

	// Keyset cursor (5.12): when a continuation cursor is present, push the
	// keyset predicate `(createdAt, id) <keyset> (?, ?)` into SQL so deep pages
	// continue from the encoded position instead of scanning + discarding an
	// offset window. Composed with the existing filter + asOf-timestamp WHERE
	// above and the per-row authz the filter already carries; ordered by the
	// declared sort below; LIMIT bounded to the page target (no *2 over-fetch).
	keyset, hasKeyset := keysetFromContext(ctx)
	if hasKeyset {
		if eligible, createdAtDesc := keysetEligibleSort(sorter); eligible {
			predicate, args := keysetWhere(keyset, createdAtDesc)
			query = query.Where(predicate, args...)
		} else {
			// Non-keyset-eligible ordering reached the SQL path with a cursor
			// set; do not push an incorrect predicate. The engine gates this
			// upstream, so this is a defensive no-op.
			hasKeyset = false
		}
	}

	orderExprs := []string{`"createdAt" DESC`}
	if sorter != nil {
		orderExprs = sorter.orderExpressions()
	}
	for _, expr := range orderExprs {
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

	sqlLimit := fetchTarget * 2
	if sqlLimit < fetchTarget {
		sqlLimit = fetchTarget
	}
	if hasKeyset {
		// The keyset predicate already excludes the prior page, so there is no
		// scan-and-discard tail to over-fetch against; cap the SQL LIMIT at the
		// page target. (The post-scan dedup below still collapses any older
		// versions of the same id into one logical row.)
		sqlLimit = fetchTarget
	}

	query = query.Limit(sqlLimit)

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	// Candidate IDs from the scan (may include older versions).
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

	// Load latest versions for all candidate IDs (respecting asOf timestamp).
	var latest map[string]memorynodes.MemoryNode
	var err error
	if len(uniqueIds) > 0 {
		latest, err = e.loadLatestNodes(ctx, uniqueIds, timestamp)
		if err != nil {
			return nil, err
		}
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

	payloadCache := make(map[string]map[string]any)
	seen := make(map[string]struct{})
	result := make([]memorynodes.MemoryNode, 0, len(uniqueIds))

	// Preserve the scan ordering by iterating over scanned rows and emitting each ID once.
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

		match, err := nodeMatchesExpression(candidate, expandedExpr, payloadCache)
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
// `payload.spaceId == "daily-..."` misses canonical-stored rows.
//
// The pre-walk runs once before SQL compile + post-filter, so both
// paths see the same canonical RHS -- avoids a class of "SQL returns
// 1 row, post-filter rejects it" bugs that would arise if only one
// side rewrote the value.
//
// No concept context (top-level expressions with no `concept==X`
// constraint), no relationship match, or non-string RHS: pass
// through unchanged.
func (e *MemQLEngine) canonicalizeRelationshipComparisons(ctx context.Context, expr ExpressionNode, conceptContext string) ExpressionNode {
	if expr == nil || e == nil || e.concepts == nil || conceptContext == "" {
		return expr
	}
	switch n := expr.(type) {
	case *ComparisonExpression:
		// payload.<field> == <value>  -- the only shape we rewrite.
		if n == nil || len(n.Field.Parts) != 2 {
			return expr
		}
		if !strings.EqualFold(strings.TrimSpace(n.Field.Parts[0]), "payload") {
			return expr
		}
		fieldName := strings.TrimSpace(n.Field.Parts[1])
		canon, ok := e.canonicalizeRelationshipFieldValue(ctx, conceptContext, fieldName, n.Value)
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
	default:
		// RelationshipExpression, SpecReferenceExpression,
		// FunctionCallExpression, etc. don't carry comparison values
		// directly; pass through. (Spec bodies get canonicalized
		// when they're expanded inline in the comparable arm above.)
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
		return &RelationshipExpression{Function: n.Function, Target: target}
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
		if !strings.EqualFold(strings.TrimSpace(rel.Direction), "outgoing") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(rel.Field), fieldName) {
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
// nil. Seen in the wild on querySpaceParticipants when a compiled sub-
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
// If the ID already contains a colon, it's assumed to be a full ID and returned as-is.
// If the ID doesn't contain a colon, the conceptContext must be provided to construct the full ID.
// Returns an error if a short ID is provided without concept context.
func resolveFullId(id string, conceptContext string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("id cannot be empty")
	}

	// If ID already contains a colon, treat it as a full ID
	if strings.Contains(id, ":") {
		return id, nil
	}

	// Short ID requires concept context to resolve
	if conceptContext == "" {
		return "", fmt.Errorf("short ID %q requires concept context; either use full ID (e.g., \"v1:concept:name:%s\") or include concept filter in query (e.g., concept==\"v1:your:concept\";id==\"%s\")", id, id, id)
	}

	// Construct full ID: concept:shortId
	return conceptContext + ":" + id, nil
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
	switch leaf {
	case "kind", "name", "trigger", "via":
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
			sql:  fmt.Sprintf("(provenance->>'%s' %s ?)", leaf, sqlOp),
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
			sql:  fmt.Sprintf("(provenance->>'%s' %s (?))", leaf, operator),
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
	jsonExpr, err := buildJSONPathExpression(path)
	if err != nil {
		return compiledExpression{}, err
	}

	// Also build JSONB path expression for array operations (OpIn, OpOut)
	jsonbExpr, err := buildJSONBPathExpression(path)
	if err != nil {
		return compiledExpression{}, err
	}

	switch op {
	case OpMissing, OpNotMissing:
		// Check if payload property is missing (NULL) or not missing (NOT NULL)
		// PostgreSQL's #>> operator returns NULL when the key doesn't exist
		operator := "IS NULL"
		if op == OpNotMissing {
			operator = "IS NOT NULL"
		}
		return compiledExpression{
			sql:  fmt.Sprintf("(%s %s)", jsonExpr, operator),
			args: nil,
		}, nil
	case OpEq, OpNe, OpGt, OpGe, OpLt, OpLe:
		kind, normalized, err := normalizeScalarValue(value)
		if err != nil {
			return compiledExpression{}, err
		}

		var expr string
		switch kind {
		case valueKindString:
			// All six comparison ops (==, !=, <, <=, >, >=) are valid on
			// strings. PostgreSQL compares strings lexicographically,
			// which is the right semantics for RFC-3339 datetime fields
			// (the dominant motivation -- `payload.expiresAt<ctx.now`
			// in the delegation / invitation sweep queries) and matches
			// the normal ordering behaviour of SQL, Go, and most
			// embedding languages. Previously we rejected ordered
			// comparisons here with "string payload comparisons only
			// support == or !=", which forced every expiry sweep to add
			// a parallel integer-unix field to work around a purely
			// artificial restriction.
			expr = jsonExpr
		case valueKindNumber:
			expr = fmt.Sprintf("(%s)::numeric", jsonExpr)
		case valueKindBool:
			if op != OpEq && op != OpNe {
				return compiledExpression{}, fmt.Errorf("boolean payload comparisons only support == or !=")
			}
			expr = fmt.Sprintf("(%s)::boolean", jsonExpr)
		default:
			return compiledExpression{}, fmt.Errorf("unsupported payload value type")
		}

		// NULL-safe inequality (#1685): a payload field that is ABSENT
		// (JSON null / key missing -> the ->> extraction yields SQL NULL)
		// is logically DISTINCT FROM any concrete value, so a `!=`
		// predicate must MATCH it. Plain SQL `<>` returns NULL (not true)
		// when either side is NULL, which silently drops every row that
		// lacks the field -- the bug behind traitIsNotDeleted
		// (`payload.deleted != true`) excluding rows that never had a
		// `deleted` key (the concept @default isn't always stamped). Use
		// IS DISTINCT FROM so absent == not-equal == included. OpEq keeps
		// plain `=` (absent is correctly NOT equal to the value).
		//
		// EXCEPTION -- comparison to the empty string (#1708/#1714): `!= ""`
		// is the canonical "field is set" idiom across the DSL
		// (traitIsDeletionScheduled = `deletionScheduledAt != ""`,
		// queryExpiredConsumedAuthCodes = `consumedAt != ""`, etc). An ABSENT
		// string field is logically EQUAL to "" (both mean "not set"), so it
		// must NOT match `!= ""`. Under the #1685 IS DISTINCT FROM rule those
		// queries wrongly returned every row whose field was absent (all
		// active users / all expired codes). COALESCE the NULL to '' so an
		// absent field reads as empty and is correctly excluded.
		if op == OpNe {
			if s, ok := normalized.(string); ok && s == "" {
				return compiledExpression{
					sql:  fmt.Sprintf("(COALESCE(%s, '') <> ?)", expr),
					args: []any{normalized},
				}, nil
			}
			return compiledExpression{
				sql:  fmt.Sprintf("(%s IS DISTINCT FROM ?)", expr),
				args: []any{normalized},
			}, nil
		}

		sqlOp, err := sqlOperatorForComparison(op)
		if err != nil {
			return compiledExpression{}, err
		}

		return compiledExpression{
			sql:  fmt.Sprintf("(%s %s ?)", expr, sqlOp),
			args: []any{normalized},
		}, nil
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
		collection, err := normalizeCollectionValues(value)
		if err != nil {
			return compiledExpression{}, err
		}

		switch collection.kind {
		case valueKindString:
			// For string collections, support both scalar fields and array fields.
			// - If field is an array: use jsonb_exists_any (any element matches)
			// - If field is a scalar: use IN operator (value matches)

			arrayArg := pq.Array(collection.strings)

			if op == OpIn {
				// (jsonb_typeof(payload->'field') = 'array' AND jsonb_exists_any(payload->'field', ARRAY[...]))
				// OR
				// (payload #>> '{field}' IN (...))
				return compiledExpression{
					sql: fmt.Sprintf(
						"((jsonb_typeof(%s) = 'array' AND jsonb_exists_any(%s, ?::text[])) OR (%s IN (?)))",
						jsonbExpr, jsonbExpr, jsonExpr,
					),
					args: []any{arrayArg, bun.In(collection.strings)},
				}, nil
			}
			// OpOut: field is array AND array does NOT contain any of the values
			//        OR field is NOT array AND scalar is NOT in the list
			return compiledExpression{
				sql: fmt.Sprintf(
					"((jsonb_typeof(%s) = 'array' AND NOT jsonb_exists_any(%s, ?::text[])) OR (jsonb_typeof(%s) != 'array' AND %s NOT IN (?)))",
					jsonbExpr, jsonbExpr, jsonbExpr, jsonExpr,
				),
				args: []any{arrayArg, bun.In(collection.strings)},
			}, nil
		case valueKindNumber:
			operator := "IN"
			if op == OpOut {
				operator = "NOT IN"
			}
			expr := fmt.Sprintf("(%s)::numeric", jsonExpr)
			return compiledExpression{
				sql:  fmt.Sprintf("(%s %s (?))", expr, operator),
				args: []any{bun.In(collection.numbers)},
			}, nil
		case valueKindBool:
			operator := "IN"
			if op == OpOut {
				operator = "NOT IN"
			}
			expr := fmt.Sprintf("(%s)::boolean", jsonExpr)
			return compiledExpression{
				sql:  fmt.Sprintf("(%s %s (?))", expr, operator),
				args: []any{bun.In(collection.bools)},
			}, nil
		default:
			return compiledExpression{}, fmt.Errorf("unsupported payload collection type")
		}
	case OpHas:
		// has checks if a JSONB array field contains a scalar value.
		// Uses the @> containment operator: payload->'field' @> to_jsonb('value'::text)
		kind, normalized, err := normalizeScalarValue(value)
		if err != nil {
			return compiledExpression{}, err
		}

		switch kind {
		case valueKindString:
			return compiledExpression{
				sql:  fmt.Sprintf("(%s @> to_jsonb(?::text))", jsonbExpr),
				args: []any{normalized},
			}, nil
		case valueKindNumber:
			return compiledExpression{
				sql:  fmt.Sprintf("(%s @> to_jsonb(?::numeric))", jsonbExpr),
				args: []any{normalized},
			}, nil
		case valueKindBool:
			return compiledExpression{
				sql:  fmt.Sprintf("(%s @> to_jsonb(?::boolean))", jsonbExpr),
				args: []any{normalized},
			}, nil
		default:
			return compiledExpression{}, fmt.Errorf("unsupported value type for has operator")
		}
	default:
		return compiledExpression{}, fmt.Errorf("operator %q is not supported for payload filters", op)
	}
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
		segments[i] = trimmed
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
		segments[i] = fmt.Sprintf("'%s'", trimmed)
	}

	return fmt.Sprintf("payload->%s", strings.Join(segments, "->")), nil
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
	default:
		return 0, nil, fmt.Errorf("unsupported literal type %T", value)
	}
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
		switch info.kind {
		case intrinsicFieldConcept:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			actual := strings.TrimSpace(node.Concept)
			return compareStringValues(actual, strings.TrimSpace(want), cmp.Operator)
		case intrinsicFieldId:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			actual := strings.TrimSpace(node.ID)
			return compareStringValues(actual, strings.TrimSpace(want), cmp.Operator)
		case intrinsicFieldType:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			actual := strings.TrimSpace(node.Type)
			return compareStringValues(actual, strings.TrimSpace(want), cmp.Operator)
		case intrinsicFieldCreatedBy:
			want, err := ensureString(cmp.Value)
			if err != nil {
				return false, err
			}
			actual := strings.TrimSpace(node.CreatedBy)
			return compareStringValues(actual, strings.TrimSpace(want), cmp.Operator)
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
			return compareStringValues(provenanceLeafFromJSON(node.Provenance, leaf), strings.TrimSpace(want), cmp.Operator)
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
			switch cmp.Operator {
			case OpMissing:
				return true, nil
			case OpNotMissing:
				return false, nil
			default:
				return false, nil
			}
		}
		value, exists := valueAtPath(payloadMap, cmp.Field.Parts[1:])
		switch cmp.Operator {
		case OpMissing:
			return !exists || value == nil, nil
		case OpNotMissing:
			return exists && value != nil, nil
		default:
			if !exists || value == nil {
				// NULL-safe inequality (#1685): an absent field is DISTINCT
				// FROM any concrete value, so `!=` MATCHES it -- mirrors the
				// SQL IS DISTINCT FROM path so the post-filter and the SQL
				// scan agree. Every other operator treats absent as a
				// non-match.
				//
				// EXCEPTION (#1708/#1714): `!= ""` is the "is set" idiom; an
				// absent string field equals "" (not set) and must NOT match.
				// Mirrors the COALESCE(expr,'') <> '' SQL path above.
				if cmp.Operator == OpNe {
					if s, ok := cmp.Value.(string); ok && s == "" {
						return false, nil
					}
					return true, nil
				}
				return false, nil
			}
			return compareScalarValues(value, cmp.Operator, cmp.Value)
		}
	}

	return false, fmt.Errorf("field %q is not supported in queries", cmp.Field.Raw)
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

func compareScalarValues(actual any, op ComparisonOperator, expected any) (bool, error) {
	// `now()` / `timestamp()` -> RFC3339Nano string at eval time.
	// Same lazy substitution used by compareEquality + the SQL
	// compile path so comparison semantics stay aligned across
	// the three branches.
	if _, ok := expected.(*ast.TimestampExprFunc); ok {
		expected = time.Now().UTC().Format(time.RFC3339Nano)
	}
	switch op {
	case OpEq, OpNe:
		return compareEquality(actual, expected, op)
	case OpGt, OpGe, OpLt, OpLe:
		// String ordering branch: RFC-3339 timestamps compare
		// lexicographically and that's the dominant case for `<` /
		// `>` (every expiry-sweep query uses this shape). Try the
		// numeric path first; if expected is a string and actual is
		// a string, fall through to lexicographic comparison.
		if expectedStr, ok := expected.(string); ok {
			if actualStr, ok := toString(actual); ok {
				switch op {
				case OpGt:
					return actualStr > expectedStr, nil
				case OpGe:
					return actualStr >= expectedStr, nil
				case OpLt:
					return actualStr < expectedStr, nil
				case OpLe:
					return actualStr <= expectedStr, nil
				}
			}
			return false, nil
		}
		expectedNum, ok := literalToFloat(expected)
		if !ok {
			return false, fmt.Errorf("numeric comparison requires a number literal")
		}
		actualNum, ok := toFloat(actual)
		if !ok {
			return false, nil
		}
		switch op {
		case OpGt:
			return actualNum > expectedNum, nil
		case OpGe:
			return actualNum >= expectedNum, nil
		case OpLt:
			return actualNum < expectedNum, nil
		case OpLe:
			return actualNum <= expectedNum, nil
		}
	case OpIn, OpOut:
		// Owner-bypass: caller-reference resolver may have substituted
		// the collection with ownerWildcardSentinel. OpIn means "match
		// everything"; OpOut means "match nothing".
		if _, ok := expected.(ownerWildcardSentinel); ok {
			if op == OpIn {
				return true, nil
			}
			return false, nil
		}
		collection, err := normalizeCollectionValues(expected)
		if err != nil {
			return false, err
		}
		inSet, err := valueInCollection(actual, collection)
		if err != nil {
			return false, err
		}
		if op == OpIn {
			return inSet, nil
		}
		return !inSet, nil
	case OpHas:
		// OpHas tests array containment: does the array field `actual`
		// contain the scalar `expected`. The parser desugars the
		// canonical membership form `<scalar> in payload.<arrayField>`
		// to `payload.<arrayField> has <scalar>` (#976), so by the time
		// it reaches the in-memory evaluator the array field is `actual`
		// (LHS) and the membership scalar is `expected` (RHS). The SQL
		// fast-path compiles this via the @> containment operator; this
		// branch is the post-filter / non-pushdown mirror (#1674).
		items, ok := payloadArrayElements(actual)
		if !ok {
			// Field is absent or not an array -> contains nothing.
			return false, nil
		}
		for _, item := range items {
			match, err := compareEquality(item, expected, OpEq)
			if err != nil {
				// Heterogeneous element vs scalar type mismatch is a
				// non-match, not a query failure.
				continue
			}
			if match {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("operator %q is not supported for payload comparisons", op)
	}
	return false, nil
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

func compareEquality(actual any, expected any, op ComparisonOperator) (bool, error) {
	// Lazily evaluate `now()` / `timestamp()` AST nodes. They show up
	// when the SQL fast-path didn't compile the comparison (e.g. JSON
	// path comparisons that fall back to in-memory filtering). Same
	// resolution semantics as the SQL path: substitute the current
	// time at evaluation time.
	if _, ok := expected.(*ast.TimestampExprFunc); ok {
		expected = time.Now().UTC().Format(time.RFC3339Nano)
	}
	switch want := expected.(type) {
	case nil:
		match := actual == nil
		if op == OpEq {
			return match, nil
		}
		return !match, nil
	case bool:
		actualBool, ok := toBool(actual)
		if !ok {
			if op == OpEq {
				return false, nil
			}
			return true, nil
		}
		if op == OpEq {
			return actualBool == want, nil
		}
		return actualBool != want, nil
	case int64:
		actualNum, ok := toFloat(actual)
		if !ok {
			if op == OpEq {
				return false, nil
			}
			return true, nil
		}
		expectedNum := float64(want)
		if op == OpEq {
			return actualNum == expectedNum, nil
		}
		return actualNum != expectedNum, nil
	case float64:
		actualNum, ok := toFloat(actual)
		if !ok {
			if op == OpEq {
				return false, nil
			}
			return true, nil
		}
		if op == OpEq {
			return actualNum == want, nil
		}
		return actualNum != want, nil
	case string:
		actualStr, ok := toString(actual)
		if !ok {
			if op == OpEq {
				return false, nil
			}
			return true, nil
		}
		if op == OpEq {
			return actualStr == want, nil
		}
		return actualStr != want, nil
	default:
		return false, fmt.Errorf("unsupported literal type %T in comparison", expected)
	}
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func literalToFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func toBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	default:
		return false, false
	}
}

func toString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v), true
	default:
		return "", false
	}
}

func valueInCollection(actual any, collection *normalizedCollection) (bool, error) {
	if collection == nil {
		return false, fmt.Errorf("collection is nil")
	}
	switch collection.kind {
	case valueKindString:
		actualStr, ok := toString(actual)
		if !ok {
			return false, nil
		}
		for _, candidate := range collection.strings {
			if actualStr == candidate {
				return true, nil
			}
		}
		return false, nil
	case valueKindNumber:
		actualNum, ok := toFloat(actual)
		if !ok {
			return false, nil
		}
		for _, candidate := range collection.numbers {
			if actualNum == candidate {
				return true, nil
			}
		}
		return false, nil
	case valueKindBool:
		actualBool, ok := toBool(actual)
		if !ok {
			return false, nil
		}
		for _, candidate := range collection.bools {
			if actualBool == candidate {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unsupported collection kind %v", collection.kind)
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

func (e *MemQLEngine) executeFilterQuery(ctx context.Context, cmp *ComparisonExpression, filter compiledExpression, timestamp *time.Time, target int, sorter *compiledSort) ([]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	var nodes []memorynodes.MemoryNode

	query := db.NewSelect().Model(&nodes)

	if filter.sql != "" {
		query = query.Where(filter.sql, filter.args...)
	}
	if timestamp != nil {
		query = query.Where(`"createdAt" <= ?`, timestamp.UTC())
	}
	orderExprs := []string{`"createdAt" DESC`}
	if sorter != nil {
		orderExprs = sorter.orderExpressions()
	}
	for _, expr := range orderExprs {
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

	sqlLimit := fetchTarget * 2
	if sqlLimit < fetchTarget {
		sqlLimit = fetchTarget
	}

	query = query.Limit(sqlLimit)

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	uniqueIds := make([]string, 0, len(nodes))
	idSet := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		if _, exists := idSet[id]; exists {
			continue
		}
		idSet[id] = struct{}{}
		uniqueIds = append(uniqueIds, id)
	}

	var latest map[string]memorynodes.MemoryNode
	var err error
	if len(uniqueIds) > 0 {
		latest, err = e.loadLatestNodes(ctx, uniqueIds, timestamp)
		if err != nil {
			return nil, err
		}
	}

	payloadCache := make(map[string]map[string]any)
	seen := make(map[string]struct{})
	result := make([]memorynodes.MemoryNode, 0, len(nodes))

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
		if cmp != nil {
			match, err := nodeMatchesComparison(candidate, cmp, payloadCache)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
		}

		seen[id] = struct{}{}
		result = append(result, candidate)
		if target > 0 && len(result) >= target {
			break
		}
	}

	return result, nil
}

// evaluateShapeTemplatesExpression lists available shape templates.
