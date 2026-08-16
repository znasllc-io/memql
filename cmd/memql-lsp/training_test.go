package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// trainingDoc is the fixture buffer the contract test drives: one construct per
// state, in a file whose directory names the `cognition` domain. Line numbers
// are load-bearing (they appear in the expected JSON below), so keep edits to
// the end of the buffer or update the expectation with them.
//
//	line  3: query participant trainedQuery
//	line  9: query participant driftedQuery
//	line 15: query participant untrainedQuery
//	line 21: mutate participant seededMutation
//	line 28: action deployCluster
const trainingDoc = `use cognition.concepts.{ participant }

@description("Promoted, and the local source matches -- trained.")
query participant trainedQuery {
  filter  isActiveRecord
  shape   participantFull
}

@description("Promoted, and the local source has moved on -- drifted.")
query participant driftedQuery {
  filter  isActiveRecord
  shape   participantFull
}

@description("The cluster has never heard of it -- untrained.")
query participant untrainedQuery {
  filter  isActiveRecord
  shape   participantFull
}

@description("Loaded from disk at boot -- seeded.")
mutate participant seededMutation {
  insert {
    id: args.id
  }
}

@description("A kind the catalog structurally cannot carry -- unknown.")
action deployCluster {
  param cluster string
}
`

// trainingDocURI sits under a `cognition` directory because that directory IS
// the domain, and the domain is what a concept declaration is matched on.
const trainingDocURI = "file:///w/dsl/cognition/queries.memql"

// pushCatalog delivers a catalog the way the extension does: the
// `memql/clusterCatalog` NOTIFICATION, driven through the wrapper exactly as
// glsp's server drives it. Raw JSON rather than a marshalled struct so the
// PARAMS field names are pinned too -- `catalog` and its four entry fields are
// half of the contract.
//
// Pass `null` for the disconnected case; pass `[]` for a connected cluster that
// has loaded nothing. NOT calling this at all is a third, distinct case -- a
// server nobody has told about a cluster -- and it is the state every server
// starts in.
func pushCatalog(t *testing.T, h *customHandler, catalogJSON string) {
	t.Helper()
	res, validMethod, validParams, err := h.Handle(&glsp.Context{
		Method: methodClusterCatalog,
		Params: json.RawMessage(fmt.Sprintf(`{"catalog":%s}`, catalogJSON)),
		Notify: func(string, any) {},
	})
	if err != nil {
		t.Fatalf("clusterCatalog notification: %v", err)
	}
	if !validMethod || !validParams {
		t.Fatalf("clusterCatalog: validMethod=%v validParams=%v; want true/true", validMethod, validParams)
	}
	// A notification has no reply, so the wrapper must produce no result to be
	// sent as one. jsonrpc2 would discard it either way; returning nil is what
	// makes that discard uninteresting rather than lucky.
	if res != nil {
		t.Fatalf("clusterCatalog notification returned a result %#v; a notification has no reply", res)
	}
}

// callTraining drives the request through the wrapper the way glsp's server
// does.
func callTraining(t *testing.T, h *customHandler, uri string) (any, bool, bool, error) {
	t.Helper()
	return h.Handle(&glsp.Context{
		Method: methodTrainingState,
		Params: json.RawMessage(fmt.Sprintf(`{"textDocument":{"uri":%q}}`, uri)),
		Notify: func(string, any) {},
	})
}

// localHashOf is the hash the language server computes for one construct of a
// fixture document -- which is exactly what a cluster that had been promoted
// THAT source would report back. Computing it rather than hard-coding hex keeps
// the fixture editable and keeps this test from re-implementing the hash.
func localHashOf(t *testing.T, doc, name string) string {
	t.Helper()
	for _, c := range sense.ConstructHashes(doc) {
		if c.Name == name {
			return c.SourceHash
		}
	}
	t.Fatalf("the fixture document declares no construct named %q", name)
	return ""
}

// decodeTraining runs the request and returns the decoded result.
func decodeTraining(t *testing.T, h *customHandler, uri string) trainingStateResult {
	t.Helper()
	res, validMethod, validParams, err := callTraining(t, h, uri)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !validMethod || !validParams {
		t.Fatalf("validMethod=%v validParams=%v; want true/true", validMethod, validParams)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var decoded trainingStateResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return decoded
}

// statesByName projects a result to a name -> state map, which is what most of
// these tests actually assert on.
func statesByName(res trainingStateResult) map[string]string {
	out := map[string]string{}
	for _, c := range res.Constructs {
		out[c.Name] = c.State
	}
	return out
}

// fullTrainingCatalog is the catalog that produces four of the five states over
// trainingDoc. `untrainedQuery` is deliberately absent, and `deployCluster` is
// absent because ListConstructs structurally cannot carry an `action`.
func fullTrainingCatalog(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`[
		{"name":"trainedQuery","kind":"query","origin":"promoted","sourceHash":%q},
		{"name":"driftedQuery","kind":"query","origin":"promoted","sourceHash":"0000000000000000000000000000000000000000000000000000000000000000"},
		{"name":"seededMutation","kind":"mutation","origin":"core","sourceHash":%q}
	]`,
		localHashOf(t, trainingDoc, "trainedQuery"),
		localHashOf(t, trainingDoc, "seededMutation"),
	)
}

// THE CONTRACT TEST. The TypeScript consumer is written against exactly this
// JSON, field for field, so the assertion is on the marshalled bytes rather than
// on the Go structs -- a renamed tag, a dropped `omitempty`, or a nil slice
// serialising as `null` all have to fail here.
//
// It is also half of the issue's first acceptance criterion: all five states
// from one real document. The other half -- against a REAL catalog rather than a
// hand-written one -- is training_acceptance_test.go.
func TestTrainingState_HandleProducesContractJSON(t *testing.T) {
	h, s := newInitializedHandler(t)
	openDoc(t, s, trainingDocURI, trainingDoc)
	pushCatalog(t, h, fullTrainingCatalog(t))

	res, validMethod, validParams, err := callTraining(t, h, trainingDocURI)
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

	const want = `{"connected":true,"constructs":[` +
		`{"kind":"query","name":"trainedQuery","concept":"participant",` +
		`"signatureRange":{"start":{"line":3,"character":0},"end":{"line":3,"character":30}},` +
		`"state":"trained","origin":"promoted"},` +
		`{"kind":"query","name":"driftedQuery","concept":"participant",` +
		`"signatureRange":{"start":{"line":9,"character":0},"end":{"line":9,"character":30}},` +
		`"state":"drifted","origin":"promoted"},` +
		`{"kind":"query","name":"untrainedQuery","concept":"participant",` +
		`"signatureRange":{"start":{"line":15,"character":0},"end":{"line":15,"character":32}},` +
		`"state":"untrained"},` +
		`{"kind":"mutate","name":"seededMutation","concept":"participant",` +
		`"signatureRange":{"start":{"line":21,"character":0},"end":{"line":21,"character":33}},` +
		`"state":"seeded","origin":"core"},` +
		`{"kind":"action","name":"deployCluster",` +
		`"signatureRange":{"start":{"line":28,"character":0},"end":{"line":28,"character":20}},` +
		`"state":"unknown"}` +
		`]}`
	if string(got) != want {
		t.Errorf("result JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

// THE RULE THE ISSUE CALLS THE MOST IMPORTANT ONE: not connected means
// `unknown`, never `untrained`.
//
// Asserted over a document whose constructs the cluster demonstrably DOES know
// -- the contract test above proves the same buffer produces trained / drifted /
// seeded against a catalog -- so a regression that reported them as untrained
// would be claiming something the previous test disproves.
//
// All three spellings of "no cluster" are exercised, and the FIRST is the one
// that matters most now that the catalog is pushed rather than passed: a server
// nobody has told about a cluster is the state every server starts in and the
// state it stays in for the whole life of a client that never pushes. The other
// two are a client that has no cluster, and a client that HAD one -- the case
// where a wrong answer would be worst, because a catalog was there a moment ago
// and something has to actively forget it.
func TestTrainingState_DisconnectedIsUnknownForEveryConstruct(t *testing.T) {
	for _, tc := range []struct {
		name string
		push func(t *testing.T, h *customHandler)
	}{
		{"never pushed", func(*testing.T, *customHandler) {}},
		{"explicit null", func(t *testing.T, h *customHandler) { pushCatalog(t, h, `null`) }},
		{"disconnected after a real catalog", func(t *testing.T, h *customHandler) {
			pushCatalog(t, h, fullTrainingCatalog(t))
			pushCatalog(t, h, `null`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := newInitializedHandler(t)
			openDoc(t, s, trainingDocURI, trainingDoc)
			tc.push(t, h)

			res := decodeTraining(t, h, trainingDocURI)
			if res.Connected {
				t.Errorf("connected = true with no catalog held; want false")
			}
			if len(res.Constructs) == 0 {
				t.Fatal("no constructs reported: this assertion would pass vacuously")
			}
			for _, c := range res.Constructs {
				if c.State != trainingStateUnknown {
					t.Errorf("%s %s = %q; want %q -- an absent cluster must never read as "+
						"\"the cluster does not have this\"", c.Kind, c.Name, c.State, trainingStateUnknown)
				}
				if c.Origin != "" {
					t.Errorf("%s %s carries origin %q with no catalog to have read it from",
						c.Kind, c.Name, c.Origin)
				}
			}
		})
	}
}

// The mirror of the rule above, and the half that proves the distinction is real
// rather than a state nothing ever reaches: a CONNECTED cluster that has loaded
// nothing reports untrained, not unknown.
//
// Without this, a handler that answered `unknown` unconditionally would pass the
// disconnected test perfectly.
func TestTrainingState_EmptyCatalogIsUntrainedNotUnknown(t *testing.T) {
	h, s := newInitializedHandler(t)
	openDoc(t, s, trainingDocURI, trainingDoc)
	pushCatalog(t, h, `[]`)

	res := decodeTraining(t, h, trainingDocURI)
	if !res.Connected {
		t.Errorf("connected = false for an empty (but pushed) catalog; want true -- " +
			"an empty catalog is a cluster that has loaded nothing, not the absence of a cluster")
	}
	states := statesByName(res)
	for _, name := range []string{"trainedQuery", "driftedQuery", "untrainedQuery", "seededMutation"} {
		if states[name] != trainingStateUntrained {
			t.Errorf("%s = %q against an empty catalog; want %q", name, states[name], trainingStateUntrained)
		}
	}
	// The `action` stays unknown even here: the catalog being empty says nothing
	// about a kind it could never have carried.
	if states["deployCluster"] != trainingStateUnknown {
		t.Errorf("action deployCluster = %q against an empty catalog; want %q",
			states["deployCluster"], trainingStateUnknown)
	}
}

// A push REPLACES; it never merges. The case that decides it is a demote: a
// construct the cluster had and no longer has must stop reading as trained, and
// a merge has no way to express "gone" -- an absent entry is indistinguishable
// from one the client simply did not mention this time.
func TestClusterCatalogPush_ReplacesWholesale(t *testing.T) {
	h, s := newInitializedHandler(t)
	openDoc(t, s, trainingDocURI, trainingDoc)

	pushCatalog(t, h, fullTrainingCatalog(t))
	if got := statesByName(decodeTraining(t, h, trainingDocURI))["trainedQuery"]; got != trainingStateTrained {
		t.Fatalf("trainedQuery = %q after the first push; want %q", got, trainingStateTrained)
	}

	// The same cluster, one construct demoted: the entry is simply gone.
	pushCatalog(t, h, fmt.Sprintf(
		`[{"name":"seededMutation","kind":"mutation","origin":"core","sourceHash":%q}]`,
		localHashOf(t, trainingDoc, "seededMutation")))

	states := statesByName(decodeTraining(t, h, trainingDocURI))
	if states["trainedQuery"] != trainingStateUntrained {
		t.Errorf("trainedQuery = %q after a push that omits it; want %q -- a push replaces, "+
			"so an omitted construct is one the cluster no longer has", states["trainedQuery"], trainingStateUntrained)
	}
	if states["seededMutation"] != trainingStateSeeded {
		t.Errorf("seededMutation = %q after the second push; want %q -- the entries that "+
			"WERE pushed must survive, or this test would pass for a push that did nothing",
			states["seededMutation"], trainingStateSeeded)
	}
}

// The distinction between "no cluster" and "an empty catalog" is STRUCTURAL, not
// a branch somebody has to remember. This pins the two zero values that make it
// so: an unset clusterCatalog is disconnected, and an unset catalogAnswer is
// silence.
//
// A nil map and an empty map are indistinguishable at a lookup, so a model that
// carried only the map would have collapsed the two cases with no call site able
// to recover them. These assertions fail the moment someone "simplifies" the
// `connected` field away or reorders the catalogAnswer constants so that
// catalogAbsent lands on zero.
func TestTrainingState_ZeroValuesFailSafeToUnknown(t *testing.T) {
	var unset clusterCatalog
	declared := sense.ConstructHash{Kind: "query", Name: "anything", SourceHash: "deadbeef"}

	entry, answer := unset.find(declared, "cognition")
	if answer != catalogSilent {
		t.Errorf("the zero-value clusterCatalog answered %v; want catalogSilent -- "+
			"an unset catalog is a disconnected one", answer)
	}
	if got := trainingStateFor(declared, entry, answer); got != trainingStateUnknown {
		t.Errorf("the zero-value clusterCatalog produced state %q; want %q", got, trainingStateUnknown)
	}

	var unsetAnswer catalogAnswer
	if unsetAnswer != catalogSilent {
		t.Errorf("the zero value of catalogAnswer is %v; want catalogSilent -- "+
			"forgetting to set it must land on \"no answer\", never on \"not there\"", unsetAnswer)
	}
	if got := trainingStateFor(declared, catalogConstruct{}, unsetAnswer); got != trainingStateUnknown {
		t.Errorf("the zero-value catalogAnswer produced state %q; want %q", got, trainingStateUnknown)
	}

	// And the server's own field: a server that has been initialized but never
	// pushed a catalog holds that zero value, which is the state the editor
	// finds it in on the very first CodeLens refresh after start.
	if snap := newTestServer().clusterCatalogSnapshot(); snap.connected {
		t.Error("a freshly-constructed server reports connected; want false -- " +
			"nobody has told it about a cluster yet")
	}
}

// The issue's third acceptance criterion: a construct edited in the buffer but
// NOT saved reports drifted. The buffer is what the developer is looking at, so
// it is what gets hashed -- reading the file off disk would report `trained` for
// source that is no longer on the screen.
func TestTrainingState_UnsavedBufferEditReportsDrifted(t *testing.T) {
	h, s := newInitializedHandler(t)
	openDoc(t, s, trainingDocURI, trainingDoc)
	pushCatalog(t, h, fullTrainingCatalog(t))

	if got := statesByName(decodeTraining(t, h, trainingDocURI))["trainedQuery"]; got != trainingStateTrained {
		t.Fatalf("trainedQuery = %q before the edit; want %q", got, trainingStateTrained)
	}

	// A whole-document didChange, with no didSave after it: exactly the state a
	// buffer is in between a keystroke and Ctrl-S.
	edited := strings.Replace(trainingDoc,
		"query participant trainedQuery {\n  filter  isActiveRecord",
		"query participant trainedQuery {\n  filter  isActiveRecord && status==\"open\"", 1)
	if edited == trainingDoc {
		t.Fatal("the fixture edit matched nothing: this test would assert on an unchanged buffer")
	}
	if err := s.didChange(noopCtx(), &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: trainingDocURI}},
		ContentChanges: []any{protocol.TextDocumentContentChangeEventWhole{Text: edited}},
	}); err != nil {
		t.Fatalf("didChange: %v", err)
	}

	states := statesByName(decodeTraining(t, h, trainingDocURI))
	if states["trainedQuery"] != trainingStateDrifted {
		t.Errorf("trainedQuery = %q after an unsaved edit; want %q -- the buffer is what "+
			"the developer is looking at", states["trainedQuery"], trainingStateDrifted)
	}
	// The untouched constructs must not move with it.
	if states["seededMutation"] != trainingStateSeeded {
		t.Errorf("seededMutation = %q after an unrelated edit; want %q",
			states["seededMutation"], trainingStateSeeded)
	}
}

// `action` and `capability` are authored kinds ListConstructs deliberately does
// not carry -- component/actions loads them, no engine registry holds them -- so
// their absence from the catalog is evidence of nothing.
//
// This is the disconnected bug in miniature, and it is the one that would ship:
// the cluster IS connected and the catalog IS populated, so every guard about
// connections passes, and the construct is still reported against a question
// nobody asked.
func TestTrainingState_KindsTheCatalogCannotCarryAreUnknown(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/dsl/deploy/actions.memql"
	const doc = `@description("Roll the cluster forward.")
action rolloutCluster {
  param version string
}

@description("The script behind it.")
capability rolloutScript {
  script "scripts/deploy/rollout.sh"
}

@description("An ordinary query, to prove the catalog is being consulted at all.")
query participant knownQuery {
  filter  isActiveRecord
}
`
	openDoc(t, s, uri, doc)

	// A populated catalog, so nothing here can be blamed on a missing cluster.
	pushCatalog(t, h, fmt.Sprintf(`[{"name":"knownQuery","kind":"query","origin":"core","sourceHash":%q}]`,
		localHashOf(t, doc, "knownQuery")))
	states := statesByName(decodeTraining(t, h, uri))

	for _, name := range []string{"rolloutCluster", "rolloutScript"} {
		if states[name] != trainingStateUnknown {
			t.Errorf("%s = %q; want %q -- the catalog carries no action or capability, "+
				"so its silence about one is not absence", name, states[name], trainingStateUnknown)
		}
	}
	if states["knownQuery"] != trainingStateSeeded {
		t.Errorf("knownQuery = %q; want %q -- if this is unknown too, the catalog was "+
			"never consulted and the assertions above prove nothing", states["knownQuery"], trainingStateSeeded)
	}
}

// A concept's registry key is its canonical id, and two domains may each declare
// a `state`. So a concept is matched on (domain, short name), with the domain
// taken from the directory the file sits in -- the same derivation the engine's
// own source index makes.
func TestTrainingState_ConceptsMatchOnTheirDomain(t *testing.T) {
	const doc = `@description("A state machine's state.")
concept state {
  name  string  @required
}
`
	// One catalog, two documents: same short name, different directories.
	catalog := `[{"name":"v1:cognition:state","kind":"concept","origin":"core","sourceHash":"%s"}]`

	for _, tc := range []struct {
		uri  string
		want string
		why  string
	}{
		{"file:///w/dsl/cognition/concepts.memql", trainingStateSeeded,
			"the catalog's v1:cognition:state IS this declaration"},
		{"file:///w/dsl/knowledge/concepts.memql", trainingStateUntrained,
			"v1:knowledge:state is a different concept that happens to share a short name"},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			h, s := newInitializedHandler(t)
			openDoc(t, s, tc.uri, doc)
			pushCatalog(t, h, fmt.Sprintf(catalog, localHashOf(t, doc, "state")))
			if got := statesByName(decodeTraining(t, h, tc.uri))["state"]; got != tc.want {
				t.Errorf("concept state in %s = %q; want %q -- %s", tc.uri, got, tc.want, tc.why)
			}
		})
	}
}

// An EMPTY hash is a missing answer, not a value, and two missing answers must
// not compare equal as though both were known. sense.ConstructSourceHash says so
// explicitly; this pins that the state machine honours it.
//
// The case is real rather than hypothetical: a promotion marker that is a name
// RESERVATION carries no source, so the catalog stamps an empty hash for it, and
// a plain `==` against a construct the language server also could not hash would
// report `trained` -- the most confident possible way to be wrong about a
// construct nobody can see.
func TestTrainingState_EmptyHashNeverReadsAsTrained(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/dsl/cognition/queries.memql"
	const doc = "query participant reservedName {\n  filter  isActiveRecord\n}\n"
	openDoc(t, s, uri, doc)

	pushCatalog(t, h, `[{"name":"reservedName","kind":"query","origin":"promoted","sourceHash":""}]`)
	if got := statesByName(decodeTraining(t, h, uri))["reservedName"]; got != trainingStateDrifted {
		t.Errorf("a promoted construct with an EMPTY catalog hash = %q; want %q", got, trainingStateDrifted)
	}

	if hashesAgree("", "") {
		t.Error("hashesAgree(\"\", \"\") = true; two missing answers must never compare equal")
	}
	if !hashesAgree("abc", "abc") {
		t.Error("hashesAgree(\"abc\", \"abc\") = false; two known, equal hashes must agree")
	}
}

// ORIGIN DECIDES THE TIER BEFORE THE HASH DECIDES ANYTHING: a seeded construct
// stays seeded when the local source differs, because drift is defined against a
// promotion and a seeded construct has none.
//
// The consequence this protects is an affordance, not a label. `drifted` carries
// a Promote lens; the engine refuses to let a promoted construct shadow a core
// name, so a locally-edited core construct rendered as drifted would offer an
// action that can only ever be refused. `seeded` offers none, which is the
// honest answer: what this needs is a rollout.
func TestTrainingState_SeededStaysSeededWhenTheLocalSourceDiffers(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/dsl/cognition/queries.memql"
	const doc = "query participant coreQuery {\n  filter  isActiveRecord && status==\"edited\"\n}\n"
	openDoc(t, s, uri, doc)

	for _, origin := range []string{"core", "bundle"} {
		pushCatalog(t, h, fmt.Sprintf(
			`[{"name":"coreQuery","kind":"query","origin":%q,"sourceHash":"1111111111111111111111111111111111111111111111111111111111111111"}]`,
			origin))
		if got := statesByName(decodeTraining(t, h, uri))["coreQuery"]; got != trainingStateSeeded {
			t.Errorf("an edited %s construct = %q; want %q", origin, got, trainingStateSeeded)
		}
	}

	// An unrecognised origin degrades the same way, on purpose: `seeded` is the
	// one state that offers no action, so a client bug costs a missing
	// affordance rather than a refused one.
	pushCatalog(t, h, `[{"name":"coreQuery","kind":"query","origin":"","sourceHash":"1111"}]`)
	if got := statesByName(decodeTraining(t, h, uri))["coreQuery"]; got != trainingStateSeeded {
		t.Errorf("a catalog entry with an unrecognised origin = %q; want %q", got, trainingStateSeeded)
	}
}

// The editor asks about files the server has never been told about (a CodeLens
// refresh racing a close, a URI from another workspace). That is an empty
// answer, not a JSON-RPC error -- and `connected` still reports honestly,
// because whether a cluster is attached has nothing to do with which document
// was asked about.
func TestTrainingState_UnknownURIReturnsEmptyConstructs(t *testing.T) {
	h, _ := newInitializedHandler(t)
	pushCatalog(t, h, `[]`)

	res, validMethod, validParams, err := callTraining(t, h, "file:///w/never-opened.memql")
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
	if string(got) != `{"connected":true,"constructs":[]}` {
		t.Errorf("unknown URI result = %s; want an empty constructs array with connected reported", got)
	}
}

// A half-typed buffer is the normal state of a document being edited. It must
// answer rather than error, and the constructs it CAN locate must still get a
// state -- withholding one would report a construct as untrained, which is a
// claim about the cluster rather than about the buffer.
func TestTrainingState_MalformedBufferStillAnswers(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/dsl/cognition/queries.memql"
	openDoc(t, s, uri, "query participant finished {\n  filter  isActiveRecord\n}\n\n@description(\"wip\")\nquery partici")
	pushCatalog(t, h, `[]`)

	states := statesByName(decodeTraining(t, h, uri))
	if states["finished"] != trainingStateUntrained {
		t.Errorf("the complete construct in a half-typed buffer = %q; want %q",
			states["finished"], trainingStateUntrained)
	}
}

// The client feature-detects on this rather than calling blind and handling
// MethodNotFound. One key covers both the request and the catalog push: they are
// one feature, and a pair of flags that must agree is a pair that can disagree.
func TestTrainingState_CapabilityIsAdvertised(t *testing.T) {
	s := newTestServer()
	res, err := s.initialize(nil, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ir, ok := res.(protocol.InitializeResult)
	if !ok {
		t.Fatalf("initialize result = %T; want protocol.InitializeResult", res)
	}
	experimental, ok := ir.Capabilities.Experimental.(map[string]any)
	if !ok {
		t.Fatalf("experimental capabilities = %#v; want map[string]any", ir.Capabilities.Experimental)
	}
	if enabled, _ := experimental[capabilityTrainingState].(bool); !enabled {
		t.Errorf("experimental[%q] = %v; want true", capabilityTrainingState, experimental[capabilityTrainingState])
	}
}

// catalogFieldWireNames maps each field of the pushed catalog entry to its
// spelling in ListConstructs' ConstructInfo, which is where every one of them
// comes from: the extension holds the cluster stream, reads the catalog, and
// forwards it over the notification. A rename on the proto side with no
// corresponding rename here does not fail loudly -- the field simply arrives
// empty, and an empty origin reads as `seeded` while an empty hash reads as
// `drifted`. Both are plausible-looking states, which is what makes the silence
// dangerous.
//
// The names are identical on both sides on purpose; the map exists so a rename
// has somewhere to be recorded rather than to paper one over.
var catalogFieldWireNames = map[string]string{
	"name":       "name",
	"kind":       "kind",
	"origin":     "origin",
	"sourceHash": "sourceHash",
}

// catalogFieldsDeliberatelyNotRead is every remaining ConstructInfo field, each
// listed because it has no bearing on training state -- not because nobody got
// to it. Naming them is the point: a NEW proto field lands in neither map and
// fails the test below, which turns "should training state read this?" into a
// decision somebody makes rather than one that gets made by omission.
//
// memql#3805 added `trigger`, and it landed here -- as this comment predicted,
// and more to the point as a DECISION rather than by omission, which is the
// whole point of the two maps. Left as the worked example: the next field gets
// the same treatment.
var catalogFieldsDeliberatelyNotRead = map[string]string{
	"trigger":      "an automation's trigger decides its RUN form -- which payload modes, which concept the row picker browses -- and says nothing about whether the construct is trained, drifted or seeded (memql#3805)",
	"namespace":    "a concept's domain is already inside its canonical name, and memql.CatalogConstructKey reads it out; accepting a second answer would let the two contradict each other",
	"originPath":   "which file a construct came from does not change what state it is in",
	"description":  "presentation, and the buffer has its own copy anyway",
	"runnable":     "picks the Run affordance, not the training one",
	"args":         "the argument form; memql/runnableConstructs owns it for an open document",
	"boundConcept": "part of the construct's signature, which the buffer already states",
	"source":       "carried only for a construct with no file; the hash is what a comparison needs",
	"owner": "which USER staged a construct (memql#3928). Training state answers what the CALLER's " +
		"relationship to a construct is, and `origin` already carries that -- `staged` means staged " +
		"for whoever asked, because the catalog a caller receives is scoped to them. `owner` " +
		"distinguishes two staged constructs FROM EACH OTHER, which only the cluster-owner catalog " +
		"view has more than one of, and which no state rule reads. Reading it here would need the " +
		"server to know the connected user's id, which it has no other reason to hold",
}

// TestCatalogConstructMatchesConstructInfo pins the pushed catalog entry to the
// message it is copied from. See catalogFieldWireNames for what goes wrong
// without it.
//
// jsonName and protoJSONName live in constructs_parity_test.go, which pins the
// other half of this boundary (the argument form) for the same reason.
func TestCatalogConstructMatchesConstructInfo(t *testing.T) {
	lsp := reflect.TypeOf(catalogConstruct{})
	wire := reflect.TypeOf(memqlv1.ConstructInfo{})

	lspByJSON := map[string]reflect.StructField{}
	for i := 0; i < lsp.NumField(); i++ {
		f := lsp.Field(i)
		lspByJSON[jsonName(f)] = f
	}
	wireByJSON := map[string]reflect.StructField{}
	for i := 0; i < wire.NumField(); i++ {
		f := wire.Field(i)
		if name := protoJSONName(f); name != "" {
			wireByJSON[name] = f
		}
	}

	for lspName, wireName := range catalogFieldWireNames {
		lf, ok := lspByJSON[lspName]
		if !ok {
			t.Errorf("the catalog push no longer accepts %q; either it was renamed (update catalogFieldWireNames) or it stopped reading a field it needs", lspName)
			continue
		}
		wf, ok := wireByJSON[wireName]
		if !ok {
			t.Errorf("ConstructInfo no longer carries %q, which the catalog push reads as %q -- the field would arrive empty, and an empty origin reads as seeded while an empty hash reads as drifted", wireName, lspName)
			continue
		}
		if lf.Type != wf.Type {
			t.Errorf("field %q: the catalog push has %s, ConstructInfo has %s", lspName, lf.Type, wf.Type)
		}
	}

	for name := range lspByJSON {
		if _, mapped := catalogFieldWireNames[name]; !mapped {
			t.Errorf("the catalog push gained field %q with no ConstructInfo counterpart to fill it", name)
		}
	}
	for name := range wireByJSON {
		if _, read := catalogFieldWireNames[name]; read {
			continue
		}
		if _, declined := catalogFieldsDeliberatelyNotRead[name]; declined {
			continue
		}
		t.Errorf("ConstructInfo gained field %q. Training state either needs it (add it to catalogConstruct and catalogFieldWireNames) or does not (record why in catalogFieldsDeliberatelyNotRead) -- but the choice must be made rather than defaulted", name)
	}
}

// Nothing is answerable before initialization, and the failure must be reported
// the way glsp's other custom methods report it -- an error with validMethod and
// validParams both true, which becomes an ordinary JSON-RPC error rather than
// MethodNotFound or InvalidParams.
//
// The catalog push holds the same precondition. Its error goes to a log rather
// than to a client (a notification has no reply), which is precisely why it must
// not be allowed to half-apply: refusing outright leaves the zero value, and the
// zero value is `unknown`.
func TestTrainingState_UninitializedServerErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{"request", methodTrainingState, `{"textDocument":{"uri":"file:///w/x.memql"}}`},
		{"catalog push", methodClusterCatalog, `{"catalog":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCustomHandler(newTestServer())
			_, validMethod, validParams, err := h.Handle(&glsp.Context{
				Method: tc.method,
				Params: json.RawMessage(tc.params),
				Notify: func(string, any) {},
			})
			if err == nil {
				t.Fatal("Handle succeeded before initialization; want an error")
			}
			if !validMethod || !validParams {
				t.Errorf("validMethod=%v validParams=%v; want true/true -- an uninitialized server "+
					"must not report this as MethodNotFound or InvalidParams", validMethod, validParams)
			}
		})
	}
}

// Malformed params are InvalidParams, not a silently-applied partial catalog.
// The push cannot report failure to the client, so the only safe outcome is that
// nothing changed -- and a catalog that never arrived is `unknown`, which is the
// state the whole design falls back to.
func TestClusterCatalogPush_MalformedParamsLeaveTheCatalogUntouched(t *testing.T) {
	h, s := newInitializedHandler(t)
	openDoc(t, s, trainingDocURI, trainingDoc)
	pushCatalog(t, h, fullTrainingCatalog(t))

	_, validMethod, validParams, err := h.Handle(&glsp.Context{
		Method: methodClusterCatalog,
		Params: json.RawMessage(`{"catalog":"not a list"}`),
		Notify: func(string, any) {},
	})
	if err == nil {
		t.Fatal("a malformed catalog push succeeded; want a decode error")
	}
	if !validMethod {
		t.Error("validMethod=false for a known method")
	}
	if validParams {
		t.Error("validParams=true for params that did not decode; want false (InvalidParams)")
	}

	if got := statesByName(decodeTraining(t, h, trainingDocURI))["trainedQuery"]; got != trainingStateTrained {
		t.Errorf("trainedQuery = %q after a REJECTED push; want %q -- a push that did not "+
			"decode must not disturb the catalog that did", got, trainingStateTrained)
	}
}
