package memoryNodes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
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

	// DisplayCard captures the per-concept rendering hints declared
	// via the `@displayCard(...)` annotation. Concept-agnostic
	// clients (the cockpit's Concepts tab, future generic browsers)
	// project rows through these slot names instead of carrying
	// per-concept rendering code. Nil when the concept didn't
	// declare the annotation -- clients should fall back to a
	// generic "id + intrinsics" rendering.
	//
	// See memql#160 for the design notes + slot semantics.
	DisplayCard struct {
		// Primary is the payload field that names the row (e.g.
		// "name" for agents, "title" for spaces, "goal" for plans,
		// "inviteeEmail" for invitations). Mandatory when the
		// annotation is present.
		Primary string `json:"primary"`
		// Secondary is contextual: a role, a type discriminator, a
		// kind enum. Optional.
		Secondary string `json:"secondary,omitempty"`
		// Tertiary is extra context (owner display, parent space,
		// short tag). Optional.
		Tertiary string `json:"tertiary,omitempty"`
		// Status is a boolean or short-enum field that drives a
		// colored badge in the row chrome. Optional.
		Status string `json:"status,omitempty"`
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

		// DisplayCard carries the per-concept rendering hints
		// declared via `@displayCard(...)`. Nil when the concept
		// did not declare the annotation. See memql#160.
		DisplayCard *DisplayCard `json:"displayCard,omitempty"`

		// RowAuthz carries the row-authorization tier declared via
		// `@rowAuthz(...)`: who may see this concept's rows, stated
		// once on the concept instead of as an `actor.*` term each
		// author must remember to type into every filter over it.
		// Nil when the concept declared no tier.
		//
		// PHASE 1 IS INERT (memql#2920). Nothing on the query path
		// reads this field -- no predicate is injected and no result
		// set changes. It exists so #2803's Phase 2 shadow mode has a
		// declared tier to compute against, and TestRowAuthzIsInert
		// gates that nothing starts reading it by accident.
		//
		// The type is the parser's own, not a local mirror, so the
		// loader and the memqlmigrate codemod cannot drift in what a
		// declaration means.
		RowAuthz *parser.RowAuthzDecl `json:"rowAuthz,omitempty"`

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

// PIIFields returns the names of every top-level field the concept's
// definition schema marks with the x-pii custom keyword (emitted from
// the field's @pii annotation). The engine's hard-delete scrub
// (memql#1711) consults this list to zero every personally-identifying
// field generically, so adding a new PII field to a concept needs only
// the @pii annotation -- the scrub picks it up automatically and cannot
// drift out of sync with a hand-maintained list. Returns nil when the
// concept declares no PII fields. Order follows JSON map iteration and
// is not significant -- the scrub clears every name regardless.
func (c *Concept) PIIFields() []string {
	if c == nil {
		return nil
	}
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}

	var schema struct {
		Properties map[string]struct {
			PII bool `json:"x-pii"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}

	result := make([]string, 0, len(schema.Properties))
	for name, prop := range schema.Properties {
		if prop.PII {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	}
	return result
}

// SecretFields returns the names of every top-level field the concept's
// definition schema marks with the x-secret custom keyword (emitted from
// the field's @secret annotation).
//
// The engine's validation-error redaction (memql#3036) consults this list.
// Derived generically, exactly as PIIFields is: annotating a new field is the
// whole change, and there is no hand-maintained list to drift out of sync.
//
// SCOPE, and it is deliberately partial. Read this before assuming a value is
// covered: @secret reaches exactly ONE validation surface, named below. The
// uncovered list is what has been VERIFIED uncovered, not a closed enumeration
// -- three successive passes over this engine each found a surface the previous
// pass had walked past (the most recent is memql#3117). Treat a surface absent
// from this list as unclassified, never as covered.
//
// COVERED: the memql function-args validator
// (component/memql/function_validator.go), which quotes a rejected argument
// value into its message at five sites (enum / minimum / maximum / pattern /
// date-time). Those five print <redacted> for a secret field.
//
// Matching is BY ARGUMENT NAME, not by write target. markSecretArgsFields
// stamps an args field whose NAME appears in this list, so a mutation whose
// insert block writes `apiKey: args.credential` on a @secret `apiKey` field
// leaves `credential` UNREDACTED -- the arg name is what is compared, and
// renaming between arg and field is the common style in this corpus.
//
// NOT COVERED -- query results. A @secret value is returned in full by any
// query projecting it. That is an authorization decision needing a definition
// of "elevated", and it interacts with the per-row authz model deferred under
// memql#2803.
//
// COVERED -- the automation args binder
// (component/automations/args_binding.go), a second validator mirroring this
// rule set over EVENT payloads, closed by memql#3111. A graph.node.created
// event carries the concept row's fields flattened into its payload
// (component/memql/executor_mutation.go), so a secret value used to be quoted
// in full there AND written to a WARN log (refuseFireForArgs) -- the one place
// a concept row value demonstrably reached a structured log.
//
// It resolves the secret set differently from the function-args path, and the
// difference is worth knowing: the automation contract carries no concept
// binding, so the binder recovers the concept from the EVENT TOPIC
// (graph.node.created.<concept>) at bind time rather than from a compile-time
// flag. Matching is still by field NAME, so the by-name caveat above applies
// there too.
//
// NOT COVERED -- the tool-args validator, MemQLEngine.validateToolArgs
// (component/memql/tool_execution.go:66). It is compiled from the SAME
// ArgsSchema that carries the Secret flag and is auto-registered for every
// enabled query and mutation, so it covers the same declarations this list
// does. It is the worst of the uncovered set: it runs BEFORE the covered
// validator on the agent path (tool_execution.go:366 precedes the handler at
// :375), it serializes the ENTIRE args map into a WARN log rather than quoting
// one value (:112-115), and formatToolValidationError (:256) does no redaction
// before the message is returned to the model. Tracked as memql#3117.
//
// NOT COVERED -- concept payload JSON-schema validation (Create, below), which
// enforces @minimum / @maximum / @format declared on the CONCEPT and
// interpolates the instance value into the jsonschema message. Any constraint
// the concept declares that the args block does not mirror is validated only
// there, so this redaction is bypassed entirely. Tracked separately.
//
// NOT COVERED -- length. "value too long (%d runes, max %d)"
// (function_validator.go:204, args_binding.go:119) reports a rune count for a
// secret field too. It quotes no value, but it is a disclosure.
//
// Returns nil when the concept declares no secret fields. Order follows JSON
// map iteration and is not significant.
func (c *Concept) SecretFields() []string {
	if c == nil {
		return nil
	}
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}

	var schema struct {
		Properties map[string]struct {
			Secret bool `json:"x-secret"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}

	result := make([]string, 0, len(schema.Properties))
	for name, prop := range schema.Properties {
		if prop.Secret {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	}
	return result
}

// fieldClassification carries the C5 (memql#2035) per-field access
// flags read off the concept's definition schema. A field is one of:
//   - internal  (x-internal): server-only -- never projected, never
//     accepted from caller args.
//   - serverSet (x-serverSet): stamped server-side -- not accepted
//     from caller args, but MAY be projected.
//   - public    (neither): projectable AND caller-acceptable.
type fieldClassification struct {
	Name      string
	Internal  bool
	ServerSet bool
}

// classifyFields returns every top-level payload field the concept
// declares, each tagged with its C5 access flags (x-internal /
// x-serverSet). Order follows JSON map iteration and is not
// significant; callers that need determinism sort the result. Returns
// nil when the concept declares no fields or has no definition schema.
func (c *Concept) classifyFields() []fieldClassification {
	if c == nil {
		return nil
	}
	raw, ok := c.Schemas[definitionSchemaKey]
	if !ok || len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]struct {
			Internal  bool `json:"x-internal"`
			ServerSet bool `json:"x-serverSet"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	result := make([]fieldClassification, 0, len(schema.Properties))
	for name, prop := range schema.Properties {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		result = append(result, fieldClassification{
			Name:      trimmed,
			Internal:  prop.Internal,
			ServerSet: prop.ServerSet,
		})
	}
	return result
}

// DeclaredFields returns the names of every top-level payload field
// declared on the concept's definition schema, INCLUDING @internal /
// @serverSet fields. Backs the spec/shape binding validator (epic
// #2281): a concept-bound spec may predicate by bare name on any
// declared field, internal ones included. Names are returned in
// ascending order for deterministic diagnostics.
func (c *Concept) DeclaredFields() []string {
	out := make([]string, 0)
	for _, f := range c.classifyFields() {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// ProjectableFields returns the names of every top-level payload field
// a shape's default projection should expose: all declared fields
// EXCEPT those marked @internal (x-internal). @serverSet fields ARE
// projectable (a `status` a mutation stamps is routinely rendered), so
// the default projection includes public + serverSet fields and
// excludes only the server-only @internal set. Names are returned in
// ascending order for deterministic shape templates. Backs the C5
// empty-shape default projection (memql#2035).
//
// NOTE createdAt / createdBy are NOT examples of this: they are
// reserved row intrinsics (see reservedPayloadFields), rejected as
// declared payload fields by ensureReservedFieldsNotDeclared, so they
// can never be @serverSet at all. They were cited here as examples
// until memql#2960, and the same error reached the authoring
// reference from this comment.
func (c *Concept) ProjectableFields() []string {
	out := make([]string, 0)
	for _, f := range c.classifyFields() {
		if f.Internal {
			continue
		}
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// PublicFields returns the names of every top-level payload field that
// is neither @internal nor @serverSet -- the caller-writable,
// projectable subset. Names are returned in ascending order. Backs the
// C5 mutation accept-list validation (memql#2035).
func (c *Concept) PublicFields() []string {
	out := make([]string, 0)
	for _, f := range c.classifyFields() {
		if f.Internal || f.ServerSet {
			continue
		}
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// InternalFields returns the names of every top-level payload field the
// concept marks @internal (x-internal) -- server-only fields that are
// never projected and never accepted from caller args. Ascending order.
func (c *Concept) InternalFields() []string {
	out := make([]string, 0)
	for _, f := range c.classifyFields() {
		if f.Internal {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ServerSetFields returns the names of every top-level payload field the
// concept marks @serverSet (x-serverSet) -- fields stamped server-side
// and never accepted from caller args (but projectable). Ascending order.
func (c *Concept) ServerSetFields() []string {
	out := make([]string, 0)
	for _, f := range c.classifyFields() {
		if f.ServerSet {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
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
// docs/public/concepts/identifiers.md ("Anti-patterns").
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
	// Delegate to the single source of truth in core/id so the storage
	// gate and every system-automation id generator (issue #1712) agree
	// on exactly one rule.
	return id.ValidateShortId(c.Name, nodeId)
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
