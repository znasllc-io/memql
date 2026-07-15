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
	// priorPredefined is the prior row's `predefined` flag (false when
	// absent), captured before the delta overwrites it so the RBAC
	// base-role immutability guard can reject a write that targets an
	// already-predefined role even if the caller tries to flip the flag
	// to false in the same delta (memql#2070).
	priorPredefined bool
	// priorBaseTier is the prior row's healing `tier`==base flag (false
	// when absent), captured before the delta overwrites it so the
	// self-healing base-tier immutability guard can reject a write that
	// targets an already-base-tier override even if the caller tries to
	// flip tier to overlay in the same delta (memql#2140).
	priorBaseTier bool
	// priorHealingValid is the prior row's healing `valid` flag (false when
	// absent), captured before the delta overwrites it so the self-healing
	// blast-radius validation guard only gates the valid false->true ACCEPT
	// transition, not a re-write of an already-accepted override (memql#2143).
	priorHealingValid bool
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
		// #2441: the retired `partition` dimension no longer rides the event
		// payload. It was dead data (storage stopped scoping on partition in
		// #56 phase 3, and the publish topic is concept-keyed, not
		// partition-keyed) that only ever surfaced on the client wire.
		eventPayload := map[string]any{
			"id":       updatedNode.Id,
			"nodeId":   updatedNode.Id,
			"concept":  meta.conceptName,
			"nodeType": updatedNode.Type,
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
		// G4 (memql#2366): stamp the acting identity on the envelope (and
		// mirror it into the payload for parity with the .created shape).
		updateActor, _ := mutationActor(ctx)
		if updateActor != "" {
			eventPayload["actor"] = updateActor
		}
		e.publishEventWithActor(
			events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, meta.conceptName),
			events.KindNodeUpdated,
			eventPayload,
			updateActor,
		)
		// 5.6 (memql#1970): the update() path's underlying executeWrite
		// already emitted a cache.invalidate for the row's concept (every
		// write materialises a new row and fires graph.node.created), so a
		// second emit here would be redundant -- the eviction already
		// fired. We keep update() free of an extra invalidate; the create
		// path is the single emit point per write.
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
// because a mutation like toggleComputerUseEnabled writes a
// single key into User.preferences and would otherwise wipe every
// sibling preference (theme, timezone, archiveRetentionDays, ...) on
// each kill-switch flip (memql#1339).
//
// When a merge-listed field's prior or partial value is not an object
// (absent, null, scalar, array), the field falls back to plain
// replacement -- mirroring the default and avoiding type surprises.
// appendPayloadFields rewrites the partial update payload so that, for each
// field named in appendFields, the partial array's elements are APPENDED to
// the prior (stored) array rather than replacing it. It runs BEFORE
// mergePayloadFields, mutating partial[field] in place to the combined array;
// the subsequent top-level-replace merge then writes that combined array.
//
// appendFields is opt-in per named mutation via @appendFields("a", "b") (see
// mutationAppendFields in mutation_templates.go). The default update() contract
// remains top-level replacement, so an array field NOT listed still replaces
// wholesale. This is the engine-level append primitive that lets a single
// pure mutation accumulate list elements (attach one id to
// request.attachmentIds) without a read-merge-append logic wrapper -- the
// logic-purity-clean form (memql#2240). Not deduped: appending an
// already-present element yields a duplicate, matching the prior
// read-merge-append behavior.
//
// A non-array prior value (absent / null / scalar) is treated as an empty
// array, so the FIRST append on a row with no array yet starts fresh. A
// non-array partial value is wrapped as a single element.
func appendPayloadFields(prior, partial map[string]any, appendFields []string) {
	for _, f := range appendFields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		newVal, present := partial[f]
		if !present {
			// Field not written in this call -- nothing to append; the
			// stored array survives untouched via the omitted-field inherit.
			continue
		}
		combined := append([]any{}, toAppendSlice(prior[f])...)
		combined = append(combined, toAppendSlice(newVal)...)
		partial[f] = combined
	}
}

// toAppendSlice normalises an array-ish payload value to []any for appending.
// nil / absent -> empty; []any and []string pass through; any other scalar is
// wrapped as a single element (so @appendFields tolerates a scalar partial).
func toAppendSlice(v any) []any {
	switch arr := v.(type) {
	case nil:
		return nil
	case []any:
		return arr
	case []string:
		out := make([]any, len(arr))
		for i, s := range arr {
			out[i] = s
		}
		return out
	default:
		return []any{v}
	}
}

// dropCreateOnlyFields removes each @createOnly-named field from the partial
// (delta) payload. It runs only on the read-merge path (an existing prior
// row), BEFORE appendPayloadFields/mergePayloadFields, so the stored value
// for those fields survives the merge untouched -- the create-only contract
// (fylo#63). On a genuine create there is no prior row and this is never
// called, so the delta's create-only fields (e.g. status="pending",
// attempts=0) are written as-is. A nil/empty createOnlyFields (every
// unannotated mutation) is a no-op.
func dropCreateOnlyFields(partial map[string]any, createOnlyFields []string) {
	for _, f := range createOnlyFields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		delete(partial, f)
	}
}

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

// scrubPIIFields overwrites every named field in the payload with its
// type's zero value, leaving non-PII fields untouched. Used by the
// @scrubPii hard-delete path (memql#1711): the field set comes from the
// bound concept's @pii annotations (Concept.PIIFields()), so the scrub
// is exhaustive by construction -- a newly-annotated PII field is
// cleared automatically with no change to this code or the mutation.
//
// The zero value matches the field's current JSON type so the scrubbed
// row still satisfies the concept schema (an empty string for a
// string-typed @required field stays present and valid; a numeric field
// goes to 0; a bool to false). A field absent from the payload (never
// populated) needs no scrub and is skipped -- it is already clear.
// Nested-object PII fields are cleared to an empty object; the common
// case (name, email, phone, free-text role) is a top-level string.
func scrubPIIFields(payload map[string]any, piiFields []string) {
	for _, field := range piiFields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		existing, present := payload[field]
		if !present {
			// Field was never set on this row; nothing to clear.
			continue
		}
		payload[field] = zeroValueFor(existing)
	}
}

// zeroValueFor returns the type-appropriate zero value for a payload
// field's current value, so scrubbing preserves the JSON type and keeps
// the merged row schema-valid.
func zeroValueFor(v any) any {
	switch v.(type) {
	case string:
		return ""
	case bool:
		return false
	case float64, int, int64:
		return 0
	case []any:
		return []any{}
	case map[string]any:
		return map[string]any{}
	case nil:
		return ""
	default:
		// Unknown shape: blank it to a string rather than leave PII in
		// place. Any string-typed schema field accepts this; the
		// fail-safe is to scrub, never to retain.
		return ""
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
// is semantically an update -- e.g. leaveSpace, revokeDelegation
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
			// Capture the PRIOR predefined flag before the delta
			// overwrites it (memql#2070) so the RBAC base-role
			// immutability guard can reject an edit that targets an
			// already-predefined role even if the delta flips the flag.
			meta.priorPredefined = boolFromAny(priorPayload["predefined"])
			// Capture the PRIOR healing tier before the delta overwrites
			// it (memql#2140) so the self-healing base-tier immutability
			// guard can reject an edit that targets an already-base-tier
			// override even if the delta flips tier to overlay.
			meta.priorBaseTier = isBaseTier(priorPayload["tier"])
			// Capture the PRIOR valid flag (memql#2143) so the self-healing
			// blast-radius validation guard only gates the valid false->true
			// ACCEPT transition, not a re-write of an already-accepted override.
			meta.priorHealingValid = boolFromAny(priorPayload["valid"])
			// @createOnly fields are written on create only (fylo#63): drop
			// them from the delta BEFORE the merge so the stored value wins.
			// A deterministic-id re-stage of a row another writer owns after
			// creation (e.g. stageOutboundRequest onto a row the outbound
			// worker moved to sent) then preserves that lifecycle state
			// instead of resetting it. Empty for every unannotated mutation,
			// so this is a no-op on the whole existing tree.
			dropCreateOnlyFields(payload, mutation.CreateOnlyFields)
			appendPayloadFields(priorPayload, payload, mutation.AppendFields)
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

	// Annotation-driven PII scrub (memql#1711). A mutation tagged
	// @scrubPii (the hard-delete / data-deletion path) clears EVERY field
	// the bound concept declares with @pii, derived from the schema at
	// execution time rather than a hand-maintained field list. Runs AFTER
	// the read-merge so the scrub is authoritative: even if the persisted
	// row (or the supplied delta) carried a PII value, it is zeroed here.
	// Adding a new @pii field to the concept makes it scrubbed
	// automatically -- the list cannot drift out of sync.
	if mutation.ScrubPii {
		scrubPIIFields(payload, conceptMeta.PIIFields())
	}

	// Auto-canonicalize @relationship payload fields. Every concept's
	// outgoing relationships (foreign-key fields like
	// participant.userId -> v1:identity:user, utterance.partitionId ->
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
	// AI-participant guard: server-stamps forUserId, enforces per-user
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
	// edit, frontend edit, GA-driven extend, automation). See
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
	// RBAC base-role immutability guard (memql#2070): a predefined
	// v1:rbac:role is authored in the DSL (dsl/rbac/seeds.memql) and may
	// only be (re)materialized by a system actor. Any non-system-actor
	// write to a predefined role -- redefine, re-rank, rename, deactivate
	// -- is rejected, blocking escalation-by-redefinition. The prior-row
	// predefined flag (captured in the read-merge above) gates the case
	// where the delta tries to flip predefined to false. See
	// rbac_role_immutable_validation.go.
	if conceptMeta.Name == conceptRbacRole {
		if err := e.validateRbacBaseRoleImmutable(ctx, payload, meta.priorPredefined, actor); err != nil {
			return nil, meta, err
		}
		// RBAC custom-role rank-bound guard (memql#2072): a non-system actor
		// creating/editing a DATA-defined custom role (predefined=false) may
		// only set a rank STRICTLY BELOW their own -- a creator can't mint a
		// peer/superior role (escalation). Lets custom roles slot BETWEEN
		// existing ranks while bounding them by the creator's rank. See
		// rbac_custom_role_rankbound.go.
		if err := e.validateRbacCustomRoleRankBound(ctx, payload, meta.priorPredefined, actor); err != nil {
			return nil, meta, err
		}
	}
	// Identity credential-row actor-scope guard (memql#2513): a
	// v1:identity:identity row of a machine-credential identityType
	// (badge / worker_token / node_token / voice_agent_token /
	// service_account) may only be written by a system actor -- its
	// dedicated mint handler / CLI / bootstrap path. Blocks a user
	// actor from forging a credential row over the raw ExecuteQuery
	// mutation surface (per-row authz can't see the args.userId-bound
	// owner). See identity_credential_actor_validation.go.
	if conceptMeta.Name == conceptIdentityIdentity {
		if err := e.validateIdentityCredentialActorScope(ctx, payload, actor); err != nil {
			return nil, meta, err
		}
	}
	// Self-healing base-tier immutability guard (memql#2140): a tier=base
	// v1:healing:healedOverride is the authored/embedded deploy spine and may
	// only be materialized by a system actor (the SeedMaterializer). Any
	// non-system-actor write resolving to tier=base is rejected, so a healed
	// override can never be forged as base to escape the override-is-data
	// contract. The prior-row base-tier flag (captured in the read-merge
	// above) gates the case where the delta tries to flip tier to overlay.
	// See healing_base_immutable_validation.go.
	if conceptMeta.Name == conceptHealingOverride {
		if err := e.validateHealingBaseImmutable(ctx, payload, meta.priorBaseTier, actor); err != nil {
			return nil, meta, err
		}
		// Self-healing blast-radius validation guard (memql#2143): the write
		// that ACCEPTS a heal (valid false->true) is gated on the actor's role
		// rank meeting the override's blastRadius-required rank (personal->user,
		// shared->admin, spine_adjacent->developer; owner always allowed). A
		// wider-reaching heal needs a higher-ranked validator. Only the
		// false->true transition is gated (prior-valid flag captured above). See
		// healing_validation_rankbound.go.
		if err := e.validateHealingValidationRankBound(ctx, payload, meta.priorHealingValid, actor); err != nil {
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
	// resume produced by attachPlanFeedback (a feedbackResponse with
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
	// Forge request guard (issue #1787): enforces the v1:forge:request
	// approval-pipeline state machine and role authority. The append-only
	// DSL cannot gate a transition on actor.role, so the rule lives here.
	// See forge_request_validation.go.
	if conceptMeta.Name == conceptForgeRequest {
		if err := e.validateForgeRequestTransition(ctx, payload, mutation.ID, actor); err != nil {
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

	// Embed a freshly-minted action-library action's `intent` into
	// node_vectors (vectorField='intent') so the planner can cosine-search
	// the library (#1758). Same best-effort contract as observations -- the
	// action row is durable regardless of embed success.
	if conceptMeta.Name == memorynodes.ConceptActionsAction {
		e.embedActionIntent(ctx, result.ID, result.Payload)
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

	e.publishEventWithActor(
		events.BuildTopicWithConcept(events.TopicGraphNodeCreated, conceptMeta.Name),
		events.KindNodeCreated,
		eventPayload,
		actor,
	)
	// 5.6 (memql#1970): also emit the dedicated cache-invalidation event
	// on its own broadcast channel. ONLY the result-cache evictor
	// subscribes, and a single cache.invalidate.* routing rule forwards
	// it to every node -- so cross-node eviction needs no per-concept
	// graph forwarding (which would risk double-firing automations on
	// sibling replicas). Same commit point as the graph-write publish.
	e.publishCacheInvalidate(conceptMeta.Name)

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

// embedActionIntent embeds a freshly-inserted v1:actions:action's `intent`
// into node_vectors (vectorField='intent') so the action library is
// cosine-searchable by the planner (#1758). Mirrors embedHarnessObservation:
// it dispatches the already-registered integration.embedding.store capability
// (one embedding write-path), and is best-effort -- any failure (no embedding
// integration on this binary, empty intent, embed error) is logged and
// swallowed; the action row is already durable and a re-embed can backfill.
func (e *MemQLEngine) embedActionIntent(ctx context.Context, id string, payload []byte) {
	if e.integrations == nil || strings.TrimSpace(id) == "" || len(payload) == 0 {
		return
	}
	handler, ok := e.integrations.Get("integration.embedding.store")
	if !ok {
		return
	}
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	intent, _ := p["intent"].(string)
	if strings.TrimSpace(intent) == "" {
		return
	}
	args := map[string]any{
		"nodeId":      id,
		"text":        intent,
		"concept":     memorynodes.ConceptActionsAction,
		"vectorField": "intent",
	}
	if _, err := handler(ctx, args, 1); err != nil {
		if e.Logger != nil {
			e.Logger.Warn("action intent embed failed (planner search will skip this action until backfilled)",
				"id", id, "error", err)
		}
		return
	}
	if e.Logger != nil {
		e.Logger.Debug("action intent embedded for planner search", "id", id, "intent_len", len(intent))
	}
}
