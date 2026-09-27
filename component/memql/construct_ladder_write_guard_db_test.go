package memql

// Real-engine tests for the construct ladder write guard (epic memql#5408,
// issue #5409): a learned procedure's certification ladder is written only by
// integrations/procedure, under internal origin.
//
// v1:authoring:construct is @rowAuthz(owner="ownerUserId"), so the row-authz
// write guard admits an owner's write to their OWN construct -- and the raw
// insert(...) / update(...) literals never consult the @serverOnly on
// recordProcedure, recordConstructLadder, recordConstructReliability and
// recordConstructGoalSignature. Before the guard, an ordinary writer could put
// `ladder: "trusted"`, a procedure and a goal signature onto their own
// construct, and compile would serve their goals from code nobody reviewed,
// with no model and no app.
//
// Driven through Engine.Execute against a real Postgres (the shared read-merge
// engine, db-gated) because the hole is the WHOLE write path: parser, raw
// write short-circuit, row-authz guard, read-merge, schema validation and
// persistence. A test of the guard function alone would pass on an engine
// that never calls it.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/provenance"
)

// constructLadderGuardFile is what every refusal names, so a refusal from
// some other gate cannot pass for this one's.
const constructLadderGuardFile = "construct_ladder_write_guard.go"

// plantedLadderValues is one value for each field only the ladder writes --
// each one a thing a person could want to forge.
var plantedLadderValues = map[string]any{
	"ladder":              "trusted",
	"preconditions":       map[string]any{"tools": map[string]any{"node": "22.1.0"}},
	"procedure":           map[string]any{"v": 1, "title": "planted", "steps": []any{map[string]any{"tool": "exec", "symbol": "s0"}}},
	"procedureHash":       "sha256:planted",
	"shadowMatches":       99,
	"canaryMatches":       99,
	"distinctBindings":    map[string]any{"s0.command.1": []any{"sha256:aa", "sha256:bb"}},
	"failures":            7,
	"insufficient":        7,
	"promotionApprovalId": "v1:work:approval:planted",
	"lastReplayAt":        "2026-09-26T10:00:00Z",
	"ladderReason":        "planted",
	"ladderChangedAt":     "2026-09-26T10:00:00Z",
}

// plantedLearnedEvidence is one value for each field that becomes the
// ladder's once a construct IS a learned procedure: the signature compile
// serves it by, and the reliability that ranks it.
var plantedLearnedEvidence = map[string]any{
	"goalSignature":  "sig-planted",
	"reliability":    1,
	"reinforceCount": 99,
	"lastReinforced": "2026-09-26T10:00:00Z",
}

func sortedFieldNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rawConstructInsert renders the raw insert(...) literal -- the public write
// that bypasses every mutation's @serverOnly. Aimed at an id that already has
// a row, it is a read-merge REWRITE of that row.
func rawConstructInsert(t *testing.T, id string, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return fmt.Sprintf(`insert("%s", id="%s", payload=%s)`, constructConceptID, id, raw)
}

// rawConstructUpdate runs the update() form at the write chokepoint: what a
// mutation body's `update { }` reaches -- a person's own authored mutation
// bound to their construct included -- with no template's @serverOnly in
// front of it. The engine's Execute does not take an update(...) literal, so
// the form is driven through executeMutation, which is what every update
// reaches.
func rawConstructUpdate(t *testing.T, ctx context.Context, eng *MemQLEngine, id string, payload map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	// Stamped the way Execute stamps a raw write: direct, against the concept.
	ctx = provenance.ContextWithProvenance(ctx, provenance.Direct("rawUpdate:"+constructConceptID))
	_, err = eng.executeMutation(ctx, MutationNode{
		Kind:       ast.MutationKindUpdate,
		Concept:    constructConceptID,
		ID:         id,
		PayloadRaw: string(raw),
	})
	return err
}

// wholeConstruct is every field a construct row requires, for a raw insert of
// a new one.
func wholeConstruct(name string) map[string]any {
	return map[string]any{
		"bundleId":        "bundle-" + name,
		"kind":            "automation",
		"name":            name,
		"targetNamespace": "procedure",
		"source":          "automation " + name + " { }",
		"status":          "draft",
	}
}

func constructVersionCount(t *testing.T, db *bun.DB, id string) int {
	t.Helper()
	n, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).
		Where("concept = ?", constructConceptID).
		Where("id = ?", id).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

// seedLearnedProcedure writes a construct the way the lift does -- created by
// its owner, then given its procedure, its rung and its evidence through the
// four @serverOnly writes under internal origin -- and returns its stored id.
func seedLearnedProcedure(t *testing.T, eng *MemQLEngine, owner, constructId string) string {
	t.Helper()
	rowID := seedLearnedConstruct(t, eng, owner, constructId)
	ladderWrite(t, eng, owner, "recordProcedure", map[string]any{
		"constructId":   constructId,
		"source":        "automation procedure_" + constructId + " { }",
		"procedure":     map[string]any{"v": 1, "title": "Write the greeting file"},
		"preconditions": map[string]any{"tools": map[string]any{"mkdir": "9.4"}},
		"procedureHash": "sha256:learned",
	})
	ladderWrite(t, eng, owner, "recordConstructLadder", map[string]any{
		"constructId":   constructId,
		"ladder":        "shadow",
		"shadowMatches": 2,
		"ladderReason":  "lifted into shadow",
	})
	ladderWrite(t, eng, owner, "recordConstructGoalSignature", map[string]any{
		"constructId":   constructId,
		"goalSignature": "sig-learned",
	})
	ladderWrite(t, eng, owner, "recordConstructReliability", map[string]any{
		"constructId":    constructId,
		"reliability":    0.5,
		"reinforceCount": 2,
		"lastReinforced": "2026-09-20T10:00:00Z",
	})
	return rowID
}

// THE HOLE, ONE FIELD AT A TIME, down every raw path: a writer's insert(...)
// of a NEW construct carrying a ladder field, the same literal aimed at their
// OWN construct's id (a read-merge rewrite), and the update() form at the
// write chokepoint. Every one is refused, naming the field, and nothing lands.
func TestConstructLadderWriteGuard_ARawWriteOfALadderFieldIsRefused(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	suffix := uniqueSuffix("ladderguard")
	owner := "ladder-guard-owner-" + suffix
	ownedID := seedLearnedConstruct(t, eng, owner, "lg-"+suffix)

	refused := func(t *testing.T, err error, field, what string) {
		t.Helper()
		require.Error(t, err, "%s wrote %s", what, field)
		require.ErrorContains(t, err, constructLadderGuardFile, "refused, but not by the ladder guard")
		require.ErrorContains(t, err, "`"+field+"`", "the refusal must name the field")
	}
	for _, field := range sortedFieldNames(plantedLadderValues) {
		value := plantedLadderValues[field]
		t.Run("insert/"+field, func(t *testing.T) {
			name := "planted_" + strings.ToLower(field) + "_" + strings.ReplaceAll(suffix, "-", "_")
			id := constructConceptID + ":" + name
			payload := wholeConstruct(name)
			payload[field] = value
			_, err := eng.Execute(rowAuthzCallerCtx(owner), rawConstructInsert(t, id, payload))
			refused(t, err, field, "a raw insert(...) of a new construct")
			require.Zero(t, constructVersionCount(t, db, id), "refused, yet a row landed")
		})
		t.Run("rewrite/"+field, func(t *testing.T) {
			before := constructVersionCount(t, db, ownedID)
			_, err := eng.Execute(rowAuthzCallerCtx(owner), rawConstructInsert(t, ownedID, map[string]any{field: value}))
			refused(t, err, field, "a raw insert(...) onto the owner's own construct")
			require.Equal(t, before, constructVersionCount(t, db, ownedID), "refused, yet a version landed")
			require.NotContains(t, latestPayload(t, ctx, db, constructConceptID, ownedID), field)
		})
		t.Run("update/"+field, func(t *testing.T) {
			before := constructVersionCount(t, db, ownedID)
			err := rawConstructUpdate(t, rowAuthzCallerCtx(owner), eng, ownedID, map[string]any{field: value})
			refused(t, err, field, "an update() of the owner's own construct")
			require.Equal(t, before, constructVersionCount(t, db, ownedID), "refused, yet a version landed")
			require.NotContains(t, latestPayload(t, ctx, db, constructConceptID, ownedID), field)
		})
	}
}

// ON A LEARNED PROCEDURE the signature and the reliability are the ladder's
// too: the signature is what compile serves a goal from it by, and the
// reliability ranks it among the procedures that answer one. A rewrite and an
// update of each are refused, and the stored row keeps what the ladder wrote.
func TestConstructLadderWriteGuard_OnALearnedProcedureTheSignatureAndReliabilityAreRefused(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	suffix := uniqueSuffix("learnedguard")
	owner := "learned-guard-owner-" + suffix
	rowID := seedLearnedProcedure(t, eng, owner, "lp-"+suffix)
	stored := latestPayload(t, ctx, db, constructConceptID, rowID)

	writes := map[string]func(field string, value any) error{
		"rewrite": func(field string, value any) error {
			_, err := eng.Execute(rowAuthzCallerCtx(owner), rawConstructInsert(t, rowID, map[string]any{field: value}))
			return err
		},
		"update": func(field string, value any) error {
			return rawConstructUpdate(t, rowAuthzCallerCtx(owner), eng, rowID, map[string]any{field: value})
		},
	}
	for _, field := range sortedFieldNames(plantedLearnedEvidence) {
		value := plantedLearnedEvidence[field]
		for _, op := range []string{"rewrite", "update"} {
			write := writes[op]
			t.Run(op+"/"+field, func(t *testing.T) {
				before := constructVersionCount(t, db, rowID)
				err := write(field, value)
				require.Error(t, err, "a raw %s rewrote %s on a learned procedure", op, field)
				require.ErrorContains(t, err, constructLadderGuardFile, "refused, but not by the ladder guard")
				require.ErrorContains(t, err, "`"+field+"`", "the refusal must name the field")
				require.Equal(t, before, constructVersionCount(t, db, rowID), "refused, yet a version landed")
				require.Equal(t, stored[field], latestPayload(t, ctx, db, constructConceptID, rowID)[field])
			})
		}
	}

	// An AUTHORED construct -- no ladder -- is not a learned procedure, and
	// its signature is not this guard's: the rule is scoped to what the ladder
	// governs, and a guard that reached further would refuse writes it has no
	// reason to judge.
	authoredID := seedLearnedConstruct(t, eng, owner, "la-"+suffix)
	_, err := eng.Execute(rowAuthzCallerCtx(owner), rawConstructInsert(t, authoredID, map[string]any{"goalSignature": "sig-authored"}))
	require.NoError(t, err, "a signature on a construct that is not on the ladder is not this guard's to refuse")
}

// THE POSITIVE CONTROL: the same raw writes with internal origin -- the stamp
// integrations/procedure carries -- land. Without it, the refusals above would
// pass on a guard that refused every write to a construct.
func TestConstructLadderWriteGuard_TheSameWritesWithInternalOriginLand(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	suffix := uniqueSuffix("ladderinternal")
	owner := "ladder-internal-owner-" + suffix
	internal := auth.ContextWithInternalOrigin(rowAuthzCallerCtx(owner))

	// A new row, carrying the whole ladder. Internal-origin Go states the
	// owner itself (the raw insert's owner stamp is for the untrusted path).
	name := "internal_" + strings.ReplaceAll(suffix, "-", "_")
	id := constructConceptID + ":" + name
	payload := wholeConstruct(name)
	payload["ownerUserId"] = owner
	for field, value := range plantedLadderValues {
		payload[field] = value
	}
	_, err := eng.Execute(internal, rawConstructInsert(t, id, payload))
	require.NoError(t, err, "an internal-origin insert carrying the ladder was refused")
	require.Equal(t, "trusted", latestPayload(t, ctx, db, constructConceptID, id)["ladder"])

	// A rewrite and an update of a learned procedure, moving the rung and the
	// evidence that is the ladder's on it.
	rowID := seedLearnedProcedure(t, eng, owner, "li-"+suffix)
	rewrite := map[string]any{"ladder": "canary"}
	for field, value := range plantedLearnedEvidence {
		rewrite[field] = value
	}
	_, err = eng.Execute(internal, rawConstructInsert(t, rowID, rewrite))
	require.NoError(t, err, "an internal-origin rewrite of a learned procedure was refused")
	require.NoError(t, rawConstructUpdate(t, internal, eng, rowID, map[string]any{"ladder": "trusted", "failures": 0}),
		"an internal-origin update of a learned procedure was refused")
	got := latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Equal(t, "trusted", got["ladder"])
	require.Equal(t, "sig-planted", got["goalSignature"])
	require.EqualValues(t, 0, got["failures"])
}

// THE NEGATIVE CONTROL: an owner's write that leaves the ladder as it is --
// renaming their construct, re-sending the stored ladder values unchanged
// beside a rename the way a client re-sends the row it read, and retiring it
// through the mutation the OS calls -- passes, and the ladder survives it. A
// guard keyed on the PRESENCE of a field rather than on a change to it would
// refuse the second.
func TestConstructLadderWriteGuard_AnOwnersWriteThatLeavesTheLadderAlonePasses(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	suffix := uniqueSuffix("ladderowner")
	owner := "ladder-owner-writes-" + suffix
	rowID := seedLearnedProcedure(t, eng, owner, "lo-"+suffix)
	stored := latestPayload(t, ctx, db, constructConceptID, rowID)
	renamed := "renamed_" + strings.ReplaceAll(suffix, "-", "_")

	_, err := eng.Execute(rowAuthzCallerCtx(owner), rawConstructInsert(t, rowID, map[string]any{"name": renamed}))
	require.NoError(t, err, "the owner's rename of their learned construct was refused")

	resent := map[string]any{"name": renamed + "_again"}
	for _, field := range append(sortedFieldNames(plantedLadderValues), sortedFieldNames(plantedLearnedEvidence)...) {
		if v, ok := stored[field]; ok {
			resent[field] = v
		}
	}
	require.NoError(t, rawConstructUpdate(t, rowAuthzCallerCtx(owner), eng, rowID, resent),
		"a write re-sending the stored ladder values unchanged was refused")

	runMutation(t, rowAuthzCallerCtx(owner), eng, "setConstructStatus", map[string]any{
		"constructId": "lo-" + suffix,
		"status":      "retired",
	})

	got := latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Equal(t, "retired", got["status"])
	require.Equal(t, renamed+"_again", got["name"])
	for _, field := range append(sortedFieldNames(plantedLadderValues), sortedFieldNames(plantedLearnedEvidence)...) {
		require.Equal(t, stored[field], got[field], "the owner's write moved %s", field)
	}
}
