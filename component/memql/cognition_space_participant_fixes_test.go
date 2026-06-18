package memql

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// loadFullCognitionEngine boots the engine against the entire embedded
// DSL tree (same path as TestEngineInitLoadsFullDSL) and returns it so a
// test can introspect the ACTUAL loaded query/mutation definitions -- not
// an inlined copy that could drift from what ships.
func loadFullCognitionEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	require.NotNil(t, registry)
	eng, err := New(nil)
	require.NoError(t, err)
	// The provider loader WARNs once per provider with no DB/secrets; not
	// what these tests check.
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	return eng
}

func mustLoadFn(t *testing.T, eng *MemQLEngine, name string) *Function {
	t.Helper()
	fn, ok := eng.Functions().Lookup(name)
	require.Truef(t, ok, "function %q not loaded", name)
	return fn
}

// TestSpaceUserIdFilterMatchesOwnerUserId pins memql#1636.
//
// queryActiveSpaces / queryArchivedSpaces / querySavedSpaces took a
// `userId` filter arg but compared it against `createdBy` -- which holds
// the owner EMAIL (e.g. znas@znas.io), not the v1:identity:user id the
// caller passes. So a user-id value never matched and the lists came back
// empty. The fix filters on payload.ownerUserId (the id) and gates the
// query on payload.ownerUserId==actor.userId so a caller only ever sees
// their own spaces.
func TestSpaceUserIdFilterMatchesOwnerUserId(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	for _, name := range []string{"queryActiveSpaces", "queryArchivedSpaces", "querySavedSpaces"} {
		t.Run(name, func(t *testing.T) {
			fn := mustLoadFn(t, eng, name)
			filter := queryFilterBody(t, fn.ExprSource)
			require.Contains(t, filter, "payload.ownerUserId==args.userId",
				"%s must narrow on payload.ownerUserId (the user id), not createdBy", name)
			require.Contains(t, filter, "payload.ownerUserId==actor.userId",
				"%s must gate on the owner == caller (authz + conformance)", name)
			require.NotContains(t, filter, "createdBy==args.userId",
				"%s must NOT compare the userId arg against createdBy (that field holds the owner email)", name)
		})
	}
}

// TestSavedArchivedQueriesDropActiveTrait pins memql#1637 (the query half).
//
// Saved + archived spaces carry active=false (mutationArchiveSpace /
// mutationSaveSpace / the SPA wrappers set it when a space leaves the
// active list). The queries applied traitIsActiveRecord (payload.active==
// true), so a saved/archived row could NEVER come back. The fix drops
// traitIsActiveRecord from those queries and excludes hard-delete
// tombstones via traitIsNotDeleted instead.
func TestSavedArchivedQueriesDropActiveTrait(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	cases := []struct {
		name   string
		status string
	}{
		{"querySavedSpaces", "traitStatusIsSaved"},
		{"queryArchivedSpaces", "traitStatusIsArchived"},
		{"queryAllArchivedSpacesAcrossUsers", "traitStatusIsArchived"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := mustLoadFn(t, eng, tc.name)
			filter := queryFilterBody(t, fn.ExprSource)
			require.Contains(t, filter, tc.status, "%s must still gate on the lifecycle status", tc.name)
			require.NotContains(t, filter, "traitIsActiveRecord",
				"%s must NOT apply traitIsActiveRecord -- saved/archived rows are active=false and would never match", tc.name)
			require.Contains(t, filter, "traitIsNotDeleted",
				"%s must exclude hard-delete tombstones via traitIsNotDeleted", tc.name)
		})
	}
}

// TestRenameSpaceIsPartialUpdate pins memql#1637 (the rename-wipe half).
//
// mutationRenameSpace used `insert { id, ownerUserId, args.payload }`. A
// caller passing a partial payload={name} produced a brand-new full-record
// version with every unstated field (goal / maxAgents / maxHumans / status
// / active) wiped to null. Switching to `update {}` makes it a read-merge-
// validate-write so only the supplied fields change. (Same class as the
// engine read-merge work in #1628; fixed here at the DSL level.)
func TestRenameSpaceIsPartialUpdate(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	fn := mustLoadFn(t, eng, "mutationRenameSpace")
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, ast.MutationKindUpdate, fn.MutationTemplate.Kind,
		"mutationRenameSpace must be an `update` (partial read-merge), not an `insert` (full-record replace)")
	require.Equal(t, "v1:cognition:space", fn.MutationTemplate.Concept)
}

// TestArchiveSpaceStampsArchivedAt pins memql#1637 (the archivedAt half).
//
// queryArchivedSpaces gates on status=='archived', but the Archived tab +
// retention cron also rely on archivedAt being present. A minimal-payload
// caller of mutationArchiveSpace produced a row with NO archivedAt. The
// fix stamps archivedAt server-side when the caller omits it.
func TestArchiveSpaceStampsArchivedAt(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	fn := mustLoadFn(t, eng, "mutationArchiveSpace")
	require.NotNil(t, fn.MutationTemplate)

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-archivist",
		Role:   auth.RoleWriter,
	})

	// Caller deliberately omits archivedAt (and ownerUserId) -- the
	// minimal-payload shape #1637 calls out as never surfacing.
	mutation, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"spaceId": "space-001",
		"payload": map[string]any{
			"name":   "Quarterly Review",
			"status": "archived",
			"active": false,
		},
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	require.NotEmpty(t, payload["archivedAt"],
		"mutationArchiveSpace must stamp archivedAt when the caller omits it")
	require.Equal(t, "archived", payload["status"])
}

// TestArchiveSpacePreservesCallerArchivedAt asserts the stamp does not
// clobber an archivedAt the caller already computed (the SPA stamps
// archivedAt + the matching expiresAt = archivedAt + retentionDays; an
// override would desync the pair).
func TestArchiveSpacePreservesCallerArchivedAt(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	fn := mustLoadFn(t, eng, "mutationArchiveSpace")

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-archivist",
		Role:   auth.RoleWriter,
	})
	const callerArchivedAt = "2026-01-02T03:04:05Z"
	mutation, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"spaceId": "space-001",
		"payload": map[string]any{
			"status":     "archived",
			"active":     false,
			"archivedAt": callerArchivedAt,
		},
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	require.Equal(t, callerArchivedAt, payload["archivedAt"],
		"a caller-provided archivedAt must be preserved, not overwritten by the server stamp")
}

// TestActiveHumanParticipantQueryMatchesActiveHuman pins memql#1638.
//
// queryActiveHumanParticipants applied traitIsActiveRecord
// (payload.active==true), but the v1:cognition:participant concept has NO
// `active` field -- lifecycle lives on the `status` enum. So the predicate
// could never match and the query returned [] even with an active human in
// the space (one querySpaceParticipants happily returned). The fix drops
// traitIsActiveRecord and keeps the status==active + participantType==human
// gate, mirroring querySiParticipantForSpace.
func TestActiveHumanParticipantQueryMatchesActiveHuman(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	fn := mustLoadFn(t, eng, "queryActiveHumanParticipants")
	filter := queryFilterBody(t, fn.ExprSource)

	require.Contains(t, filter, `payload.participantType=="human"`)
	require.Contains(t, filter, "traitStatusIsActive",
		"must gate on status==active (the participant lifecycle field)")
	require.NotContains(t, filter, "traitIsActiveRecord",
		"participant has no `active` field -- traitIsActiveRecord (payload.active==true) can never match a participant row")
}

// TestTouchSessionBindsToAuthSession pins memql#1639.
//
// mutationTouchSession is meant to heartbeat a v1:identity:authSession but
// was bound to v1:cognition:session, whose required participantId/spaceId
// an auth session never carries -- so validation was unsatisfiable. The fix
// rebinds it to v1:identity:authSession.
func TestTouchSessionBindsToAuthSession(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	fn := mustLoadFn(t, eng, "mutationTouchSession")
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, "v1:identity:authSession", fn.MutationTemplate.Concept,
		"mutationTouchSession must validate against v1:identity:authSession, not v1:cognition:session")
}

// TestPresenceAutoIdIsValidShortId pins memql#1640.
//
// When mutationUpdateParticipantPresence auto-derived presenceId it did
// `concat("presence-", args.participantId)`. participantId is a canonical
// colon id (v1:cognition:participant:...), so the derived id carried colons
// and was rejected as an invalid shortId. The fix hash()-encodes the
// participantId to a colon-free, deterministic digest (latest-wins reads
// still collapse on the same participant). The explicit-presenceId
// workaround must keep winning.
func TestPresenceAutoIdIsValidShortId(t *testing.T) {
	eng := loadFullCognitionEngine(t)
	fn := mustLoadFn(t, eng, "mutationUpdateParticipantPresence")
	require.NotNil(t, fn.MutationTemplate)

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-1",
		Role:   auth.RoleWriter,
	})
	const participantID = "v1:cognition:participant:abc123def456"

	// Auto-derived id (no presenceId passed): must be colon-free.
	auto, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"participantId": participantID,
		"spaceId":       "v1:cognition:space:room1",
		"state":         "thinking",
		"label":         "Thinking…",
	})
	require.NoError(t, err)
	require.NotEmpty(t, auto.ID)
	require.NotContains(t, auto.ID, ":",
		"auto-derived presence id must not contain colons (invalid shortId): got %q", auto.ID)
	require.True(t, strings.HasPrefix(auto.ID, "presence-"),
		"auto-derived presence id should keep the presence- prefix: got %q", auto.ID)

	// Determinism: same participant -> same id (latest-wins reads).
	auto2, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"participantId": participantID,
		"spaceId":       "v1:cognition:space:room1",
		"state":         "responding",
		"label":         "Responding…",
	})
	require.NoError(t, err)
	require.Equal(t, auto.ID, auto2.ID, "auto-derived presence id must be deterministic per participant")

	// Explicit-presenceId workaround still wins.
	explicit, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"presenceId":    "presence-custom-1",
		"participantId": participantID,
		"spaceId":       "v1:cognition:space:room1",
		"state":         "idle",
		"label":         "Idle",
	})
	require.NoError(t, err)
	require.Equal(t, "presence-custom-1", explicit.ID,
		"an explicit presenceId must override the auto-derivation")
}

// queryFilterBody extracts the filter expression from a loaded query's
// ExprSource. Loaded queries store the rewritten procedural form whose body
// is `return shape(concept==X;<filter>, "shapeName"), nil`. We return the
// <filter> portion (between the first `;` after concept== and the trailing
// `, "shape"`) so substring assertions target the predicate, not the
// surrounding doc-comment / args block.
func queryFilterBody(t *testing.T, exprSource string) string {
	t.Helper()
	idx := strings.Index(exprSource, "shape(concept==")
	require.GreaterOrEqual(t, idx, 0, "query ExprSource has no shape(concept== body: %q", exprSource)
	rest := exprSource[idx:]
	semi := strings.Index(rest, ";")
	require.GreaterOrEqual(t, semi, 0, "shape() body has no concept== separator: %q", rest)
	return rest[semi+1:]
}
