package main

// training_acceptance_test.go is memql#3759's first acceptance criterion taken
// literally: EVERY STATE, FROM A REAL DOCUMENT AGAINST A REAL CATALOG.
//
// "Real" is doing work in both halves, and the point of a separate file is that
// neither half is a fixture:
//
//   - the document is a checked-in `.memql` file from this repository's own
//     tree, read off disk, not a string literal written to make a test pass;
//   - the catalog is what `MemQLEngine.ConstructCatalog` produces after a real
//     `Init` over the embedded tree -- 1700-odd entries with the ENGINE's own
//     `origin` and the ENGINE's own `sourceHash`.
//
// That second half is what makes this more than a longer unit test. The engine
// cuts a construct's source out with a per-keyword header regexp and a brace
// match; the language server locates the same construct by walking the token
// stream. `trained` is the assertion that those two mechanisms produced the same
// bytes for the same declaration -- which is the one contract risk design §3
// names, and the one whose failure mode is every construct in the tree reading
// as drifted at once. component/memql's corpus parity gate proves the two hash
// functions agree; this proves the STATE MACHINE consuming them agrees, through
// the JSON-RPC handler a client actually calls.
//
// The engine build is DB-free (LoadUnifiedConcepts + New + Init over the default
// registry, the same sequence component/memql's own catalog tests use) and takes
// about a second and a half.

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// realCatalogDocPath is a checked-in file from the engine's own DSL tree, and
// realCatalogDocURI places it in a directory named for its domain, which is what
// the concept-matching rule reads.
//
// `dsl/cognition/queries.memql` is chosen for being ordinary: a dozen-odd
// queries, a file-top `use` block, and comment blocks between declarations --
// the shapes a hand-written fixture tends not to have. Any other domain file
// would do; nothing here depends on which constructs it holds.
const realCatalogDocPath = "../../dsl/cognition/queries.memql"
const realCatalogDocURI = "file:///w/dsl/cognition/queries.memql"

// realEngineCatalog builds a real engine over the embedded tree and returns its
// catalog as the notification payload a client would push.
//
// It mutates process-global state (the default concept registry) exactly as
// component/memql's catalog tests do, and deliberately does NOT restore it: the
// loaded state is the normal one, an empty registry is not, and a cleanup that
// emptied it would leave the process in the more surprising of the two for
// whatever runs next.
func realEngineCatalog(t *testing.T) []catalogConstruct {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts over the embedded tree: %v", err)
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init over the embedded tree: %v", err)
	}

	entries := eng.ConstructCatalog()
	if len(entries) == 0 {
		t.Fatal("the engine's construct catalog is empty; every assertion below would be vacuous")
	}
	out := make([]catalogConstruct, 0, len(entries))
	for _, e := range entries {
		out = append(out, catalogConstruct{
			Name:       e.Name,
			Kind:       e.Kind,
			Origin:     e.Origin,
			SourceHash: e.SourceHash,
		})
	}
	return out
}

// pushRealCatalog marshals a catalog the way the extension does and pushes it.
func pushRealCatalog(t *testing.T, h *customHandler, catalog []catalogConstruct) {
	t.Helper()
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	pushCatalog(t, h, string(raw))
}

// withOrigin returns a copy of the catalog with one construct's origin replaced,
// and fails if the construct was not there to replace -- so a renamed or deleted
// construct in the tree surfaces as "the fixture moved", not as a state change.
//
// This is how `trained` and `drifted` are reached from a repository whose every
// construct is `core`: the entry keeps the ENGINE's name, kind and source hash
// and changes only the tier it is claimed to have arrived on. Fabricating the
// hash instead would make the assertion a tautology about this test's own
// arithmetic.
func withOrigin(t *testing.T, catalog []catalogConstruct, kind, name, origin string) []catalogConstruct {
	t.Helper()
	out := make([]catalogConstruct, len(catalog))
	copy(out, catalog)
	for i := range out {
		if out[i].Kind == kind && out[i].Name == name {
			if out[i].SourceHash == "" {
				t.Fatalf("the real catalog carries no source hash for %s %s, so promoting it "+
					"would test the empty-hash rule rather than the drift rule", kind, name)
			}
			out[i].Origin = origin
			return out
		}
	}
	t.Fatalf("the real catalog has no %s named %q", kind, name)
	return nil
}

// without returns a copy of the catalog with one construct removed.
func without(t *testing.T, catalog []catalogConstruct, kind, name string) []catalogConstruct {
	t.Helper()
	out := make([]catalogConstruct, 0, len(catalog))
	removed := false
	for _, e := range catalog {
		if e.Kind == kind && e.Name == name {
			removed = true
			continue
		}
		out = append(out, e)
	}
	if !removed {
		t.Fatalf("the real catalog has no %s named %q to remove", kind, name)
	}
	return out
}

// firstQueryName returns the name of the first `query` the document declares, so
// the assertions below name a construct the file actually has rather than one
// pinned by a constant that rots the next time the tree is reorganised.
func firstQueryName(t *testing.T, doc string) string {
	t.Helper()
	for _, c := range sense.ConstructHashes(doc) {
		if c.Kind == "query" {
			return c.Name
		}
	}
	t.Fatal("the real document declares no query")
	return ""
}

// insertDocLine returns doc with a `///` line spliced in at the named
// construct's own start offset -- inside its span, so its source hash moves and
// no other construct's does.
//
// ConstructHash.Start is a RUNE offset, which is why the splice goes through a
// []rune rather than through strings.Index: every position this package reports
// counts runes, and slicing a Go string by one would shift after the first
// multi-byte character in the file.
func insertDocLine(t *testing.T, doc, name, line string) string {
	t.Helper()
	for _, c := range sense.ConstructHashes(doc) {
		if c.Name != name {
			continue
		}
		runes := []rune(doc)
		edited := string(runes[:c.Start]) + line + "\n" + string(runes[c.Start:])
		if edited == doc {
			t.Fatalf("splicing into %s changed nothing", name)
		}
		return edited
	}
	t.Fatalf("the real document declares no construct named %q", name)
	return ""
}

// TestTrainingState_AllStatesAgainstTheRealEngineCatalog is the acceptance
// criterion. One real document, one real catalog, every state.
func TestTrainingState_AllStatesAgainstTheRealEngineCatalog(t *testing.T) {
	source, err := os.ReadFile(realCatalogDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", realCatalogDocPath, err)
	}
	doc := string(source)
	catalog := realEngineCatalog(t)
	target := firstQueryName(t, doc)

	// --- seeded -------------------------------------------------------------
	//
	// Every construct in this repository's tree arrived on the engine image, so
	// the whole file reads `seeded` against an untouched catalog. That is also
	// the broadest assertion available here: a hash disagreement between the two
	// slicing mechanisms would NOT show up as `seeded` going wrong -- origin
	// decides the tier first -- so this pass proves matching, and the `trained`
	// case below proves the hashes.
	h, s := newInitializedHandler(t)
	openDoc(t, s, realCatalogDocURI, doc)
	pushRealCatalog(t, h, catalog)

	res := decodeTraining(t, h, realCatalogDocURI)
	if !res.Connected {
		t.Fatal("connected = false with a real catalog pushed")
	}
	if len(res.Constructs) < 5 {
		t.Fatalf("the real document reported %d constructs; too few for this test to mean anything", len(res.Constructs))
	}
	seeded := 0
	for _, c := range res.Constructs {
		if c.State != trainingStateSeeded {
			t.Errorf("%s %s = %q against the engine's own catalog; want %q -- every construct "+
				"in this repository's tree arrived on the image", c.Kind, c.Name, c.State, trainingStateSeeded)
			continue
		}
		if c.Origin != memql.ConstructOriginCore {
			t.Errorf("%s %s reported origin %q; want %q", c.Kind, c.Name, c.Origin, memql.ConstructOriginCore)
		}
		seeded++
	}
	if seeded == 0 {
		t.Fatal("no construct reported seeded; the loop above asserted nothing")
	}

	// --- edited ---------------------------------------------------------------
	//
	// The same unsaved edit the `drifted` case below makes, against the
	// UNTOUCHED catalog: origin is still `core`, so the mismatch reads as
	// `edited`, not `drifted` -- drift is defined against a promotion, and this
	// construct has none.
	openDoc(t, s, realCatalogDocURI, insertDocLine(t, doc, target, "/// an unsaved edit"))
	pushRealCatalog(t, h, catalog)
	if got := statesByName(decodeTraining(t, h, realCatalogDocURI))[target]; got != trainingStateEdited {
		t.Errorf("query %s = %q after an unsaved edit against an untouched (core) catalog; want %q",
			target, got, trainingStateEdited)
	}
	openDoc(t, s, realCatalogDocURI, doc)

	// --- trained ------------------------------------------------------------
	//
	// The same entry, claimed to have been promoted. Its source hash is still the
	// ENGINE's, computed by the engine's own slicing of the same file, so this
	// passes only if the language server cut the identical bytes out of the
	// buffer. It is the corpus parity gate's guarantee, asserted end to end.
	promoted := withOrigin(t, catalog, memql.ConstructKindQuery, target, memql.ConstructOriginPromoted)
	pushRealCatalog(t, h, promoted)
	if got := statesByName(decodeTraining(t, h, realCatalogDocURI))[target]; got != trainingStateTrained {
		t.Errorf("query %s = %q with the ENGINE's own hash for the ENGINE's own source; want %q -- "+
			"a mismatch here means the two sides sliced the construct differently, which would "+
			"read as \"everything is drifted\" rather than as a hash bug", target, got, trainingStateTrained)
	}

	// --- drifted ------------------------------------------------------------
	//
	// An unsaved edit to the buffer, against the same promoted entry. Nothing on
	// disk changed; the developer is looking at source the cluster does not have.
	//
	// The edit is a `///` doc line spliced at the construct's own start offset,
	// and it has to be a doc line: NormalizeConstructSource strips ordinary `//`
	// comments and collapses whitespace before hashing, so an edit made of either
	// is correctly NOT drift. That is the hash behaving, and a test that used one
	// would be asserting the opposite of what the design says.
	openDoc(t, s, realCatalogDocURI, insertDocLine(t, doc, target, "/// an unsaved edit"))
	if got := statesByName(decodeTraining(t, h, realCatalogDocURI))[target]; got != trainingStateDrifted {
		t.Errorf("query %s = %q after an unsaved edit; want %q", target, got, trainingStateDrifted)
	}
	openDoc(t, s, realCatalogDocURI, doc)

	// --- untrained ----------------------------------------------------------
	//
	// The cluster is connected and its catalog is fully populated; this one
	// construct is simply not in it. That is a claim about the cluster, and it is
	// a true one -- which is exactly what makes the disconnected case's `unknown`
	// necessary rather than pedantic.
	pushRealCatalog(t, h, without(t, catalog, memql.ConstructKindQuery, target))
	if got := statesByName(decodeTraining(t, h, realCatalogDocURI))[target]; got != trainingStateUntrained {
		t.Errorf("query %s = %q with the entry removed from a real catalog; want %q", target, got, trainingStateUntrained)
	}

	// --- unknown ------------------------------------------------------------
	//
	// The same real document and the same real catalog, with the cluster gone.
	// Every state above must become `unknown` -- not `untrained`, which is what
	// the catalog would say if the absence of a cluster were read as an answer.
	pushCatalog(t, h, `null`)
	res = decodeTraining(t, h, realCatalogDocURI)
	if res.Connected {
		t.Error("connected = true after a null push")
	}
	for _, c := range res.Constructs {
		if c.State != trainingStateUnknown {
			t.Errorf("%s %s = %q with no cluster; want %q", c.Kind, c.Name, c.State, trainingStateUnknown)
		}
	}
}

// --- the wire vocabulary, pinned to the client that already ships ------------

// trainingWireNames is every string this feature puts on the wire, mapped to the
// TypeScript constant the extension declares it as.
//
// A GATE RATHER THAN A CONVENTION, because every one of these fails silently.
// `state` is a closed vocabulary crossing as a bare string; an unrecognised
// value degrades to `unknown` on the client and `unknown` renders as nothing at
// all. A renamed method or capability makes the client feature-detect false and
// draw nothing. In every case the surface simply disappears, which is
// indistinguishable from a cluster that is not connected -- so a divergence
// looks exactly like the feature working correctly.
//
// The extension declared these BEFORE this server existed (memql#3761, posted on
// the issue so the engine side could correct them) and has since shipped the
// rendering against them. They are no longer either side's to change alone.
var trainingWireNames = map[string]string{
	methodTrainingState:     "TRAINING_STATE_METHOD",
	capabilityTrainingState: "TRAINING_STATE_CAPABILITY",
	trainingStateUntrained:  "TRAINING_STATES",
	trainingStateDrifted:    "TRAINING_STATES",
	trainingStateTrained:    "TRAINING_STATES",
	trainingStateSeeded:     "TRAINING_STATES",
	trainingStateEdited:     "TRAINING_STATES",
	trainingStateUnknown:    "TRAINING_STATES",
	trainingStateStaged:     "TRAINING_STATES",
}

// trainingWireSources are the extension files those constants are declared in.
const (
	trainingStateModulePath   = "../../editors/vscode/src/state/training.ts"
	clusterCatalogModulePath  = "../../editors/vscode/src/constructs/clusterCatalog.ts"
	clusterCatalogConstantTS  = "CLUSTER_CATALOG_METHOD"
	trainingStatesDeclaration = "TRAINING_STATES"
)

// tsStringLiteral finds `export const NAME = "value"` and returns the value.
var tsStringLiteral = regexp.MustCompile(`export const ([A-Z_]+)\s*=\s*"([^"]*)"`)

// tsStringArray finds `export const NAME = ["a", "b", ...]` and returns the raw
// bracket contents.
var tsStringArray = regexp.MustCompile(`export const ([A-Z_]+)\s*=\s*\[([^\]]*)\]`)

// TestTrainingWireNamesMatchTheExtension pins every wire string this server
// produces to the constant the shipped client compares it against.
//
// It reads the TypeScript as text rather than executing it, which is the same
// trade vscodeimportrule_test.go makes: a textual scan can be fooled by a
// comment, and being fooled costs a two-second edit, where NOT having the gate
// costs a silently blank surface nobody can see is blank.
func TestTrainingWireNamesMatchTheExtension(t *testing.T) {
	declared := map[string]string{}
	states := map[string]bool{}

	for _, path := range []string{trainingStateModulePath, clusterCatalogModulePath} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v -- this gate cannot be skipped by deleting the file it reads", path, err)
		}
		text := string(source)
		for _, m := range tsStringLiteral.FindAllStringSubmatch(text, -1) {
			declared[m[1]] = m[2]
		}
		for _, m := range tsStringArray.FindAllStringSubmatch(text, -1) {
			if m[1] != trainingStatesDeclaration {
				continue
			}
			for _, part := range strings.Split(m[2], ",") {
				if v := strings.Trim(strings.TrimSpace(part), `"`); v != "" {
					states[v] = true
				}
			}
		}
	}

	for value, tsName := range trainingWireNames {
		if tsName == trainingStatesDeclaration {
			if !states[value] {
				t.Errorf("the server emits state %q, which %s does not list -- the client would "+
					"degrade it to \"unknown\" and render nothing, which looks like a disconnected cluster",
					value, trainingStatesDeclaration)
			}
			continue
		}
		if got := declared[tsName]; got != value {
			t.Errorf("%s = %q in the extension; the server uses %q", tsName, got, value)
		}
	}

	// And the reverse: a state the client knows that the server can never emit is
	// dead rendering, which is a smaller problem but the same kind of drift.
	for value := range states {
		if _, emitted := trainingWireNames[value]; !emitted {
			t.Errorf("%s lists %q, which this server never emits", trainingStatesDeclaration, value)
		}
	}

	if got := declared[clusterCatalogConstantTS]; got != methodClusterCatalog {
		t.Errorf("%s = %q in %s; the server listens on %q. A push to the wrong method is "+
			"accepted by nothing and reported to nobody -- a notification has no reply -- so the "+
			"server would hold no catalog and answer \"unknown\" forever",
			clusterCatalogConstantTS, got, clusterCatalogModulePath, methodClusterCatalog)
	}
}
