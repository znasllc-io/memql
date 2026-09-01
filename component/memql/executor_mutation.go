package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
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
	//
	// SECOND CONSUMER since epic memql#4794: the D10 lifecycle guards read it
	// too. They judge a TRANSITION rather than a destination -- "archived from
	// what" is the whole rule -- and a guard reading only the merged payload
	// could not see the prior value at all. It also carries
	// v1:platform:packageDeployment's append-only property: terminal means the
	// PRIOR status was terminal, so a delta that also rewrites status cannot
	// launder that away.
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
	// priorUserRole is the prior row's v1:identity:user `role` (empty when
	// absent), captured before the delta overwrites it so the last-owner
	// deletion guard judges a PARTIAL update correctly (memql#3967).
	// `deleteUserHard` writes `active: false` and nothing else, so without
	// this the guard would see no role at all and pass every deletion.
	priorUserRole string
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
	// priorSystemOwned is the prior row's `systemOwned` flag (false when
	// absent), captured before the delta overwrites it so the site
	// systemOwned-delete guard cannot be bypassed by a write that flips
	// systemOwned to false in the same delta as deleted:true (memql#3717).
	priorSystemOwned bool
	// priorHostname is the prior row's v1:platform:site `hostname` (empty
	// when absent), captured so the site hostname POLICY only judges a
	// hostname this write is actually choosing (memql#4344).
	//
	// Without it the rule fires on the merged payload of every write, so an
	// ordinary user who owns a site at a cluster-owner-created custom
	// hostname could never publish to it, disable it or delete it -- each of
	// those inherits the stored hostname through the read-merge, and the
	// user policy would refuse a value the user never supplied.
	priorHostname string
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

	// MemQL is append-only: every successful write materialises as a new
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
	//
	// STAGED DATA (memql#3985): withheld for a staged concept, on the same
	// argument as the graph.node.created emit in executeWrite, which this build
	// deliberately mirrors field for field -- so it leaks the same complete row,
	// and additionally the PRIOR row's status as `oldStatus`. Suppressing only
	// .created would hide a staged row's birth and then announce every
	// subsequent write to it in full.
	if result != nil && result.Bundle != nil && len(result.Bundle.Nodes) > 0 &&
		!e.withholdGraphWriteEvent(meta.conceptName) {
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
//
// THIS READ IS NOT STAGED-FILTERED, AND MUST NEVER BE (memql#3985). It looks
// exactly like a read the staged-DATA tier should hide, which is why it is
// called out here rather than left to inference -- a later pass aligning "every
// read consults conceptDataIsStaged" would find it, gate it, and pass its own
// tests.
//
// What that would cost: a staged prior row returns len(priorNodes)==0 four lines
// below, so exists comes back false, so executeWrite's read-merge has nothing to
// merge onto and the UPDATE degrades into a CREATE. Every field the caller did
// not restate -- which for a partial update is nearly all of them -- is written
// away. That is SILENT DATA LOSS on a write the user believes is a partial
// update: no error, no warning, a successful mutation, and a row that has
// forgotten most of itself. It would land on every concept an author had staged,
// which is to say on data whose entire purpose is to be sitting there
// accumulating until training makes it visible.
//
// The distinction the tier turns on: staging withholds rows from CALLERS asking
// what exists. This is the ENGINE asking what it is about to overwrite. Hiding a
// row from the mechanism that preserves it does not hide the row, it destroys
// it. TestStagedWrite_PartialUpdatePreservesOmittedFields (staged_write_test.go)
// fails loudly if this ever grows a gate.
func (e *MemQLEngine) loadPriorPayload(ctx context.Context, conceptMeta *memorynodes.Concept, id string) (payload map[string]any, exists bool, err error) {
	store := newBunStore(e.database())
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

// dropNoUnsetFields removes each @noUnset-named field from the partial (delta)
// payload when the incoming value is EMPTY and the stored value is not, so the
// stored value survives the merge. Runs only on the read-merge path (an
// existing prior row), BEFORE appendPayloadFields/mergePayloadFields.
//
// Why this is not covered by read-merge itself (memql#3415). Read-merge
// inherits a stored value only for fields ABSENT from the delta. A mutation
// body written as `bootstrappedAt: args.bootstrappedAt ?? ""` materialises an
// omitted arg into an explicit empty string -- present in the delta, and so the
// winner of the merge. A caller who never mentioned the field thereby blanked
// it, un-bootstrapping a live cluster and re-opening the unauthenticated
// ownership wizard. @noUnset makes set -> unset an inexpressible write.
//
// Only the set -> unset direction is refused. Setting a field that is currently
// empty, and replacing one non-empty value with another, both still apply --
// otherwise the legitimate later stamp (the magic-link verifier writing
// bootstrappedAt) could never land. A nil/empty noUnsetFields (every
// unannotated mutation) is a no-op.
func dropNoUnsetFields(prior, partial map[string]any, noUnsetFields []string) {
	for _, f := range noUnsetFields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		incoming, present := partial[f]
		if !present || !isEmptyPayloadValue(incoming) {
			continue
		}
		if stored, ok := prior[f]; ok && !isEmptyPayloadValue(stored) {
			delete(partial, f)
		}
	}
}

// isEmptyPayloadValue reports whether a payload value counts as "unset" for
// @noUnset. Deliberately narrow: nil, an empty/whitespace string, and an empty
// collection. A numeric or boolean zero is a legitimate value, not an unset --
// treating false or 0 as "empty" would make @noUnset silently un-writable for
// those types.
func isEmptyPayloadValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
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

	// THERE IS NO ANONYMOUS WRITE, EVER (epic memql#4541, D4).
	//
	// Placed first -- before the concept is even resolved -- because
	// unlike the retired and mirror checks below this is a property of
	// the CALLER, not of the concept, so nothing about the target can
	// change the answer.
	//
	// It has to be here rather than in the row-authz write guard, and
	// that is the whole reason for a separate check. That guard returns
	// nil for the public tier ("a public tier is a statement that the
	// rows are not owned, so there is no owner to compare a writer
	// against") and for an undeclared concept -- both correct for the
	// callers it was written for, and both an open door for a stranger.
	// `public` is a READ tier: it says who may SEE a row and nothing
	// whatever about who may create one.
	//
	// executeWrite is the single write chokepoint (memql#1709), so one
	// check covers mutations, raw inserts, tool handlers and staged
	// writes. The bridge's surface pin refuses the payloads that would
	// reach a write in the first place; this is the second of the two
	// independent gates, because this is the one direction where a
	// mistake cannot be taken back.
	if auth.IsAnonymousActor(ctx) {
		return nil, meta, fmt.Errorf(
			"row-authz: write to %s refused -- this call carries no identity, and there is no "+
				"anonymous write in MemQL. @rowAuthz(public) is a READ tier: it declares who may "+
				"see a row, never who may create one (epic memql#4541, D4)",
			conceptName)
	}

	conceptMeta, err := e.concepts.Get(conceptName)
	if err != nil {
		return nil, meta, fmt.Errorf("concept %q not found: %w", conceptName, err)
	}
	meta.conceptName = conceptMeta.Name

	// RETIRED CONCEPT: registered, readable, CLOSED TO NEW WRITES (memql#3756).
	// A promoted concept demoted while rows already existed under it keeps its
	// registry entry precisely so those rows stay readable -- which means the
	// lookup above succeeds and every other check downstream would pass. This is
	// the one place the refusal can live.
	//
	// Placed FIRST, before the payload is even parsed, for two reasons: the
	// refusal is a property of the concept rather than of anything the caller
	// sent, and it must not cost a database round-trip (the read-merge below) to
	// reach. It covers insert() and update() alike because executeWrite is the
	// single write chokepoint (memql#1709).
	if e.conceptIsRetired(conceptMeta.Name) {
		return nil, meta, errConceptWriteRetired(conceptMeta.Name)
	}

	// MIRROR CONCEPT: read-only by construction (epic memql#4378, D3).
	// A concept whose @origin names an external system holds a faithful
	// copy of data MemQL does not own; only that system's connector
	// writes it, under its own actor. Refused HERE, beside the retired
	// check and for the same two reasons: the refusal is a property of
	// the concept rather than of anything the caller sent, and it must
	// not cost the read-merge round-trip below to reach.
	//
	// Because executeWrite is the single write chokepoint (memql#1709),
	// one check covers mutations, raw inserts, tool handlers and staged
	// writes -- which is what the acceptance criteria mean by "all hit
	// the same seam". See component/memql/mirror_write_guard.go.
	if err := e.guardMirrorWrite(ctx, conceptMeta.Name); err != nil {
		return nil, meta, err
	}

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

		// ROW-AUTHZ ON THE WRITE PATH (memql#3174). When the target
		// concept declares a tier, the caller must satisfy it against
		// the row being written -- update, soft-delete/status-flip, or a
		// raw insert() aimed at an id that already has a row all arrive
		// here, and none of them can express the check in the DSL.
		//
		// Placed HERE for two reasons, both load-bearing:
		//
		//  1. BEFORE the merge below. mergePayloadFields overwrites
		//     priorPayload in place with the caller's delta, and every
		//     owned-tier mutation in the tree re-stamps
		//     `ownerUserId: actor.userId`. A guard reading the merged
		//     payload would compare the attacker against the attacker.
		//  2. Before any write, so a refusal cannot be partial.
		if err := guardRowAuthzWrite(ctx, conceptMeta.Name, id, priorPayload, existed, requirePrior); err != nil {
			return nil, meta, err
		}

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
			// Capture the PRIOR user role before the delta overwrites it
			// (memql#3967) so the last-owner deletion guard can tell an owner
			// from anybody else on a partial `active: false` update.
			meta.priorUserRole = stringFromAny(priorPayload["role"])
			// Capture the PRIOR valid flag (memql#2143) so the self-healing
			// blast-radius validation guard only gates the valid false->true
			// ACCEPT transition, not a re-write of an already-accepted override.
			meta.priorHealingValid = boolFromAny(priorPayload["valid"])
			// Capture the PRIOR systemOwned flag (memql#3717) so the site
			// systemOwned-delete guard sees the persisted value even when the
			// delta tries to flip it to false in the same write that sets
			// deleted:true.
			meta.priorSystemOwned = boolFromAny(priorPayload["systemOwned"])
			// Capture the PRIOR hostname (memql#4344) so the site hostname
			// policy can tell "this write is choosing a hostname" from "this
			// write inherited one through the read-merge".
			meta.priorHostname = stringFromAny(priorPayload["hostname"])
			// @createOnly fields are written on create only (fylo#63): drop
			// them from the delta BEFORE the merge so the stored value wins.
			// A deterministic-id re-stage of a row another writer owns after
			// creation (e.g. stageOutboundRequest onto a row the outbound
			// worker moved to sent) then preserves that lifecycle state
			// instead of resetting it. Empty for every unannotated mutation,
			// so this is a no-op on the whole existing tree.
			dropCreateOnlyFields(payload, mutation.CreateOnlyFields)
			// @noUnset fields are one-way (memql#3415): drop a named field
			// from the delta when it arrived EMPTY over a stored non-empty
			// value, so the stored value wins. Read-merge alone cannot do
			// this -- it only inherits fields the delta omits, and a body
			// written `f: args.f ?? ""` puts an explicit empty string in the
			// delta. Empty for every unannotated mutation.
			dropNoUnsetFields(priorPayload, payload, mutation.NoUnsetFields)
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

	// ROW-AUTHZ OWNER STAMP for writes that bypassed the mutation
	// template (memql#3175, carrying memql#3059). The other half of
	// #3174's guard above: that one refuses a write onto a row the caller
	// does not own, and a CREATE has no such row -- so its problem is
	// STAMPING rather than guarding. A raw `insert(` short-circuits the
	// planner before any template is rendered, so `args`/`accept`/`stamp`
	// never ran and the payload would otherwise be written as supplied,
	// declared owner and all.
	//
	// Placed HERE for two reasons, both load-bearing:
	//
	//  1. AFTER the read-merge above. That block REPLACES the payload map
	//     wholesale with the merged prior row (`payload = priorPayload`),
	//     so a stamp applied earlier would be discarded on any write that
	//     lands on an existing id.
	//  2. Before validation and the store, so what the declaration says
	//     is what gets validated and what gets persisted.
	//
	// Driven by the concept's `@rowAuthz(owner="F")` declaration rather
	// than by the per-concept list below: the declaration already names
	// exactly the field to stamp, so a concept is covered the moment it
	// declares a tier. See rowauthz_insert_stamp.go.
	if err := stampRowAuthzOwner(ctx, conceptMeta.Name, payload, mutation.FromTemplate); err != nil {
		return nil, meta, err
	}

	// Site OWNER STAMP (memql#4344). The same stamp one layer up, for the
	// TEMPLATE path the call above deliberately skips: createSite states no
	// `ownerUserId:` answer in its stamp{} block because it cannot -- the
	// portal seed must land cluster-owned (EMPTY owner) and "a cluster owner
	// may hand a site to somebody else" is conditional on the actor's role.
	// So the answer is supplied here, and a privileged caller's value (absent
	// included) passes through untouched.
	//
	// Placed BESIDE stampRowAuthzOwner rather than in the per-concept guard
	// table below, and that is load-bearing: ownerUserId is an outgoing
	// @relationship, so canonicalizeRelationshipFields (immediately after this)
	// rewrites it to `v1:identity:user:<id>`. A stamp applied after that lands
	// a BARE owner in a column every other row spells canonically, and the SQL
	// read path canonicalises the right-hand side -- so the owner's own site
	// would match nothing. See platform_site_hostname_policy.go.
	if conceptMeta.Name == conceptPlatformSite {
		// The actor string is resolved again further down (mutationActor, for
		// the createdBy stamp). Resolved here too, best-effort: a context with
		// no actor at all is refused by that later call, and until then an
		// empty actor simply fails the `system:` prefix test -- which is the
		// fail-closed reading, since it means the stamp RUNS rather than being
		// skipped.
		siteActor, _ := mutationActor(ctx)
		if err := applySiteOwnerStamp(ctx, payload, meta.priorExisted, siteActor); err != nil {
			return nil, meta, err
		}
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
	// path). Raw insert("v1:...", payload={...}) -- the documented
	// second write surface (docs/public/language/memql.md), reachable by
	// ANY authenticated caller and used both by server-side Go that
	// builds an insert query string directly (the polyphon utterance
	// handler, the automation step recorder) and by clients over
	// ExecuteQueryMsg -- bypassed that canon and landed bare-slug values
	// that the canonicalize-RHS query path then couldn't match. Moving
	// the call here covers both paths uniformly; the named-mutation
	// pre-call stays as a no-op idempotent belt-and-suspenders.
	//
	// (The earlier wording called this path a "system-actor write",
	// which read as a claim about WHO can reach it and contradicted the
	// credential-forging guard further down. It never was one: the
	// `insert(` short-circuit fires before origin or role is consulted.
	// Reconciled in memql#3175, which is why the owner stamp above
	// exists at all.)
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

		parentDefs := filterRelationshipDefinitions(conceptDefs, relationshipTypeParent, []string{relationshipDirectionOutgoing}, nil)
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

		aliasDefs := filterRelationshipDefinitions(conceptDefs, relationshipTypeAlias, nil, nil)
		if len(aliasDefs) == 0 {
			aliasDefs = filterRelationshipDefinitions(conceptDefs, relationshipTypeEquals, nil, nil)
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
	// Agent-role predefined-lock guard (memql#2985): a predefined
	// v1:agents:agentRole is seeded from dsl/agents/roles/*.memql on every
	// startup, and the concept documents such a row as LOCKED -- but
	// `predefined` was a UI hint with no enforcement, so any caller could
	// rename, re-categorise, re-skill or deactivate a catalog row through the
	// mutation surface. Mirrors the rbac guard above, including its shape: a
	// whole-row lock with a system-actor carve-out for the SeedMaterializer.
	// The field-scoped variant that shipped for review is gone -- it was inert
	// and the one field it would have unlocked steers the AI router. See
	// agent_role_predefined_validation.go.
	if conceptMeta.Name == conceptAgentsAgentRole {
		if err := e.validateAgentRolePredefinedLock(ctx, payload, meta.priorPredefined, actor); err != nil {
			return nil, meta, err
		}
		// Agent-role slug uniqueness (memql#3207). The concept documents `slug`
		// as canonical and never renamed, and nothing enforced it: because
		// createAgentRole opens `id: args.agentRoleId ?? args.slug`, a caller
		// supplying an explicit id alongside a seeded slug minted a SECOND
		// active row carrying it -- invisible to the guard above, which keys on
		// the row id. Runs after the read-merge, so a RENAME onto a taken slug
		// is caught as well as a create. See
		// agent_role_slug_unique_validation.go.
		if err := e.validateAgentRoleSlugUnique(ctx, payload, mutation.ID, actor); err != nil {
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
	//
	// This framing is the correct one and the canonicalize comment above
	// now agrees with it (memql#3175): the raw surface is reachable by a
	// USER actor, which is why it needs guarding at all. What this guard
	// adds beyond the generic owner stamp is orthogonal to ownership --
	// WHICH KIND of credential row may be minted, a question no
	// `@rowAuthz` declaration answers.
	if conceptMeta.Name == conceptIdentityIdentity {
		if err := e.validateIdentityCredentialActorScope(ctx, payload, actor); err != nil {
			return nil, meta, err
		}
	}
	// Last-owner deletion guard (memql#3967): a write that would leave the
	// cluster with NO active owner is refused. accountDeletionSweep calls
	// deleteUserHard unconditionally for every user whose cooldown elapsed, and
	// nothing counted owners -- so the last one could be swept away, after
	// which nothing could promote a replacement (promoting an owner requires an
	// owner) and any recovery key bound to them became permanently
	// unredeemable.
	//
	// Deliberately NOT actor-gated, unlike its neighbours above: the sweep is a
	// system actor, and the sweep is the caller this exists to stop. See
	// last_owner_deletion_validation.go.
	if conceptMeta.Name == conceptIdentityUser {
		if err := e.validateLastOwnerNotDeleted(ctx, payload, mutation.ID, meta.priorUserRole); err != nil {
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
	// Site systemOwned-delete guard (memql#3717): a systemOwned
	// v1:platform:site row (the portal's own row, re-seeded at boot) must
	// never be deletable by a non-system actor -- a UI-only block is not a
	// block, since anything holding the gRPC surface could otherwise
	// soft-delete the row over the raw mutation surface and lose cluster
	// management until the next boot re-seed. The prior-row systemOwned flag
	// (captured in the read-merge above) gates the write even if the delta
	// tries to flip systemOwned to false in the same write. See
	// platform_site_delete_guard.go.
	// Site HOSTNAME POLICY guard (memql#4344), beside the systemOwned refusal
	// because it is the same kind of rule: a user's site is <slug>.<domain>
	// under the domain this cluster serves, with a bounded slug, a reserved
	// list and cluster-wide uniqueness -- none of which a mutation body can
	// express (the domain comes from the environment, uniqueness needs a read,
	// and the shape rule is waived for a cluster owner). See
	// platform_site_hostname_policy.go.
	if conceptMeta.Name == conceptPlatformSite {
		if err := e.validateSiteSystemOwnedDelete(ctx, payload, meta.priorSystemOwned, actor); err != nil {
			return nil, meta, err
		}
		if err := e.validateSiteHostnamePolicy(ctx, payload, mutation.ID, actor, meta.priorExisted, meta.priorHostname); err != nil {
			return nil, meta, err
		}
		// The D10 lifecycle law (epic memql#4794), beside the two above
		// because it is the same kind of rule and the same reason: what may
		// follow what is a comparison against the PRIOR row, which no
		// mutation body can make. See platform_site_status_guard.go.
		if err := e.validateSiteStatusTransition(ctx, payload, meta.priorExisted, meta.priorStatus, meta.priorSystemOwned, actor); err != nil {
			return nil, meta, err
		}
	}

	// v1:platform:packageDeployment is APPEND-ONLY past a terminal status
	// (epic memql#4794, D7). Enforced here rather than left to the pipeline
	// that happens to respect it: the timeline's value is that it records what
	// was attempted, and a retry quietly rewriting the row it is retrying
	// would destroy exactly that.
	if conceptMeta.Name == conceptPlatformPackageDeployment {
		if err := validatePackageDeploymentAppendOnly(meta.priorExisted, meta.priorStatus); err != nil {
			return nil, meta, err
		}
	}

	// Custom-domain guards (epic memql#4805, design D10). Beside the site
	// policy above and for the same reason: none of the three -- not under
	// this cluster's own domain, not a collision, not past the env-tunable
	// per-site maximum -- is expressible in a mutation body, and a UI-only
	// check is not a check. The concept is clusterOwner tier, so these are the
	// only thing standing between an operator's typo and a certificate request
	// for the platform's own sign-in host.
	// See component/memql/platform_custom_domain_policy.go.
	if conceptMeta.Name == conceptPlatformCustomDomain {
		if err := e.validateCustomDomainPolicy(ctx, payload, mutation.ID, actor, meta.priorExisted, meta.priorHostname, meta.priorStatus); err != nil {
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

	// THE OUTBOX APPEND (epic memql#4378, D5). A concept whose dataState
	// is `origin` is one MemQL owns and external systems mirror, so every
	// write to it has to reach those systems -- and the record of that
	// obligation is written in the SAME TRANSACTION as the row, or a
	// process dying between the two statements leaves a committed change
	// nothing will ever propagate. See outbox_append.go for both failure
	// directions and why only a transaction closes them.
	//
	// The transaction is opened ONLY for such a concept. Every other
	// write -- which is every concept in the tree an author has not
	// declared -- takes the single-statement path below unchanged.
	var result memorynodes.Node
	if targets := outboxTargetsFor(conceptMeta.Name); len(targets) > 0 {
		retire := outboxPayloadRetires(payload)
		txErr := e.runInWriteTx(ctx, func(txStore memorynodes.Store) error {
			created, createErr := conceptMeta.Create(ctx, txStore, createParams)
			if createErr != nil {
				return createErr
			}
			result = created
			return e.appendOutboxEntries(ctx, txStore, conceptMeta.Name, created.ID, created.CreatedAt, targets, retire)
		})
		if txErr != nil {
			return nil, meta, txErr
		}
		// THE OUTBOX ENTRIES ARE A SECOND WRITE, TO A SECOND CONCEPT, AND
		// NOTHING ELSE ANNOUNCES THEM (memql#4531). appendOutboxEntries
		// writes through the transaction's store directly -- it never goes
		// back through executeWrite -- so the invalidation published at the
		// bottom of this function names the ORIGIN concept and says nothing
		// about the queue. A cached read of the queue therefore survives a
		// write that just added to it.
		//
		// This is the gap the task told us to look for, and it was real: the
		// full clear removed above was covering it by accident, because
		// wiping everything also wipes the queue's cached reads. The fix is
		// to publish the event that was missing rather than to keep the
		// sledgehammer that hid its absence.
		//
		// AFTER the transaction commits, deliberately. Evicting earlier
		// would open a window in which a concurrent read repopulates the
		// cache from pre-commit state, and would evict for a write that a
		// rollback then discarded.
		//
		// It generalises: ANY row reaching the store outside executeWrite
		// is invisible to invalidation. This is the only such path inside
		// the engine today; the observability rollups are the other family,
		// and they are documented as TTL-only rather than fixed here
		// because nothing in their loop is a MemQL write at all.
		e.InvalidateCacheForConcept(OutboxEntryConcept)
	} else {
		store := newBunStore(e.database())
		created, createErr := conceptMeta.Create(ctx, store, createParams)
		if createErr != nil {
			return nil, meta, createErr
		}
		result = created
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

	// STAGED DATA (memql#3985): a write to a STAGED concept publishes NO
	// graph.node.created event. The row is written, durable and addressable --
	// this is not the retirement refusal above, it is its opposite -- but the
	// event that would announce it is withheld until the concept is trained.
	//
	// Forced, not chosen, and the build immediately below is the evidence: it
	// flattens the ENTIRE stored payload into the envelope's top level and then
	// retains the whole object again under "payload". events.Bus.Publish matches
	// on topic and fans a clone out to every subscriber -- component/events has
	// no AccessContext and no authorization hook of any kind -- so emitting this
	// while the read seam hides the row would publish the complete row to every
	// in-process subscriber and every automation triggering on the concept, and
	// call that hidden. See staged_write.go for the full argument, including why
	// the withheld events are NOT replayed when the concept is later trained.
	//
	// Guarded around the BUILD as well as the publish, deliberately: a staged
	// row's payload should never be assembled into an event envelope at all,
	// not assembled and then dropped.
	if !e.withholdGraphWriteEvent(conceptMeta.Name) {
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
	}
	// 5.6 (memql#1970): also emit the dedicated cache-invalidation event
	// on its own broadcast channel. ONLY the result-cache evictor
	// subscribes, and a single cache.invalidate.* routing rule forwards
	// it to every node -- so cross-node eviction needs no per-concept
	// graph forwarding (which would risk double-firing automations on
	// sibling replicas). Same commit point as the graph-write publish.
	//
	// DELIBERATELY OUTSIDE the staged guard above (memql#3985), and the
	// asymmetry is the ruling rather than an oversight -- the two emits look
	// alike and are not:
	//
	//   - There is nothing here to leak. The payload is {"concept": <id>} and
	//     the sole subscriber (result_cache_invalidation.go) does not even read
	//     it -- it recovers the concept from the TOPIC and evicts. A staged
	//     concept's NAME is public anyway: it is registered, resolvable and
	//     listed, which is the entire difference between this tier and the
	//     memql#3928 construct tier. Only its ROWS are withheld.
	//   - Suppressing it would cause the failure this tier exists to avoid.
	//     While a concept is staged its reads answer empty, and empty results
	//     are cacheable. With no invalidation, the cache still holds "empty" at
	//     the moment memql#3986 trains the concept -- so rows that just went
	//     live stay invisible until a TTL lapses or a node restarts. Rows
	//     intact, addressable, and absent from every read: indistinguishable
	//     from having lost them, which is the exact direction
	//     authoring_concept_staged.go's fold refuses to point.
	//   - The channel is safe BECAUSE it carries no side effects. That property
	//     is what makes graph.node.created dangerous and this one not; reading
	//     the two as one rule inverts it.
	//
	// SYNCHRONOUS LOCALLY (memql#4531). This call site used to be preceded,
	// ~100 lines up, by an e.invalidateCache() that did cache.Clear() plus a
	// full dependency-index wipe -- so on the writing node every write nuked
	// the ENTIRE result cache and the surgical per-concept eviction only ever
	// earned its keep for writes arriving from other replicas. That full clear
	// is gone; InvalidateCacheForConcept evicts exactly this concept's
	// dependent entries inline, before this function returns, and then
	// publishes for the siblings. The inline half is load-bearing: Publish
	// delivers to subscribers in separate goroutines, so relying on the local
	// subscriber alone would have raced a client's read-your-writes.
	e.InvalidateCacheForConcept(conceptMeta.Name)

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

// incomingLookupTarget is the row target for an INCOMING relationship lookup
// -- "which rows point AT these ids" (memql#3432).
//
// The two lookups below used to derive it from `len(values)`, the count of
// SOURCE ids. That is right for fetchNodesByIds, whose filter is `id IN
// (...)`: each id resolves to at most one row, so the two counts coincide and
// asking for more than were named would be waste. It is wrong here by
// construction. The answer to "which rows name this parent" is not bounded by
// how many parents were asked about -- one parent with ten children resolved
// to exactly ONE, because len(values) was 1, and the other nine were never
// scanned. No error and no short-read signal: a traversal that answers "this
// row has one child" about a row with ten, which is the same silent
// under-reporting shape as memql#3388 and #3397.
//
// The correct bound is the QUERY's, and the caller already carries it: every
// relationship resolver threads the traversal's `limit` down from
// evaluateRelationshipExpression, which receives the query's effective limit
// (clamped to MaxResults at executor.go's entry, or MaxResults itself for the
// graph-expansion callers). So the target IS that limit, and an absent one
// (<= 0) is left for executeFilterQuery to fill from MaxWindow -- the engine's
// own bound on an unbounded read, which is the only honest answer when the
// query states none.
//
// This composes with memql#3397 rather than duplicating it. That change made
// executeFilterQuery's SQL enumerate the result set, so its LIMIT bounds
// distinct ids instead of raw versions -- a correct scan. This is the number
// that scan is bounded TO, and a correct scan bounded to an incorrect target
// is still wrong.
func incomingLookupTarget(limit int) int {
	if limit < 0 {
		return 0
	}
	return limit
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

	// OUTGOING: `id IN (...)` resolves each named id to at most one row, so
	// len(unique) is a true upper bound and NOT the memql#3432 defect -- see
	// incomingLookupTarget for why the two sibling lookups below differ.
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

		if e.Component != nil && e.Logger != nil {
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

// fetchNodesByJSONFieldValues answers an INCOMING relationship question:
// which rows of `conceptName` carry one of `values` at `fieldPath`, whether
// that field holds a single id or an ARRAY of them. It backs childOf, the
// incoming branches of owns / references / createdBy, and the alias/equals
// canonical lookup -- every one of which is many-rows-per-value, so the target
// comes from incomingLookupTarget and never from len(values).
func (e *MemQLEngine) fetchNodesByJSONFieldValues(ctx context.Context, conceptName string, fieldPath []string, values []string, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
	unique := uniqueStrings(values)
	if len(unique) == 0 {
		return nil, nil
	}

	pathExpr, err := buildJSONPathExpression(fieldPath)
	if err != nil {
		return nil, err
	}
	jsonbExpr, err := buildJSONBPathExpression(fieldPath)
	if err != nil {
		return nil, err
	}

	// A relationship field holds EITHER one id or an array of them, and the
	// same declaration is read from both ends: `owns` over memberIds resolves
	// element-wise going out, so it has to match element-wise coming back.
	//
	// The scalar comparison alone could not do that. `#>>` renders an array as
	// its JSON TEXT -- `["v1:x:cadet:a","v1:x:cadet:b"]` -- which can never
	// equal a single member id, so every INCOMING lookup against an
	// array-valued field answered "nobody", with no error and no warning
	// (memql#3670). The outgoing half of the very same declaration worked,
	// which is what made one-directional declarations look symmetrical.
	//
	// Two branches rather than containment alone: jsonb_exists_any also
	// matches an OBJECT's top-level keys and does not match a numeric scalar,
	// so gating it on jsonb_typeof keeps the scalar path exactly as it was.
	// Same shape the `in` operator compiles to for payload fields.
	expr := fmt.Sprintf(
		"(concept = ? AND ((jsonb_typeof(%s) = 'array' AND jsonb_exists_any(%s, ?::text[])) OR (%s IN (?))))",
		jsonbExpr, jsonbExpr, pathExpr,
	)
	args := []any{strings.TrimSpace(conceptName), pq.Array(unique), bun.In(unique)}

	nodes, err := e.executeFilterQuery(ctx, nil, compiledExpression{sql: expr, args: args}, timestamp, incomingLookupTarget(limit), nil)
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes, nil
}

// fetchNodesByNodeFieldValues is fetchNodesByJSONFieldValues over a table
// COLUMN rather than a payload path -- `createdBy` is the only one
// mapNodeFieldToColumn admits. It backs resolveCreatedBy's incoming branch
// ("which rows did this identity create"), which is one-source-to-many by
// definition, so it takes the same target derivation.
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

	nodes, err := e.executeFilterQuery(ctx, nil, compiledExpression{sql: expr, args: args}, timestamp, incomingLookupTarget(limit), nil)
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes, nil
}

// staged-data: GATE -- and this is the read that makes an SQL-only reading of
// the whole tier wrong (epic memql#3974; enforcement is memql#3983's).
//
// It filters on `id IN (?)` and an optional `createdAt <= ?` and nothing else:
// no concept, no filter, no authz. Its one caller, latestMatchingNodes, then
// INSTALLS what comes back as the result row, overwriting the version the scan
// selected. So a predicate that lives only in the scan's WHERE clause is a
// predicate this function can hand a row straight around, and a gate proved
// only against the emitted SQL is green while leaking.
//
// memql#3983 closes it twice on purpose -- the injected conjunct is spelled so
// the in-process re-check can evaluate it, AND the swapped-in candidate is
// admitted directly -- because the first depends on the caller's expression
// still carrying the term, and the unbound plans are exactly the ones where it
// does not. Adjudicated GATE here, enforced there; a third check in this
// function would be a third place to keep in step.
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

// staged-data: MUST-NOT-GATE -- the ruling memql#3985 already recorded below,
// carried here in memql#3984's machine-readable form so the read-site sweep can
// see that it was made. Same reasoning, one statement of it.
//
// checkNodeExists reports whether any version of (concept, id) is stored. Its
// caller is previewInsert, which derives a CONTENT-ADDRESSED id from the
// normalized payload and reports back whether that id is already taken.
//
// NOT STAGED-FILTERED, AND MUST NEVER BE (memql#3985), for the same reason
// loadPriorPayload is not. A gate here makes this answer "free" about an id a
// staged row already occupies; the caller, told the id is unused, inserts -- and
// the append-only store puts that write on top of the staged row as a new
// version of it. The row it was told did not exist is the row it silently
// overwrote.
//
// This is the collision probe. A collision probe that cannot see half the rows
// is not a narrower probe, it is a wrong answer.
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
		if e.Component != nil && e.Logger != nil {
			e.Logger.Warn("harness observation embed failed (recall will skip this row until backfilled)",
				"id", id, "error", err)
		}
		return
	}
	if e.Component != nil && e.Logger != nil {
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
		if e.Component != nil && e.Logger != nil {
			e.Logger.Warn("action intent embed failed (planner search will skip this action until backfilled)",
				"id", id, "error", err)
		}
		return
	}
	if e.Component != nil && e.Logger != nil {
		e.Logger.Debug("action intent embedded for planner search", "id", id, "intent_len", len(intent))
	}
}
