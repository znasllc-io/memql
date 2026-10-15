package memql

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestReviewedSkillSeedsMaterializeAndClientsCannotForgeBaselines(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	for _, name := range []string{"software-development", "distributed-systems", "regression-testing", "go-backend-engineering"} {
		t.Run(name, func(t *testing.T) {
			original, ok := engine.Seeds().Get(name)
			require.True(t, ok)
			def := *original
			def.Body.fields = maps.Clone(original.Body.fields)
			id := uniqueSuffix("skill-seed-" + name)
			def.Body.fields["id"] = seedValue{kind: seedString, str: id}
			rowID := "v1:skills:skill:" + id
			t.Cleanup(func() {
				_, err := db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("concept = ?", "v1:skills:skill").Where("id IN (?, ?)", rowID, rowID+"-forged").Exec(ctx)
				require.NoError(t, err)
			})
			m := engine.SeedMaterializer()
			require.NoError(t, m.materializeGlobal(ctx, &def))
			payload := latestPayload(t, ctx, db, "v1:skills:skill", rowID)
			expected := buildArgsFromBody(def.Body, def.UseConcept, id, "")
			for _, field := range []string{"instructions", "version", "status", "predefined"} {
				require.True(t, jsonEquivalent(expected[field], payload[field]), "exact baseline field %s", field)
			}
			count, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).Where("id = ?", rowID).Count(ctx)
			require.NoError(t, err)
			require.NoError(t, m.materializeGlobal(ctx, &def))
			after, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).Where("id = ?", rowID).Count(ctx)
			require.NoError(t, err)
			require.Equal(t, count, after, "unchanged baseline must not produce a fresh row on every boot")
			for _, target := range []string{id, id + "-forged"} {
				args := buildArgsFromBody(def.Body, def.UseConcept, target, "")
				args["instructions"] = map[string]any{"body": "An unreviewed replacement."}
				call, err := renderArgsCallList(args)
				require.NoError(t, err)
				_, err = engine.Execute(rowAuthzCallerCtx("skill-seed-user"), "mutation createSkill("+call+")")
				require.ErrorContains(t, err, "server-only")
			}
		})
	}
}
