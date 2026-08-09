package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// runnableDoc is the fixture buffer the contract test drives. Line numbers are
// load-bearing (they appear in the expected JSON below), so keep edits to the
// end of the buffer or update the expectation with them.
//
//	line 0: use cognition.concepts.{ participant }
//	line 3: query participant spaceParticipants {
//	line 14: automation sweepStalePlans {
const runnableDoc = `use cognition.concepts.{ participant }

@description("Get space participants")
query participant spaceParticipants {
  args {
    /// The space whose participants to read.
    spaceId  string  @required
    kind     string  @enum("human", "ai")
  }
  filter  spaceId==args.spaceId
  shape   participantFull
}

@trigger(schedule="0 */10 * * * *")
automation sweepStalePlans {
  step decide {
    logic sweepStalePlans ( event )
  }
}
`

// newInitializedHandler builds the wrapped handler in the state a real client
// leaves it in after `initialize`: Sense built, initialization latched.
func newInitializedHandler(t *testing.T) (*customHandler, *server) {
	t.Helper()
	s := newTestServer()
	if _, err := s.initialize(nil, &protocol.InitializeParams{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	h := newCustomHandler(s)
	h.Handler.SetInitialized(true)
	return h, s
}

func openDoc(t *testing.T, s *server, uri, text string) {
	t.Helper()
	if err := s.didOpen(noopCtx(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, Text: text},
	}); err != nil {
		t.Fatalf("didOpen: %v", err)
	}
}

// callRunnable drives the wrapper exactly as glsp's server does: a raw
// JSON-RPC params payload on a glsp.Context carrying the custom method name.
func callRunnable(t *testing.T, h *customHandler, rawParams string) (any, bool, bool, error) {
	t.Helper()
	return h.Handle(&glsp.Context{
		Method: methodRunnableConstructs,
		Params: json.RawMessage(rawParams),
		Notify: func(string, any) {},
	})
}

// THE CONTRACT TEST. The TypeScript consumer is written against exactly this
// JSON, field for field, so the assertion is on the marshalled bytes rather
// than on the Go structs -- a renamed tag, a dropped `omitempty`, or a nil
// slice serialising as `null` all have to fail here.
func TestRunnableConstructs_HandleProducesContractJSON(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/queries.memql"
	openDoc(t, s, uri, runnableDoc)

	res, validMethod, validParams, err := callRunnable(t, h, `{"textDocument":{"uri":"`+uri+`"}}`)
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

	const want = `{"constructs":[` +
		`{"kind":"query","name":"spaceParticipants","concept":"participant",` +
		`"signatureRange":{"start":{"line":3,"character":0},"end":{"line":3,"character":35}},` +
		`"args":[` +
		`{"name":"spaceId","type":"string","required":true,"description":"The space whose participants to read."},` +
		`{"name":"kind","type":"string","required":false,"enum":["human","ai"]}` +
		`]},` +
		`{"kind":"automation","name":"sweepStalePlans",` +
		`"signatureRange":{"start":{"line":14,"character":0},"end":{"line":14,"character":26}},` +
		`"args":[],"trigger":{"schedule":"0 */10 * * * *"}}` +
		`]}`
	if string(got) != want {
		t.Errorf("result JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

// `constructs` and `args` are arrays the consumer iterates directly; a `null`
// in either position would throw on the TypeScript side.
func TestRunnableConstructs_EmptyResultsSerialiseAsArrays(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/empty.memql"
	openDoc(t, s, uri, "// nothing runnable here\n")

	res, _, _, err := callRunnable(t, h, `{"textDocument":{"uri":"`+uri+`"}}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `{"constructs":[]}` {
		t.Errorf("empty result = %s; want {\"constructs\":[]}", got)
	}

	// And a runnable construct with no declared args carries [] too.
	const noArgs = "@description(\"Every space\")\nquery space allSpaces {\n  filter  isActiveRecord\n  shape   spaceCard\n}\n"
	const uri2 = "file:///w/noargs.memql"
	openDoc(t, s, uri2, noArgs)
	res2, _, _, err := callRunnable(t, h, `{"textDocument":{"uri":"`+uri2+`"}}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got2, err := json.Marshal(res2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(got2), `"args":[]`) {
		t.Errorf("no-args construct = %s; want an empty args array, never null", got2)
	}
}

// The editor asks about files the server has never been told about (a CodeLens
// refresh racing a close, a URI from another workspace). That is an empty
// answer, not a JSON-RPC error.
func TestRunnableConstructs_UnknownURIReturnsEmptyResult(t *testing.T) {
	h, _ := newInitializedHandler(t)

	res, validMethod, validParams, err := callRunnable(t, h, `{"textDocument":{"uri":"file:///w/never-opened.memql"}}`)
	if err != nil {
		t.Fatalf("Handle returned an error for an unopened URI: %v", err)
	}
	if !validMethod || !validParams {
		t.Fatalf("validMethod=%v validParams=%v; want true/true", validMethod, validParams)
	}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `{"constructs":[]}` {
		t.Errorf("unknown URI result = %s; want an empty constructs array", got)
	}
}

// A half-typed buffer is the normal state of a document being edited, so it
// must answer empty rather than erroring the request out.
func TestRunnableConstructs_MalformedBufferReturnsEmptyResult(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/broken.memql"
	openDoc(t, s, uri, "@description(\"wip\")\nquery partici")

	res, _, _, err := callRunnable(t, h, `{"textDocument":{"uri":"`+uri+`"}}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := json.Marshal(res)
	if string(got) != `{"constructs":[]}` {
		t.Errorf("malformed buffer result = %s; want an empty constructs array", got)
	}
}

// Sense reports RUNE columns; LSP characters count UTF-16 code units. They
// diverge above U+FFFF, where one rune is a surrogate pair. Converting by
// arithmetic instead of through internal/position would put the CodeLens
// anchor one unit short on a line like this one.
func TestRunnableConstructs_SignatureCharactersAreUTF16(t *testing.T) {
	h, s := newInitializedHandler(t)
	// U+1D465 MATHEMATICAL ITALIC SMALL X is a Unicode letter (so the lexer
	// admits it in an identifier) and a supplementary-plane rune (so it is two
	// UTF-16 code units).
	const doc = "@description(\"Search\")\n" +
		"@handler(type=\"query\", query=\"concept==v1:memql:backend:user\")\n" +
		"tool search\U0001D465Users {\n" +
		"  active  boolean  @description(\"Filter by active status\")\n" +
		"}\n"
	const uri = "file:///w/tools.memql"
	openDoc(t, s, uri, doc)

	res, _, _, err := callRunnable(t, h, `{"textDocument":{"uri":"`+uri+`"}}`)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded runnableConstructsResult
	raw, _ := json.Marshal(res)
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Constructs) != 1 {
		t.Fatalf("constructs = %s; want exactly one tool", raw)
	}
	rng := decoded.Constructs[0].SignatureRange
	// `tool search𝑥Users` is 17 runes and 18 UTF-16 code units.
	if rng.Start.Line != 2 || rng.Start.Character != 0 {
		t.Errorf("signature start = %+v; want line 2, character 0", rng.Start)
	}
	if rng.End.Line != 2 || rng.End.Character != 18 {
		t.Errorf("signature end character = %d; want 18 (a rune count would give 17)", rng.End.Character)
	}
}

// The wrapper exists to add one method without disturbing the rest. Anything
// that is not the custom method must reach the embedded protocol.Handler with
// its four-value contract intact: an unhandled method still reports
// validMethod=false so glsp answers MethodNotFound rather than an error.
func TestCustomHandler_DelegatesEveryOtherMethod(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/queries.memql"
	openDoc(t, s, uri, runnableDoc)

	// A method the server DOES implement round-trips through the embedded
	// handler and answers.
	res, validMethod, validParams, err := h.Handle(&glsp.Context{
		Method: protocol.MethodTextDocumentHover,
		Params: json.RawMessage(`{"textDocument":{"uri":"` + uri + `"},"position":{"line":3,"character":2}}`),
		Notify: func(string, any) {},
	})
	if err != nil {
		t.Fatalf("delegated hover: %v", err)
	}
	if !validMethod || !validParams {
		t.Errorf("delegated hover: validMethod=%v validParams=%v; want true/true (res=%v)", validMethod, validParams, res)
	}

	// A method the server does NOT implement must still report
	// validMethod=false rather than being swallowed by the wrapper.
	_, validMethod, _, err = h.Handle(&glsp.Context{
		Method: protocol.MethodTextDocumentReferences,
		Params: json.RawMessage(`{}`),
		Notify: func(string, any) {},
	})
	if err != nil {
		t.Fatalf("delegated unimplemented method returned an error: %v", err)
	}
	if validMethod {
		t.Error("unimplemented method reported validMethod=true; the wrapper must not claim it")
	}
}

// Malformed params are InvalidParams, not an internal error: validMethod stays
// true and validParams goes false, matching how protocol.Handler reports every
// standard request.
func TestRunnableConstructs_InvalidParamsReportInvalidParams(t *testing.T) {
	h, _ := newInitializedHandler(t)

	_, validMethod, validParams, err := callRunnable(t, h, `{"textDocument":`)
	if err == nil {
		t.Fatal("expected an unmarshal error for malformed params")
	}
	if !validMethod {
		t.Error("validMethod = false; the method IS ours even when its params are bad")
	}
	if validParams {
		t.Error("validParams = true; want false for unparseable params")
	}
}

// Nothing but `initialize` is answerable before initialization -- the same
// precondition protocol.Handler enforces for every standard method.
func TestRunnableConstructs_RejectedBeforeInitialize(t *testing.T) {
	s := newTestServer()
	h := newCustomHandler(s)

	_, validMethod, validParams, err := callRunnable(t, h, `{"textDocument":{"uri":"file:///w/x.memql"}}`)
	if err == nil {
		t.Fatal("expected an error before initialize")
	}
	if !validMethod || !validParams {
		t.Errorf("validMethod=%v validParams=%v; want true/true (the method and params are fine, the server is not ready)", validMethod, validParams)
	}
}

func TestServer_InitializeAdvertisesRunnableConstructsCapability(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ir, ok := res.(protocol.InitializeResult)
	if !ok {
		t.Fatalf("initialize returned %T; want protocol.InitializeResult", res)
	}
	experimental, ok := ir.Capabilities.Experimental.(map[string]any)
	if !ok {
		t.Fatalf("experimental capabilities = %#v; want map[string]any", ir.Capabilities.Experimental)
	}
	if enabled, _ := experimental[capabilityRunnableConstructs].(bool); !enabled {
		t.Errorf("experimental[%q] = %v; want true so the client can feature-detect",
			capabilityRunnableConstructs, experimental[capabilityRunnableConstructs])
	}
}

// lifecycleDoc carries the two states memql#3333 added to the contract, in one
// buffer, so the wire shape of both can be asserted together.
//
//	line 2: tool produceArtifact {
//	line 9: automation disabledBootstrap {
const lifecycleDoc = `@description("Produce a file deliverable")
@handler(type="function", function="produceArtifact")
tool produceArtifact {
  filename     string  @required @description("Name of the file to write")
  ownerUserId  string  @autoInjected @description("Server-stamped owner")
}

@disabled
@trigger(schedule="0 */10 * * * *")
automation disabledBootstrap {
  step decide {
    logic sweepStalePlans ( event )
  }
}
`

// THE CONTRACT TEST for the memql#3333 fields. Asserted on the marshalled
// bytes, like the base contract test: the TypeScript consumer reads
// `autoInjected` and `disabled` by name, so a renamed tag has to fail here.
//
// Both carry `omitempty`, which is itself contract. An enabled construct and a
// plain field emit NOTHING -- the overwhelming majority of the corpus -- and
// TypeScript reads an absent boolean as false. The base contract test above
// locks that absence; this one locks the present case.
func TestRunnableConstructs_LifecycleFieldsProduceContractJSON(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/lifecycle.memql"
	openDoc(t, s, uri, lifecycleDoc)

	res, validMethod, validParams, err := callRunnable(t, h, `{"textDocument":{"uri":"`+uri+`"}}`)
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

	const want = `{"constructs":[` +
		`{"kind":"tool","name":"produceArtifact",` +
		`"signatureRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":20}},` +
		`"args":[` +
		`{"name":"filename","type":"string","required":true,"description":"Name of the file to write"},` +
		`{"name":"ownerUserId","type":"string","required":false,"description":"Server-stamped owner","autoInjected":true}` +
		`]},` +
		`{"kind":"automation","name":"disabledBootstrap",` +
		`"signatureRange":{"start":{"line":9,"character":0},"end":{"line":9,"character":28}},` +
		`"args":[],"disabled":true,"trigger":{"schedule":"0 */10 * * * *"}}` +
		`]}`
	if string(got) != want {
		t.Errorf("result JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}
