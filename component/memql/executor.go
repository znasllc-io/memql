package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type builtinExecutorHandler func(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error)

type (
	// valueKind identifies the type of a scalar value.
	valueKind int

	// compiledExpression represents a SQL expression and its associated arguments.
	compiledExpression struct {
		sql  string
		args []any
	}

	// normalizedCollection represents a collection of scalar values of the same type.
	normalizedCollection struct {
		kind    valueKind
		strings []string
		numbers []float64
		bools   []bool
	}
)

const (
	// valueKindString represents a string value.
	valueKindString valueKind = iota
	// valueKindNumber represents a number value.
	valueKindNumber
	// valueKindBool represents a boolean value.
	valueKindBool
)

func mutationActor(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFromContext(ctx))
	if actor == "" {
		return "", fmt.Errorf("no actor found in context")
	}
	return actor, nil
}

type bunStore struct {
	db *bun.DB
}

func (s *bunStore) InsertMemoryNode(ctx context.Context, node *memorynodes.MemoryNode) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("memory engine database not configured")
	}
	if node == nil {
		return fmt.Errorf("memory node is nil")
	}

	if strings.TrimSpace(node.ID) == "" {
		return fmt.Errorf("memory node id is required")
	}
	if strings.TrimSpace(node.Concept) == "" {
		return fmt.Errorf("memory node concept is required")
	}

	// ON CONFLICT DO NOTHING on the (partition, id, createdAt) PK.
	// Two simultaneous mutations with the same deterministic id can
	// land at the same microsecond -- React StrictMode in dev fires
	// effects twice, and concurrent autoJoin / dailySpace ensure
	// flows can race. Both inserts compute the same id (memQL
	// mutation ids are content-addressed) and the engine stamps the
	// same `time.Now()` if the second call arrives within the same
	// microsecond bucket. Without ON CONFLICT, the second insert
	// fails with SQLSTATE 23505 ("duplicate key value violates
	// unique constraint") and the user sees a mutation error in the
	// UI even though the row is already there. Append-only
	// semantics are preserved: distinct versions still get distinct
	// createdAt timestamps in the common case; only the
	// pathological "same row, same microsecond" case dedups, which
	// is the right behavior anyway.
	_, err := s.db.NewInsert().
		Model(node).
		On("CONFLICT (id, \"createdAt\") DO NOTHING").
		Exec(ctx)
	return err
}

func (s *bunStore) QueryMemoryNodes(ctx context.Context, params memorynodes.QueryParams) ([]memorynodes.MemoryNode, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	var nodes []memorynodes.MemoryNode
	query := s.db.NewSelect().Model(&nodes)

	if len(params.IDs) > 0 {
		query = query.Where("id IN (?)", bun.In(params.IDs))
	}

	if concept := strings.TrimSpace(params.Concept); concept != "" {
		query = query.Where("concept = ?", concept)
	}

	if createdBy := strings.TrimSpace(params.CreatedBy); createdBy != "" {
		query = query.Where(`"createdBy" = ?`, createdBy)
	}

	switch params.Order {
	case memorynodes.QueryOrderCreatedAtAsc:
		query = query.OrderExpr(`"createdAt" ASC`)
	default:
		query = query.OrderExpr(`"createdAt" DESC`)
	}

	if params.Limit > 0 {
		query = query.Limit(params.Limit)
	}

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	return nodes, nil
}

func (e *MemQLEngine) evaluateExpression(ctx context.Context, expr ExpressionNode, timestamp *time.Time, limit, offset int, sorter *compiledSort) ([]memorynodes.MemoryNode, error) {
	if expr == nil {
		return nil, nil
	}

	target := e.fetchTarget(limit, offset)
	if sorter != nil && sorter.needsFullScan() {
		target = e.config.MaxWindow
	}

	set, err := e.evaluateExpressionSet(ctx, expr, timestamp, target, sorter)
	if err != nil {
		return nil, err
	}

	nodes, err := mapToSortedSlice(set, sorter)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if offset >= len(nodes) {
			return []memorynodes.MemoryNode{}, nil
		}
		nodes = nodes[offset:]
	}

	effectiveLimit := limit
	if effectiveLimit <= 0 || effectiveLimit > e.config.MaxResults {
		effectiveLimit = e.config.MaxResults
	}

	if effectiveLimit > 0 && len(nodes) > effectiveLimit {
		nodes = nodes[:effectiveLimit]
	}

	return nodes, nil
}

func (e *MemQLEngine) evaluateExpressionSet(ctx context.Context, expr ExpressionNode, timestamp *time.Time, target int, sorter *compiledSort) (map[string]memorynodes.MemoryNode, error) {
	// Resolve any caller.X references using the AccessContext on ctx
	// before we start compiling SQL. This substitutes *CallerReference
	// comparison values with concrete values (or the owner-wildcard
	// sentinel for owners); subsequent compile stages never see the
	// reference type.
	resolved, err := resolveCallerReferences(ctx, expr)
	if err != nil {
		return nil, err
	}
	// Extract concept context from the entire expression tree once at the top level.
	// This allows short IDs to be resolved when concept is specified anywhere in the query.
	conceptContext := extractConceptFromExpression(resolved)
	// Auto-canonicalize payload.<fk-field> RHS values so queries with
	// bare-slug input still match canonical-stored rows. Mirrors the
	// insert-side auto-canon: inserts store canonical, queries compare
	// canonical, the engine handles both transparently per the
	// concept's @relationship metadata.
	resolved = e.canonicalizeRelationshipComparisons(ctx, resolved, conceptContext)
	return e.evaluateExpressionSetWithContext(ctx, resolved, timestamp, target, sorter, conceptContext)
}

func (e *MemQLEngine) evaluateExpressionSetWithContext(ctx context.Context, expr ExpressionNode, timestamp *time.Time, target int, sorter *compiledSort, conceptContext string) (map[string]memorynodes.MemoryNode, error) {
	// Optimization: Try to compile the entire expression tree into a single SQL query.
	// This is much more efficient than running separate queries and intersecting/unioning results.
	if combined, ok := e.tryCompileCombinedFilter(expr, conceptContext); ok {
		resolvedExpr, err := resolveExpressionForExecution(expr, conceptContext)
		if err != nil {
			return nil, err
		}
		nodes, err := e.executeCombinedFilterQuery(ctx, resolvedExpr, combined, timestamp, target, sorter)
		if err != nil {
			return nil, err
		}
		return nodesToMap(nodes), nil
	}

	switch node := expr.(type) {
	case *LogicalExpression:
		leftSet, err := e.evaluateExpressionSetWithContext(ctx, node.Left, timestamp, target, sorter, conceptContext)
		if err != nil {
			return nil, err
		}
		rightSet, err := e.evaluateExpressionSetWithContext(ctx, node.Right, timestamp, target, sorter, conceptContext)
		if err != nil {
			return nil, err
		}

		switch node.Op {
		case LogicalAnd:
			return intersectNodeSets(leftSet, rightSet), nil
		case LogicalOr:
			return unionNodeSets(leftSet, rightSet), nil
		default:
			return nil, fmt.Errorf("unsupported logical operator %q", node.Op)
		}
	case *ComparisonExpression:
		filter, err := e.compileComparisonExpressionWithContext(node, conceptContext)
		if err != nil {
			return nil, err
		}
		// Resolve short IDs to full IDs for post-filter comparison.
		// This ensures all internal operations use full IDs while short IDs
		// remain syntactic sugar at the query boundary.
		resolvedNode, err := resolveComparisonForExecution(node, conceptContext)
		if err != nil {
			return nil, err
		}
		nodes, err := e.executeFilterQuery(ctx, resolvedNode, filter, timestamp, target, sorter)
		if err != nil {
			return nil, err
		}
		return nodesToMap(nodes), nil
	case *RelationshipExpression:
		nodes, err := e.evaluateRelationshipExpression(ctx, node, timestamp, target)
		if err != nil {
			return nil, err
		}
		return nodesToMap(nodes), nil
	case *BuiltinFunctionExpression:
		nodes, err := e.evaluateBuiltinFunctionExpression(ctx, node, target)
		if err != nil {
			return nil, err
		}
		return nodesToMap(nodes), nil
	case *SpecReferenceExpression:
		// Look up the spec in the registry
		spec, err := e.specs.Get(node.Name)
		if err != nil {
			return nil, fmt.Errorf("spec reference %q: %w", node.Name, err)
		}
		// Evaluate the spec's expression inline
		return e.evaluateExpressionSetWithContext(ctx, spec.Expr, timestamp, target, sorter, conceptContext)
	case *SIExpression:
		return nil, fmt.Errorf("si() cannot be evaluated in this context; use it only in projection expressions")
	case *FunctionCallExpression:
		// Functions should be expanded during parsing; if we reach here, something went wrong
		return nil, fmt.Errorf("function %q was not expanded during parsing; this is a bug", node.Name)
	case *ErrorRefExpression:
		return nil, fmt.Errorf("error() cannot be evaluated in query context; it is only valid in automation onError handlers")
	case *ErrorExpression:
		return nil, fmt.Errorf("error() cannot be evaluated in query context; it is only valid in automation control flow")
	case nil:
		return map[string]memorynodes.MemoryNode{}, nil
	default:
		return nil, fmt.Errorf("unsupported expression node %T", expr)
	}
}

// ownerWildcardSentinel is a marker value used by the caller-reference
// resolver to signal that the comparison should match every row. It is
// recognised by compilePayloadComparison and friends, which emit `TRUE`
// as their SQL fragment when the value is of this type. Cluster owners
// hit this branch for owner-bypass semantics.
type ownerWildcardSentinel struct{}

// resolveCallerReferences walks an expression tree and substitutes every
// *CallerReference comparison value with a concrete value pulled from
// the AccessContext on ctx. For owners (and when no AccessContext is
// attached), `caller.partitions` resolves to ownerWildcardSentinel so
// the SQL compiler emits TRUE for that comparison -- the classic
// owner-bypass. For non-owner callers, `caller.partitions` resolves to
// the list of partition names their ACL grants access to.
//
// Supported paths (when a non-owner caller is making the request):
//
//	caller.partitions -> []string (partition names)
//	caller.userId     -> string
//	caller.identityId -> string
//	caller.role       -> string (owner/admin/writer/reader)
//	caller.primaryEmail -> string
//
// Only the partitions path emits the ownerWildcardSentinel. Scalar
// paths return the concrete string value regardless of role.
func resolveCallerReferences(ctx context.Context, expr ExpressionNode) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		ref, ok := node.Value.(*CallerReference)
		if !ok {
			return expr, nil
		}
		value, err := resolveCallerPath(ctx, ref.Path, node.Operator)
		if err != nil {
			return nil, err
		}
		resolved := *node
		resolved.Value = value
		return &resolved, nil
	case *LogicalExpression:
		left, err := resolveCallerReferences(ctx, node.Left)
		if err != nil {
			return nil, err
		}
		right, err := resolveCallerReferences(ctx, node.Right)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{Op: node.Op, Left: left, Right: right}, nil
	case *RelationshipExpression:
		target, err := resolveCallerReferences(ctx, node.Target)
		if err != nil {
			return nil, err
		}
		return &RelationshipExpression{Function: node.Function, Target: target}, nil
	default:
		return expr, nil
	}
}

// resolveCallerPath translates a dotted caller.X path into the scalar
// or list value that the comparison evaluator expects. op determines
// the expected shape -- OpIn/OpOut always produce a list (or the
// owner-wildcard sentinel).
func resolveCallerPath(ctx context.Context, path string, op ComparisonOperator) (any, error) {
	ac, _ := auth.AccessFromContext(ctx)
	switch path {
	case "partitions":
		// caller.partitions is going away in #56 phase 5. Owners still
		// get the wildcard sentinel (no-op in the SQL builder); everyone
		// else gets an empty list. The DSL reference itself is stripped
		// in phase 5.
		if op != OpIn && op != OpOut {
			return nil, fmt.Errorf("caller.partitions must be used with 'in' or 'not in'")
		}
		if ac == nil || ac.IsClusterOwner() {
			return ownerWildcardSentinel{}, nil
		}
		return []string{}, nil
	case "userId":
		if ac == nil {
			return "", nil
		}
		return ac.UserId, nil
	case "identityId":
		if ac == nil {
			return "", nil
		}
		return ac.IdentityId, nil
	case "role":
		if ac == nil {
			return "", nil
		}
		return string(ac.Role), nil
	case "primaryEmail":
		if ac == nil {
			return "", nil
		}
		return ac.PrimaryEmail, nil
	case "isOwner":
		if ac == nil {
			// No auth context (dev mode) -- treat as owner.
			return true, nil
		}
		return ac.IsClusterOwner(), nil
	default:
		return nil, fmt.Errorf("unsupported caller reference path %q (valid: partitions, userId, identityId, role, primaryEmail, isOwner)", path)
	}
}

// resolveExpressionForExecution creates a copy of an expression tree with any short IDs resolved to
// full IDs. This ensures that post-filter comparisons operate on the canonical storage IDs.
func resolveExpressionForExecution(expr ExpressionNode, conceptContext string) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		return resolveComparisonForExecution(node, conceptContext)
	case *LogicalExpression:
		left, err := resolveExpressionForExecution(node.Left, conceptContext)
		if err != nil {
			return nil, err
		}
		right, err := resolveExpressionForExecution(node.Right, conceptContext)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{
			Op:    node.Op,
			Left:  left,
			Right: right,
		}, nil
	default:
		// Relationship expressions and other nodes are not part of the combined filter fast-path.
		return expr, nil
	}
}

// expandSpecReferences recursively expands all SpecReferenceExpression nodes in an expression tree.
// This is necessary before passing expressions to functions that don't have access to the specs registry.
func (e *MemQLEngine) expandSpecReferences(expr ExpressionNode) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *SpecReferenceExpression:
		spec, err := e.specs.Get(node.Name)
		if err != nil {
			return nil, fmt.Errorf("spec reference %q: %w", node.Name, err)
		}
		// Recursively expand in case the spec itself contains spec references
		return e.expandSpecReferences(spec.Expr)
	case *LogicalExpression:
		left, err := e.expandSpecReferences(node.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.expandSpecReferences(node.Right)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{
			Op:    node.Op,
			Left:  left,
			Right: right,
		}, nil
	case *RelationshipExpression:
		target, err := e.expandSpecReferences(node.Target)
		if err != nil {
			return nil, err
		}
		return &RelationshipExpression{
			Function: node.Function,
			Target:   target,
		}, nil
	default:
		// Other expression types don't contain nested expressions that need expansion
		return expr, nil
	}
}

// nodeMatchesExpression evaluates a MemQL expression tree against a single node. This is used to
// validate matches against the latest version of a node after a candidate scan.
func unionNodeSets(a, b map[string]memorynodes.MemoryNode) map[string]memorynodes.MemoryNode {
	if len(a) == 0 && len(b) == 0 {
		return map[string]memorynodes.MemoryNode{}
	}

	result := make(map[string]memorynodes.MemoryNode, len(a)+len(b))
	for id, node := range a {
		result[id] = node
	}
	for id, node := range b {
		if existing, ok := result[id]; ok {
			result[id] = moreRecentNode(existing, node)
		} else {
			result[id] = node
		}
	}
	return result
}

func intersectNodeSets(a, b map[string]memorynodes.MemoryNode) map[string]memorynodes.MemoryNode {
	if len(a) == 0 || len(b) == 0 {
		return map[string]memorynodes.MemoryNode{}
	}

	// Iterate over the smaller map to reduce lookups.
	if len(a) > len(b) {
		a, b = b, a
	}

	result := make(map[string]memorynodes.MemoryNode)
	for id, node := range a {
		if other, ok := b[id]; ok {
			result[id] = moreRecentNode(node, other)
		}
	}
	return result
}

func nodesToMap(nodes []memorynodes.MemoryNode) map[string]memorynodes.MemoryNode {
	result := make(map[string]memorynodes.MemoryNode, len(nodes))
	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		if existing, ok := result[id]; !ok || node.CreatedAt.After(existing.CreatedAt) {
			result[id] = node
		}
	}
	return result
}

func mapToSortedSlice(nodes map[string]memorynodes.MemoryNode, sorter *compiledSort) ([]memorynodes.MemoryNode, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	result := make([]memorynodes.MemoryNode, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node)
	}
	if sorter == nil {
		sort.Slice(result, func(i, j int) bool {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		})
		return result, nil
	}
	if err := sorter.sortNodes(result); err != nil {
		return nil, err
	}
	return result, nil
}

func moreRecentNode(a, b memorynodes.MemoryNode) memorynodes.MemoryNode {
	if a.CreatedAt.After(b.CreatedAt) {
		return a
	}
	return b
}



func normalizeIntrinsicFieldName(field string) string {
	trimmed := strings.TrimSpace(field)
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(trimmed, "_", ""))
}

func nodeFieldValue(node memorynodes.MemoryNode, field string) (string, error) {
	switch normalizeIntrinsicFieldName(field) {
	case "createdby":
		return strings.TrimSpace(node.CreatedBy), nil
	default:
		return "", fmt.Errorf("node field %q is not supported for relationships", field)
	}
}

func mapNodeFieldToColumn(field string) (string, error) {
	switch normalizeIntrinsicFieldName(field) {
	case "createdby":
		return `"createdBy"`, nil
	default:
		return "", fmt.Errorf("node field %q is not supported for relationship queries", field)
	}
}

func trimNodeResults(nodes []memorynodes.MemoryNode, max int) []memorynodes.MemoryNode {
	if max <= 0 || len(nodes) <= max {
		return nodes
	}
	return nodes[:max]
}

func (e *MemQLEngine) conceptForNode(node memorynodes.MemoryNode) (*memorynodes.Concept, error) {
	if e.concepts == nil {
		return nil, fmt.Errorf("concept registry is not initialized")
	}

	name := strings.TrimSpace(node.Concept)
	if name == "" {
		return nil, fmt.Errorf("memory node %q has empty concept", node.ID)
	}

	conceptMeta, err := e.concepts.Get(name)
	if err == nil {
		return conceptMeta, nil
	}

	if builtin := builtinConceptMetadata(name); builtin != nil {
		return builtin, nil
	}

	for _, candidate := range e.concepts.List() {
		if candidate == nil {
			continue
		}
		if strings.EqualFold(candidate.Name, name) {
			return candidate, nil
		}
	}

	// Integration capabilities frequently return synthetic memory
	// nodes with concept names like "integration:auth:user" or
	// "integration:knowledge:augmentAnalyze" -- not real registered
	// concepts, just envelope tags so the WS bridge can deliver the
	// payload back to the caller. The default graph-expand pass
	// (depth=1) would otherwise reject them here. Treat the
	// namespace as transient: return a stub Concept with no
	// relationships, so expandGraph's per-relationship pass sees an
	// empty definitions list and short-circuits cleanly. The WS
	// payload still carries the full node + payload through to the
	// caller. Without this stub the auth / database / embedding /
	// knowledge integrations all fail the moment a frontend calls
	// them via executeQuery (their handlers run, then the engine
	// chokes on the result).
	if strings.HasPrefix(strings.ToLower(name), "integration:") {
		return &memorynodes.Concept{
			Name:        name,
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Transient integration-capability result; no relationships.",
		}, nil
	}

	return nil, fmt.Errorf("unable to resolve concept %q", name)
}

func builtinConceptMetadata(name string) *memorynodes.Concept {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "memql:concept":
		return &memorynodes.Concept{
			Name:        "memql:concept",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Concept metadata exposed via concepts().",
		}
	case "memql:concepts":
		return &memorynodes.Concept{
			Name:        "memql:concepts",
			NodeType:    memorynodes.NodeTypeCollection,
			Description: "Concept listing metadata exposed via concepts().",
		}
	case "memql:docs":
		return &memorynodes.Concept{
			Name:        "memql:docs",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Embedded MemQL reference documentation.",
		}
	case "memql:functions":
		return &memorynodes.Concept{
			Name:        "memql:functions",
			NodeType:    memorynodes.NodeTypeCollection,
			Description: "Named function metadata exposed via functions().",
		}
	case "memql:tools":
		return &memorynodes.Concept{
			Name:        "memql:tools",
			NodeType:    memorynodes.NodeTypeCollection,
			Description: "MCP-compatible tool metadata exposed via tools().",
		}
	case "memql:help":
		return &memorynodes.Concept{
			Name:        "memql:help",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Help/introspection payload exposed via help().",
		}
	case "memql:validate":
		return &memorynodes.Concept{
			Name:        "memql:validate",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Validation result payload exposed via validate().",
		}
	case "memql:shapetemplates":
		return &memorynodes.Concept{
			Name:        "memql:shapeTemplates",
			NodeType:    memorynodes.NodeTypeCollection,
			Description: "Shape template metadata exposed via shapeTemplates().",
		}
	case "memql:shapehelp":
		return &memorynodes.Concept{
			Name:        "memql:shapeHelp",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Shape template details exposed via shapeHelp().",
		}
	case "memql:contentid":
		return &memorynodes.Concept{
			Name:        "memql:contentId",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Content-addressed ID preview exposed via contentId().",
		}
	case "memql:previewinsert":
		return &memorynodes.Concept{
			Name:        "memql:previewInsert",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Insert preview and validation result exposed via previewInsert().",
		}
	case "memql:preflight":
		return &memorynodes.Concept{
			Name:        "memql:preflight",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Preflight validation result exposed via contentId() and previewInsert().",
		}
	case "memql:version":
		return &memorynodes.Concept{
			Name:        "memql:version",
			NodeType:    memorynodes.NodeTypeObject,
			Description: "Service version information exposed via memqlVersion().",
		}
	default:
		return nil
	}
}

func payloadToMap(payload json.RawMessage) (map[string]any, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("payload is empty")
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func extractStringFromMap(data map[string]any, path []string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("payload path is empty")
	}

	var current any = data
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("payload path %s is missing", strings.Join(path, "."))
		}
		value, ok := next[key]
		if !ok {
			return "", fmt.Errorf("payload path %s is missing", strings.Join(path, "."))
		}
		current = value
	}

	if current == nil {
		return "", fmt.Errorf("payload path %s is null", strings.Join(path, "."))
	}

	switch v := current.(type) {
	case string:
		return strings.TrimSpace(v), nil
	case float64, float32, int, int64, int32, int16, int8:
		return strings.TrimSpace(fmt.Sprintf("%v", v)), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("payload path %s must be a string-compatible value", strings.Join(path, "."))
	}
}

func valueAtPath(data map[string]any, path []string) (any, bool) {
	current := any(data)
	for _, segment := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := m[segment]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func extractStringArrayFromMap(data map[string]any, path []string) ([]string, bool, error) {
	value, ok := valueAtPath(data, path)
	if !ok {
		return nil, false, nil
	}

	array, ok := value.([]any)
	if !ok {
		return nil, false, fmt.Errorf("payload path %s must be an array of strings", strings.Join(path, "."))
	}

	result := make([]string, 0, len(array))
	for idx, item := range array {
		str, ok := item.(string)
		if !ok {
			return nil, false, fmt.Errorf("payload path %s array index %d must be a string", strings.Join(path, "."), idx)
		}
		trimmed := strings.TrimSpace(str)
		if trimmed == "" {
			return nil, false, fmt.Errorf("payload path %s array index %d must not be empty", strings.Join(path, "."), idx)
		}
		result = append(result, trimmed)
	}

	return result, true, nil
}

func extractStringValueOrArrayFromMap(data map[string]any, path []string) ([]string, bool, error) {
	value, ok := valueAtPath(data, path)
	if !ok {
		return nil, false, nil
	}

	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, false, fmt.Errorf("payload path %s must not be empty", strings.Join(path, "."))
		}
		return []string{trimmed}, true, nil
	case []any:
		result := make([]string, 0, len(v))
		for idx, item := range v {
			str, ok := item.(string)
			if !ok {
				return nil, false, fmt.Errorf("payload path %s array index %d must be a string", strings.Join(path, "."), idx)
			}
			trimmed := strings.TrimSpace(str)
			if trimmed == "" {
				return nil, false, fmt.Errorf("payload path %s array index %d must not be empty", strings.Join(path, "."), idx)
			}
			result = append(result, trimmed)
		}
		return result, true, nil
	default:
		return nil, false, fmt.Errorf("payload path %s must be a string or array of strings", strings.Join(path, "."))
	}
}

func setPayloadValue(payload map[string]any, field string, value string) error {
	path, err := splitRelationshipField(field)
	if err != nil {
		return err
	}

	current := payload
	for i, segment := range path {
		if i == len(path)-1 {
			current[segment] = value
			return nil
		}

		nextVal, exists := current[segment]
		if !exists || nextVal == nil {
			nextMap := make(map[string]any)
			current[segment] = nextMap
			current = nextMap
			continue
		}

		nextMap, ok := nextVal.(map[string]any)
		if !ok {
			return fmt.Errorf("payload path %s conflicts with existing non-object value", strings.Join(path[:i+1], "."))
		}
		current = nextMap
	}

	return nil
}

func addAllowedConcept(m map[string]map[string]struct{}, id, conceptName string) {
	id = strings.TrimSpace(id)
	conceptName = strings.TrimSpace(conceptName)
	if id == "" || conceptName == "" {
		return
	}
	set, ok := m[id]
	if !ok {
		set = make(map[string]struct{})
		m[id] = set
	}
	set[conceptName] = struct{}{}
}

func keys(m map[string]map[string]struct{}) []string {
	result := make([]string, 0, len(m))
	for key := range m {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := set[key]; ok {
			continue
		}
		set[key] = struct{}{}
		result = append(result, trimmed)
	}
	sort.Strings(result)
	return result
}

// executeMutation dispatches an insert() or update() mutation to the
// right path based on the parsed Kind. Empty Kind defaults to insert
// for backwards compatibility -- mutation templates compiled before
// update() landed don't carry the field.
func (e *MemQLEngine) buildGraphBundle(ctx context.Context, nodes []memorynodes.MemoryNode, depth int, timestamp *time.Time) (*memqlv1.GraphBundle, error) {
	builder := newGraphBundleBuilder()
	if len(nodes) == 0 {
		return builder.toBundle(), nil
	}

	for _, node := range nodes {
		builder.addRoot(node.ID)
		visited := make(map[string]struct{})
		if err := e.expandGraph(ctx, node, depth, timestamp, visited, builder, 0); err != nil {
			return nil, err
		}
	}

	return builder.toBundle(), nil
}

func (e *MemQLEngine) expandGraph(ctx context.Context, node memorynodes.MemoryNode, depth int, timestamp *time.Time, visited map[string]struct{}, builder *graphBundleBuilder, currentDepth int) error {
	id := strings.TrimSpace(node.ID)
	if id != "" {
		if _, ok := visited[id]; ok {
			return nil
		}
		visited[id] = struct{}{}
	}

	apiNode, err := toAPIMemoryNode(&node)
	if err != nil {
		return err
	}
	builder.addNode(apiNode)

	if depth == 0 {
		return nil
	}

	nextDepth := depth
	if depth > 0 {
		nextDepth = depth - 1
	}

	conceptMeta, err := e.conceptForNode(node)
	if err != nil {
		return err
	}

	defs := e.relationshipDefinitionsForConcept(conceptMeta.Name)
	if len(defs) == 0 {
		return nil
	}

	childDefs := filterRelationshipDefinitions(defs, relationshipTypeParent, []string{relationshipDirectionIncoming, relationshipDirectionBidirectional})
	if len(childDefs) > 0 {
		children, err := e.resolveChildOf(ctx, []memorynodes.MemoryNode{node}, timestamp, e.config.MaxResults)
		if err != nil {
			return err
		}
		children = trimNodeResults(children, e.config.MaxResults)
		for _, child := range children {
			childId := strings.TrimSpace(child.ID)
			if childId == "" {
				continue
			}
			builder.addEdge(id, childId, graphEdgeTypeChild, currentDepth+1)
			if _, ok := visited[childId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, child, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	aliasDefs := filterRelationshipDefinitions(defs, relationshipTypeAlias, nil)
	if len(aliasDefs) > 0 {
		aliases, err := e.resolveAliasOrEquals(ctx, []memorynodes.MemoryNode{node}, timestamp, relationshipTypeAlias, e.config.MaxResults)
		if err != nil {
			return err
		}
		aliases = trimNodeResults(aliases, e.config.MaxResults)
		for _, alias := range aliases {
			aliasId := strings.TrimSpace(alias.ID)
			if aliasId == "" || strings.EqualFold(aliasId, id) {
				continue
			}
			builder.addEdge(id, aliasId, relationshipTypeAlias, currentDepth+1)
			if _, ok := visited[aliasId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, alias, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	equalsDefs := filterRelationshipDefinitions(defs, relationshipTypeEquals, nil)
	if len(equalsDefs) > 0 {
		equalNodes, err := e.resolveAliasOrEquals(ctx, []memorynodes.MemoryNode{node}, timestamp, relationshipTypeEquals, e.config.MaxResults)
		if err != nil {
			return err
		}
		equalNodes = trimNodeResults(equalNodes, e.config.MaxResults)
		for _, eq := range equalNodes {
			eqId := strings.TrimSpace(eq.ID)
			if eqId == "" || strings.EqualFold(eqId, id) {
				continue
			}
			builder.addEdge(id, eqId, relationshipTypeEquals, currentDepth+1)
			if _, ok := visited[eqId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, eq, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	interactionDefs := filterRelationshipDefinitions(defs, relationshipTypeInteracts, nil)
	if len(interactionDefs) > 0 {
		related, err := e.resolveInteractsWith(ctx, []memorynodes.MemoryNode{node}, timestamp, e.config.MaxResults)
		if err != nil {
			return err
		}
		related = trimNodeResults(related, e.config.MaxResults)
		for _, interaction := range related {
			interactionId := strings.TrimSpace(interaction.ID)
			if interactionId == "" || strings.EqualFold(interactionId, id) {
				continue
			}
			builder.addEdge(id, interactionId, relationshipTypeInteracts, currentDepth+1)
			if _, ok := visited[interactionId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, interaction, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	createdByDefs := filterRelationshipDefinitions(defs, relationshipTypeCreatedBy, nil)
	if len(createdByDefs) > 0 {
		related, err := e.resolveCreatedBy(ctx, []memorynodes.MemoryNode{node}, timestamp, e.config.MaxResults)
		if err != nil {
			return err
		}
		related = trimNodeResults(related, e.config.MaxResults)
		for _, creator := range related {
			creatorId := strings.TrimSpace(creator.ID)
			if creatorId == "" || strings.EqualFold(creatorId, id) {
				continue
			}
			builder.addEdge(id, creatorId, relationshipTypeCreatedBy, currentDepth+1)
			if _, ok := visited[creatorId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, creator, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	containsDefs := filterRelationshipDefinitions(defs, relationshipTypeContains, []string{relationshipDirectionOutgoing, relationshipDirectionBidirectional})
	if len(containsDefs) > 0 {
		contained, err := e.resolveContains(ctx, []memorynodes.MemoryNode{node}, timestamp, e.config.MaxResults)
		if err != nil {
			return err
		}
		contained = trimNodeResults(contained, e.config.MaxResults)
		for _, child := range contained {
			childId := strings.TrimSpace(child.ID)
			if childId == "" || strings.EqualFold(childId, id) {
				continue
			}
			builder.addEdge(id, childId, relationshipTypeContains, currentDepth+1)
			if _, ok := visited[childId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, child, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	ownsDefs := filterRelationshipDefinitions(defs, relationshipTypeOwns, nil)
	if len(ownsDefs) > 0 {
		owned, err := e.resolveOwns(ctx, []memorynodes.MemoryNode{node}, timestamp, e.config.MaxResults)
		if err != nil {
			return err
		}
		owned = trimNodeResults(owned, e.config.MaxResults)
		for _, ownedNode := range owned {
			ownedId := strings.TrimSpace(ownedNode.ID)
			if ownedId == "" || strings.EqualFold(ownedId, id) {
				continue
			}
			builder.addEdge(id, ownedId, relationshipTypeOwns, currentDepth+1)
			if _, ok := visited[ownedId]; ok {
				continue
			}
			if err := e.expandGraph(ctx, ownedNode, nextDepth, timestamp, visited, builder, currentDepth+1); err != nil {
				return err
			}
		}
	}

	return nil
}

