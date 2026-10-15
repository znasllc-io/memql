package memql

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestGraphReadsResolveTheStoredSchemaVersion(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	id := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id": uniqueSuffix("schema6059"), "nodeType": "bff", "address": "schema-test:50051", "health": "healthy",
	})
	var row memorynodes.MemoryNode
	require.NoError(t, db.NewSelect().Model(&row).Where("id=?", id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx))
	// The historical schema is deliberately unlike the currently registered
	// cluster:node schema. Another process has only this persisted catalog.
	document := fmt.Sprintf(`{"$id":%q,"type":"object","description":"original historical definition"}`, id)
	var fingerprint string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT encode(sha256(convert_to(?::jsonb::text,'UTF8')),'hex')`, document).Scan(&fingerprint))
	_, err := db.ExecContext(ctx, `INSERT INTO memory_node_schemas (fingerprint,document) VALUES (?,?::jsonb) ON CONFLICT DO NOTHING`, fingerprint, document)
	require.NoError(t, err)
	defer db.ExecContext(ctx, `DELETE FROM memory_node_schemas WHERE fingerprint=?`, fingerprint)
	ref, err := json.Marshal(map[string]string{"$id": id, "$ref": "memql-schema:" + fingerprint})
	require.NoError(t, err)
	row.Schema = ref
	bundle, err := eng.buildGraphBundle(ctx, []memorynodes.MemoryNode{row}, 0, nil)
	require.NoError(t, err)
	require.Len(t, bundle.Nodes, 1)
	require.Equal(t, "original historical definition", bundle.Nodes[0].Schema.Fields["description"].GetStringValue())
	require.NotContains(t, bundle.Nodes[0].Schema.Fields, "$ref")
}
