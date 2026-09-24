package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// THE MESH REPORT, THROUGH THE REAL ENGINE (memql#5338, D7).
//
// Three facts the OS surface depends on, each one a way the report could be
// silently wrong with every unit test green:
//
//  1. the node's heartbeat WRITES it -- the rendered call parses, the
//     mutation accepts the argument, the concept admits the field;
//  2. a PEER-TRANSITION write, which carries no report, KEEPS it -- another
//     node recording this one as offline must not blank what this node said
//     it heard (an absent arg is omitted from the read-merge);
//  3. the read the OS makes, clusterNodes, RETURNS it -- a concept field is
//     not a readable field until some read projects it.
//
// The row is created THROUGH THE ENGINE, as registerNode creates it. A row
// inserted straight into the table is not one update() can read back, so a
// fixture built that way sends every write down persistHealthTransition's
// self-heal insert -- which takes no report and blanks deploymentId and
// labels -- and the test would be measuring the fallback, not the heartbeat.
// deploymentId and labels surviving the heartbeat is the proof it did not.
func TestTheMeshReportIsWrittenKeptAndRead(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "mesh report", dbtest.DSN(), err)
		return
	}
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	engine, err := memqlengine.New(db)
	require.NoError(t, err)
	engine.Logger = testLogger()
	require.NoError(t, engine.Init(memorynodes.DefaultRegistry()))

	nodeID := fmt.Sprintf("mesh-report-%d", time.Now().UnixNano())
	rowID := "v1:cluster:node:" + nodeID
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", "v1:cluster:node").Where("id = ?", rowID).Exec(ctx)
	})
	now := time.Now().UTC().Truncate(time.Second)
	_, err = engine.Execute(systemWriteContext(ctx), fmt.Sprintf(
		`createNode(id: %s, nodeType: "edge", address: "10.0.0.7:50062", health: "healthy", deploymentId: "deploy-7", labels: {"zone": "a"})`,
		langparser.QuoteString(nodeID)))
	require.NoError(t, err)

	// 1. THE HEARTBEAT WRITES IT.
	lifecycle := NewNodeLifecycle()
	require.NoError(t, lifecycle.MarkReady())
	writer := NewSelfStatusWriter(&Identity{ID: nodeID, Type: NodeTypeEdge, Address: "10.0.0.7:50062"}, lifecycle, engine, testLogger())
	writer.now = func() time.Time { return now }
	writer.SetMeshReporter(func() MeshReport {
		return MeshReport{
			Receives: true,
			Since:    now.Add(-10 * time.Minute),
			Links:    []MeshLink{{Node: "bff-a", Type: "bff", Via: "dialed"}},
			Heard:    1204, Duplicates: 310, Originated: 7, Relayed: 3,
			LastHeardAt: now.Add(-2 * time.Second),
		}
	})
	require.NoError(t, writer.refresh(ctx))
	var merged map[string]any
	require.NoError(t, json.Unmarshal(latestRow(t, ctx, db, rowID).Payload, &merged))
	require.Equal(t, "deploy-7", merged["deploymentId"],
		"the heartbeat did not merge onto registerNode's row: the self-heal insert ran and blanked it")
	require.Equal(t, map[string]any{"zone": "a"}, merged["labels"])
	mesh := latestMesh(t, ctx, db, rowID)
	require.Equal(t, float64(1204), mesh["heard"])
	require.Equal(t, true, mesh["receives"])
	require.Equal(t, now.Add(-2*time.Second).Format(time.RFC3339), mesh["lastHeardAt"])
	links, _ := mesh["links"].([]any)
	require.Len(t, links, 1)
	require.Equal(t, "dialed", links[0].(map[string]any)["via"])

	// 2. A PEER-TRANSITION WRITE KEEPS IT.
	peerWriter := NewNodeStatusWriter(&engineExecutorAdapter{engine: engine}, nil, testLogger())
	call, err := buildUpdateNodeHealthCall(nodeID, "edge", "10.0.0.7:50062", "degraded", now.Add(time.Second).Format(time.RFC3339), nil)
	require.NoError(t, err)
	require.NoError(t, peerWriter.persistHealthTransition(systemWriteContext(ctx), call, nodeID, "edge", "10.0.0.7:50062", "degraded", now.Format(time.RFC3339)))
	var stored map[string]any
	require.NoError(t, json.Unmarshal(latestRow(t, ctx, db, rowID).Payload, &stored))
	require.Equal(t, "degraded", stored["health"], "negative control: the peer write must have landed, or keeping the report proves nothing")
	require.Equal(t, float64(1204), latestMesh(t, ctx, db, rowID)["heard"], "a write that carries no report blanked the node's own")

	// 3. THE READ THE OS MAKES RETURNS IT.
	result, err := engine.Execute(systemWriteContext(ctx), "query clusterNodes()")
	require.NoError(t, err)
	require.NotNil(t, result.Bundle)
	found := false
	for _, n := range result.Bundle.Nodes {
		if n.GetId() != rowID {
			continue
		}
		found = true
		got := n.GetPayload().AsMap()["mesh"]
		require.NotNil(t, got, "clusterNodes does not project the mesh field")
		require.Equal(t, float64(1204), got.(map[string]any)["heard"])
	}
	require.True(t, found, "clusterNodes did not return the node at all")
}

func latestRow(t *testing.T, ctx context.Context, db *bun.DB, rowID string) memorynodes.MemoryNode {
	t.Helper()
	var latest memorynodes.MemoryNode
	require.NoError(t, db.NewSelect().Model(&latest).Where("id = ?", rowID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx))
	return latest
}

func latestMesh(t *testing.T, ctx context.Context, db *bun.DB, rowID string) map[string]any {
	t.Helper()
	var stored map[string]any
	require.NoError(t, json.Unmarshal(latestRow(t, ctx, db, rowID).Payload, &stored))
	mesh, ok := stored["mesh"].(map[string]any)
	require.True(t, ok, "no mesh object on the latest row: %v", stored)
	return mesh
}

// systemWriteContext attributes a test write the way the status writers do.
func systemWriteContext(ctx context.Context) context.Context {
	return auth.ContextWithUserActor(ctx, "system:node-status-writer")
}
