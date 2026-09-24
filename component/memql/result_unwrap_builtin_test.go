package memql

import (
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A TOP-LEVEL BUILTIN'S ANSWER REACHES MaterializeRows.
//
// Go code that asks a builtin through the engine (`builtin storefrontProbe(...)`,
// `builtin groupEnsureForAccount(...)`, `builtin recall(...)`) is handed an
// ExecuteResult whose output is the builtin branch's node set -- a
// map[string]memorynodes.MemoryNode keyed by id (engine.go, nodesToMap). The
// unwrapper knew slices and loose maps, JSON-roundtripped the node map into an
// object keyed by node ids, found none of its envelope keys there, and answered
// NOTHING. So every such reader saw an empty answer from a builtin that had
// answered: integrations/sitepreview reported "the store probe answered
// nothing" on every probe, and the account-group sweep counted no group as
// created.
//
// Driven through the real engine (parse, RegisterIntegration, dispatch, row
// gate), not a hand-built result, because a fake that returns the tidier []any
// is exactly what hid this.
func TestMaterializeRowsReadsATopLevelBuiltinsAnswer(t *testing.T) {
	const envelope = "memql:probe"
	e := builtinProbeEngine(t, []memorynodes.MemoryNode{
		rowOf(t, envelope, envelope+":b", map[string]any{"created": true, "catalog": map[string]any{"ok": true}}),
		rowOf(t, envelope, envelope+":a", map[string]any{"created": false}),
	})

	res, err := e.Execute(callerCtx("user-a"), "builtin "+rowAuthzBuiltinProbeCall)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The control: this measures the builtin branch's node map, not some
	// other output type a later refactor might hand back.
	if _, ok := res.OutputPayload().(map[string]memorynodes.MemoryNode); !ok {
		t.Fatalf("the builtin branch's output is %T, not the node map this test is about", res.OutputPayload())
	}

	rows := MaterializeRows(res)
	if len(rows) != 2 {
		t.Fatalf("MaterializeRows read %d rows from a builtin that answered 2: %#v", len(rows), rows)
	}
	// Ordered by id: the set is a map, so the only stable order is one the
	// reader imposes, and a caller taking rows[0] must get the same row twice.
	if rows[0]["id"] != envelope+":a" || rows[1]["id"] != envelope+":b" {
		t.Fatalf("rows not ordered by id: %v, %v", rows[0]["id"], rows[1]["id"])
	}
	// The answer is the PAYLOAD, read flat -- the way every in-process caller
	// reads it (row["created"], row["catalog"]).
	if rows[1]["created"] != true {
		t.Errorf("payload field not on the row: %#v", rows[1])
	}
	if catalog, ok := rows[1]["catalog"].(map[string]any); !ok || catalog["ok"] != true {
		t.Errorf("nested payload object not on the row: %#v", rows[1]["catalog"])
	}
	if rows[1]["concept"] != envelope {
		t.Errorf("the node's concept column is gone: %#v", rows[1])
	}
	if p, ok := rows[1]["payload"].(map[string]any); !ok || p["created"] != true {
		t.Errorf("the nested payload a []MemoryNode row carries is gone: %#v", rows[1]["payload"])
	}

	// Zero nodes is zero rows, not one empty row.
	empty := builtinProbeEngine(t, nil)
	res, err = empty.Execute(callerCtx("user-a"), "builtin "+rowAuthzBuiltinProbeCall)
	if err != nil {
		t.Fatalf("execute (empty): %v", err)
	}
	if rows := MaterializeRows(res); len(rows) != 0 {
		t.Fatalf("an empty builtin answer materialized as %d rows: %#v", len(rows), rows)
	}
}
