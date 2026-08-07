package memql

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// seed_materializer_rowids_test.go -- memql#3217.
//
// listUserIds calls m.engine.Execute, which returns *ExecuteResult, and hands
// that value to extractRowIds. Before this change extractRowIds' type switch
// handled []any and map[string]any and ended in `default: return nil`, so
// *ExecuteResult fell through and the STARTUP per-user seed sweep enumerated
// zero users on every boot.
//
// FALSIFIABILITY. TestExtractRowIdsReadsWhatEngineExecuteReturns is RED against
// untouched main -- the arm it exercises is the change. The GraphBundle case is
// new-code coverage for the same class one layer down: OutputPayload falls back
// to r.Bundle when a query carries no shape, so a shapeless query would have
// re-entered `default: return nil` for the identical reason.
//
// What these do NOT establish is the sweep working end to end -- that needs a
// live database (the db-tests lane), because turning the sweep on materializes
// per-user seed rows for every pre-existing user on the next boot. These pin
// the extraction seam; the lane pins the behaviour it switches on.

// The arm that is the fix. `*ExecuteResult` is what Execute returns, and it is
// what listUserIds passes in.
func TestExtractRowIdsReadsWhatEngineExecuteReturns(t *testing.T) {
	res := newExecuteResult(nil)
	res.setOutput([]any{
		map[string]any{"id": "v1:identity:user:alice"},
		map[string]any{"id": "v1:identity:user:bob"},
	})

	got := extractRowIds(res)
	if len(got) != 2 || got[0] != "v1:identity:user:alice" || got[1] != "v1:identity:user:bob" {
		t.Fatalf("extractRowIds(*ExecuteResult) = %v, want both user ids.\n\n"+
			"MemQLEngine.Execute returns *ExecuteResult and listUserIds passes it straight in. "+
			"Without an arm for it the type switch falls to `default: return nil`, and the "+
			"startup per-user seed sweep silently materializes nothing -- 'the query matched no "+
			"users' and 'we could not read what the query returned' are the same value here. "+
			"memql#3217.", got)
	}
}

// The same seam one layer down: OutputPayload returns r.Bundle when the query
// projected no shape, so an unshaped query must not re-enter the silent nil.
func TestExtractRowIdsReadsAGraphBundle(t *testing.T) {
	payload, err := structpb.NewStruct(map[string]any{"active": true})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{Id: "v1:identity:user:alice", Concept: "v1:identity:user", Payload: payload},
			{Id: "v1:identity:user:bob", Concept: "v1:identity:user", Payload: payload},
		},
	}
	res := newExecuteResult(bundle)

	got := extractRowIds(res)
	if len(got) != 2 || got[0] != "v1:identity:user:alice" || got[1] != "v1:identity:user:bob" {
		t.Fatalf("extractRowIds(bundle-backed *ExecuteResult) = %v, want both user ids.\n\n"+
			"OutputPayload falls back to r.Bundle when no shape was applied. usersForSeedSweep "+
			"carries a shape today, so this is prophylactic -- but a shapeless query dropping to "+
			"nil here is the SAME silent no-op memql#3217 fixed, and it would look like an empty "+
			"cluster.", got)
	}
}

// A genuinely empty result and an unreadable one must not be distinguished by
// accident: an empty row set is still zero ids, and the arms above must not
// invent entries for rows carrying no id.
func TestExtractRowIdsSkipsRowsWithNoId(t *testing.T) {
	res := newExecuteResult(nil)
	res.setOutput([]any{
		map[string]any{"id": ""},
		map[string]any{"displayName": "no id here"},
		map[string]any{"id": "v1:identity:user:alice"},
		"not a row at all",
	})

	got := extractRowIds(res)
	if len(got) != 1 || got[0] != "v1:identity:user:alice" {
		t.Fatalf("extractRowIds = %v, want only the one row that carries an id", got)
	}
}

// The payload-nested spelling the extractor has always supported, now reachable
// through the wrapper.
func TestExtractRowIdsReadsAPayloadNestedId(t *testing.T) {
	res := newExecuteResult(nil)
	res.setOutput(map[string]any{"nodes": []any{
		map[string]any{"payload": map[string]any{"id": "v1:identity:user:carol"}},
	}})

	got := extractRowIds(res)
	if len(got) != 1 || got[0] != "v1:identity:user:carol" {
		t.Fatalf("extractRowIds = %v, want the payload-nested id", got)
	}
}
