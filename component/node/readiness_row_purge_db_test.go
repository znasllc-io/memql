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

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The statements in readiness_row_purge.go, against real Postgres. Everything
// here is about WHICH rows go, which is the half a fake purger cannot answer:
// the join between a readiness row's bare nodeId and the prefixed
// v1:cluster:node id, the latest-version rule on health, and "every version,
// not only the newest".

// purgeFixture seeds readiness rows and cluster-node rows under a unique
// prefix so parallel lanes on one shared database never see each other's.
type purgeFixture struct {
	db     *bun.DB
	prefix string
	ctx    context.Context
}

func newPurgeFixture(t *testing.T) *purgeFixture {
	t.Helper()
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "readiness row purge", dbtest.DSN(), err)
		return nil
	}
	f := &purgeFixture{db: db, prefix: fmt.Sprintf("purge-%d", time.Now().UnixNano()), ctx: ctx}
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept IN (?)", bun.In([]string{memqlengine.ModuleReadinessConcept, memorynodes.ConceptClusterNode})).
			Where("id LIKE ?", "%"+f.prefix+"%").Exec(ctx)
	})
	return f
}

func (f *purgeFixture) node(id string) string { return f.prefix + "-" + id }

// readiness writes `versions` versions of one module row for one node.
func (f *purgeFixture) readiness(t *testing.T, nodeId, module string, versions int) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Duration(versions) * time.Minute).Truncate(time.Second)
	payload, err := json.Marshal(map[string]any{
		"module": module, "nodeId": nodeId, "nodeType": "agent",
		"state": "configured", "core": true, "reportedAt": at.Format(time.RFC3339),
	})
	require.NoError(t, err)
	rows := make([]memorynodes.MemoryNode, 0, versions)
	for i := 0; i < versions; i++ {
		rows = append(rows, memorynodes.MemoryNode{
			ID: memqlengine.ModuleReadinessConcept + ":" + module + "--" + nodeId,
			// Distinct createdAt per version: "MemoryNodes" is keyed
			// (id, createdAt), which is exactly why a purge that removed only
			// the newest would strand the rest.
			CreatedAt: at.Add(time.Duration(i) * time.Minute), CreatedBy: "system:test",
			Concept: memqlengine.ModuleReadinessConcept, Type: memorynodes.NodeTypeObject,
			Schema: json.RawMessage(`{}`), Payload: payload,
			Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`),
		})
	}
	_, err = f.db.NewInsert().Model(&rows).Exec(f.ctx)
	require.NoError(t, err)
}

// clusterNode writes one version of a cluster-node row per health word, in
// order, so the LAST one given is the latest version.
func (f *purgeFixture) clusterNode(t *testing.T, nodeId string, healths ...string) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	rows := make([]memorynodes.MemoryNode, 0, len(healths))
	for i, h := range healths {
		at := base.Add(time.Duration(i) * time.Minute)
		payload, err := json.Marshal(map[string]any{
			"nodeType": "agent", "address": "agent:50051",
			"health": h, "lastSeen": at.Format(time.RFC3339),
		})
		require.NoError(t, err)
		rows = append(rows, memorynodes.MemoryNode{
			ID: memorynodes.ConceptClusterNode + ":" + nodeId, CreatedAt: at, CreatedBy: "system:test",
			Concept: memorynodes.ConceptClusterNode, Type: memorynodes.NodeTypeObject,
			Schema: json.RawMessage(`{}`), Payload: payload,
			Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`),
		})
	}
	_, err := f.db.NewInsert().Model(&rows).Exec(f.ctx)
	require.NoError(t, err)
}

// versions counts the readiness versions still standing for one node.
func (f *purgeFixture) versions(t *testing.T, nodeId string) int {
	t.Helper()
	n, err := f.db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", memqlengine.ModuleReadinessConcept).
		Where("payload->>'nodeId' = ?", nodeId).Count(f.ctx)
	require.NoError(t, err)
	return n
}

func TestPurgingANodeRemovesEveryVersionAndOnlyThatNodes(t *testing.T) {
	f := newPurgeFixture(t)
	if f == nil {
		return
	}
	gone, staying := f.node("gone"), f.node("staying")
	f.readiness(t, gone, "ai", 4)
	f.readiness(t, gone, "storage", 3)
	f.readiness(t, staying, "ai", 4)

	versions, err := sqlReadinessRowPurger{db: f.db}.purgeForNode(f.ctx, gone)
	require.NoError(t, err)
	require.Equal(t, int64(7), versions, "every version of both modules")

	require.Equal(t, 0, f.versions(t, gone), "the retired node keeps nothing")
	require.Equal(t, 4, f.versions(t, staying), "another node's rows are untouched")
}

func TestTheSweepTakesAStoppedNodeAndLeavesOneThatCameBack(t *testing.T) {
	f := newPurgeFixture(t)
	if f == nil {
		return
	}
	stopped, returned, live := f.node("stopped"), f.node("returned"), f.node("live")

	f.readiness(t, stopped, "ai", 3)
	f.clusterNode(t, stopped, "healthy", "stopped")

	// The pod name came back -- a Deployment that recreated it identically.
	// The LATEST version is what decides, so reading any version would purge
	// the rows of a node that is serving right now.
	f.readiness(t, returned, "ai", 3)
	f.clusterNode(t, returned, "healthy", "stopped", "connecting", "healthy")

	f.readiness(t, live, "ai", 3)
	f.clusterNode(t, live, "healthy")

	// A node with rows and NO cluster-node row at all: a pod between its first
	// readiness pass and its registration. Left alone on purpose.
	unregistered := f.node("unregistered")
	f.readiness(t, unregistered, "ai", 3)

	_, err := sqlReadinessRowPurger{db: f.db}.purgeForStoppedNodes(f.ctx)
	require.NoError(t, err)

	require.Equal(t, 0, f.versions(t, stopped), "a stopped node's rows go")
	require.Equal(t, 3, f.versions(t, returned), "a node that came back keeps its rows")
	require.Equal(t, 3, f.versions(t, live), "a live node keeps its rows")
	require.Equal(t, 3, f.versions(t, unregistered), "a node with no cluster row is never guessed at")
}

func TestASecondSweepFindsNothingLeftToDo(t *testing.T) {
	f := newPurgeFixture(t)
	if f == nil {
		return
	}
	stopped := f.node("idempotent")
	f.readiness(t, stopped, "ai", 2)
	f.clusterNode(t, stopped, "stopped")

	p := sqlReadinessRowPurger{db: f.db}
	first, err := p.purgeForStoppedNodes(f.ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, first, int64(2))

	// Every replica may hold the lease over time and the sweep is unlocked, so
	// a second pass over the same window has to be free rather than an error.
	second, err := p.purgeForStoppedNodes(f.ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), second)
}
