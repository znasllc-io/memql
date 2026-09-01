package library

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/protobuf/types/known/structpb"
)

// stubEngine implements memql.IntegrationEngineAccess for the edit-path
// tests. It models an append-only documentVersion store + a single
// backing generatedOutput, so a sequence of editDocument / restore
// calls exercises the real handler logic (read history -> compute next
// version -> append) end to end without a database.
type stubEngine struct {
	// doc is the backing generatedOutput, keyed by id. Updated in place
	// by updateGeneratedOutputContent so the test can assert the
	// latest-pointer reflects the newest content.
	doc map[string]map[string]any
	// versions is the append-only history: every appended documentVersion
	// row, in append order. Nothing is ever removed -- the test asserts
	// the count grows by one per edit.
	versions []map[string]any
	// artifacts is the v1:library:artifact index-row store, keyed by an
	// arbitrary id the stub assigns (createArtifact's real id derivation
	// is a DSL expression this stub does not need to reproduce -- lookups
	// go by sourceConceptRef or by the caller-supplied artifactId, never
	// by re-deriving the id in Go). createArtifact REPLACES the row at
	// that id wholesale -- modelling the real engine's bare `insert{}`
	// semantics (D3: a version carries only the fields THIS call names,
	// nothing carried over from the prior version) -- which is exactly
	// the shape the label-loss regression test below needs to be able to
	// fail against.
	artifacts map[string]map[string]any
	// calls records every mutation/query string for assertion.
	calls []string
	// failArtifactSourceRefLookup, when set, makes
	// libraryArtifactBySourceConceptRef return an error -- simulating a
	// genuine read failure so a test can prove touchArtifact skips its
	// re-stamp entirely rather than writing labels: [] over a real row.
	failArtifactSourceRefLookup bool
}

func newStubEngine() *stubEngine {
	return &stubEngine{doc: map[string]map[string]any{}, artifacts: map[string]map[string]any{}}
}

func (s *stubEngine) seedDocument(d map[string]any) {
	s.doc[d["id"].(string)] = d
}

// seedArtifact seeds the library index row backing a document, keyed by
// its own "id" field.
func (s *stubEngine) seedArtifact(a map[string]any) {
	s.artifacts[a["id"].(string)] = a
}

// stubArtifactId is the stub's own arbitrary bookkeeping key for an
// artifact row, deterministic in sourceConceptRef so a seeded row and a
// later createArtifact re-version for the SAME source ref land on the
// SAME stub key (re-versioning it in place) -- mirroring the one
// index-row-per-source-ref property the real engine's hash-derived id
// provides, without needing to reproduce that hash. Used both by the
// createArtifact case below and by any test that seeds a row it expects
// a later re-version to land on.
func stubArtifactId(sourceRef string) string {
	return "artifact:" + sourceRef
}

// artifactFullShapeFields lists the fields the REAL artifactFull shape
// projects (dsl/library/shapes.memql). DERIVED, not hand-copied: TestMain
// (dsl_fidelity_test.go) parses the actual shape block out of the DSL
// source once before any test runs, so this list cannot silently drift
// from the shape it claims to model (memql#4298). A shaped read must
// return ONLY these keys, never the full underlying row, or the stub
// cannot catch a Go call site that assumes an UNprojected field survived
// a shaped read -- which is exactly the shape of the real bug the review
// found: artifactFull omitted producedByWorkerId / producedByWorkerName,
// loadArtifactUnderOwner read them as "" through the shaped
// libraryArtifactById, and writeArtifactLabels wrote that "" back,
// permanently destroying machine attribution on the first label change.
var artifactFullShapeFields []string

// projectShapeFields models a MemQL shaped read: a FRESH map containing
// only the given fields' values from row (when present), never the row
// itself. A real shape returns a template-fresh map per row; leaking
// extra row keys through a "shaped" stub read would let production code
// get away with reading a field the real shape does not actually project.
func projectShapeFields(row map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := row[f]; ok {
			out[f] = v
		}
	}
	return out
}

// artifactEnumValues mirrors createArtifact's enum arg declarations
// (dsl/library/mutations.memql). DERIVED, not hand-copied: TestMain
// (dsl_fidelity_test.go) parses the actual args block out of the DSL
// source once before any test runs, so a value added to (or dropped from)
// a declared enum can never silently go unmodeled here again (memql#4298:
// this map's hand-maintained "source" entry was missing "user_created" on
// main -- the exact value the portal's create path writes -- and nothing
// failed, because no test happened to push that value through the stub).
// lens/kind/source are required(!), so an existing row already carries a
// valid value for them; format, scope and validationStatus are OPTIONAL,
// and a real row's stored value is routinely blank -- none of the five
// promotion automations pass scope, and a record-lens row has no format
// at all.
var artifactEnumValues map[string][]string

// validateCreateArtifactEnums models the one piece of the real engine's
// arg validation (validateArgsField) this suite needs: a call naming an
// enum argument with a value outside its declared set is REFUSED --
// exactly the "argument \"scope\": value  is not in enum [...]" error the
// review reproduced against the real engine. An OMITTED key is not
// validated at all: that omitted/present-and-blank distinction is the
// entire point of writeArtifactLabels's conditional-enum-omission fix,
// and a stub that could not tell the two apart could not prove the fix
// correct.
func validateCreateArtifactEnums(args map[string]any) error {
	for field, allowed := range artifactEnumValues {
		v, present := args[field]
		if !present {
			continue
		}
		s, _ := v.(string)
		ok := false
		for _, a := range allowed {
			if s == a {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("argument %q: value %s is not in enum %v", field, s, allowed)
		}
	}
	return nil
}

// argRe extracts `key: value` pairs from a `name({...})` call. Values
// are quoted strings or bare numbers.
var argRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*):\s*(?:"((?:[^"\\]|\\.)*)"|(-?\d+))`)

// listArgRe extracts `key: [ ... ]` array-literal arguments -- the shape a
// []string arg (labels) renders as. argRe above only matches scalar
// (quoted-string / bare-number) values, so an array value needs its own
// pass. Captured as []any (not []string) so it round-trips through
// structpb.NewStruct (bundleOf) identically to how the real engine's
// AsMap() decodes a JSONB string array.
var listArgRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*):\s*\[([^\[\]]*)\]`)
var quotedItemRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// boolArgRe extracts `key: true` / `key: false` bare-boolean arguments
// (e.g. live: %t in writeArtifactLabels / touchArtifact) -- argRe's value
// alternation only covers quoted strings and bare numbers, so an unquoted
// true/false falls through it entirely and the arg silently vanishes.
var boolArgRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*):\s*(true|false)\b`)

func parseCall(q string) (name string, args map[string]any) {
	args = map[string]any{}
	if i := strings.IndexByte(q, '('); i > 0 {
		name = strings.TrimSpace(q[:i])
		// Strip any leading kind prefix (`query `/`mutation `) so the
		// bare construct name keys the switch below.
		if fields := strings.Fields(name); len(fields) > 0 {
			name = fields[len(fields)-1]
		}
	}
	for _, m := range listArgRe.FindAllStringSubmatch(q, -1) {
		key := m[1]
		items := []any{}
		for _, im := range quotedItemRe.FindAllStringSubmatch(m[2], -1) {
			items = append(items, unescape(im[1]))
		}
		args[key] = items
	}
	for _, m := range boolArgRe.FindAllStringSubmatch(q, -1) {
		key := m[1]
		if _, already := args[key]; already {
			continue
		}
		args[key] = m[2] == "true"
	}
	for _, m := range argRe.FindAllStringSubmatch(q, -1) {
		key := m[1]
		if _, already := args[key]; already {
			continue
		}
		if m[3] != "" {
			var n int
			fmt.Sscanf(m[3], "%d", &n)
			args[key] = n
		} else {
			args[key] = unescape(m[2])
		}
	}
	return name, args
}

func unescape(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	s = strings.ReplaceAll(s, `\n`, "\n")
	return s
}

func (s *stubEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	s.calls = append(s.calls, query)
	name, args := parseCall(query)
	switch name {
	case "generatedOutputById":
		id := args["outputId"].(string)
		if d, ok := s.doc[id]; ok {
			return bundleOf([]map[string]any{d}), nil
		}
		return bundleOf(nil), nil
	case "documentVersionsForOwner", "documentVersions":
		docId, _ := args["documentId"].(string)
		out := []map[string]any{}
		for _, v := range s.versions {
			if v["documentId"] == docId {
				out = append(out, v)
			}
		}
		return bundleOf(out), nil
	case "appendDocumentVersion":
		row := map[string]any{}
		for k, v := range args {
			row[k] = v
		}
		row["id"] = args["versionId"]
		// Model the engine's stamp, not the args: appendDocumentVersion
		// writes `ownerUserId: actor.userId` (memql#2989), so the value
		// comes off the CONTEXT. Reading it from args here would let a
		// regression that reintroduces a caller-supplied owner pass.
		row["ownerUserId"] = actorUserId(ctx)
		s.versions = append(s.versions, row)
		return bundleOf(nil), nil
	case "updateGeneratedOutputContent":
		id := args["outputId"].(string)
		if d, ok := s.doc[id]; ok {
			d["body"] = args["body"]
			// Same stamp as above: the re-inserted row's owner comes off
			// the context actor, never the caller's args.
			d["ownerUserId"] = actorUserId(ctx)
			if av, ok := args["attachmentId"]; ok {
				d["attachmentId"] = av
			}
		}
		return bundleOf(nil), nil
	case "libraryArtifactById":
		id, _ := args["artifactId"].(string)
		if a, ok := s.artifacts[id]; ok {
			// Shaped (shape artifactFull): a fresh, template-only projection,
			// not the underlying row -- see artifactFullShapeFields.
			return bundleOf([]map[string]any{projectShapeFields(a, artifactFullShapeFields)}), nil
		}
		return bundleOf(nil), nil
	case "libraryArtifactBySourceConceptRef":
		if s.failArtifactSourceRefLookup {
			return nil, fmt.Errorf("stub: simulated read failure")
		}
		sourceRef, _ := args["sourceConceptRef"].(string)
		for _, a := range s.artifacts {
			if stringField(a, "sourceConceptRef") == sourceRef {
				// Shaped (shape artifactFull), same projection as above.
				return bundleOf([]map[string]any{projectShapeFields(a, artifactFullShapeFields)}), nil
			}
		}
		return bundleOf(nil), nil
	case "createArtifact":
		// The real engine's validateArgsField refuses a PRESENT enum value
		// outside its declared set -- including "" (present-and-blank is not
		// omitted). Modelling this is what lets the stub catch
		// writeArtifactLabels sending a blank optional enum unconditionally
		// (the CRITICAL fix): without it, this stub would silently accept
		// the exact call the real engine refuses.
		if err := validateCreateArtifactEnums(args); err != nil {
			return nil, err
		}
		sourceRef, _ := args["sourceConceptRef"].(string)
		id := stubArtifactId(sourceRef)
		// Full replace, not merge: mirrors createArtifact's bare `insert{}`
		// body (D3) -- a re-version carries only the fields THIS call names.
		// A field this call omits is ABSENT from the new version, not
		// carried over from whatever s.artifacts[id] held before. Modelling
		// this any other way (e.g. shallow-merging onto the prior row) would
		// make the label-loss regression test pass even without the fix.
		row := map[string]any{}
		for k, v := range args {
			row[k] = v
		}
		row["id"] = id
		s.artifacts[id] = row
		return bundleOf(nil), nil
	}
	return bundleOf(nil), nil
}

// actorUserId reads the acting user off the context the way the engine
// actually resolves `actor.userId`: from the ACCESS CONTEXT
// (auth.AccessFromContext), which is what resolveActorReference reads.
//
// It read the CLAIMS until memql#2989's review. That is a different context
// key, and modelling it here is what let the whole suite stay green while the
// stamp resolved to the inbound caller -- a stub that models the engine
// wrongly proves only that the stub agrees with itself. Keep this reading the
// same surface the engine does, or these tests stop being evidence.
//
// Returns "" when no access context is bound, which is the case the handlers
// must refuse to reach: ContextWithUserActor passes the context through
// unchanged for a blank owner, so a write on that path would be attributed to
// whoever the inbound caller happened to be.
func actorUserId(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	return ac.UserId
}

// bundleOf wraps rows in an *ExecuteResult whose Bundle holds matching
// MemoryNodes -- the shape extractRows walks.
func bundleOf(rows []map[string]any) *memql.ExecuteResult {
	nodes := make([]*memqlv1.MemoryNode, 0, len(rows))
	for _, r := range rows {
		fields := map[string]any{}
		for k, v := range r {
			fields[k] = v
		}
		st, _ := structpb.NewStruct(fields)
		nodes = append(nodes, &memqlv1.MemoryNode{
			Id:      asStr(r["id"]),
			Concept: asStr(r["concept"]),
			Payload: st,
		})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// --- unused IntegrationEngineAccess methods (stubbed to satisfy the interface) ---

func (s *stubEngine) RegisterIntegration(memql.IntegrationProvider) error { return nil }
func (s *stubEngine) InvokeAI(context.Context, string, map[string]any) (any, error) {
	return nil, nil
}
func (s *stubEngine) InvokeAIStructured(context.Context, string, map[string]any, string, json.RawMessage, bool) (string, error) {
	return "", nil
}
func (s *stubEngine) RenderPrompt(string, map[string]any) (string, error) { return "", nil }
func (s *stubEngine) ChatStreamProvider() common.ChatStreamProvider       { return nil }
func (s *stubEngine) ChatStreamProviderByName(string) common.ChatStreamProvider {
	return nil
}
func (s *stubEngine) ChatStreamWithToolsProviderByName(string) common.ChatStreamWithToolsProvider {
	return nil
}
func (s *stubEngine) ToolDefinitionsForNames([]string) []common.ToolDefinition { return nil }
func (s *stubEngine) ExecuteToolByName(context.Context, string, map[string]any) (string, error) {
	return "", nil
}
func (s *stubEngine) ResolveSkills(context.Context, []string) (memql.SkillBundle, error) {
	return memql.SkillBundle{}, nil
}

// --- helpers ---

func seededDoc() map[string]any {
	return map[string]any{
		"id":          "doc-1",
		"ownerUserId": "user-a",
		"title":       "Birds",
		"summary":     "A list of birds",
		"body":        "v1 body",
		"format":      "markdown",
		"source":      "agent_generated",
		"partitionId": "space-1",
	}
}

// unwrap decodes the single result MemoryNode the edit/restore handlers
// return into an editResult.
func unwrap(t *testing.T, nodes []memorynodes.MemoryNode) editResult {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 result node, got %d", len(nodes))
	}
	var r editResult
	if err := json.Unmarshal(nodes[0].Payload, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return r
}

// --- tests ---

// TestEditDocument_TwoEditsRetainBothVersions proves the core append-only
// guarantee: two edits produce two RETAINED versions (v1 + v2, no loss),
// the version numbers are monotonic, and the latest pointer (backing
// generatedOutput body) reflects the newest content.
func TestEditDocument_TwoEditsRetainBothVersions(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	i := NewIntegration(eng)
	ctx := context.Background()

	// First edit -> version 1.
	out1, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1",
		"content":    "edited once",
		"authorKind": "user",
		"authorId":   "user-a",
		"note":       "first edit",
	}, 0)
	if err != nil {
		t.Fatalf("first edit: %v", err)
	}
	r1 := unwrap(t, out1)
	if r1.NewVersion != 1 {
		t.Fatalf("first edit NewVersion = %d, want 1", r1.NewVersion)
	}

	// Second edit -> version 2.
	out2, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1",
		"content":    "edited twice",
		"authorKind": "user",
		"authorId":   "user-a",
		"note":       "second edit",
	}, 0)
	if err != nil {
		t.Fatalf("second edit: %v", err)
	}
	r2 := unwrap(t, out2)
	if r2.NewVersion != 2 {
		t.Fatalf("second edit NewVersion = %d, want 2", r2.NewVersion)
	}
	if r2.PriorVersion != 1 {
		t.Fatalf("second edit PriorVersion = %d, want 1", r2.PriorVersion)
	}

	// Both versions retained: nothing overwritten.
	if len(eng.versions) != 2 {
		t.Fatalf("expected 2 retained versions, got %d", len(eng.versions))
	}
	nums := versionNumbers(eng)
	if nums[0] != 1 || nums[1] != 2 {
		t.Fatalf("retained version numbers = %v, want [1 2]", nums)
	}
	// v2 chains back to v1.
	if eng.versions[1]["parentVersionId"] != eng.versions[0]["id"] {
		t.Fatalf("v2 parentVersionId = %v, want v1 id %v", eng.versions[1]["parentVersionId"], eng.versions[0]["id"])
	}
	// Latest pointer reflects newest content.
	if got := eng.doc["doc-1"]["body"]; got != "edited twice" {
		t.Fatalf("backing body = %v, want 'edited twice'", got)
	}
}

// TestTouchArtifact_PreservesLabelsAcrossEdit is the D3 regression test: the
// one place this feature can lose user data. touchArtifact re-versions the
// v1:library:artifact index row by re-calling createArtifact with a FIXED
// argument list; createArtifact's insert body is a bare `insert{}` block, so
// MemQL's insert-versioning means the new version carries only the fields
// THIS call names. Before the fix, touchArtifact never named `labels`, so
// every document edit silently dropped the artifact's labels -- nothing
// errors, nothing logs. This must fail against the unfixed touchArtifact.
func TestTouchArtifact_PreservesLabelsAcrossEdit(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())

	// Seed the artifact index row the way indexGeneratedOutputOnCreate would
	// have already promoted it, carrying labels a person or an earlier agent
	// turn put on it before this edit.
	sourceRef := "v1:library:generatedOutput:doc-1"
	artifactId := stubArtifactId(sourceRef)
	eng.seedArtifact(map[string]any{
		"id":               artifactId,
		"sourceConceptRef": sourceRef,
		"ownerUserId":      "user-a",
		"lens":             "artifact",
		"kind":             "generated_output",
		"source":           "agent_generated",
		"title":            "Birds",
		"summary":          "A list of birds",
		"labels":           []any{"reports", "q3"},
	})

	i := NewIntegration(eng)
	ctx := context.Background()

	// Edit the document -- handleEditDocument calls touchArtifact at the end
	// to re-stamp the index row's updatedAt watermark.
	if _, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1",
		"content":    "edited once",
		"authorKind": "user",
		"authorId":   "user-a",
	}, 0); err != nil {
		t.Fatalf("edit: %v", err)
	}

	got := stringSliceField(eng.artifacts[artifactId], "labels")
	want := []string{"reports", "q3"}
	if !slices.Equal(got, want) {
		t.Fatalf("artifact labels after document edit = %v, want %v (unchanged) -- "+
			"touchArtifact must read the current row's labels and pass them through "+
			"the createArtifact re-version call, or an edit silently wipes them", got, want)
	}
}

// TestTouchArtifact_SkipsReStampOnReadFailure is the finding-4 regression:
// currentArtifactCarryForward distinguishes "read failed" from "no row yet" /
// "row carries nothing" -- only the latter two legitimately carry forward as
// empty. A genuine read failure must make touchArtifact skip the
// createArtifact re-stamp ENTIRELY; writing labels: [] on a failed read
// would DESTROY real labels, not merely leave updatedAt stale (the
// documented, accepted best-effort price of skipping).
func TestTouchArtifact_SkipsReStampOnReadFailure(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	sourceRef := "v1:library:generatedOutput:doc-1"
	artifactId := stubArtifactId(sourceRef)
	eng.seedArtifact(map[string]any{
		"id":               artifactId,
		"sourceConceptRef": sourceRef,
		"ownerUserId":      "user-a",
		"lens":             "artifact",
		"kind":             "generated_output",
		"source":           "agent_generated",
		"title":            "Birds",
		"labels":           []any{"reports", "q3"},
	})
	eng.failArtifactSourceRefLookup = true

	i := NewIntegration(eng)
	if _, err := i.handleEditDocument(context.Background(), map[string]any{
		"documentId": "doc-1",
		"content":    "edited once",
		"authorKind": "user",
		"authorId":   "user-a",
	}, 0); err != nil {
		t.Fatalf("edit: %v (the edit itself must still succeed; only the best-effort re-stamp is skipped)", err)
	}

	// touchArtifact must not have called createArtifact at all -- the row
	// is untouched, labels intact, rather than re-versioned with labels: [].
	if n := countCalls(eng, "createArtifact"); n != 0 {
		t.Fatalf("createArtifact called %d times after a failed label read, want 0 -- "+
			"touchArtifact must skip the re-stamp entirely on a read failure, not risk "+
			"writing labels: [] over the row's real labels", n)
	}
	got := stringSliceField(eng.artifacts[artifactId], "labels")
	want := []string{"reports", "q3"}
	if !slices.Equal(got, want) {
		t.Fatalf("artifact labels = %v, want %v (untouched -- the re-stamp must have been skipped)", got, want)
	}
}

// TestMergeLabel_ComparesTrimmed covers the minor review finding:
// removeArtifactLabel trims the CALLER's label, but a label already
// stored on the row can carry whitespace padding if it arrived through
// some path other than mergeLabelAdd (createArtifact directly, an
// automation) -- an exact-match compare would make that label permanently
// unremovable through this capability. mergeLabelAdd gets the same
// trimmed compare for symmetry: an add of the trimmed form must recognise
// an existing padded label as already present, not append a
// visually-identical duplicate.
func TestMergeLabel_ComparesTrimmed(t *testing.T) {
	padded := []string{"reports", " urgent "}

	remaining, changed := mergeLabelRemove(padded, "urgent")
	if !changed {
		t.Fatalf("mergeLabelRemove(%v, %q) changed=false, want true -- a trimmed compare must match the padded stored label", padded, "urgent")
	}
	if slices.Contains(remaining, " urgent ") {
		t.Fatalf("mergeLabelRemove(%v, %q) = %v, the padded label was not removed", padded, "urgent", remaining)
	}

	_, changed = mergeLabelAdd(padded, "urgent")
	if changed {
		t.Fatalf("mergeLabelAdd(%v, %q) changed=true, want false -- %q already matches the padded %q", padded, "urgent", "urgent", " urgent ")
	}
}

// TestEditDocument_OptimisticConflict proves an edit naming an
// expectedVersion that no longer matches the latest is rejected as a
// conflict WITHOUT appending (history stays intact).
func TestEditDocument_OptimisticConflict(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	i := NewIntegration(eng)
	ctx := context.Background()

	// Establish v1.
	if _, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1", "content": "one", "authorId": "user-a",
	}, 0); err != nil {
		t.Fatalf("seed edit: %v", err)
	}

	// Caller thinks the latest is v0 (stale) -> conflict.
	out, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1", "content": "stale base", "authorId": "user-a",
		"expectedVersion": 0,
	}, 0)
	if err != nil {
		t.Fatalf("conflicting edit returned hard error: %v", err)
	}
	r := unwrap(t, out)
	if !r.Conflict {
		t.Fatalf("expected Conflict=true, got %+v", r)
	}
	if len(eng.versions) != 1 {
		t.Fatalf("conflict must NOT append: have %d versions, want 1", len(eng.versions))
	}

	// Correct expectedVersion (1) succeeds.
	out2, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1", "content": "fresh base", "authorId": "user-a",
		"expectedVersion": 1,
	}, 0)
	if err != nil {
		t.Fatalf("matching edit: %v", err)
	}
	if r2 := unwrap(t, out2); r2.NewVersion != 2 || r2.Conflict {
		t.Fatalf("matching edit = %+v, want v2 no-conflict", r2)
	}
}

// TestAssistantEdit_LandsAssistantAuthorKind proves an assistant edit
// lands a version with authorKind=assistant and authorId from the
// injected agentId (memql#1231).
func TestAssistantEdit_LandsAssistantAuthorKind(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	i := NewIntegration(eng)
	ctx := context.Background()

	out, err := i.handleEditDocumentAsAssistant(ctx, map[string]any{
		"documentId":       "doc-1",
		"content":          "assistant revision",
		"agentId":          "agent-7",
		"producedByPlanId": "plan-9",
		"note":             "added three more birds",
	}, 0)
	if err != nil {
		t.Fatalf("assistant edit: %v", err)
	}
	r := unwrap(t, out)
	if r.AuthorKind != "assistant" {
		t.Fatalf("AuthorKind = %q, want assistant", r.AuthorKind)
	}
	v := eng.versions[len(eng.versions)-1]
	if v["authorKind"] != "assistant" {
		t.Fatalf("version authorKind = %v, want assistant", v["authorKind"])
	}
	if v["authorId"] != "agent-7" {
		t.Fatalf("version authorId = %v, want agent-7 (the injected agentId)", v["authorId"])
	}
	if v["producedByPlanId"] != "plan-9" {
		t.Fatalf("version producedByPlanId = %v, want plan-9", v["producedByPlanId"])
	}
}

// TestRestore_AppendsNewLatestEqualToChosen proves restore appends a NEW
// latest version equal to the chosen one, with history intact and a
// 'restored from vN' note (memql#1230).
func TestRestore_AppendsNewLatestEqualToChosen(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	i := NewIntegration(eng)
	ctx := context.Background()

	// Build history: v1 ("apple"), v2 ("banana").
	if _, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1", "content": "apple", "authorId": "user-a",
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleEditDocument(ctx, map[string]any{
		"documentId": "doc-1", "content": "banana", "authorId": "user-a",
	}, 0); err != nil {
		t.Fatal(err)
	}
	v1Id := eng.versions[0]["id"].(string)

	// Restore v1 -> appends v3 with v1's content.
	out, err := i.handleRestoreDocumentVersion(ctx, map[string]any{
		"documentId": "doc-1",
		"versionId":  v1Id,
		"authorId":   "user-a",
	}, 0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	r := unwrap(t, out)
	if r.NewVersion != 3 {
		t.Fatalf("restore NewVersion = %d, want 3 (append-as-latest)", r.NewVersion)
	}
	if r.AuthorKind != "system" {
		t.Fatalf("restore AuthorKind = %q, want system", r.AuthorKind)
	}
	// History intact: 3 versions retained.
	if len(eng.versions) != 3 {
		t.Fatalf("expected 3 versions after restore (non-destructive), got %d", len(eng.versions))
	}
	v3 := eng.versions[2]
	// New latest content equals the chosen version's content.
	if v3["content"] != "apple" {
		t.Fatalf("restored v3 content = %v, want 'apple' (equal to v1)", v3["content"])
	}
	if note, _ := v3["note"].(string); note != "restored from v1" {
		t.Fatalf("restore note = %q, want 'restored from v1'", note)
	}
	// Latest pointer reflects restored content.
	if got := eng.doc["doc-1"]["body"]; got != "apple" {
		t.Fatalf("backing body after restore = %v, want 'apple'", got)
	}
}

// TestEditDocument_OwnerThreadedFromRow proves ownerUserId on the
// appended version is taken from the document row, not from any caller-
// supplied value.
//
// The MECHANISM changed in memql#2989 and the guarantee did not. The
// handler used to pass `ownerUserId` as a mutation argument; it now runs
// the write under the row owner's actor and the mutation stamps
// `actor.userId`. The stub models that (see actorUserId), so this still
// asserts the same thing -- an attacker-supplied owner does not land --
// and additionally proves the value now arrives by the un-forgeable
// route.
func TestEditDocument_OwnerThreadedFromRow(t *testing.T) {
	eng := newStubEngine()
	doc := seededDoc()
	doc["ownerUserId"] = "real-owner"
	eng.seedDocument(doc)
	i := NewIntegration(eng)

	if _, err := i.handleEditDocument(context.Background(), map[string]any{
		"documentId": "doc-1", "content": "x", "authorId": "someone-else",
		"ownerUserId": "attacker-supplied", // must be ignored
	}, 0); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := eng.versions[0]["ownerUserId"]; got != "real-owner" {
		t.Fatalf("version ownerUserId = %v, want real-owner (stamped from the owner actor)", got)
	}
	if got := eng.doc["doc-1"]["ownerUserId"]; got != "real-owner" {
		t.Fatalf("backing row ownerUserId = %v, want real-owner -- the re-insert must not "+
			"reassign ownership", got)
	}
	// The owner must not appear as an ARGUMENT to the two mutations that
	// now stamp it. Leaving it there would put a forgeable field back on
	// the generated client surface even though the engine ignores it.
	//
	// Scoped to those two by name on purpose: createArtifact still takes
	// an ownerUserId arg, and legitimately -- v1:library:artifact declares
	// no @rowAuthz tier, so it is one of the concepts memql#2803 has yet
	// to reach, not a miss here.
	for _, q := range eng.calls {
		name, _ := parseCall(q)
		switch name {
		case "appendDocumentVersion", "updateGeneratedOutputContent":
			if strings.Contains(q, "ownerUserId:") {
				t.Fatalf("%s still passes ownerUserId as an argument (memql#2989 stamps it "+
					"from actor.userId): %s", name, q)
			}
		}
	}
}

// TestEditDocument_RefusesOwnerlessDocument covers the edge memql#2989's
// ruling flagged: ContextWithUserActor returns the context UNCHANGED for a
// blank owner, so a write on that path would be stamped with whatever actor
// the INBOUND caller carried -- silently attributing another user's document
// to them. The handler must refuse instead.
//
// Both cases build REAL prior state before blanking the owner, and that is the
// point rather than set dressing. The restore case originally passed
// `versionId: "v-1"` against a document with no such version, so the handler
// bailed at the version lookup and the subtest went green with BOTH owner
// guards deleted -- it asserted nothing. Seeding a genuine version leaves the
// owner guard as the only thing that can refuse.
func TestEditDocument_RefusesOwnerlessDocument(t *testing.T) {
	for _, tc := range []struct {
		name string
		// call receives a versionId that really exists on the document, so a
		// refusal can only come from the owner guard.
		call func(*Integration, context.Context, string) error
	}{
		{
			name: "editDocument",
			call: func(i *Integration, ctx context.Context, _ string) error {
				_, err := i.handleEditDocument(ctx, map[string]any{
					"documentId": "doc-1", "content": "x",
				}, 0)
				return err
			},
		},
		{
			name: "restoreDocumentVersion",
			call: func(i *Integration, ctx context.Context, versionId string) error {
				_, err := i.handleRestoreDocumentVersion(ctx, map[string]any{
					"documentId": "doc-1", "versionId": versionId,
				}, 0)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := newStubEngine()
			doc := seededDoc()
			eng.seedDocument(doc)
			i := NewIntegration(eng)

			// Real history, written while the document still HAS an owner.
			if _, err := i.handleEditDocument(context.Background(), map[string]any{
				"documentId": "doc-1", "content": "apple", "authorId": "user-a",
			}, 0); err != nil {
				t.Fatalf("seeding a version: %v", err)
			}
			versionId, _ := eng.versions[0]["id"].(string)
			if versionId == "" {
				t.Fatal("seeded version has no id; the guard below would be untested")
			}
			before := len(eng.versions)

			// Now the document loses its owner. seedDocument stores by
			// reference, so this is the same row the handler will load.
			doc["ownerUserId"] = ""

			// An inbound caller who is NOT the document owner. If the handler
			// proceeded, the stamp would land on this identity.
			ctx := auth.ContextWithUserActor(context.Background(), "inbound-caller")

			if err := tc.call(i, ctx, versionId); err == nil {
				t.Fatal("handler accepted an ownerless document; it must refuse rather than " +
					"attribute the write to the inbound caller")
			}
			if len(eng.versions) != before {
				t.Fatalf("wrote %d version(s) for an ownerless document, want %d",
					len(eng.versions)-before, 0)
			}
		})
	}
}

// --- addArtifactLabel / removeArtifactLabel ---

// unwrapLabel decodes the single result MemoryNode the label handlers
// return into an artifactLabelResult.
func unwrapLabel(t *testing.T, nodes []memorynodes.MemoryNode) artifactLabelResult {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 result node, got %d", len(nodes))
	}
	var r artifactLabelResult
	if err := json.Unmarshal(nodes[0].Payload, &r); err != nil {
		t.Fatalf("unmarshal label result: %v", err)
	}
	return r
}

// testArtifactId is the stub id createArtifact will assign the backing
// doc-1 generatedOutput's index row (stubArtifactId is the stub's own
// deterministic-in-sourceConceptRef bookkeeping key, not a reproduction
// of the real engine's hash-based id -- see its doc comment).
// seededArtifact's "id" MUST equal this: writeArtifactLabels never passes
// an explicit id: arg (exactly like touchArtifact) -- createArtifact
// derives it from sourceConceptRef on every write-back -- so a seeded row
// whose id does not match what THIS STUB derives from its own
// sourceConceptRef would have its write-back land at a different stub key
// than the one a test reads back from -- silently validating nothing.
var testArtifactId = stubArtifactId("v1:library:generatedOutput:doc-1")

// seededArtifact returns a fully-populated artifact row, every field a
// distinct sentinel value, so a test that round-trips it through
// writeArtifactLabels can assert nothing but labels changed.
func seededArtifact() map[string]any {
	return map[string]any{
		"id":                   testArtifactId,
		"sourceConceptRef":     "v1:library:generatedOutput:doc-1",
		"ownerUserId":          "user-a",
		"lens":                 "artifact",
		"kind":                 "generated_output",
		"source":               "agent_generated",
		"title":                "Birds",
		"summary":              "A list of birds",
		"format":               "markdown",
		"mimeType":             "text/markdown",
		"live":                 false,
		"scope":                "private",
		"labels":               []any{"reports", "q3"},
		"partitionId":          "space-1",
		"agentId":              "agent-1",
		"producedByPlanId":     "plan-1",
		"producedByWorkerId":   "worker-1",
		"producedByWorkerName": "MacBook-Pro",
		"validationStatus":     "validated",
	}
}

func countCalls(eng *stubEngine, name string) int {
	n := 0
	for _, q := range eng.calls {
		if callName, _ := parseCall(q); callName == name {
			n++
		}
	}
	return n
}

func TestAddArtifactLabel_AddsNewLabel(t *testing.T) {
	eng := newStubEngine()
	eng.seedArtifact(seededArtifact())
	i := NewIntegration(eng)

	out, err := i.handleAddArtifactLabel(context.Background(), map[string]any{
		"artifactId": testArtifactId,
		"label":      "urgent",
	}, 0)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	r := unwrapLabel(t, out)
	if !r.Changed {
		t.Fatalf("Changed = false, want true (new label)")
	}
	want := []string{"reports", "q3", "urgent"}
	if !slices.Equal(r.Labels, want) {
		t.Fatalf("result Labels = %v, want %v", r.Labels, want)
	}
	if got := stringSliceField(eng.artifacts[testArtifactId], "labels"); !slices.Equal(got, want) {
		t.Fatalf("stored labels = %v, want %v", got, want)
	}
}

func TestAddArtifactLabel_IdempotentOnExistingLabel(t *testing.T) {
	eng := newStubEngine()
	eng.seedArtifact(seededArtifact())
	i := NewIntegration(eng)

	out, err := i.handleAddArtifactLabel(context.Background(), map[string]any{
		"artifactId": testArtifactId,
		"label":      "reports", // already present
	}, 0)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	r := unwrapLabel(t, out)
	if r.Changed {
		t.Fatalf("Changed = true, want false (label already present -- must be a no-op)")
	}
	want := []string{"reports", "q3"}
	if !slices.Equal(r.Labels, want) {
		t.Fatalf("result Labels = %v, want %v (unchanged)", r.Labels, want)
	}
	// A true no-op: nothing was written back, so no re-version happened
	// and the row's other fields / version were never touched.
	if n := countCalls(eng, "createArtifact"); n != 0 {
		t.Fatalf("createArtifact called %d times on an idempotent add, want 0", n)
	}
}

func TestRemoveArtifactLabel_RemovesExistingLabel(t *testing.T) {
	eng := newStubEngine()
	eng.seedArtifact(seededArtifact())
	i := NewIntegration(eng)

	out, err := i.handleRemoveArtifactLabel(context.Background(), map[string]any{
		"artifactId": testArtifactId,
		"label":      "reports",
	}, 0)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	r := unwrapLabel(t, out)
	if !r.Changed {
		t.Fatalf("Changed = false, want true (label was present)")
	}
	want := []string{"q3"}
	if !slices.Equal(r.Labels, want) {
		t.Fatalf("result Labels = %v, want %v", r.Labels, want)
	}
	if got := stringSliceField(eng.artifacts[testArtifactId], "labels"); !slices.Equal(got, want) {
		t.Fatalf("stored labels = %v, want %v", got, want)
	}
}

func TestRemoveArtifactLabel_IdempotentOnAbsentLabel(t *testing.T) {
	eng := newStubEngine()
	eng.seedArtifact(seededArtifact())
	i := NewIntegration(eng)

	out, err := i.handleRemoveArtifactLabel(context.Background(), map[string]any{
		"artifactId": testArtifactId,
		"label":      "not-there",
	}, 0)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	r := unwrapLabel(t, out)
	if r.Changed {
		t.Fatalf("Changed = true, want false (label was never present -- must be a no-op)")
	}
	want := []string{"reports", "q3"}
	if !slices.Equal(r.Labels, want) {
		t.Fatalf("result Labels = %v, want %v (unchanged)", r.Labels, want)
	}
	if n := countCalls(eng, "createArtifact"); n != 0 {
		t.Fatalf("createArtifact called %d times on an idempotent remove, want 0", n)
	}
}

func TestArtifactLabel_RefusesBlankLabel(t *testing.T) {
	eng := newStubEngine()
	eng.seedArtifact(seededArtifact())
	i := NewIntegration(eng)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"add empty", func() error {
			_, err := i.handleAddArtifactLabel(context.Background(), map[string]any{"artifactId": testArtifactId, "label": ""}, 0)
			return err
		}},
		{"add whitespace", func() error {
			_, err := i.handleAddArtifactLabel(context.Background(), map[string]any{"artifactId": testArtifactId, "label": "   "}, 0)
			return err
		}},
		{"remove empty", func() error {
			_, err := i.handleRemoveArtifactLabel(context.Background(), map[string]any{"artifactId": testArtifactId, "label": ""}, 0)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("blank label was accepted; it must be refused")
			}
		})
	}
	if n := countCalls(eng, "createArtifact"); n != 0 {
		t.Fatalf("a refused blank-label call still wrote, createArtifact called %d times, want 0", n)
	}
}

// TestArtifactLabel_RefusesUnknownOrOwnerlessArtifact covers "labelling an
// artifact the caller does not own is refused". The Go-level stub does not
// model the real engine's per-row authz gate (libraryArtifactById's
// ownerUserId==actor.userId filter, which makes another user's row simply
// not come back from the load) -- so, mirroring how this file's existing
// TestEditDocument_RefusesOwnerlessDocument stands in for the same real
// gate, "not visible to this caller" is modelled as "the row is not in the
// store" (unknown artifactId) and "the row carries no owner to attribute
// the change to" (ownerUserId blank). Both must refuse for both add and
// remove, and neither may write.
func TestArtifactLabel_RefusesUnknownOrOwnerlessArtifact(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*stubEngine)
	}{
		{"unknown artifactId", func(eng *stubEngine) {}},
		{"ownerless artifact", func(eng *stubEngine) {
			a := seededArtifact()
			a["ownerUserId"] = ""
			eng.seedArtifact(a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := newStubEngine()
			tc.seed(eng)
			i := NewIntegration(eng)

			if _, err := i.handleAddArtifactLabel(context.Background(), map[string]any{
				"artifactId": testArtifactId, "label": "x",
			}, 0); err == nil {
				t.Fatal("addArtifactLabel accepted an inaccessible artifact; it must refuse")
			}
			if _, err := i.handleRemoveArtifactLabel(context.Background(), map[string]any{
				"artifactId": testArtifactId, "label": "x",
			}, 0); err == nil {
				t.Fatal("removeArtifactLabel accepted an inaccessible artifact; it must refuse")
			}
			if n := countCalls(eng, "createArtifact"); n != 0 {
				t.Fatalf("a refused label change still wrote, createArtifact called %d times, want 0", n)
			}
		})
	}
}

// TestArtifactLabel_OwnerThreadedFromRow proves the write is attributed to
// the ARTIFACT ROW's own owner, never to whatever actor is on the inbound
// ctx -- the same guarantee TestEditDocument_OwnerThreadedFromRow proves
// for the document-edit path, and the mechanism that makes "you cannot
// label someone else's artifact" true without a new authorization check:
// the row simply will not resolve to any owner other than its own.
func TestArtifactLabel_OwnerThreadedFromRow(t *testing.T) {
	eng := newStubEngine()
	a := seededArtifact()
	a["ownerUserId"] = "real-owner"
	eng.seedArtifact(a)
	i := NewIntegration(eng)

	// The inbound ctx carries a DIFFERENT actor entirely.
	ctx := auth.ContextWithUserActor(context.Background(), "someone-else")

	if _, err := i.handleAddArtifactLabel(ctx, map[string]any{
		"artifactId": testArtifactId, "label": "urgent",
	}, 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := stringField(eng.artifacts[testArtifactId], "ownerUserId"); got != "real-owner" {
		t.Fatalf("written artifact ownerUserId = %q, want %q (the row's own owner, not the inbound caller)", got, "real-owner")
	}
}

// TestArtifactLabel_PreservesOtherFieldsOnWrite guards writeArtifactLabels
// against reproducing the exact D3 hazard for a DIFFERENT set of fields:
// createArtifact's insert body is a full replace, so any field
// writeArtifactLabels forgets to thread through would silently vanish on
// the very first label change. Every field on seededArtifact is a distinct
// sentinel value; only labels may differ after the write.
func TestArtifactLabel_PreservesOtherFieldsOnWrite(t *testing.T) {
	eng := newStubEngine()
	eng.seedArtifact(seededArtifact())
	i := NewIntegration(eng)

	if _, err := i.handleAddArtifactLabel(context.Background(), map[string]any{
		"artifactId": testArtifactId, "label": "urgent",
	}, 0); err != nil {
		t.Fatalf("add: %v", err)
	}

	got := eng.artifacts[testArtifactId]
	want := seededArtifact()
	for field, wantVal := range want {
		if field == "labels" || field == "id" {
			continue
		}
		if got[field] != wantVal {
			t.Fatalf("field %q = %v, want %v (unrelated field must survive a label write untouched)", field, got[field], wantVal)
		}
	}
}

// TestArtifactLabel_SurvivesBlankOptionalEnums is the CRITICAL regression
// test the review's real-engine verification found: format, scope and
// validationStatus are OPTIONAL enum args on createArtifact, and a real
// promoted artifact routinely carries a BLANK one -- none of the five
// promotion automations pass scope, and a record-lens row has no format.
// writeArtifactLabels used to thread every field through unconditionally
// via QuoteString, which renders a blank field as a PRESENT empty-string
// argument; the real engine's arg validation refuses a present value
// outside an enum's declared set (including ""), so EVERY label change on
// EVERY automation-promoted artifact was refused. This seeds exactly that
// shape -- scope/format/validationStatus all blank, mirroring a real
// promoted row -- and asserts the write still succeeds.
func TestArtifactLabel_SurvivesBlankOptionalEnums(t *testing.T) {
	eng := newStubEngine()
	a := seededArtifact()
	a["scope"] = ""
	a["format"] = ""
	a["validationStatus"] = ""
	eng.seedArtifact(a)
	i := NewIntegration(eng)

	if _, err := i.handleAddArtifactLabel(context.Background(), map[string]any{
		"artifactId": testArtifactId, "label": "urgent",
	}, 0); err != nil {
		t.Fatalf("add with blank optional enums: %v -- writeArtifactLabels must OMIT a blank "+
			"optional enum arg rather than send it as a present empty string", err)
	}
	got := eng.artifacts[testArtifactId]
	for _, field := range []string{"scope", "format", "validationStatus"} {
		if got[field] != nil && got[field] != "" {
			t.Fatalf("field %q = %v, want blank/absent (nothing set it)", field, got[field])
		}
	}
}

func versionNumbers(eng *stubEngine) []int {
	nums := make([]int, 0, len(eng.versions))
	for _, v := range eng.versions {
		n, _ := intArg(v["versionNumber"])
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

// TestIntArgNarrowsSafely verifies the numeric coercion narrows JSON-decoded
// values through core/num rather than a bare conversion.
//
// THE BOUND IS THE PLATFORM'S, not int32 (memql#4779). The helper this
// replaced clamped at math.MaxInt32 and called that "the worst-case int
// width" -- a portability proxy rather than a domain rule, and on a 64-bit
// build it truncated a legitimate 5 GB size to 2147483647 while protecting
// nothing. The out-of-range ANSWERS are unchanged: an int64 saturates because
// its callers are version ordinals, a float zeroes.
func TestIntArgNarrowsSafely(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
		ok   bool
	}{
		{"int", 7, 7, true},
		{"int64", int64(42), 42, true},
		{"float64", float64(10), 10, true},
		{"float32", float32(3), 3, true},
		{"json.Number", json.Number("99"), 99, true},
		// Past int32 and well inside int: carried, not truncated. This is the
		// case the old int32 bound got wrong.
		{"past-int32-high", float64(1) + math.MaxInt32, math.MaxInt32 + 1, true},
		{"past-int32-low", float64(-1) + math.MinInt32, math.MinInt32 - 1, true},
		// Past int: the float arm's answer is zero.
		{"overflow-high", 1e30, 0, true},
		{"overflow-low", -1e30, 0, true},
		{"nan", math.NaN(), 0, true},
		// The int64 arm's answer is saturation, because a version ordinal that
		// inverts is worse than one that stops.
		{"int64-saturates", int64(math.MaxInt64), math.MaxInt, true},
		{"non-numeric", "nope", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := intArg(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("intArg(%v) = (%d, %t), want (%d, %t)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
