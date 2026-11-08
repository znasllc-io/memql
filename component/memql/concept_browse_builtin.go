package memql

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"google.golang.org/protobuf/encoding/protojson"
)

// Generic inspection is the one concept-agnostic reader. Build its predicate
// from registered, public text fields and execute through the ordinary query
// planner so row authorization, version selection and opaque cursors remain
// identical to named queries. No field paths or executable text come from args.
func (e *MemQLEngine) conceptRowsSearchBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil || access.UserId == "" {
		return nil, fmt.Errorf("concept search requires sign-in")
	}
	conceptID := strings.TrimSpace(stringArg(args, "conceptId"))
	concept, err := e.concepts.Get(conceptID)
	if err != nil {
		return nil, fmt.Errorf("unknown concept %q", conceptID)
	}
	needle := strings.TrimSpace(stringArg(args, "search"))
	if needle == "" {
		return nil, fmt.Errorf("concept search requires nonblank text")
	}
	fields, err := conceptSearchFields(concept)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return singleVirtualRow("memql:conceptRowsPage", "page", map[string]any{"rows": []any{}, "nextCursor": ""})
	}
	var matching ExpressionNode
	for _, name := range fields {
		part := &ComparisonExpression{Field: FieldReference{Raw: "payload." + name, Parts: []string{"payload", name}}, Operator: OpIncludesFold, Value: needle}
		if matching == nil {
			matching = part
		} else {
			matching = &LogicalExpression{Op: LogicalOr, Left: matching, Right: part}
		}
	}
	root := &LogicalExpression{Op: LogicalAnd, Left: &ComparisonExpression{Field: FieldReference{Raw: "concept", Parts: []string{"concept"}}, Operator: OpEq, Value: concept.Name}, Right: matching}
	// This raw inspector has no authored projection. Giving its temporary
	// query a binding below enables ordinary row-tier pushdown, but cannot
	// grant the PII exemption trusted named projections receive. Reuse the
	// generic-browse policy before LIMIT: if only the subject may read an
	// undeclared PII concept, narrow to that subject's canonical row id. A
	// post-page redaction would hide an older own row behind foreign matches.
	if rowAuthzDeclFor(concept.Name) == nil && rowAuthzPIIUnboundDenies(contextWithRowAuthzBinding(ctx, ""), concept.Name, "") {
		subject := concept.Name + ":" + BareShortId(rowAuthzActorUserId(ctx))
		root = &LogicalExpression{Op: LogicalAnd, Left: root, Right: &ComparisonExpression{
			Field: FieldReference{Raw: "id", Parts: []string{"id"}}, Operator: OpEq, Value: subject,
		}}
	}
	signature := sha256.Sum256([]byte(concept.Name + "\x00" + needle + "\x00" + strings.Join(fields, "\x00")))
	queryHash := hex.EncodeToString(signature[:])
	name := "conceptBrowse" + queryHash
	cursor, err := decodeConceptSearchCursor(stringArg(args, "cursor"), queryHash)
	if err != nil {
		return nil, err
	}
	// A request-local query definition gives the planner a declared binding
	// without registering user-controlled constructs in the shared registry.
	functions := newFunctionRegistry()
	if err := functions.add(&Function{Name: name, Expr: root, ExprSource: canonicalExpression(root), BoundConcept: concept.Name, FunctionKind: "query", Enabled: true, CacheTTL: "0"}); err != nil {
		return nil, err
	}
	result, err := e.executeWith(ContextWithCursor(ctx, cursor), fmt.Sprintf(`sort(paginate(query %s(), 10), "createdAt", "desc")`, name), functions, nil, false)
	if err != nil {
		return nil, err
	}
	// Retain the raw node schema/intrinsics that a generic inspector presents,
	// but omit graph-expansion neighbors: only this page's roots are list rows.
	byID := map[string]map[string]any{}
	for _, node := range result.Bundle.GetNodes() {
		if node.GetConcept() != concept.Name {
			continue
		}
		wire, err := protojson.Marshal(node)
		if err != nil {
			return nil, err
		}
		var row map[string]any
		if err := json.Unmarshal(wire, &row); err != nil {
			return nil, err
		}
		byID[node.GetId()] = row
	}
	rows := make([]map[string]any, 0, len(result.Bundle.GetRootIds()))
	for _, id := range result.Bundle.GetRootIds() {
		if row, ok := byID[id]; ok {
			rows = append(rows, row)
		}
	}
	nextCursor := ""
	if result.GetMeta().Cursor != "" {
		encoded, err := json.Marshal(conceptSearchCursor{Query: queryHash, Inner: result.GetMeta().Cursor})
		if err != nil {
			return nil, err
		}
		nextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return singleVirtualRow("memql:conceptRowsPage", "page", map[string]any{"rows": rows, "nextCursor": nextCursor})
}

func conceptSearchFields(concept *memorynodes.Concept) ([]string, error) {
	raw, err := concept.DefinitionSchema()
	if err != nil {
		return nil, err
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	secrets := map[string]bool{}
	for _, name := range concept.SecretFields() {
		secrets[name] = true
	}
	fields := []string{}
	for name, property := range schema.Properties {
		if secrets[name] {
			continue
		}
		text := property["type"] == "string"
		if types, ok := property["type"].([]any); ok {
			for _, kind := range types {
				if kind == "string" {
					text = true
				}
			}
		}
		if text {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields, nil
}

// The inner engine cursor protects sort direction; this envelope additionally
// binds it to the concept and search question without replica-local state.
type conceptSearchCursor struct {
	Query string `json:"q"`
	Inner string `json:"c"`
}

func decodeConceptSearchCursor(token, query string) (string, error) {
	if token == "" {
		return "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("invalid concept search cursor")
	}
	var cursor conceptSearchCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Query != query || cursor.Inner == "" {
		return "", fmt.Errorf("concept search cursor does not match this search")
	}
	return cursor.Inner, nil
}
