package library

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"testing"

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
	// calls records every mutation/query string for assertion.
	calls []string
}

func newStubEngine() *stubEngine {
	return &stubEngine{doc: map[string]map[string]any{}}
}

func (s *stubEngine) seedDocument(d map[string]any) {
	s.doc[d["id"].(string)] = d
}

// argRe extracts `key: value` pairs from a `name({...})` call. Values
// are quoted strings or bare numbers.
var argRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*):\s*(?:"((?:[^"\\]|\\.)*)"|(-?\d+))`)

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
	for _, m := range argRe.FindAllStringSubmatch(q, -1) {
		key := m[1]
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
		s.versions = append(s.versions, row)
		return bundleOf(nil), nil
	case "updateGeneratedOutputContent":
		id := args["outputId"].(string)
		if d, ok := s.doc[id]; ok {
			d["body"] = args["body"]
			if av, ok := args["attachmentId"]; ok {
				d["attachmentId"] = av
			}
		}
		return bundleOf(nil), nil
	case "createArtifact":
		return bundleOf(nil), nil
	}
	return bundleOf(nil), nil
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
		t.Fatalf("version ownerUserId = %v, want real-owner (threaded from row)", got)
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

// TestIntArgClampsToInt32Range verifies the numeric coercion narrows
// JSON-decoded values to the 32-bit int range so the int conversion is
// overflow-safe on every platform (CodeQL go/incorrect-integer-conversion).
func TestIntArgClampsToInt32Range(t *testing.T) {
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
		{"overflow-high", float64(1) + math.MaxInt32, 0, true},
		{"overflow-low", float64(-1) + math.MinInt32, 0, true},
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
