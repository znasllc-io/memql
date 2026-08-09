package main

import (
	"encoding/json"
	"testing"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// importsDoc is the fixture the contract test drives. The block comment is the
// point of it: a line-anchored regex reads the commented-out `use` as an
// import, and the lexer -- which skips /* ... */ outright -- does not.
const importsDoc = `use cognition.concepts.{ participant, space }

/*
Retired -- fold these back in when the shapes land:
use cognition.shapes.{ participantFull }
*/

use common.traits.{ isActiveRecord }

@description("Get space participants")
query participant spaceParticipants {
  filter  isActiveRecord
  shape   participantFull
}
`

// callImports drives the wrapper exactly as glsp's server does: a raw JSON-RPC
// params payload on a glsp.Context carrying the custom method name.
func callImports(t *testing.T, h *customHandler, rawParams string) (any, bool, bool, error) {
	t.Helper()
	return h.Handle(&glsp.Context{
		Method: methodImports,
		Params: json.RawMessage(rawParams),
		Notify: func(string, any) {},
	})
}

// THE CONTRACT TEST. The TypeScript consumer is written against exactly this
// JSON, field for field, so the assertion is on the marshalled bytes rather
// than on the Go structs -- a renamed tag or a nil slice serialising as `null`
// has to fail here.
func TestImports_HandleProducesContractJSON(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/queries.memql"
	openDoc(t, s, uri, importsDoc)

	res, validMethod, validParams, err := callImports(t, h, `{"textDocument":{"uri":"`+uri+`"}}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !validMethod || !validParams {
		t.Fatalf("validMethod=%v validParams=%v; want true/true", validMethod, validParams)
	}

	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	// cognition.shapes is ABSENT: it is inside a block comment. That single
	// omission is the whole reason this request exists.
	const want = `{"imports":[` +
		`{"path":"cognition.concepts","names":["participant","space"]},` +
		`{"path":"common.traits","names":["isActiveRecord"]}` +
		`]}`
	if string(got) != want {
		t.Errorf("result JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

// The import walk traverses files the editor has never opened -- a CLEAN file
// is a legitimate route to a DIRTY one -- so the client supplies the bytes for
// anything the server does not hold. Supplied text WINS over the server's copy:
// the client's view (open-document text, else disk) is the one the bundle is
// assembled from, and analysing a different one would silently answer about a
// file the run is not using.
func TestImports_SuppliedTextWinsOverTheOpenDocument(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/queries.memql"
	openDoc(t, s, uri, importsDoc)

	params := `{"textDocument":{"uri":"` + uri + `"},"text":"use platform.concepts.{ partition }\n"}`
	res, _, _, err := callImports(t, h, params)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"imports":[{"path":"platform.concepts","names":["partition"]}]}`
	if string(got) != want {
		t.Errorf("result = %s; want %s", got, want)
	}
}

// A file the server was never told about is the ORDINARY case for this
// request, not an error: the client passes its text in.
func TestImports_UnopenedURIWithSuppliedTextIsAnswered(t *testing.T) {
	h, _ := newInitializedHandler(t)

	params := `{"textDocument":{"uri":"file:///w/never-opened.memql"},"text":"use common.traits.{ isActiveRecord }\n"}`
	res, _, _, err := callImports(t, h, params)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"imports":[{"path":"common.traits","names":["isActiveRecord"]}]}`
	if string(got) != want {
		t.Errorf("result = %s; want %s", got, want)
	}
}

// An explicit "" is a client saying the file is empty, which imports nothing.
// It must NOT fall through to the server's copy -- that would make a cleared
// buffer report its last-known imports.
func TestImports_ExplicitEmptyTextIsNotAFallback(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/queries.memql"
	openDoc(t, s, uri, importsDoc)

	res, _, _, err := callImports(t, h, `{"textDocument":{"uri":"`+uri+`"},"text":""}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `{"imports":[]}` {
		t.Errorf("result = %s; want an empty import list", got)
	}
}

// `imports` is an array the consumer iterates directly; a `null` there would
// throw on the TypeScript side.
func TestImports_EmptyResultSerialisesAsAnArray(t *testing.T) {
	h, _ := newInitializedHandler(t)

	res, _, _, err := callImports(t, h, `{"textDocument":{"uri":"file:///w/unknown.memql"}}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `{"imports":[]}` {
		t.Errorf("unknown-URI result = %s; want {\"imports\":[]}", got)
	}
}

func TestImports_InvalidParamsReportInvalidParams(t *testing.T) {
	h, _ := newInitializedHandler(t)

	_, validMethod, validParams, err := callImports(t, h, `{"textDocument":`)
	if err == nil {
		t.Fatal("malformed params: want an error")
	}
	if !validMethod {
		t.Error("validMethod = false; want true -- the method exists, the params did not decode")
	}
	if validParams {
		t.Error("validParams = true; want false so glsp reports InvalidParams")
	}
}

func TestImports_RejectedBeforeInitialize(t *testing.T) {
	s := newTestServer()
	h := newCustomHandler(s)

	_, validMethod, validParams, err := callImports(t, h, `{"textDocument":{"uri":"file:///w/q.memql"}}`)
	if err == nil {
		t.Fatal("pre-initialize call: want an error")
	}
	if !validMethod || !validParams {
		t.Errorf("validMethod=%v validParams=%v; want true/true -- the method and params were fine, the server was not ready", validMethod, validParams)
	}
}

func TestServer_InitializeAdvertisesImportsCapability(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	result, ok := res.(protocol.InitializeResult)
	if !ok {
		t.Fatalf("initialize returned %T; want protocol.InitializeResult", res)
	}
	experimental, ok := result.Capabilities.Experimental.(map[string]any)
	if !ok {
		t.Fatalf("experimental capabilities = %T; want map[string]any", result.Capabilities.Experimental)
	}
	if experimental[capabilityImports] != true {
		t.Errorf("experimental[%q] = %v; want true -- a client feature-detects on this", capabilityImports, experimental[capabilityImports])
	}
	// The pre-existing capability must survive alongside it.
	if experimental[capabilityRunnableConstructs] != true {
		t.Errorf("experimental[%q] = %v; want true", capabilityRunnableConstructs, experimental[capabilityRunnableConstructs])
	}
}
