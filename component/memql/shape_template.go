package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type shapeTemplate interface {
	isShapeTemplate()
}

type shapeObject struct {
	Fields map[string]shapeTemplate
}

func (*shapeObject) isShapeTemplate() {}

type shapeArray struct {
	Items []shapeTemplate
}

func (*shapeArray) isShapeTemplate() {}

type shapeLiteral struct {
	Value any
}

func (*shapeLiteral) isShapeTemplate() {}

type shapeNodeFunc struct {
	Fields []FieldReference
}

func (*shapeNodeFunc) isShapeTemplate() {}

type shapeRelationFunc struct {
	Relation string
	Template shapeTemplate
}

func (*shapeRelationFunc) isShapeTemplate() {}

type shapeSIValue struct {
	Invocation *AIInvocation
	Data       shapeTemplate
}

func (*shapeSIValue) isShapeTemplate() {}

// shapeMatchExpr represents a match() conditional expression with case branches and optional default.
type shapeMatchExpr struct {
	Cases   []*shapeCaseBranch
	Default shapeTemplate
}

func (*shapeMatchExpr) isShapeTemplate() {}

// shapeCaseBranch represents a single case(condition, value) branch in a match expression.
type shapeCaseBranch struct {
	Condition shapeCondition
	Value     shapeTemplate
}

// shapeCondition is the interface for match case conditions.
type shapeCondition interface {
	isShapeCondition()
}

// shapeSpecCondition references a named spec as a condition.
type shapeSpecCondition struct {
	SpecName string
}

func (*shapeSpecCondition) isShapeCondition() {}

// shapeComparisonCondition represents an inline comparison like node("field") == "value".
type shapeComparisonCondition struct {
	Left     shapeTemplate      // typically shapeNodeFunc
	Operator ComparisonOperator // ==, !=, in, >, <, etc.
	Right    any                // literal value or list
}

func (*shapeComparisonCondition) isShapeCondition() {}

// shapeJSONFunc wraps a template and serializes its output to a JSON string.
type shapeJSONFunc struct {
	Inner shapeTemplate
}

func (*shapeJSONFunc) isShapeTemplate() {}

func applyShapeTemplate(ctx context.Context, bundle *memqlv1.GraphBundle, tmpl shapeTemplate, runtime *siRuntime, specs map[string]*Spec) (any, error) {
	if tmpl == nil || bundle == nil {
		return bundle, nil
	}

	graph := newShapeGraph(bundle, specs)
	rootIds := make([]string, 0, len(bundle.RootIds))
	for _, id := range bundle.RootIds {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		rootIds = append(rootIds, trimmed)
	}
	if len(rootIds) == 0 {
		for i := range bundle.Nodes {
			if bundle.Nodes[i] == nil {
				continue
			}
			rootIds = append(rootIds, strings.TrimSpace(bundle.Nodes[i].Id))
		}
	}

	results := make([]any, 0, len(rootIds))
	for _, rootId := range rootIds {
		if rootId == "" {
			continue
		}
		if graph.node(rootId) == nil {
			continue
		}
		value, err := graph.renderTemplate(ctx, rootId, tmpl, runtime, map[string]struct{}{rootId: {}})
		if err != nil {
			return nil, err
		}
		results = append(results, value)
	}

	// Canonical query result envelope (memql#1710): a shape-projected query
	// ALWAYS returns an array, so 0 / 1 / many results serialize to the same
	// shape -- [] / [x] / [x, y, ...]. This is the single, central enforcement
	// point for the contract: every shaped query funnels through here, so no
	// per-query code path can diverge. Previously a single match was unwrapped
	// to a bare object (`return results[0]`), which forced every consumer to
	// special-case single-vs-multi; that unwrap lived ONLY here, so removing it
	// makes the array envelope universal.
	//
	// `results` is initialised via make([]any, 0, ...), so the empty case is a
	// non-nil empty slice that marshals to `[]` (not `null`).
	return results, nil
}

type shapeGraph struct {
	nodes map[string]*memqlv1.MemoryNode
	edges map[string]map[string][]string
	specs map[string]*Spec
}

func newShapeGraph(bundle *memqlv1.GraphBundle, specs map[string]*Spec) *shapeGraph {
	graph := &shapeGraph{
		nodes: make(map[string]*memqlv1.MemoryNode),
		edges: make(map[string]map[string][]string),
		specs: specs,
	}
	if bundle == nil {
		return graph
	}
	for i := range bundle.Nodes {
		node := bundle.Nodes[i]
		if node == nil {
			continue
		}
		id := strings.TrimSpace(node.Id)
		if id == "" {
			continue
		}
		graph.nodes[id] = node
	}
	for _, edge := range bundle.Edges {
		edgeType := strings.ToLower(strings.TrimSpace(edge.Type))
		if edgeType == "" {
			continue
		}
		from := strings.TrimSpace(edge.FromId)
		to := strings.TrimSpace(edge.ToId)
		if from == "" || to == "" {
			continue
		}
		if _, ok := graph.edges[edgeType]; !ok {
			graph.edges[edgeType] = make(map[string][]string)
		}
		graph.edges[edgeType][from] = append(graph.edges[edgeType][from], to)
	}
	return graph
}

func (g *shapeGraph) node(id string) *memqlv1.MemoryNode {
	if g == nil {
		return nil
	}
	return g.nodes[strings.TrimSpace(id)]
}

func (g *shapeGraph) renderTemplate(ctx context.Context, nodeId string, tmpl shapeTemplate, runtime *siRuntime, visited map[string]struct{}) (any, error) {
	switch typed := tmpl.(type) {
	case *shapeObject:
		result := make(map[string]any, len(typed.Fields))
		for key, child := range typed.Fields {
			value, err := g.renderTemplate(ctx, nodeId, child, runtime, copyVisited(visited))
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		return result, nil
	case *shapeArray:
		values := make([]any, 0, len(typed.Items))
		for _, child := range typed.Items {
			value, err := g.renderTemplate(ctx, nodeId, child, runtime, copyVisited(visited))
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case *shapeLiteral:
		return typed.Value, nil
	case *shapeNodeFunc:
		// When extracting a single field, return the leaf value directly (not wrapped in a map)
		if len(typed.Fields) == 1 {
			return extractNodeFieldValue(g.node(nodeId), typed.Fields[0]), nil
		}
		return buildNodeProjection(g.node(nodeId), typed.Fields), nil
	case *shapeRelationFunc:
		return g.renderRelation(ctx, nodeId, typed, runtime, visited)
	case *shapeSIValue:
		return g.renderSIValue(ctx, nodeId, typed, runtime, visited)
	case *shapeMatchExpr:
		return g.renderMatchExpr(ctx, nodeId, typed, runtime, visited)
	case *shapeJSONFunc:
		return g.renderJSONFunc(ctx, nodeId, typed, runtime, visited)
	default:
		return nil, fmt.Errorf("unsupported shape template element %T", tmpl)
	}
}

func (g *shapeGraph) renderRelation(ctx context.Context, nodeId string, rel *shapeRelationFunc, runtime *siRuntime, visited map[string]struct{}) ([]any, error) {
	edgeTypes := relationEdgeTypes(rel.Relation)
	targets := make([]string, 0)
	for _, edgeType := range edgeTypes {
		if nextByType, ok := g.edges[edgeType]; ok {
			targets = append(targets, nextByType[nodeId]...)
		}
	}
	if len(targets) == 0 {
		return []any{}, nil
	}

	results := make([]any, 0, len(targets))
	for _, targetId := range targets {
		trimmed := strings.TrimSpace(targetId)
		if trimmed == "" {
			continue
		}
		if _, seen := visited[trimmed]; seen {
			continue
		}
		nextVisited := copyVisited(visited)
		nextVisited[trimmed] = struct{}{}
		value, err := g.renderTemplate(ctx, trimmed, rel.Template, runtime, nextVisited)
		if err != nil {
			return nil, err
		}
		results = append(results, value)
	}

	return results, nil
}

func (g *shapeGraph) renderSIValue(ctx context.Context, nodeId string, value *shapeSIValue, runtime *siRuntime, visited map[string]struct{}) (any, error) {
	if value == nil || value.Invocation == nil {
		return nil, fmt.Errorf("ai() invocation is not defined")
	}
	if runtime == nil {
		return nil, fmt.Errorf("ai() runtime is not configured")
	}

	var data any
	if value.Data != nil {
		rendered, err := g.renderTemplate(ctx, nodeId, value.Data, runtime, copyVisited(visited))
		if err != nil {
			return nil, err
		}
		data = rendered
	}

	return runtime.Invoke(ctx, value.Invocation, data)
}

// renderJSONFunc renders the inner template and serializes the result to a JSON string.
// This is useful for embedding structured data as JSON strings in AI prompt templates.
func (g *shapeGraph) renderJSONFunc(ctx context.Context, nodeId string, fn *shapeJSONFunc, runtime *siRuntime, visited map[string]struct{}) (string, error) {
	if fn == nil || fn.Inner == nil {
		return "null", nil
	}

	rendered, err := g.renderTemplate(ctx, nodeId, fn.Inner, runtime, copyVisited(visited))
	if err != nil {
		return "", err
	}

	jsonBytes, err := json.Marshal(rendered)
	if err != nil {
		return "", fmt.Errorf("json() failed to serialize: %w", err)
	}

	return string(jsonBytes), nil
}

// renderMatchExpr evaluates a match() expression with short-circuit evaluation.
// It iterates through case branches in order, returning the value of the first matching condition.
// Falls back to default if no case matches, or returns nil if no default is provided.
func (g *shapeGraph) renderMatchExpr(ctx context.Context, nodeId string, match *shapeMatchExpr, runtime *siRuntime, visited map[string]struct{}) (any, error) {
	if match == nil {
		return nil, nil
	}

	node := g.node(nodeId)
	if node == nil {
		// No node context, fall through to default
		if match.Default != nil {
			return g.renderTemplate(ctx, nodeId, match.Default, runtime, copyVisited(visited))
		}
		return nil, nil
	}

	// Evaluate cases in order (short-circuit)
	for _, branch := range match.Cases {
		if branch == nil || branch.Condition == nil {
			continue
		}

		matched, err := g.evaluateCondition(ctx, nodeId, branch.Condition, runtime, visited)
		if err != nil {
			return nil, fmt.Errorf("match() condition evaluation failed: %w", err)
		}

		if matched {
			return g.renderTemplate(ctx, nodeId, branch.Value, runtime, copyVisited(visited))
		}
	}

	// No case matched, use default
	if match.Default != nil {
		return g.renderTemplate(ctx, nodeId, match.Default, runtime, copyVisited(visited))
	}

	return nil, nil
}

// evaluateCondition evaluates a match case condition against the current node.
func (g *shapeGraph) evaluateCondition(ctx context.Context, nodeId string, cond shapeCondition, runtime *siRuntime, visited map[string]struct{}) (bool, error) {
	switch typed := cond.(type) {
	case *shapeSpecCondition:
		return g.evaluateSpecCondition(nodeId, typed)
	case *shapeComparisonCondition:
		return g.evaluateComparisonCondition(ctx, nodeId, typed, runtime, visited)
	default:
		return false, fmt.Errorf("unsupported match condition type %T", cond)
	}
}

// evaluateSpecCondition evaluates a spec reference against the current node's payload.
func (g *shapeGraph) evaluateSpecCondition(nodeId string, cond *shapeSpecCondition) (bool, error) {
	if cond == nil || cond.SpecName == "" {
		return false, fmt.Errorf("spec condition requires a spec name")
	}

	spec, ok := g.specs[cond.SpecName]
	if !ok || spec == nil {
		return false, fmt.Errorf("spec %q not found", cond.SpecName)
	}

	node := g.node(nodeId)
	if node == nil {
		return false, nil
	}

	// Evaluate the spec expression against the node's payload
	payload := structToMap(node.Payload)
	return evaluateExprAgainstPayload(spec.Expr, payload)
}

// evaluateComparisonCondition evaluates an inline comparison like node("field") == "value".
func (g *shapeGraph) evaluateComparisonCondition(ctx context.Context, nodeId string, cond *shapeComparisonCondition, runtime *siRuntime, visited map[string]struct{}) (bool, error) {
	if cond == nil {
		return false, nil
	}

	// Render the left side to get the actual value
	leftValue, err := g.renderTemplate(ctx, nodeId, cond.Left, runtime, copyVisited(visited))
	if err != nil {
		return false, fmt.Errorf("failed to evaluate left side of comparison: %w", err)
	}

	// Compare using the operator
	return compareValues(leftValue, cond.Operator, cond.Right)
}

// evaluateExprAgainstPayload evaluates a spec expression against a node's payload.
func evaluateExprAgainstPayload(expr ExpressionNode, payload map[string]any) (bool, error) {
	if expr == nil {
		return true, nil
	}

	switch node := expr.(type) {
	case *ComparisonExpression:
		return evaluateComparisonAgainstPayload(node, payload)
	case *LogicalExpression:
		left, err := evaluateExprAgainstPayload(node.Left, payload)
		if err != nil {
			return false, err
		}
		right, err := evaluateExprAgainstPayload(node.Right, payload)
		if err != nil {
			return false, err
		}
		switch node.Op {
		case LogicalAnd:
			return left && right, nil
		case LogicalOr:
			return left || right, nil
		default:
			return false, fmt.Errorf("unsupported logical operator %q", node.Op)
		}
	case *RelationshipExpression:
		// Relationship expressions in specs cannot be evaluated against a single node's payload
		// They require graph traversal which is not available in match() context
		return false, fmt.Errorf("relationship expressions in specs are not supported in match() conditions")
	default:
		return false, fmt.Errorf("unsupported expression type %T in spec condition", expr)
	}
}

// evaluateComparisonAgainstPayload evaluates a comparison expression against a payload.
func evaluateComparisonAgainstPayload(expr *ComparisonExpression, payload map[string]any) (bool, error) {
	if expr == nil {
		return true, nil
	}

	// Get the field value from payload
	fieldValue := getPayloadFieldValue(expr.Field, payload)

	return compareValues(fieldValue, expr.Operator, expr.Value)
}

// getPayloadFieldValue extracts a field value from a payload map.
func getPayloadFieldValue(ref FieldReference, payload map[string]any) any {
	if len(ref.Parts) == 0 {
		return nil
	}

	// Skip "payload" prefix if present
	parts := ref.Parts
	if len(parts) > 0 && strings.EqualFold(parts[0], "payload") {
		parts = parts[1:]
	}

	current := any(payload)
	for _, part := range parts {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[part]
		default:
			return nil
		}
	}

	return current
}

// compareValues compares two values using the given operator.
func compareValues(left any, op ComparisonOperator, right any) (bool, error) {
	// Handle map[string]any from node() calls - extract single value if present
	if m, ok := left.(map[string]any); ok {
		if len(m) == 0 {
			// Empty map means the field was not found
			left = nil
		} else if len(m) == 1 {
			for _, v := range m {
				left = v
				break
			}
		}
	}

	switch op {
	case OpEq:
		return valuesEqual(left, right), nil
	case OpNe:
		return !valuesEqual(left, right), nil
	case OpMissing:
		return left == nil, nil
	case OpNotMissing:
		return left != nil, nil
	case OpIn:
		return valueInList(left, right), nil
	case OpOut:
		return !valueInList(left, right), nil
	case OpHas:
		return valueInList(right, left), nil
	case OpGt, OpGe, OpLt, OpLe:
		return compareOrdered(left, op, right)
	default:
		return false, fmt.Errorf("unsupported comparison operator %q", op)
	}
}

// valuesEqual checks if two values are equal.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Convert numbers for comparison
	aNum, aIsNum := toFloat64(a)
	bNum, bIsNum := toFloat64(b)
	if aIsNum && bIsNum {
		return aNum == bNum
	}

	// String comparison
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// valueInList checks if a value is in a list.
func valueInList(value any, list any) bool {
	if list == nil {
		return false
	}

	switch typed := list.(type) {
	case []any:
		for _, item := range typed {
			if valuesEqual(value, item) {
				return true
			}
		}
	case []string:
		strVal := fmt.Sprintf("%v", value)
		for _, item := range typed {
			if strVal == item {
				return true
			}
		}
	}

	return false
}

// compareOrdered compares two values using ordered comparison operators.
func compareOrdered(left any, op ComparisonOperator, right any) (bool, error) {
	leftNum, leftIsNum := toFloat64(left)
	rightNum, rightIsNum := toFloat64(right)

	if leftIsNum && rightIsNum {
		switch op {
		case OpGt:
			return leftNum > rightNum, nil
		case OpGe:
			return leftNum >= rightNum, nil
		case OpLt:
			return leftNum < rightNum, nil
		case OpLe:
			return leftNum <= rightNum, nil
		}
	}

	// Fall back to string comparison
	leftStr := fmt.Sprintf("%v", left)
	rightStr := fmt.Sprintf("%v", right)

	switch op {
	case OpGt:
		return leftStr > rightStr, nil
	case OpGe:
		return leftStr >= rightStr, nil
	case OpLt:
		return leftStr < rightStr, nil
	case OpLe:
		return leftStr <= rightStr, nil
	}

	return false, nil
}

// toFloat64 attempts to convert a value to float64.
func toFloat64(v any) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case string:
		if f, err := strconv.ParseFloat(typed, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func relationEdgeTypes(name string) []string {
	switch strings.ToLower(name) {
	case "children":
		return []string{graphEdgeTypeChild}
	case "aliases":
		return []string{strings.ToLower(relationshipTypeAlias), strings.ToLower(relationshipTypeEquals)}
	case "contains":
		return []string{strings.ToLower(relationshipTypeContains)}
	case "owns":
		return []string{strings.ToLower(relationshipTypeOwns)}
	case "createdby":
		return []string{strings.ToLower(relationshipTypeCreatedBy)}
	case "interactions":
		return []string{strings.ToLower(relationshipTypeInteracts)}
	default:
		return []string{}
	}
}

func copyVisited(src map[string]struct{}) map[string]struct{} {
	dst := make(map[string]struct{}, len(src))
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}

// extractNodeFieldValue extracts a single field value from a node.
// This returns the leaf value directly (e.g., a string or number), not wrapped in a map.
// For example, node("id") returns "v1:examples:..." instead of {"id": "v1:examples:..."}.
func extractNodeFieldValue(node *memqlv1.MemoryNode, ref FieldReference) any {
	if node == nil || len(ref.Parts) == 0 {
		return nil
	}

	field := strings.ToLower(strings.TrimSpace(ref.Parts[0]))

	// Handle metadata fields
	switch field {
	case "id":
		return node.Id
	case "concept":
		return node.Concept
	case "type":
		return node.Type
	case "createdby":
		return node.CreatedBy
	case "createdat":
		if node.CreatedAt != nil {
			return node.CreatedAt.AsTime()
		}
		return nil
	case "schema":
		if node.Schema != nil {
			return cloneJSONSchemaDocument(node.Schema)
		}
		return nil
	case "payload":
		// Navigate into payload
		if node.Payload == nil {
			return nil
		}
		payload := structToMap(node.Payload)
		if len(ref.Parts) == 1 {
			// Just "payload" - return entire payload
			return cloneValue(payload)
		}
		// Navigate the path: payload.profile.displayName -> ["payload", "profile", "displayName"]
		return navigatePath(payload, ref.Parts[1:])
	case "provenance":
		// Provenance is a first-class proto field. Bare `row.provenance`
		// returns the whole {kind, name, trigger, via} object;
		// `row.provenance.kind` etc. returns a leaf string.
		pb := node.GetProvenance()
		if pb == nil {
			return nil
		}
		prov := map[string]any{"kind": pb.GetKind(), "name": pb.GetName()}
		if t := pb.GetTrigger(); t != "" {
			prov["trigger"] = t
		}
		if v := pb.GetVia(); v != "" {
			prov["via"] = v
		}
		if len(ref.Parts) == 1 {
			return prov
		}
		return navigatePath(prov, ref.Parts[1:])
	default:
		// Unknown field
		return nil
	}
}

// navigatePath traverses a nested map using the given path parts and returns the leaf value.
func navigatePath(data map[string]any, parts []string) any {
	if len(parts) == 0 || data == nil {
		return nil
	}

	value, ok := data[parts[0]]
	if !ok {
		return nil
	}

	if len(parts) == 1 {
		// This is the leaf - return the value directly
		return cloneValue(value)
	}

	// Need to navigate deeper
	childMap, ok := value.(map[string]any)
	if !ok {
		// Can't navigate further - the path is invalid
		return nil
	}

	return navigatePath(childMap, parts[1:])
}

func buildNodeProjection(node *memqlv1.MemoryNode, refs []FieldReference) map[string]any {
	result := make(map[string]any)
	if node == nil {
		return result
	}

	if len(refs) == 0 {
		if node.Payload != nil {
			for key, value := range structToMap(node.Payload) {
				result[key] = cloneValue(value)
			}
		}
		result["id"] = node.Id
		if node.Concept != "" {
			result["concept"] = node.Concept
		}
		if node.Type != "" {
			result["type"] = node.Type
		}
		if node.CreatedAt != nil {
			result["createdAt"] = node.CreatedAt
		}
		if node.CreatedBy != "" {
			result["createdBy"] = node.CreatedBy
		}
		if node.Schema != nil {
			result["schema"] = cloneJSONSchemaDocument(node.Schema)
		}
		return result
	}

	payloadRefs := make([]FieldReference, 0, len(refs))
	metaFields := make(map[string]struct{})

	for _, ref := range refs {
		if len(ref.Parts) == 0 {
			continue
		}
		if strings.EqualFold(ref.Parts[0], "payload") {
			payloadRefs = append(payloadRefs, ref)
			continue
		}
		metaFields[strings.ToLower(strings.TrimSpace(ref.Parts[0]))] = struct{}{}
	}

	if len(payloadRefs) > 0 {
		payload := selectPayload(structToMap(node.Payload), payloadRefs)
		for key, value := range payload {
			result[key] = value
		}
	}

	for field := range metaFields {
		switch field {
		case "id":
			result["id"] = node.Id
		case "concept":
			if node.Concept != "" {
				result["concept"] = node.Concept
			}
		case "type":
			if node.Type != "" {
				result["type"] = node.Type
			}
		case "createdat":
			if node.CreatedAt != nil {
				result["createdAt"] = node.CreatedAt
			}
		case "createdby":
			if node.CreatedBy != "" {
				result["createdBy"] = node.CreatedBy
			}
		case "schema":
			if node.Schema != nil {
				result["schema"] = cloneJSONSchemaDocument(node.Schema)
			}
		case "provenance":
			if pb := node.GetProvenance(); pb != nil {
				prov := map[string]any{"kind": pb.GetKind(), "name": pb.GetName()}
				if t := pb.GetTrigger(); t != "" {
					prov["trigger"] = t
				}
				if v := pb.GetVia(); v != "" {
					prov["via"] = v
				}
				result["provenance"] = prov
			}
		}
	}

	return result
}

func shapeTemplateSignature(t shapeTemplate) string {
	if t == nil {
		return "graph-bundle"
	}
	builder := &strings.Builder{}
	writeShapeSignature(builder, t)
	return string(cacheIdEngine.FromString(builder.String()))
}

func writeShapeSignature(builder *strings.Builder, tmpl shapeTemplate) {
	switch typed := tmpl.(type) {
	case *shapeObject:
		builder.WriteString("obj{")
		keys := make([]string, 0, len(typed.Fields))
		for key := range typed.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			builder.WriteString(key)
			builder.WriteString(":")
			writeShapeSignature(builder, typed.Fields[key])
			builder.WriteString(";")
		}
		builder.WriteString("}")
	case *shapeArray:
		builder.WriteString("arr[")
		for _, item := range typed.Items {
			writeShapeSignature(builder, item)
			builder.WriteString(",")
		}
		builder.WriteString("]")
	case *shapeLiteral:
		builder.WriteString(fmt.Sprintf("lit(%v)", typed.Value))
	case *shapeNodeFunc:
		fields := canonicalFieldReferenceStrings(typed.Fields)
		builder.WriteString("node(")
		builder.WriteString(strings.Join(fields, ","))
		builder.WriteString(")")
	case *shapeRelationFunc:
		builder.WriteString("rel(")
		builder.WriteString(typed.Relation)
		builder.WriteString("|")
		writeShapeSignature(builder, typed.Template)
		builder.WriteString(")")
	case *shapeSIValue:
		builder.WriteString("ai(")
		if typed.Invocation != nil {
			builder.WriteString(typed.Invocation.TemplateId)
			if typed.Invocation.ProviderOverride != nil {
				builder.WriteString("|")
				builder.WriteString(strings.TrimSpace(*typed.Invocation.ProviderOverride))
			}
			if typed.Invocation.CacheSeconds != nil {
				builder.WriteString("|ttl=")
				builder.WriteString(strconv.Itoa(*typed.Invocation.CacheSeconds))
			}
		}
		builder.WriteString(")")
		if typed.Data != nil {
			builder.WriteString("{")
			writeShapeSignature(builder, typed.Data)
			builder.WriteString("}")
		}
	case *shapeMatchExpr:
		builder.WriteString("match(")
		for i, branch := range typed.Cases {
			if i > 0 {
				builder.WriteString(",")
			}
			builder.WriteString("case(")
			writeConditionSignature(builder, branch.Condition)
			builder.WriteString(",")
			writeShapeSignature(builder, branch.Value)
			builder.WriteString(")")
		}
		if typed.Default != nil {
			builder.WriteString(",default(")
			writeShapeSignature(builder, typed.Default)
			builder.WriteString(")")
		}
		builder.WriteString(")")
	case *shapeJSONFunc:
		builder.WriteString("json(")
		writeShapeSignature(builder, typed.Inner)
		builder.WriteString(")")
	}
}

func writeConditionSignature(builder *strings.Builder, cond shapeCondition) {
	if cond == nil {
		builder.WriteString("nil")
		return
	}
	switch typed := cond.(type) {
	case *shapeSpecCondition:
		builder.WriteString("spec(")
		builder.WriteString(typed.SpecName)
		builder.WriteString(")")
	case *shapeComparisonCondition:
		builder.WriteString("cmp(")
		writeShapeSignature(builder, typed.Left)
		builder.WriteString(string(typed.Operator))
		builder.WriteString(fmt.Sprintf("%v", typed.Right))
		builder.WriteString(")")
	}
}
