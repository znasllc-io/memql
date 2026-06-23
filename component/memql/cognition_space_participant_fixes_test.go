package memql

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
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
// was bound to v1:cognition:session, whose required participantId/partitionId
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
		"partitionId":       "v1:cognition:space:room1",
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
		"partitionId":       "v1:cognition:space:room1",
		"state":         "responding",
		"label":         "Responding…",
	})
	require.NoError(t, err)
	require.Equal(t, auto.ID, auto2.ID, "auto-derived presence id must be deterministic per participant")

	// Explicit-presenceId workaround still wins.
	explicit, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"presenceId":    "presence-custom-1",
		"participantId": participantID,
		"partitionId":       "v1:cognition:space:room1",
		"state":         "idle",
		"label":         "Idle",
	})
	require.NoError(t, err)
	require.Equal(t, "presence-custom-1", explicit.ID,
		"an explicit presenceId must override the auto-derivation")
}

// queryFilterBody extracts the filter expression from a loaded query's
// ExprSource. Loaded queries store the rewritten procedural form whose body
// is `return shape(concept==X;<filter>, "shapeName"), nil` -- or, when the
// query declares sort+paginate (5.2 / epic #1964), the directive-wrapped form
// `return shape(paginate(sort(concept==X;<filter>, ...), N), "shapeName"), nil`.
// We anchor on `concept==` (robust to the sort/paginate wrapping) and return
// everything after the first `;` separator, so substring assertions target the
// predicate, not the surrounding doc-comment / args block / directive args.
func queryFilterBody(t *testing.T, exprSource string) string {
	t.Helper()
	idx := strings.Index(exprSource, "concept==")
	require.GreaterOrEqual(t, idx, 0, "query ExprSource has no concept== body: %q", exprSource)
	rest := exprSource[idx:]
	semi := strings.Index(rest, ";")
	require.GreaterOrEqual(t, semi, 0, "query body has no concept== filter separator: %q", rest)
	return rest[semi+1:]
}
