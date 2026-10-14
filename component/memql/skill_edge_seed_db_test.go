package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
)

func TestSkillEdgeSeedsMaterializeCommittedAndSurviveAnotherReplicaSweep(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	replica, _, _ := readMergeTestEngine(t)
	const concept = "v1:skills:skillEdge"
	// Populate the linked catalog too: real graph reads include these nodes,
	// even though the seed freshness check asks for one edge by exact id.
	for _, name := range []string{"software-development", "distributed-systems", "regression-testing", "go-backend-engineering"} {
		def, ok := eng.Seeds().Get(name)
		require.True(t, ok)
		require.NoError(t, eng.SeedMaterializer().materializeGlobal(ctx, def))
	}
	for name, from := range map[string]string{
		"distributed-systems-specialty": "distributed-systems",
		"regression-testing-specialty":  "regression-testing",
		"go-development-specialty":      "go-backend-engineering",
	} {
		t.Run(name, func(t *testing.T) {
			def, ok := eng.Seeds().Get(name)
			require.True(t, ok)
			id := concept + ":" + name
			require.NoError(t, eng.SeedMaterializer().materializeGlobal(ctx, def))
			payload := latestPayload(t, ctx, db, concept, id)
			require.Equal(t, "v1:skills:skill:"+from, payload["fromSkillId"])
			require.Equal(t, "v1:skills:skill:software-development", payload["toSkillId"])
			require.Equal(t, "specializes", payload["edgeType"])
			require.Equal(t, "committed", payload["status"])
			require.Equal(t, "", payload["ownerUserId"])
			require.Equal(t, "system", payload["proposedBy"])
			graph, err := replica.Execute(ContextWithFreshRead(systemActorContext(ctx)), "concept=="+concept+" && row.id=="+dslStringLiteral(id))
			require.NoError(t, err)
			require.Greater(t, len(graph.Bundle.GetNodes()), 1, "exercise a graph with linked skills, not just an isolated edge")
			versions := func() int {
				var n int
				require.NoError(t, db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, id).Scan(ctx, &n))
				return n
			}
			before := versions()
			// A separate engine shares the database, with no originating cache or materializer state.
			require.NoError(t, replica.SeedMaterializer().materializeGlobal(context.Background(), def))
			require.Equal(t, before, versions(), "unchanged catalog seeds must not append versions")
		})
	}
}

func TestSkillEdgeRuntimeMutationsStillProposeThenCommit(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	actor := "skill-edge-owner-" + uniqueSuffix("owner")
	writeCtx := auth.ContextWithUserActor(ctx, actor)
	id := uniqueSuffix("runtime-edge")
	call, err := parser.RenderCall("proposeSkillEdge", map[string]any{"edgeId": id, "fromSkillId": "regression-testing", "toSkillId": "software-development", "edgeType": "specializes", "proposedBy": "user"})
	require.NoError(t, err)
	_, err = eng.Execute(auth.ContextWithInternalOrigin(writeCtx), "mutation "+call)
	require.NoError(t, err)
	payload := latestPayload(t, ctx, db, "v1:skills:skillEdge", "v1:skills:skillEdge:"+id)
	require.Equal(t, "proposed", payload["status"])
	require.Equal(t, actor, payload["ownerUserId"])
	call, err = parser.RenderCall("commitSkillEdge", map[string]any{"edgeId": id})
	require.NoError(t, err)
	_, err = eng.Execute(auth.ContextWithInternalOrigin(writeCtx), "mutation "+call)
	require.NoError(t, err)
	payload = latestPayload(t, ctx, db, "v1:skills:skillEdge", "v1:skills:skillEdge:"+id)
	require.Equal(t, "committed", payload["status"])
	require.Equal(t, actor, payload["ownerUserId"])
	_, err = eng.Execute(writeCtx, `createSkillEdge(skillEdgeId="client-forged", fromSkillId="a", toSkillId="b", edgeType="specializes", proposedBy="system", status="committed", ownerUserId="")`)
	require.Error(t, err, "catalog writes must remain server-only")
}
