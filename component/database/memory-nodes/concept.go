package memoryNodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

type (
	// RelationshipDefinition captures the structure of relationship metadata declared in concept.json files.
	RelationshipDefinition struct {
		Type          string `json:"type"`
		Field         string `json:"field"`
		FieldSource   string `json:"fieldSource,omitempty"`
		TargetConcept string `json:"targetConcept"`
		Direction     string `json:"direction"`
	}

	// Store defines the persistence operations required by concept runtime helpers.
	Store interface {
		InsertMemoryNode(ctx context.Context, node *MemoryNode) error
		QueryMemoryNodes(ctx context.Context, params QueryParams) ([]MemoryNode, error)
	}

	// Concept provides runtime helpers for interacting with memory nodes described by a concept definition.
	Concept struct {
		Name          string                     `json:"concept"`
		SchemaId      string                     `json:"schemaId"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
		NodeType      string                     `json:"type"`
		Description   string                     `json:"description,omitempty"`
		Relationships []RelationshipDefinition   `json:"relationships,omitempty"`

		// Version is the explicit version prefix declared via
		// @version("vN"). Empty means the version was derived from
		// the concepts/vN/... directory layout.
		Version string `json:"version,omitempty"`

		contentIdSalt string // server-side salt for content-addressed ID derivation
	}

	// SecretConcept mirrors Concept but is intended for secret memory node handling.
	SecretConcept struct {
		Name        string                     `json:"concept"`
		SchemaId    string                     `json:"schemaId"`
		Schemas     map[string]json.RawMessage `json:"schemas"`
		NodeType    string                     `json:"type"`
		Description string                     `json:"description,omitempty"`
	}

	// Node represents a memory node materialized through a concept accessor.
	Node struct {
		ID        string
		Concept   string
		Type      string
		CreatedAt time.Time
		CreatedBy string
		Schema    json.RawMessage
		Payload   json.RawMessage
		Metadata  json.RawMessage
	}
)

// UnmarshalJSON normalizes relationship definitions authored with either camelCase or snake_case keys.
func (r *RelationshipDefinition) UnmarshalJSON(data []byte) error {
	type alias struct {
		Type          string `json:"type"`
		Field         string `json:"field"`
		FieldSource   string `json:"fieldSource"`
		TargetConcept string `json:"targetConcept"`
		Direction     string `json:"direction"`
	}

	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}

	r.Type = strings.TrimSpace(a.Type)
	r.Field = strings.TrimSpace(a.Field)
	r.Direction = strings.TrimSpace(a.Direction)

	r.FieldSource = strings.TrimSpace(a.FieldSource)
	if r.FieldSource == "" {
		r.FieldSource = strings.TrimSpace(a.FieldSource)
	}

	r.TargetConcept = strings.TrimSpace(a.TargetConcept)
	if r.TargetConcept == "" {
		r.TargetConcept = strings.TrimSpace(a.TargetConcept)
	}

	return nil
}

const (
	definitionSchemaKey = "definition"
	deleteSchemaKey     = "delete"
)

var (
	schemaIdCache   sync.Map
	contentIdEngine = id.New()
)

// SetContentIdSalt configures the server-side salt used for content-addressed ID derivation.
func (c *Concept) SetContentIdSalt(salt string) {
	if c != nil {
		c.contentIdSalt = salt
	}
}

// DeriveContentId generates a deterministic content-addressed ID from the payload.
// Uses the same algorithm and salt as Create() for exact ID prediction.
func (c *Concept) DeriveContentId(payload map[string]any) string {
	input := map[string]any{
		"concept": c.Name,
		"payload": payload,
	}
	if c.contentIdSalt != "" {
		input["salt"] = c.contentIdSalt
	}
	return string(contentIdEngine.MustFromMap(input))
}

// Create validates the payload against the definition schema and inserts a new memory node.
func (c *Concept) Create(ctx context.Context, store Store, params CreateParams) (Node, error) {
	if store == nil {
		return Node{}, fmt.Errorf("concept store is required")
	}

	payload := clonePayload(params.Payload)
	if payload == nil {
		return Node{}, fmt.Errorf("concept payload is required")
	}

	payloadId := strings.TrimSpace(stringFromPayload(payload["id"]))
	payload = StripReservedPayloadFields(payload)

	nodeId := strings.TrimSpace(params.ID)
	if nodeId == "" {
		nodeId = payloadId
	}
	if nodeId == "" {
		nodeId = c.DeriveContentId(payload)
	}

	validationPayload := clonePayload(payload)
	if err := c.validate(definitionSchemaKey, validationPayload); err != nil {
		return Node{}, fmt.Errorf("concept payload validation failed: %w", err)
	}

	now := time.Now().UTC()
	if params.Clock != nil {
		now = params.Clock().UTC()
	}

	if err := c.validateShortId(nodeId); err != nil {
		return Node{}, fmt.Errorf("concept %q: %w", c.Name, err)
	}
	storageId := c.storageId(nodeId)
	if storageId == "" {
		return Node{}, fmt.Errorf("concept storage id is required")
	}

	schemaBytes, err := c.schemaBytes(definitionSchemaKey)
	if err != nil {
		return Node{}, err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return Node{}, fmt.Errorf("marshal concept payload: %w", err)
	}

	nodeType := strings.TrimSpace(c.NodeType)
	if nodeType == "" {
		nodeType = NodeTypeObject
	}

	node := &MemoryNode{
		ID:        storageId,
		Concept:   c.Name,
		Type:      nodeType,
		CreatedAt: now,
		CreatedBy: strings.TrimSpace(params.Actor),
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	// Wire metadata from CreateParams into the node.
	if len(params.Metadata) > 0 {
		metaBytes, err := json.Marshal(params.Metadata)
		if err != nil {
			return Node{}, fmt.Errorf("marshal metadata: %w", err)
		}
		node.Metadata = metaBytes
	}

	// Stamp provenance from the Go context (engine-stamped intrinsic;
	// see component/provenance). The context-carried value is set by
	// the originating writer -- SeedMaterializer, automation step
	// runner, gRPC mutation handler, etc. NOT NULL: writes without
	// provenance are bugs, rejected here at the row-construction
	// layer.
	provBytes, provErr := provenanceJSONFromContext(ctx)
	if provErr != nil {
		return Node{}, fmt.Errorf("concept %q: %w", c.Name, provErr)
	}
	node.Provenance = provBytes

	if strings.TrimSpace(node.CreatedBy) == "" {
		return Node{}, fmt.Errorf("actor is required")
	}

	if err := store.InsertMemoryNode(ctx, node); err != nil {
		return Node{}, err
	}

	return toNode(node), nil
}

// Delete inserts a deletion tombstone for the provided identifier.
func (c *Concept) Delete(ctx context.Context, store Store, params DeleteParams) (Node, error) {
	if store == nil {
		return Node{}, fmt.Errorf("concept store is required")
	}

	nodeId := strings.TrimSpace(params.ID)
	if nodeId == "" {
		return Node{}, fmt.Errorf("id is required")
	}

	if strings.TrimSpace(params.Actor) == "" {
		return Node{}, fmt.Errorf("actor is required")
	}

	payload := map[string]any{
		"id":      c.storageId(nodeId),
		"deleted": true,
	}
	if reason := strings.TrimSpace(params.Reason); reason != "" {
		payload["reason"] = reason
	}

	if err := c.validate(deleteSchemaKey, payload); err != nil {
		return Node{}, fmt.Errorf("concept delete payload validation failed: %w", err)
	}

	now := time.Now().UTC()
	if params.Clock != nil {
		now = params.Clock().UTC()
	}

	schemaBytes, err := c.schemaBytes(deleteSchemaKey)
	if err != nil {
		return Node{}, err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return Node{}, fmt.Errorf("marshal delete payload: %w", err)
	}

	node := &MemoryNode{
		ID:        payload["id"].(string),
		Concept:   c.Name,
		CreatedAt: now,
		CreatedBy: strings.TrimSpace(params.Actor),
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	// Same provenance-stamping rule as inserts (see Create).
	// Tombstone-as-version preserves attribution of who/what deleted
	// each row in the per-version history.
	provBytes, provErr := provenanceJSONFromContext(ctx)
	if provErr != nil {
		return Node{}, fmt.Errorf("concept %q delete: %w", c.Name, provErr)
	}
	node.Provenance = provBytes

	if err := store.InsertMemoryNode(ctx, node); err != nil {
		return Node{}, err
	}

	return toNode(node), nil
}

// Query retrieves concept records applying default filters (e.g. skipping deletions).
func (c *Concept) Query(ctx context.Context, store Store, params QueryParams) ([]Node, error) {
	if store == nil {
		return nil, fmt.Errorf("concept store is required")
	}

	compositeIds := make([]string, 0, len(params.IDs))
	for _, raw := range params.IDs {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			if storageId := c.storageId(trimmed); storageId != "" {
				compositeIds = append(compositeIds, storageId)
			}
		}
	}

	queryParams := QueryParams{
		IDs:            compositeIds,
		Concept:        c.Name,
		CreatedBy:      strings.TrimSpace(params.CreatedBy),
		Limit:          params.Limit,
		Order:          QueryOrderCreatedAtDesc,
		IncludeDeleted: params.IncludeDeleted,
	}

	nodes, err := store.QueryMemoryNodes(ctx, queryParams)
	if err != nil {
		return nil, err
	}

	deletionSchemaId, _ := c.schemaVariantId(deleteSchemaKey)

	result := make([]Node, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))

	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}

		runtimeNode := toNode(&node)
		schemaId := extractSchemaId(runtimeNode.Schema)
		if schemaId == deletionSchemaId {
			seen[id] = struct{}{}
			if params.IncludeDeleted {
				result = append(result, runtimeNode)
			}
			continue
		}

		seen[id] = struct{}{}
		result = append(result, runtimeNode)
	}

	return result, nil
}

func (c *Concept) schemaBytes(variant string) ([]byte, error) {
	raw, ok := c.Schemas[variant]
	if !ok {
		return nil, fmt.Errorf("schema variant %q not registered for concept %q", variant, c.Name)
	}
	clone := make([]byte, len(raw))
	copy(clone, raw)
	return clone, nil
}

func (c *Concept) schemaVariantId(variant string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("concept runtime is not initialized")
	}
	if cached, ok := schemaIdCache.Load(c.cacheKey(variant)); ok {
		if id, valid := cached.(string); valid {
			return id, nil
		}
	}
	raw, ok := c.Schemas[variant]
	if !ok {
		return "", fmt.Errorf("schema variant %q not registered", variant)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("parse schema variant %q id: %w", variant, err)
	}
	if id, ok := parsed["$id"].(string); ok {
		id = strings.TrimSpace(id)
		schemaIdCache.Store(c.cacheKey(variant), id)
		return id, nil
	}
	return "", fmt.Errorf("schema variant %q missing $id", variant)
}

func (c *Concept) cacheKey(variant string) string {
	return c.Name + ":" + strings.TrimSpace(variant)
}

// SchemaVariantId returns the $id associated with the requested schema variant.
func (c *Concept) SchemaVariantId(variant string) (string, error) {
	return c.schemaVariantId(variant)
}

// DefinitionSchemaId returns the $id of the definition schema.
func (c *Concept) DefinitionSchemaId() (string, error) {
	return c.schemaVariantId(definitionSchemaKey)
}

// DeletionSchemaId returns the $id of the deletion schema, if registered.
func (c *Concept) DeletionSchemaId() (string, error) {
	return c.schemaVariantId(deleteSchemaKey)
}

// SchemaVariant returns the raw schema document for the requested variant.
func (c *Concept) SchemaVariant(variant string) (json.RawMessage, error) {
	raw, err := c.schemaBytes(variant)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// DefinitionSchema returns the definition schema document.
func (c *Concept) DefinitionSchema() (json.RawMessage, error) {
	return c.SchemaVariant(definitionSchemaKey)
}

// DeletionSchema returns the deletion schema document, if registered.
func (c *Concept) DeletionSchema() (json.RawMessage, error) {
	return c.SchemaVariant(deleteSchemaKey)
}

// RequiredFields extracts the list of required field names from the definition schema.
// Returns an empty slice if no required fields are declared or if the schema cannot be parsed.
func (c *Concept) RequiredFields() []string {
	if c == nil {
		return nil
	}
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}

	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}

	result := make([]string, 0, len(schema.Required))
	for _, field := range schema.Required {
		trimmed := strings.TrimSpace(field)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func toNode(node *MemoryNode) Node {
	if node == nil {
		return Node{}
	}
	return Node{
		ID:        strings.TrimSpace(node.ID),
		Concept:   strings.TrimSpace(node.Concept),
		Type:      strings.TrimSpace(node.Type),
		CreatedAt: node.CreatedAt,
		CreatedBy: strings.TrimSpace(node.CreatedBy),
		Schema:    cloneBytes(node.Schema),
		Payload:   cloneBytes(node.Payload),
		Metadata:  cloneBytes(node.Metadata),
	}
}

func (c *Concept) validate(variant string, payload any) error {
	raw, ok := c.Schemas[variant]
	if !ok {
		return fmt.Errorf("schema variant %q not registered", variant)
	}
	schema, err := compileSchema(c.cacheKey(variant), raw)
	if err != nil {
		return err
	}
	return schema.Validate(payload)
}

// validateShortId catches the class of bug where a caller hands in
// a colon-bearing compound string as the "shortId" -- the storage
// layer's HasPartition() check then misclassifies the first segment
// as a partition name and stores the malformed id without prepending
// {partition}:{concept}:. Bugs landed twice (seed materializer +
// checkpoint writer); this gate makes the next one fail loudly at
// insert time instead of silently in the DB. See
// docs/core/identifiers.md ("Anti-patterns").
//
// Two legitimate shapes for nodeId post-#56 phase 6:
//
//  1. Bare shortId with no colons -- the engine prepends
//     {concept}:. ANY shortId containing ':' falls through to
//     shape (2) and must be qualified.
//  2. Concept-qualified: starts with c.Name+":". Used by
//     dispatch-site composers (see composeReplyId in cognition for
//     the canonical example).
//
// Anything else is a caller bug.
func (c *Concept) validateShortId(nodeId string) error {
	if c == nil {
		return nil
	}
	trimmed := strings.TrimSpace(nodeId)
	if trimmed == "" {
		return nil // empty handled downstream
	}
	// Shape (1): bare, no colons -> ok.
	if !strings.ContainsRune(trimmed, ':') {
		return nil
	}
	// Shape (2): concept-qualified.
	if strings.HasPrefix(trimmed, c.Name+":") {
		return nil
	}
	return fmt.Errorf(
		"shortId %q must be a bare slug/UUID (no colons) or the concept-prefixed form (%q); got something else (see docs/core/identifiers.md)",
		trimmed, c.Name+":<short>",
	)
}

func (c *Concept) storageId(nodeId string) string {
	if c == nil {
		return ""
	}
	trimmed := strings.TrimSpace(nodeId)
	if trimmed == "" {
		return ""
	}
	// Already concept-qualified -> return as-is.
	if strings.HasPrefix(trimmed, c.Name+":") {
		return trimmed
	}
	return id.BuildNodeId(c.Name, trimmed)
}

func extractSchemaId(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return ""
	}
	if id, ok := schema["$id"].(string); ok {
		return strings.TrimSpace(id)
	}
	return ""
}

func stringFromPayload(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func clonePayload(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(payload))
	for key, value := range payload {
		cloned[key] = value
	}
	return cloned
}

// StripReservedPayloadFields removes reserved fields (id, createdAt, createdBy, etc.)
// from the payload. This modifies the input map in-place. Use clonePayload first if
// you need to preserve the original.
func StripReservedPayloadFields(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return payload
	}
	for key := range payload {
		if IsReservedPayloadField(key) {
			delete(payload, key)
		}
	}
	return payload
}

func cloneBytes(src []byte) json.RawMessage {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}
