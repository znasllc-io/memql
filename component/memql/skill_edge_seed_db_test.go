package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/skills"
)

type skillEdgeTestExecutor struct{ engine *MemQLEngine }

func (e skillEdgeTestExecutor) Execute(ctx context.Context, query string) (any, error) {
	return e.engine.Execute(ctx, query)
}

func TestSkillEdgeSeedsMaterializeCommittedAndSurviveAnotherReplicaSweep(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	replica, _, _ := readMergeTestEngine(t)
	const concept = "v1:skills:skillEdge"
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

func TestSkillEdgeRuntimeWriterStillProposesThenCommits(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	actor := "skill-edge-owner-" + uniqueSuffix("owner")
	writeCtx := auth.ContextWithUserActor(ctx, actor)
	writer := skills.NewWriter(skillEdgeTestExecutor{eng})
	id := uniqueSuffix("runtime-edge")
	edges := []skills.EdgeWrite{{EdgeID: id, From: "regression-testing", To: "software-development", Type: skills.EdgeSpecializes, ProposedBy: "user"}}
	require.NoError(t, writer.Propose(writeCtx, edges))
	payload := latestPayload(t, ctx, db, "v1:skills:skillEdge", "v1:skills:skillEdge:"+id)
	require.Equal(t, "proposed", payload["status"])
	require.Equal(t, actor, payload["ownerUserId"])
	require.NoError(t, writer.Commit(writeCtx, edges))
	payload = latestPayload(t, ctx, db, "v1:skills:skillEdge", "v1:skills:skillEdge:"+id)
	require.Equal(t, "committed", payload["status"])
	require.Equal(t, actor, payload["ownerUserId"])
	_, err := eng.Execute(writeCtx, `createSkillEdge(skillEdgeId="client-forged", fromSkillId="a", toSkillId="b", edgeType="specializes", proposedBy="system", status="committed", ownerUserId="")`)
	require.Error(t, err, "catalog writes must remain server-only")
}
