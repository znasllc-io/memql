package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/ast"
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

// writeMeta carries the read-merge bookkeeping executeWrite gathers for
// its callers -- specifically the data executeUpdate needs to publish the
// graph.node.updated transition event without re-reading the row.
type writeMeta struct {
	// priorExisted is true when a row already existed at the target id,
	// i.e. the write was an update (read-merge) rather than a create.
	priorExisted bool
	// priorStatus is the prior row's `status` field (empty when absent),
	// surfaced as the event's `oldStatus` so transition-only automations
	// can gate on a real state change (#1158).
	priorStatus string
	// finalPayloadJSON is the merged-and-written payload, marshalled for
	// the transition event's flattened-field shape.
	finalPayloadJSON string
	// conceptName is the canonical concept the row was written under.
	conceptName string
}

// executeUpdate runs the update() form: read the latest existing row by
// id, splat the partial payload on top, validate the merged result
// against the concept schema, persist the merged row, and additionally
// publish graph.node.updated so transition subscribers can distinguish a
// state change from a birth.
//
// The read-merge itself lives in executeWrite (the single write
// chokepoint shared with executeInsert -- memql#1709), so update() and a
// partial insert() onto an existing id behave identically: omitted fields
// inherit from the persisted row. update()'s only extra contract is that
// the row MUST already exist (requirePrior=true); a missing row is an
// error pointing the caller at insert().
//
// On success, the new row appended to the time-series carries the merged
// payload; queries by id return it as the latest version.
func (e *MemQLEngine) executeUpdate(ctx context.Context, mutation MutationNode) (*ExecuteResult, error) {
	if e.concepts == nil {
		return nil, fmt.Errorf("concept registry is not initialized")
	}

	if strings.TrimSpace(mutation.Concept) == "" {
		return nil, fmt.Errorf("update() mutation must specify a concept")
	}
	if strings.TrimSpace(mutation.ID) == "" {
		return nil, fmt.Errorf("update() requires an explicit id; use insert() to create new rows")
	}
	if strings.TrimSpace(mutation.PayloadRaw) == "" {
		return nil, fmt.Errorf("update() mutation payload is required")
	}

	result, meta, err := e.executeWrite(ctx, mutation, true)
	if err != nil {
		return result, err
	}

	// memQL is append-only: every successful write materialises as a new
	// row that fires graph.node.created via executeWrite. Most subscribers
	// want one or the other -- automations like emitScopeElevationCanvasCard
	// fire on the FIRST insert (kind + initial status), backend handlers
	// like cognition's plan-execution want only the state-transition deltas
	// (the same concept already exists; a subsequent write means "moved to
	// running / succeeded / failed"). Mixing both onto graph.node.created
	// forced subscribers to gate via a status filter and hope the same
	// status never recurs.
	//
	// Solution: also publish graph.node.updated on the update() path.
	// Subscribers tracking transitions listen on .updated and skip the
	// noise of every initial creation; subscribers tracking births still
	// listen on .created. Both events carry the same payload so callers
	// can pick whichever signal fits their model.
	if result != nil && result.Bundle != nil && len(result.Bundle.Nodes) > 0 {
		updatedNode := result.Bundle.Nodes[0]
		// Partition is used only for the event publish topic; the read
		// itself no longer filters on partition post-#56 phase 3.
		readPartition := e.partitionForConcept(ctx, meta.conceptName)
		eventPayload := map[string]any{
			"id":        updatedNode.Id,
			"nodeId":    updatedNode.Id,
			"concept":   meta.conceptName,
			"nodeType":  updatedNode.Type,
			"partition": readPartition,
		}
		// Flatten payload fields onto the event for direct filter access --
		// mirrors the executeWrite publish shape so subscribers can use the
		// same pattern across .created and .updated.
		var payloadMap map[string]any
		if err := json.Unmarshal([]byte(meta.finalPayloadJSON), &payloadMap); err == nil {
			maps.Copy(eventPayload, payloadMap)
			eventPayload["payload"] = payloadMap
		}
		// #1158: surface the prior status as `oldStatus` (top-level, after the
		// flatten so the new payload can't clobber it). Only when present, to
		// keep the event additive for concepts without a status field. Lets
		// .updated subscribers detect a real state transition vs a no-op rewrite.
		if meta.priorStatus != "" {
			eventPayload["oldStatus"] = meta.priorStatus
		}
		e.publishEvent(
			events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, meta.conceptName),
			events.KindNodeUpdated,
			eventPayload,
		)
	}
	return result, nil
}

// loadPriorPayload reads the latest stored version of (concept, id) and
// returns its decoded payload. exists is false (with a nil map, nil error)
// when no row is present -- the create case. Partition is not a filter
// dimension post-#56 phase 3, so a bare id lookup is correct.
func (e *MemQLEngine) loadPriorPayload(ctx context.Context, conceptMeta *memorynodes.Concept, id string) (payload map[string]any, exists bool, err error) {
	store := &bunStore{db: e.database()}
	priorNodes, err := conceptMeta.Query(ctx, store, memorynodes.QueryParams{
		IDs:   []string{id},
		Limit: 1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("read latest row for concept %q id %q: %w", conceptMeta.Name, id, err)
	}
	if len(priorNodes) == 0 {
		return nil, false, nil
	}
	priorJSON, err := priorNodes[0].Payload.MarshalJSON()
	if err != nil {
		return nil, false, fmt.Errorf("marshal prior payload for concept %q id %q: %w", conceptMeta.Name, id, err)
	}
	decoded := make(map[string]any)
	if err := json.Unmarshal(priorJSON, &decoded); err != nil {
		return nil, false, fmt.Errorf("decode prior payload for concept %q id %q: %w", conceptMeta.Name, id, err)
	}
	return decoded, true, nil
}

// mergePayloadFields splats the partial update payload onto the prior
// payload (top-level replace -- the default update() contract), except
// for the fields named in mergeFields: when BOTH the prior and partial
// values for such a field are JSON objects, the partial object's keys
// merge into the prior object (recursively) instead of replacing it
// wholesale, so sibling keys survive a single-key write.
//
// mergeFields is opt-in per named mutation via @mergeFields("a", "b")
// (see mutationMergeFields in mutation_templates.go). The default
// remains top-level replacement -- the contract every existing
// mutation was written against (memql#350 documents mutations that
// deliberately restate every nested field under it). The opt-in exists
// because a mutation like mutationToggleComputerUseEnabled writes a
// single key into User.preferences and would otherwise wipe every
// sibling preference (theme, timezone, archiveRetentionDays, ...) on
// each kill-switch flip (memql#1339).
//
// When a merge-listed field's prior or partial value is not an object
// (absent, null, scalar, array), the field falls back to plain
// replacement -- mirroring the default and avoiding type surprises.
func mergePayloadFields(prior, partial map[string]any, mergeFields []string) {
	mergeSet := make(map[string]struct{}, len(mergeFields))
	for _, f := range mergeFields {
		mergeSet[strings.TrimSpace(f)] = struct{}{}
	}
	for k, v := range partial {
		if _, merge := mergeSet[k]; merge {
			priorObj, priorIsObj := prior[k].(map[string]any)
			partialObj, partialIsObj := v.(map[string]any)
			if priorIsObj && partialIsObj {
				prior[k] = deepMergeObjects(priorObj, partialObj)
				continue
			}
		}
		prior[k] = v
	}
}

// deepMergeObjects returns a new object carrying every key of prior
// overlaid with every key of partial. On key collision the partial
// value wins, except when both sides are objects, which merge
// recursively. Inputs are not mutated.
func deepMergeObjects(prior, partial map[string]any) map[string]any {
	out := make(map[string]any)
	maps.Copy(out, prior)
	for k, v := range partial {
		if priorObj, ok := out[k].(map[string]any); ok {
			if partialObj, ok := v.(map[string]any); ok {
				out[k] = deepMergeObjects(priorObj, partialObj)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// executeInsert is the create-or-upsert entry point: it delegates to the
// shared executeWrite chokepoint with requirePrior=false, so a write whose
// id has no prior row creates it, while a write onto an existing id
// read-merges (memql#1709). The writeMeta is discarded -- only executeUpdate
// needs it for the transition event.
func (e *MemQLEngine) executeInsert(ctx context.Context, mutation MutationNode) (*ExecuteResult, error) {
	result, _, err := e.executeWrite(ctx, mutation, false)
	return result, err
}

// executeWrite is the single mutation write path for BOTH insert() and
// update(). It performs the engine-level read-merge that makes partial
// payloads safe across the entire update/delete/revoke/transition class
// (memql#1709): when the target id already names a stored row, the supplied
// payload is treated as a DELTA and shallow-merged on top of the persisted
// payload before validation, so omitted fields are preserved rather than
// wiped or rejected as "missing required property". When no prior row
// exists the supplied payload is the full create payload, validated as-is.
//
// This hoists what used to be a per-mutation concern (each update/revoke/
// delete mutation had to either be authored as update{} or re-state every
// required field) into one place, so a mutation authored as insert{} that
// is semantically an update -- e.g. mutationLeaveSpace, mutationRevokeDelegation
// -- preserves siblings automatically, without a DSL change.
//
// requirePrior gates the update() contract: when true (the update() path)
// a missing prior row is an error pointing the caller at insert(); when
// false (the insert() path) a missing prior row is a normal create.
func (e *MemQLEngine) executeWrite(ctx context.Context, mutation MutationNode, requirePrior bool) (*ExecuteResult, writeMeta, error) {
	var meta writeMeta
	if e.concepts == nil {
		return nil, meta, fmt.Errorf("concept registry is not initialized")
	}

	conceptName := strings.TrimSpace(mutation.Concept)
	if conceptName == "" {
		return nil, meta, fmt.Errorf("mutation must specify a concept")
	}

	conceptMeta, err := e.concepts.Get(conceptName)
	if err != nil {
		return nil, meta, fmt.Errorf("concept %q not found: %w", conceptName, err)
	}
	meta.conceptName = conceptMeta.Name

	rawPayload := strings.TrimSpace(mutation.PayloadRaw)
	if rawPayload == "" {
		return nil, meta, fmt.Errorf("mutation payload is required")
	}

	payload := make(map[string]any)
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		return nil, meta, fmt.Errorf("invalid payload JSON for mutation: %w", err)
	}

	// Reserved-field check at mutation time. The concept-load validator
	// already rejects reserved field names in concept definitions, but
	// the payload on a mutation call bypasses that check. Without this
	// guard, a caller could write insert("v1:foo", payload={partition: "_system"})
	// and silently clobber engine-level storage fields. Fail closed.
	// Runs on the supplied delta (a persisted prior row can't carry a
	// reserved field, so merging can't introduce one). See
	// docs/public/language/authoring-rules.md entry #12 (partitionName
	// convention) and component/database/memory-nodes/constants.go for
	// the full reserved list.
	for key := range payload {
		if memorynodes.IsReservedPayloadField(key) {
			return nil, meta, fmt.Errorf(
				"mutation payload for concept %q declares reserved field %q; "+
					"rename the field (for example partition -> partitionName) "+
					"or use the dedicated engine mechanism for that attribute",
				conceptMeta.Name, key)
		}
	}

	// ENGINE-LEVEL READ-MERGE (memql#1709). When the target id already
	// names a stored row, load it and shallow-merge the supplied delta on
	// top so omitted fields inherit from the persisted payload. This is the
	// uniform default for every "write to an existing row" -- update,
	// delete (status flip), revoke (active=false), transition -- regardless
	// of whether the mutation was authored as update{} or insert{}. A
	// genuine create (no prior row, or an auto-generated id) skips the read
	// and validates the supplied payload in full.
	//
	// mergeFields opts named object fields into a recursive deep-merge
	// (memql#1339); the default is top-level replacement, so a restated
	// nested object still replaces wholesale and only OMITTED top-level
	// fields are inherited.
	id := strings.TrimSpace(mutation.ID)
	if id != "" {
		priorPayload, existed, err := e.loadPriorPayload(ctx, conceptMeta, id)
		if err != nil {
			return nil, meta, err
		}
		meta.priorExisted = existed
		if existed {
			// Capture the PRIOR status before the delta overwrites it
			// (#1158) so executeUpdate can surface it as oldStatus.
			meta.priorStatus, _ = priorPayload["status"].(string)
			mergePayloadFields(priorPayload, payload, mutation.MergeFields)
			payload = priorPayload
		}
	}
	if requirePrior && !meta.priorExisted {
		if id == "" {
			return nil, meta, fmt.Errorf("update() requires an explicit id; use insert() to create new rows")
		}
		return nil, meta, fmt.Errorf("update(): no existing row for concept %q id %q (use insert() to create)", conceptName, id)
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
		return nil, meta, fmt.Errorf("canonicalize relationship fields: %w", err)
	}

	// Validate structured action utterances (limits + allowlist support).
	if conceptMeta.Name == memorynodes.ConceptCognitionUtterance {
		if err := validateCognitionActionUtterancePayload(payload); err != nil {
			return nil, meta, err
		}
	}

	conceptDefs := e.relationshipDefinitionsForConcept(conceptMeta.Name)

	if mutation.ParentRef != nil {
		parentId := strings.TrimSpace(*mutation.ParentRef)
		if parentId == "" {
			return nil, meta, fmt.Errorf("parent hint must not be empty")
		}

		parentDefs := filterRelationshipDefinitions(conceptDefs, relationshipTypeParent, []string{relationshipDirectionOutgoing, relationshipDirectionBidirectional})
		if len(parentDefs) == 0 {
			return nil, meta, fmt.Errorf("no relationship definition for parent on concept %q", conceptMeta.Name)
		}
		if len(parentDefs) > 1 {
			return nil, meta, fmt.Errorf("multiple parent relationship definitions for concept %q; unable to apply parent hint", conceptMeta.Name)
		}
		if err := setPayloadValue(payload, parentDefs[0].Field, parentId); err != nil {
			return nil, meta, fmt.Errorf("apply parent relationship hint: %w", err)
		}
	}

	if mutation.AliasOfRef != nil {
		aliasId := strings.TrimSpace(*mutation.AliasOfRef)
		if aliasId == "" {
			return nil, meta, fmt.Errorf("aliasOf hint must not be empty")
		}

		aliasDefs := filterRelationshipDefinitions(conceptDefs, relationshipTypeAlias, nil)
		if len(aliasDefs) == 0 {
			aliasDefs = filterRelationshipDefinitions(conceptDefs, relationshipTypeEquals, nil)
		}

		if len(aliasDefs) == 0 {
			return nil, meta, fmt.Errorf("no relationship definition for alias on concept %q", conceptMeta.Name)
		}
		if len(aliasDefs) > 1 {
			return nil, meta, fmt.Errorf("multiple alias relationship definitions for concept %q; unable to apply aliasOf hint", conceptMeta.Name)
		}
		if err := setPayloadValue(payload, aliasDefs[0].Field, aliasId); err != nil {
			return nil, meta, fmt.Errorf("apply alias relationship hint: %w", err)
		}
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, meta, err
	}
	if conceptMeta.Name == memorynodes.ConceptCognitionUtterance {
		if err := e.validateCognitionUtteranceWriteAuthorization(ctx, payload, actor); err != nil {
			return nil, meta, err
		}
	}
	// SI-participant guard: server-stamps forUserId, enforces per-user
	// 3-cap, and protects the pinned owner GA from removal. Skips human
	// participants. See validateAndStampParticipantPayload.
	if conceptMeta.Name == memorynodes.ConceptCognitionParticipant {
		if err := e.validateAndStampParticipantPayload(ctx, payload, mutation.ID, actor); err != nil {
			return nil, meta, err
		}
	}
	// Agent-role lock guard: rejects writes that would remove any id
	// in the agent's role.lockedDomainIds / lockedToolSlugs from the
	// proposed capabilities. The role catalog is the source of truth
	// for "what an agent of role X must always have"; this guard
	// makes that contract load-bearing on every write path (cockpit
	// edit, CoPresent edit, GA-driven extend, automation). See
	// agent_lock_validation.go.
	//
	// Agent-kind actor-scope guard: rejects user-actor writes that
	// try to stamp kind in ("system", "specialist"). Only the
	// SeedMaterializer and the planner integration -- both running
	// under system:* actors -- may write those kinds. See
	// agent_kind_actor_validation.go and znasllc-io/memql#403.
	if conceptMeta.Name == conceptAgentsAgent {
		if err := e.validateAgentLockedItems(ctx, payload, mutation.ID, actor); err != nil {
			return nil, meta, err
		}
		if err := e.validateAgentKindActorScope(ctx, payload, actor); err != nil {
			return nil, meta, err
		}
	}
	// Harness step guard: enforces the v1:harness:step status state
	// machine (pending -> ready -> running -> done/failed/blocked,
	// blocked -> ready, failed -> ready retry). The append-only DSL
	// cannot reject an invalid transition (e.g. done -> running) on its
	// own, so the rule lives in Go. Runs on inserts and on update()
	// (which read-merges then routes here). See harness_step_validation.go.
	if conceptMeta.Name == memorynodes.ConceptHarnessStep {
		if err := e.validateHarnessStepTransition(ctx, payload, mutation.ID); err != nil {
			return nil, meta, err
		}
	}
	// Generic feedback-intake guard (epic memql#1404 / memql#1405): the
	// resume produced by mutationAttachPlanFeedback (a feedbackResponse with
	// a respondedBy + status=running) is only legal when the Plan is
	// currently awaitingFeedback AND the actor owns it (or is privileged).
	// The append-only DSL cannot reject a conditional transition on its own,
	// so the rule lives here. A no-op for every non-intake plan write. See
	// planner_feedback_validation.go.
	if conceptMeta.Name == conceptPlannerPlan {
		if err := e.validateFeedbackIntakeTransition(ctx, payload, mutation.ID, actor); err != nil {
			return nil, meta, err
		}
	}

	createParams := memorynodes.CreateParams{
		Actor:   actor,
		ID:      strings.TrimSpace(mutation.ID),
		Payload: payload,
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
		return nil, meta, err
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
		return nil, meta, err
	}

	e.invalidateCache()

	// Observation embedding write-path (#585): when a
	// v1:harness:observation lands, embed its `content` into
	// node_vectors keyed by the observation id (vector_field='content',
	// the documentChunk pattern) so the observation becomes recallable
	// by recall()'s hybrid recency x relevance query. Best-effort: a
	// failed embed must not fail the insert -- the row is durable
	// regardless and a later re-embed can backfill. Dispatched through
	// the already-registered integration.embedding.store capability so
	// there is no second write-path to keep in sync.
	if conceptMeta.Name == memorynodes.ConceptHarnessObservation {
		e.embedHarnessObservation(ctx, result.ID, result.Payload)
	}

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

	e.publishEvent(
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptMeta.Name),
		events.KindNodeCreated,
		eventPayload,
	)

	bundle := &memqlv1.GraphBundle{
		Nodes:   []*memqlv1.MemoryNode{apiNode},
		RootIds: []string{apiNode.Id},
	}

	// Capture the final written payload for executeUpdate's transition
	// event (avoids a re-read). result.Payload is the validated, stored
	// payload after merge + server-side stamps.
	if len(result.Payload) > 0 {
		meta.finalPayloadJSON = string(result.Payload)
	}

	return newExecuteResult(bundle), meta, nil
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

// embedHarnessObservation embeds a freshly-inserted
// v1:harness:observation's `content` field into node_vectors so the
// observation is recallable by recall() (#585). It dispatches the
// already-registered integration.embedding.store capability (the same
// write-path knowledge/documentChunk use) rather than reaching into
// pgvector here, so there is one embedding write-path, not two.
//
// Best-effort by contract: any failure (no embedding integration on
// this node-type binary, empty content, embed error) is logged and
// swallowed. The observation row is already durable; recall simply
// won't surface this row until a vector exists, and a re-embed can
// backfill.
func (e *MemQLEngine) embedHarnessObservation(ctx context.Context, id string, payload []byte) {
	if e.integrations == nil || strings.TrimSpace(id) == "" || len(payload) == 0 {
		return
	}
	handler, ok := e.integrations.Get("integration.embedding.store")
	if !ok {
		// No embedding integration on this binary (e.g. a node-type
		// build without it). Nothing to do.
		return
	}
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	content, _ := p["content"].(string)
	if strings.TrimSpace(content) == "" {
		return
	}
	args := map[string]any{
		"nodeId":      id,
		"text":        content,
		"concept":     memorynodes.ConceptHarnessObservation,
		"vectorField": "content",
	}
	if _, err := handler(ctx, args, 1); err != nil {
		if e.Logger != nil {
			e.Logger.Warn("harness observation embed failed (recall will skip this row until backfilled)",
				"id", id, "error", err)
		}
		return
	}
	if e.Logger != nil {
		e.Logger.Debug("harness observation embedded for recall", "id", id, "content_len", len(content))
	}
}
