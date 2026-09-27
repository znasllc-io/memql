package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE LADDER'S TWO WRITES ON A CONSTRUCT, THROUGH THE REAL ENGINE (epic
// memql#5408, issue #5409).
//
// recordConstructLadder and recordProcedure are the only writers of the
// certification ladder's state, and two of their semantics are easy to break
// with an edit that reads like tidying:
//
//   - promotionApprovalId must be CLEARABLE. The ladder never proposes twice
//     while it is set, so an approval that cannot be cleared blocks every later
//     proposal forever. The empty string has to be written as a value --
//     which `accept` does and `?? ""` would do on every write, including the
//     ones that did not mean to touch it.
//   - A write names exactly what it moves. Everything else survives the
//     read-merge, and distinctBindings is REPLACED whole, never merged, so a
//     reset streak does not keep the last streak's bindings.
//
// And recordProcedure writes source, procedure and preconditions as ONE
// version: a re-lift that learned no preconditions clears the previous ones.
//
// The writes go through langparser.RenderCall, the renderer the integration
// uses, so a hole id with dots in a map key is exercised the way it will be
// written. Postgres-gated: skips when no database is reachable, and
// MEMQL_REQUIRE_DB=1 turns that skip into a failure.

const constructConceptID = "v1:authoring:construct"

// seedLearnedConstruct writes one construct the way the lift does, owned by the
// caller, and returns its stored id.
func seedLearnedConstruct(t *testing.T, eng *MemQLEngine, owner, constructId string) string {
	t.Helper()
	return runMutation(t, rowAuthzCallerCtx(owner), eng, "createAuthoringConstruct", map[string]any{
		"constructId":     constructId,
		"bundleId":        "bundle-" + constructId,
		"kind":            "automation",
		"name":            "procedure_" + constructId,
		"targetNamespace": "procedure",
		"source":          "automation procedure_x { }",
	})
}

// ladderWrite runs one of the two @serverOnly writes the way integrations/
// procedure does: under the owner's borrowed actor, with internal origin
// stamped for this one call.
func ladderWrite(t *testing.T, eng *MemQLEngine, owner, mutation string, args map[string]any) {
	t.Helper()
	call, err := langparser.RenderCall(mutation, args)
	require.NoError(t, err)
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), owner))
	_, err = eng.Execute(ctx, "mutation "+call)
	require.NoError(t, err, "%s must write under the owner's actor and internal origin", mutation)
}

func TestRecordConstructLadderClearsThePromotionApprovalIdAndKeepsWhatItDoesNotName(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	suffix := uniqueSuffix("ladderwrite")
	owner := "ladder-owner-" + suffix
	rowID := seedLearnedConstruct(t, eng, owner, "lc-"+suffix)

	// A shadow streak that has just proposed: bindings keyed by hole id --
	// which carries dots -- and an open approval.
	ladderWrite(t, eng, owner, "recordConstructLadder", map[string]any{
		"constructId":         "lc-" + suffix,
		"ladder":              "shadow",
		"shadowMatches":       5,
		"distinctBindings":    map[string]any{"s0.command.3": []any{"sha256:aa", "sha256:bb"}},
		"promotionApprovalId": "appr-" + suffix,
		"lastReplayAt":        "2026-09-23T10:00:00Z",
		"ladderReason":        "matched the app 5 times across 2 distinct bindings; promotion to canary is proposed",
		"ladderChangedAt":     "2026-09-20T10:00:00Z",
	})
	p := latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Equal(t, "shadow", p["ladder"])
	require.Equal(t, "appr-"+suffix, p["promotionApprovalId"])
	require.EqualValues(t, 5, p["shadowMatches"])
	require.Equal(t, map[string]any{"s0.command.3": []any{"sha256:aa", "sha256:bb"}}, p["distinctBindings"])

	// The approval was rejected: the streak resets and the approval is CLEARED.
	// The empty string is the value being written; the fields this write does
	// not name -- lastReplayAt, ladderChangedAt -- must survive it.
	ladderWrite(t, eng, owner, "recordConstructLadder", map[string]any{
		"constructId":         "lc-" + suffix,
		"ladder":              "shadow",
		"shadowMatches":       0,
		"distinctBindings":    map[string]any{},
		"promotionApprovalId": "",
		"ladderReason":        "promotion rejected; the streak restarts",
	})
	p = latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Contains(t, p, "promotionApprovalId", "the cleared approval id must be WRITTEN as the empty string, not dropped")
	require.Equal(t, "", p["promotionApprovalId"], "promotionApprovalId must be clearable, or the ladder can never propose again")
	require.EqualValues(t, 0, p["shadowMatches"])
	require.Equal(t, map[string]any{}, p["distinctBindings"], "distinctBindings is replaced whole, never merged")
	require.Equal(t, "2026-09-23T10:00:00Z", p["lastReplayAt"], "a field the write did not name must survive the read-merge")
	require.Equal(t, "2026-09-20T10:00:00Z", p["ladderChangedAt"], "a field the write did not name must survive the read-merge")

	// A write that says nothing about the approval leaves it alone -- the
	// half a `?? ""` default would break.
	ladderWrite(t, eng, owner, "recordConstructLadder", map[string]any{
		"constructId":         "lc-" + suffix,
		"ladder":              "shadow",
		"promotionApprovalId": "appr2-" + suffix,
	})
	ladderWrite(t, eng, owner, "recordConstructLadder", map[string]any{
		"constructId":   "lc-" + suffix,
		"ladder":        "shadow",
		"shadowMatches": 1,
	})
	p = latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Equal(t, "appr2-"+suffix, p["promotionApprovalId"], "a write that did not name the approval cleared it")

	// And a person cannot write their own rung: the mutation is server-only.
	call, err := langparser.RenderCall("recordConstructLadder", map[string]any{"constructId": "lc-" + suffix, "ladder": "trusted"})
	require.NoError(t, err)
	_, err = eng.Execute(rowAuthzCallerCtx(owner), "mutation "+call)
	require.ErrorContains(t, err, "server-only")
	require.Equal(t, "shadow", latestPayload(t, ctx, db, constructConceptID, rowID)["ladder"])
}

func TestRecordProcedureWritesSourceProcedureAndPreconditionsAsOneVersion(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	suffix := uniqueSuffix("procedurewrite")
	owner := "procedure-owner-" + suffix
	rowID := seedLearnedConstruct(t, eng, owner, "pc-"+suffix)

	procedure := map[string]any{
		"v":              1,
		"level":          1,
		"title":          "Write the weekly report",
		"steps":          []any{map[string]any{"tool": "exec", "symbol": "s0"}},
		"inputMap":       map[string]any{"s0.command.3": "path"},
		"freeParameters": []any{"s0.command.3"},
	}
	ladderWrite(t, eng, owner, "recordProcedure", map[string]any{
		"constructId":   "pc-" + suffix,
		"source":        "automation procedure_first { }",
		"procedure":     procedure,
		"preconditions": map[string]any{"tools": map[string]any{"node": "22.1.0"}},
		"procedureHash": "sha256:first",
	})
	p := latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Equal(t, "sha256:first", p["procedureHash"])
	require.Equal(t, map[string]any{"tools": map[string]any{"node": "22.1.0"}}, p["preconditions"])
	require.Equal(t, "Write the weekly report", p["procedure"].(map[string]any)["title"])

	// A re-lift that learned NO preconditions: the previous version's must not
	// survive beside a procedure and a hash they no longer belong to.
	ladderWrite(t, eng, owner, "recordProcedure", map[string]any{
		"constructId":   "pc-" + suffix,
		"source":        "automation procedure_second { }",
		"procedure":     procedure,
		"procedureHash": "sha256:second",
	})
	p = latestPayload(t, ctx, db, constructConceptID, rowID)
	require.Equal(t, "sha256:second", p["procedureHash"])
	require.Equal(t, "automation procedure_second { }", p["source"])
	require.Equal(t, map[string]any{}, p["preconditions"],
		"a re-lift with no preconditions must clear the previous version's")
}
