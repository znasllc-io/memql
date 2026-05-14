package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/events"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/language/ast"
)

func (e *MemQLEngine) executeMutation(ctx context.Context, mutation MutationNode) (*ExecuteResult, error) {
	switch mutation.Kind {
	case ast.MutationKindUpdate:
		return e.executeUpdate(ctx, mutation)
	case ast.MutationKindInsert, "":
		return e.executeInsert(ctx, mutation)
	default:
		return nil, fmt.Errorf("unknown mutation kind %q", mutation.Kind)
	}
}

// executeUpdate runs the update() form: read the latest existing row
// by id, splat the partial payload on top, validate the merged result
// against the concept schema, then persist the merged row. Bridges
// the time-series-storage / mental-model-of-update gap so a caller
// can write `update(id=X, payload={status: "running"})` without
// re-passing every required field.
//
// Failure modes:
//   - id missing: error (we can't merge against nothing)
//   - no prior row at this id: error (use insert() to create the row first)
//   - merged payload fails concept schema validation: error (validation
//     bubbles up the same way executeInsert's does)
//
// On success, the new row appended to the time-series carries the
// merged payload; queries by id return it as the latest version.
func (e *MemQLEngine) executeUpdate(ctx context.Context, mutation MutationNode) (*ExecuteResult, error) {
	if e.concepts == nil {
		return nil, fmt.Errorf("concept registry is not initialized")
	}

	conceptName := strings.TrimSpace(mutation.Concept)
	if conceptName == "" {
		return nil, fmt.Errorf("update() mutation must specify a concept")
	}

	id := strings.TrimSpace(mutation.ID)
	if id == "" {
		return nil, fmt.Errorf("update() requires an explicit id; use insert() to create new rows")
	}

	conceptMeta, err := e.concepts.Get(conceptName)
	if err != nil {
		return nil, fmt.Errorf("concept %q not found: %w", conceptName, err)
	}

	rawPayload := strings.TrimSpace(mutation.PayloadRaw)
	if rawPayload == "" {
		return nil, fmt.Errorf("update() mutation payload is required")
	}

	partialPayload := make(map[string]any)
	if err := json.Unmarshal([]byte(rawPayload), &partialPayload); err != nil {
		return nil, fmt.Errorf("invalid payload JSON for update() mutation: %w", err)
	}

	// Look up the latest existing row by id under the concept's
	// effective partition (same partitioning rules as executeInsert
	// uses for the write).
	readPartition := e.partitionForConcept(ctx, conceptMeta.Name)
	store := &bunStore{db: e.database()}
	priorNodes, err := conceptMeta.Query(ctx, store, memorynodes.QueryParams{
		Partition: readPartition,
		IDs:       []string{id},
		Limit:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("update(): read latest row for concept %q id %q: %w", conceptName, id, err)
	}
	if len(priorNodes) == 0 {
		return nil, fmt.Errorf("update(): no existing row for concept %q id %q (use insert() to create)", conceptName, id)
	}

	// Decode prior payload and merge the partial fields on top.
	// Shallow merge: top-level fields in the partial replace those
	// in the prior; nested objects aren't deep-merged. Matches the
	// "patch this status field" mental model -- callers wanting a
	// nested merge can read-modify-write themselves.
	priorPayloadJSON, err := priorNodes[0].Payload.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("update(): marshal prior payload: %w", err)
	}
	mergedPayload := make(map[string]any)
	if err := json.Unmarshal(priorPayloadJSON, &mergedPayload); err != nil {
		return nil, fmt.Errorf("update(): decode prior payload: %w", err)
	}
	for k, v := range partialPayload {
		mergedPayload[k] = v
	}

	mergedJSON, err := json.Marshal(mergedPayload)
	if err != nil {
		return nil, fmt.Errorf("update(): marshal merged payload: %w", err)
	}

	// Hand off to executeInsert with the merged payload. The merged
	// payload now satisfies every @required field (because the prior
	// row did, and shallow-merge can't drop fields), so schema
	// validation inside Concept.Create will pass.
	mergedMutation := mutation
	mergedMutation.Kind = ast.MutationKindInsert
	mergedMutation.PayloadRaw = string(mergedJSON)
	result, err := e.executeInsert(ctx, mergedMutation)
	if err != nil {
		return result, err
	}

	// memQL is append-only: every successful update() materialises as
	// a new row that fires graph.node.created via executeInsert. Most
	// subscribers want one or the other -- automations like
	// emitScopeElevationCanvasCard fire on the FIRST insert (kind +
	// initial status), backend handlers like cognition's
	// plan-execution want only the state-transition deltas (the same
	// concept already exists; a subsequent insert means "moved to
	// running / succeeded / failed"). Mixing both onto
	// graph.node.created forced subscribers to gate via a status
	// filter and hope the same status never recurs.
	//
	// Solution: also publish graph.node.updated on the update() path.
	// Subscribers tracking transitions can listen on .updated and
	// skip the noise of every initial creation; subscribers tracking
	// births still listen on .created. Both events carry the same
	// payload so callers can pick whichever signal fits their model.
	if result != nil && result.Bundle != nil && len(result.Bundle.Nodes) > 0 {
		updatedNode := result.Bundle.Nodes[0]
		eventPayload := map[string]any{
			"id":        updatedNode.Id,
			"nodeId":    updatedNode.Id,
			"concept":   conceptMeta.Name,
			"nodeType":  updatedNode.Type,
			"partition": readPartition,
		}
		// Flatten payload fields onto the event for direct filter
		// access -- mirrors the executeInsert publish shape so
		// subscribers can use the same pattern across .created and
		// .updated. Decode the merged JSON we already have rather
		// than re-marshalling the proto payload.
		var payloadMap map[string]any
		if err := json.Unmarshal([]byte(mergedMutation.PayloadRaw), &payloadMap); err == nil {
			maps.Copy(eventPayload, payloadMap)
			eventPayload["payload"] = payloadMap
		}
		e.publishEvent(
			events.BuildTopicWithPartitionAndConcept(events.TopicGraphNodeUpdated, readPartition, conceptMeta.Name),
			events.KindNodeUpdated,
			eventPayload,
		)
	}
	return result, nil
}

func (e *MemQLEngine) executeInsert(ctx context.Context, mutation MutationNode) (*ExecuteResult, error) {
	if e.concepts == nil {
		return nil, fmt.Errorf("concept registry is not initialized")
	}

	conceptName := strings.TrimSpace(mutation.Concept)
	if conceptName == "" {
		return nil, fmt.Errorf("insert() mutation must specify a concept")
	}

	conceptMeta, err := e.concepts.Get(conceptName)
	if err != nil {
		return nil, fmt.Errorf("concept %q not found: %w", conceptName, err)
	}

	rawPayload := strings.TrimSpace(mutation.PayloadRaw)
	if rawPayload == "" {
		return nil, fmt.Errorf("insert() mutation payload is required")
	}

	payload := make(map[string]any)
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		return nil, fmt.Errorf("invalid payload JSON for insert() mutation: %w", err)
	}

	// Reserved-field check at mutation time. The concept-load validator
	// already rejects reserved field names in concept definitions, but
	// the payload on an insert() call bypasses that check. Without this
	// guard, a caller could write insert("v1:foo", payload={partition: "_system"})
	// and silently clobber engine-level storage fields. Fail closed.
	// See docs/core/memql-authoring-rules.md entry #12 (partitionName
	// convention) and component/database/memory-nodes/constants.go for
	// the full reserved list.
	for key := range payload {
		if memorynodes.IsReservedPayloadField(key) {
			return nil, fmt.Errorf(
				"insert() payload for concept %q declares reserved field %q; "+
					"rename the field (for example partition -> partitionName) "+
					"or use the dedicated engine mechanism for that attribute",
				conceptMeta.Name, key)
		}
	}

	// Auto-canonicalize @relationship payload fields. Every concept's
	// outgoing relationships (foreign-key fields like
	// participant.userId -> v1:identity:user, utterance.spaceId ->
	// v1:cognition:space) get rewritten to canonical
	// `<partition>:<targetConcept>:<bareSlug>` form before the payload
	// reaches the validators or the storage layer.
	//
	// Why insert-time, not on read: `payload.userId == arg(...)` and
	// `id == arg(...)` lookups operate on the stored bytes. Two callers
	// inserting the same logical reference under different shapes
	// ("user-abc" vs canonical) would otherwise produce two distinct
	// stored values that don't match each other under `==`. Collapsing
	// to canonical at insert eliminates the class entirely without
	// touching every query site.
	//
	// Used to live only in mutation_templates.go (the named-mutation
	// path). Raw insert("v1:...", payload={...}) -- used by the
	// polyphon HTTP utterance handler and any other system-actor write
	// that builds an insert query string directly -- bypassed that
	// canon and landed bare-slug values that the canonicalize-RHS
	// query path then couldn't match. Moving the call here covers both
	// paths uniformly; the named-mutation pre-call stays as a no-op
	// idempotent belt-and-suspenders.
	if err := e.canonicalizeRelationshipFields(ctx, conceptMeta.Name, payload); err != nil {
		return nil, fmt.Errorf("canonicalize relationship fields: %w", err)
	}

	// Validate structured action utterances (limits + allowlist support).
	if conceptMeta.Name == memorynodes.ConceptCognitionUtterance {
		if err := validateCognitionActionUtterancePayload(payload); err != nil {
			return nil, err
		}
	}

	// Validate the partition slug at the engine layer so no caller can
	// land an illegal name in v1:platform:partition -- not the
	// createPartition mutation, not a raw insert, not an automation.
	// The slug is reused as the row id, the database PK column value,
	// and the {partition} segment in event topics, so anything that
	// breaks DNS-label rules would silently corrupt event routing or
	// the ID parser. See core/id.ValidatePartitionName for the full
	// rule set; the same validator is used by the memql-cockpit form
	// for instant feedback and by the createWorkspace mutation path
	// (defense in depth).
	if conceptMeta.Name == memorynodes.ConceptPlatformPartition {
		if err := validatePlatformPartitionPayload(payload, mutation.ID); err != nil {
			return nil, err
		}
	}

	conceptDefs := e.relationshipDefinitionsForConcept(conceptMeta.Name)

	if mutation.ParentRef != nil {
		parentId := strings.TrimSpace(*mutation.ParentRef)
		if parentId == "" {
			return nil, fmt.Errorf("parent hint must not be empty")
		}

		parentDefs := filterRelationshipDefinitions(conceptDefs, relationshipTypeParent, []string{relationshipDirectionOutgoing, relationshipDirectionBidirectional})
		if len(parentDefs) == 0 {
			return nil, fmt.Errorf("no relationship definition for parent on concept %q", conceptMeta.Name)
		}
		if len(parentDefs) > 1 {
			return nil, fmt.Errorf("multiple parent relationship definitions for concept %q; unable to apply parent hint", conceptMeta.Name)
		}
		if err := setPayloadValue(payload, parentDefs[0].Field, parentId); err != nil {
			return nil, fmt.Errorf("apply parent relationship hint: %w", err)
		}
	}

	if mutation.AliasOfRef != nil {
		aliasId := strings.TrimSpace(*mutation.AliasOfRef)
		if aliasId == "" {
			return nil, fmt.Errorf("aliasOf hint must not be empty")
		}

		aliasDefs := filterRelationshipDefinitions(conceptDefs, relationshipTypeAlias, nil)
		if len(aliasDefs) == 0 {
			aliasDefs = filterRelationshipDefinitions(conceptDefs, relationshipTypeEquals, nil)
		}

		if len(aliasDefs) == 0 {
			return nil, fmt.Errorf("no relationship definition for alias on concept %q", conceptMeta.Name)
		}
		if len(aliasDefs) > 1 {
			return nil, fmt.Errorf("multiple alias relationship definitions for concept %q; unable to apply aliasOf hint", conceptMeta.Name)
		}
		if err := setPayloadValue(payload, aliasDefs[0].Field, aliasId); err != nil {
			return nil, fmt.Errorf("apply alias relationship hint: %w", err)
		}
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}
	if conceptMeta.Name == memorynodes.ConceptCognitionUtterance {
		if err := e.validateCognitionUtteranceWriteAuthorization(ctx, payload, actor); err != nil {
			return nil, err
		}
	}
	// Server-stamp forUserId on every private-utterance write. The two-
	// thread chat model rests on this guard: a non-elevated caller cannot
	// land a private utterance in someone else's thread because the
	// stamper overwrites whatever forUserId they sent with their own
	// authenticated subject. See validateAndStampPrivateUtterancePayload.
	if conceptMeta.Name == memorynodes.ConceptCognitionPrivateUtterance {
		if err := validateAndStampPrivateUtterancePayload(ctx, payload, actor); err != nil {
			return nil, err
		}
	}
	// SI-participant guard: server-stamps forUserId, enforces per-user
	// 3-cap, and protects the pinned owner GA from removal. Skips human
	// participants. See validateAndStampParticipantPayload.
	if conceptMeta.Name == memorynodes.ConceptCognitionParticipant {
		if err := e.validateAndStampParticipantPayload(ctx, payload, mutation.ID, actor); err != nil {
			return nil, err
		}
	}

	// Resolve the partition against the concept's scope. Global-scoped
	// concepts (@scope("global") in the .memql file) always land in the
	// reserved _system partition; everything else follows the request
	// envelope via resolvePartition. Setting it explicitly -- rather
	// than letting Concept.Create fall through to DefaultPartition --
	// means writes now honor envelope.partition the same way reads do,
	// so tenant data actually ends up where the tenant asked for.
	writePartition := e.partitionForConcept(ctx, conceptMeta.Name)
	createParams := memorynodes.CreateParams{
		Actor:     actor,
		Partition: writePartition,
		ID:        strings.TrimSpace(mutation.ID),
		Payload:   payload,
	}

	// Collect contextual metadata from the request context.
	if e.metadataCollector != nil {
		createParams.Metadata = e.metadataCollector.Collect(ctx)
	}

	if mutation.CreatedAt != nil {
		ts := mutation.CreatedAt.UTC()
		createParams.Clock = func() time.Time { return ts }
	}

	store := &bunStore{db: e.database()}
	result, err := conceptMeta.Create(ctx, store, createParams)
	if err != nil {
		return nil, err
	}

	created := memorynodes.MemoryNode{
		ID:        result.ID,
		Concept:   result.Concept,
		Type:      result.Type,
		CreatedAt: result.CreatedAt,
		CreatedBy: result.CreatedBy,
		Schema:    result.Schema,
		Payload:   result.Payload,
	}

	apiNode, err := toAPIMemoryNode(&created)
	if err != nil {
		return nil, err
	}

	e.invalidateCache()

	// Emit node created event
	// Flatten node payload fields into event for easier filter access
	// This allows filters to use "participantType==\"human\"" instead of "payload.participantType==\"human\""
	eventPayload := map[string]any{
		"id":        result.ID,
		"nodeId":    result.ID, // Alias for backward compatibility
		"concept":   conceptMeta.Name,
		"actor":     actor,
		"nodeType":  result.Type,
		"createdAt": result.CreatedAt.Format(time.RFC3339),
	}
	// Unmarshal and flatten node payload fields for direct filter access
	var payloadMap map[string]any
	if len(result.Payload) > 0 {
		if err := json.Unmarshal(result.Payload, &payloadMap); err == nil {
			maps.Copy(eventPayload, payloadMap)
		}
	}
	// Keep full payload for reference (nested access still works)
	eventPayload["payload"] = payloadMap

	// Events fire under the concept's actual storage partition, not the
	// caller's envelope -- so global concepts emit topics like
	// graph.node.created._system.v1:cluster:node, and subscribers using
	// "node.*.*.<concept>" (wildcard partition) still match.
	eventPayload["partition"] = writePartition
	e.publishEvent(
		events.BuildTopicWithPartitionAndConcept(events.TopicGraphNodeCreated, writePartition, conceptMeta.Name),
		events.KindNodeCreated,
		eventPayload,
	)

	bundle := &memqlv1.GraphBundle{
		Nodes:   []*memqlv1.MemoryNode{apiNode},
		RootIds: []string{apiNode.Id},
	}

	return newExecuteResult(bundle), nil
}

func (e *MemQLEngine) fetchNodesByIds(ctx context.Context, ids []string, timestamp *time.Time, allowed map[string]map[string]struct{}, limit int) ([]memorynodes.MemoryNode, error) {
	unique := uniqueStrings(ids)
	if len(unique) == 0 {
		return nil, nil
	}

	filter := compiledExpression{
		sql:  "(id IN (?))",
		args: []any{bun.In(unique)},
	}

	target := len(unique)
	if limit > 0 && limit < target {
		target = limit
	}
	nodes, err := e.executeFilterQuery(ctx, nil, filter, timestamp, target, nil)
	if err != nil {
		return nil, err
	}

	nodesMap := nodesToMap(nodes)
	for _, id := range unique {
		if _, ok := nodesMap[id]; ok {
			continue
		}

		if timestamp != nil {
			continue
		}

		if e.Logger != nil {
			e.Logger.Warn("memql reference missing; skipping node", "id", id, "concept_allowlist", allowed[id])
		}
		continue
	}

	if len(allowed) == 0 {
		sorted, err := mapToSortedSlice(nodesMap, nil)
		if err != nil {
			return nil, err
		}
		return sorted, nil
	}

	filtered := make(map[string]memorynodes.MemoryNode)
	for id, node := range nodesMap {
		expectedConcepts, ok := allowed[id]
		if !ok || len(expectedConcepts) == 0 {
			filtered[id] = node
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		match := false
		for conceptName := range expectedConcepts {
			if strings.EqualFold(conceptMeta.Name, strings.TrimSpace(conceptName)) {
				match = true
				break
			}
		}
		if !match {
			return nil, fmt.Errorf("node %q resolved to concept %q which is not allowed for this relationship", id, conceptMeta.Name)
		}
		filtered[id] = node
	}

	result, err := mapToSortedSlice(filtered, nil)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (e *MemQLEngine) fetchNodesByJSONFieldValues(ctx context.Context, conceptName string, fieldPath []string, values []string, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
	unique := uniqueStrings(values)
	if len(unique) == 0 {
		return nil, nil
	}

	pathExpr, err := buildJSONPathExpression(fieldPath)
	if err != nil {
		return nil, err
	}

	expr := fmt.Sprintf("(concept = ? AND %s IN (?))", pathExpr)
	args := []any{strings.TrimSpace(conceptName), bun.In(unique)}

	target := len(unique)
	if limit > 0 && limit < target {
		target = limit
	}
	nodes, err := e.executeFilterQuery(ctx, nil, compiledExpression{sql: expr, args: args}, timestamp, target, nil)
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes, nil
}

func (e *MemQLEngine) fetchNodesByNodeFieldValues(ctx context.Context, conceptName string, field string, values []string, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
	unique := uniqueStrings(values)
	if len(unique) == 0 {
		return nil, nil
	}

	column, err := mapNodeFieldToColumn(field)
	if err != nil {
		return nil, err
	}

	expr := fmt.Sprintf("(concept = ? AND %s IN (?))", column)
	args := []any{strings.TrimSpace(conceptName), bun.In(unique)}

	target := len(unique)
	if limit > 0 && limit < target {
		target = limit
	}

	nodes, err := e.executeFilterQuery(ctx, nil, compiledExpression{sql: expr, args: args}, timestamp, target, nil)
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes, nil
}

func (e *MemQLEngine) loadLatestNodes(ctx context.Context, ids []string, timestamp *time.Time) (map[string]memorynodes.MemoryNode, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	unique := uniqueStrings(ids)
	if len(unique) == 0 {
		return map[string]memorynodes.MemoryNode{}, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	var latest []memorynodes.MemoryNode
	query := db.NewSelect().
		Model(&latest).
		DistinctOn("id").
		OrderExpr(`id ASC, "createdAt" DESC`).
		Where("id IN (?)", bun.In(unique))

	if timestamp != nil {
		query = query.Where(`"createdAt" <= ?`, timestamp.UTC())
	}

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	result := make(map[string]memorynodes.MemoryNode, len(latest))
	for _, node := range latest {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		result[id] = node
	}

	return result, nil
}


func (e *MemQLEngine) checkNodeExists(ctx context.Context, conceptName, id string) bool {
	if e.db == nil {
		return false
	}

	count, err := e.db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ? AND id = ?", conceptName, id).
		Limit(1).
		Count(ctx)

	return err == nil && count > 0
}
