package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseArgsBlock_BraceAwareForPatternQuantifiers pins the
// regression that closed the sdk-gen extractor over `@pattern("...{N,M}...")`
// annotations. Pre-fix the non-greedy regex captured everything
// between `args {` and the first `}` -- which was the brace INSIDE
// a regex string -- truncating the args list.
//
// The fix introduced a string-aware brace counter (extractArgsBlockBody)
// that skips `}` characters inside double-quoted strings. This test
// codifies that contract so a future regex-only rewrite can't
// silently reintroduce the truncation.
func TestParseArgsBlock_BraceAwareForPatternQuantifiers(t *testing.T) {
	body := `args {
    userId    string  @required @pattern("^v1:[a-z0-9]+:[a-z0-9_]+:[a-zA-Z0-9_-]{1,128}$")
    epoch     int     @required
  }
  update user {
    id: args.userId
  }`

	got := parseArgsBlock(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 args fields (userId, epoch), got %d: %+v", len(got), got)
	}
	if got[0].Name != "userId" {
		t.Errorf("field[0].Name = %q, want \"userId\"", got[0].Name)
	}
	if got[1].Name != "epoch" {
		t.Errorf("field[1].Name = %q, want \"epoch\" -- pre-fix this field was truncated because the `}` in `{1,128}` ended the args block early", got[1].Name)
	}
}

// TestParseArgsBlock_NoArgsBlock confirms the extractor's null path:
// a construct body without an `args { ... }` block returns nil
// rather than panicking on the missing brace.
func TestParseArgsBlock_NoArgsBlock(t *testing.T) {
	body := `update user {
    id: "v1:identity:user:alice"
    revocationEpoch: 1
  }`
	if got := parseArgsBlock(body); got != nil {
		t.Errorf("expected nil args for body with no `args {...}`, got %+v", got)
	}
}

// TestParseArgsBlock_PreservesMixedAnnotations is the existing-shape
// regression: every annotation flavor we already shipped (@required,
// @enum, @default, @description) still threads through the
// brace-aware extractor unchanged.
func TestParseArgsBlock_PreservesMixedAnnotations(t *testing.T) {
	body := `args {
    name     string  @required @description("display name")
    role     string  @enum("owner", "admin", "writer", "reader") @default("reader")
    locale   string
  }
`

	got := parseArgsBlock(body)
	if len(got) != 3 {
		t.Fatalf("expected 3 args, got %d: %+v", len(got), got)
	}
	if !got[0].Required {
		t.Errorf("name should be @required")
	}
	if got[0].Description != "display name" {
		t.Errorf("description = %q, want \"display name\"", got[0].Description)
	}
	if len(got[1].Enum) != 4 {
		t.Errorf("role enum should have 4 values, got %d", len(got[1].Enum))
	}
	if got[1].Default != "reader" {
		t.Errorf("role default = %q, want \"reader\"", got[1].Default)
	}
}

// TestEmitTSMethods_QueryWithArgs confirms the TS emission pipeline
// produces the four pieces consumers need for each construct: a
// PascalCase args interface, a builder function, a `declare module`
// augmentation that adds the method to QueryClient, and the runtime
// prototype assignment that wires the method body. Pins the contract
// so a future refactor can't silently drop any of these.
func TestEmitTSMethods_QueryWithArgs(t *testing.T) {
	c := Construct{
		Kind:        "query",
		Name:        "queryActiveSpaces",
		Concept:     "space",
		Description: "List active spaces.",
		Args: []ArgField{
			{Name: "ownerId", Type: "string", Required: true, Description: "Filter by owner"},
			{Name: "limit", Type: "integer"},
			{Name: "includeArchived", Type: "boolean"},
		},
		ShapeName: "spaceCard",
	}

	out := string(emitTSMethods([]Construct{c}, "query", ""))

	wants := []string{
		`import { QueryClient, type QueryCallOptions } from "./query.js";`,
		`import type { Result } from "./types.js";`,
		`import { renderMemQLValue } from "./memqlValue.js";`,
		`/** List active spaces. */`,
		`// Bound concept: space.`,
		`export interface QueryActiveSpacesArgs {`,
		`  /** Filter by owner */`,
		`  ownerId: string;`,
		`  limit?: number;`,
		`  includeArchived?: boolean;`,
		`export function buildQueryActiveSpaces(args: QueryActiveSpacesArgs): string {`,
		`parts.push("ownerId: " + renderMemQLValue(args.ownerId));`,
		`if (args.limit !== undefined) parts.push("limit: " + renderMemQLValue(args.limit));`,
		`if (args.includeArchived !== undefined) parts.push("includeArchived: " + renderMemQLValue(args.includeArchived));`,
		`return "queryActiveSpaces({" + parts.join(", ") + "})";`,
		`declare module "./query.js" {`,
		`queryActiveSpaces(args: QueryActiveSpacesArgs, opts?: QueryCallOptions): Promise<Result>;`,
		`QueryClient.prototype.queryActiveSpaces = function`,
		`return this.executeNamed("queryActiveSpaces", buildQueryActiveSpaces(args), opts);`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("emitTSMethods output missing %q\n--- output ---\n%s", w, out)
		}
	}
}

// TestEmitTSMethods_NoArgs verifies the zero-arg path: the args
// interface is empty, the builder short-circuits to the no-arg literal,
// and the prototype method's args parameter is optional so callers can
// invoke `qc.queryActiveAgentRoles()` without ceremony.
func TestEmitTSMethods_NoArgs(t *testing.T) {
	c := Construct{
		Kind:        "query",
		Name:        "queryActiveAgentRoles",
		Description: "List active agent roles.",
	}

	out := string(emitTSMethods([]Construct{c}, "query", ""))

	wants := []string{
		`export interface QueryActiveAgentRolesArgs {`,
		`void args;`,
		`return "queryActiveAgentRoles({})";`,
		`queryActiveAgentRoles(args?: QueryActiveAgentRolesArgs, opts?: QueryCallOptions): Promise<Result>;`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("emitTSMethods (no-args) output missing %q\n--- output ---\n%s", w, out)
		}
	}
}

// TestEmitTSMethods_Empty pins the placeholder export so the drift
// gate doesn't flip on an empty file (zero-construct kind would
// otherwise emit nothing but imports, which is a valid module).
func TestEmitTSMethods_Empty(t *testing.T) {
	out := string(emitTSMethods(nil, "query", ""))
	if !strings.Contains(out, "export {};") {
		t.Errorf("expected placeholder `export {};` for empty construct list\n--- output ---\n%s", out)
	}
}

// TestEmitTSMethods_ImportFromPackage pins the cross-package form: when
// importFrom is set (a BFF shipping the generated surface as its own
// package), the generated TS imports QueryClient/types from that single
// package specifier and augments via `declare module "<pkg>"` -- never
// the in-package relative "./query.js" form. This is what lets a
// separate package (e.g. @visionarys-io/copresent-sdk) merge its
// generated methods onto the published @znasllc-io/memql-sdk-core
// QueryClient.
func TestEmitTSMethods_ImportFromPackage(t *testing.T) {
	c := Construct{Kind: "query", Name: "queryActiveSpaces", Description: "List active spaces."}
	out := string(emitTSMethods([]Construct{c}, "query", "@znasllc-io/memql-sdk-core"))

	for _, want := range []string{
		`import { QueryClient, renderMemQLValue, type QueryCallOptions, type Result } from "@znasllc-io/memql-sdk-core";`,
		`declare module "@znasllc-io/memql-sdk-core" {`,
		`QueryClient.prototype.queryActiveSpaces = function`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("emitTSMethods(importFrom) missing %q\n--- output ---\n%s", want, out)
		}
	}
	for _, bad := range []string{`from "./query.js"`, `from "./types.js"`, `from "./memqlValue.js"`, `declare module "./query.js"`} {
		if strings.Contains(out, bad) {
			t.Errorf("emitTSMethods(importFrom) should not emit relative form %q", bad)
		}
	}
}

// TestTSType_MapsDSLToTSPrimitives confirms the type-mapping helper
// translates every DSL type token the generator currently knows about.
// Adding a new DSL type means extending tsType + this list.
func TestTSType_MapsDSLToTSPrimitives(t *testing.T) {
	cases := []struct {
		dsl, ts string
	}{
		{"string", "string"},
		{"datetime", "string"},
		{"bool", "boolean"},
		{"boolean", "boolean"},
		{"number", "number"},
		{"int", "number"},
		{"integer", "number"},
		{"object", "Record<string, unknown>"},
		{"array", "unknown[]"},
		{`enum("a", "b")`, "string"},
		{"[]string", "string[]"},
		{"[]object", "Record<string, unknown>[]"},
	}
	for _, c := range cases {
		got := tsType(ArgField{Type: c.dsl})
		if got != c.ts {
			t.Errorf("tsType(%q) = %q, want %q", c.dsl, got, c.ts)
		}
	}
}

// TestCollectConstructs_DescriptionAtFileStart guards a boundary in
// descriptionFor: when a construct's @description is the very first
// line of the file (no leading content), the backward preamble walk
// must clamp at offset 0 instead of slicing with a negative index.
// A BFF DSL file commonly opens straight with @description, so this
// path is exercised the moment a real BFF root is composed in.
func TestCollectConstructs_DescriptionAtFileStart(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "board.memql", `@description("Copresent board view")
query board queryCopresentBoard {
  args {
    boardId  string  @required
  }
  filter  payload.boardId==args.boardId
  shape   boardCard
}
`)
	got, err := CollectConstructs(root)
	if err != nil {
		t.Fatalf("CollectConstructs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 construct, got %d: %+v", len(got), got)
	}
	if got[0].Description != "Copresent board view" {
		t.Errorf("Description = %q, want \"Copresent board view\"", got[0].Description)
	}
}

// TestCollectConstructs_BuiltinSDKGating confirms that only builtins
// marked @sdk are collected (most builtins are internal and must not
// widen the client surface), that their body IS the arg schema (no
// `args { }` block), and that they classify as kind "builtin".
func TestCollectConstructs_BuiltinSDKGating(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "builtins.memql", `@enabled
@executor("integration.training.trainAgent")
@sdk
@description("Train an agent.")
builtin trainAgent {
  agentId  string  @required
  domains  array
  tools    array
}

@enabled
@executor("integration.auth.checkPermission")
@description("Internal permission check -- not client-facing.")
builtin authCheckPermission {
  userId  string  @required
}
`)
	got, err := CollectConstructs(root)
	if err != nil {
		t.Fatalf("CollectConstructs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 construct (only the @sdk builtin), got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != "builtin" {
		t.Errorf("Kind = %q, want \"builtin\"", c.Kind)
	}
	if c.Name != "trainAgent" {
		t.Errorf("Name = %q, want \"trainAgent\"", c.Name)
	}
	if len(c.Args) != 3 {
		t.Fatalf("expected 3 args parsed from the builtin body, got %d: %+v", len(c.Args), c.Args)
	}
	if c.Args[0].Name != "agentId" || !c.Args[0].Required {
		t.Errorf("Args[0] = %+v, want agentId @required", c.Args[0])
	}
	if c.Args[1].Name != "domains" || c.Args[1].Type != "array" {
		t.Errorf("Args[1] = %+v, want domains array", c.Args[1])
	}
}

// --- Multi-root merge tests -------------------------------------------------

// writeFixture writes a .memql file under dir/rel, creating parent dirs.
func writeFixture(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCollectAndMerge_MergesRootsDeterministically is the core
// multi-root acceptance test: a "core" root and a "bff" root each
// contribute constructs; the merge is the union, sorted by (kind,
// name) so neither root order nor file-walk order changes the result.
func TestCollectAndMerge_MergesRootsDeterministically(t *testing.T) {
	core := t.TempDir()
	bff := t.TempDir()

	// Core root: two queries + one mutation, intentionally authored so
	// alphabetical order differs from declaration order.
	writeFixture(t, core, "queries/spaces.memql", `
@description("List active spaces")
query space queryActiveSpaces {
  args {
    ownerId  string  @required
  }
  filter  payload.ownerId==args.ownerId
  shape   spaceCard
}

@description("List archived spaces")
query space queryArchivedSpaces {
  args {
    ownerId  string  @required
  }
  filter  payload.ownerId==args.ownerId
  shape   spaceCard
}
`)
	writeFixture(t, core, "mutations/createSpace.memql", `
@description("Create a space")
mutation space mutationCreateSpace {
  args {
    name  string  @required
  }
  insert {
    name: args.name
  }
}
`)

	// BFF root: one query + one logic. The BFF query (queryCopresentBoard)
	// sorts BEFORE the core queries, proving the merge re-sorts across
	// roots rather than appending.
	writeFixture(t, bff, "copresent/board.memql", `
@description("Copresent board view")
query board queryCopresentBoard {
  args {
    boardId  string  @required
  }
  filter  payload.boardId==args.boardId
  shape   boardCard
}

@description("Provision a copresent board")
logic logicProvisionBoard {
  args {
    event  object  @required
  }
  body {
    return provisionBoard({ id: args.event.payload.id })
  }
}
`)

	merged, err := CollectAndMerge([]string{core, bff})
	if err != nil {
		t.Fatalf("CollectAndMerge: %v", err)
	}

	// Expected order: sorted by (kind, name). Kinds sort
	// alphabetically: logic < mutation < query.
	wantOrder := []struct{ kind, name string }{
		{"logic", "logicProvisionBoard"},
		{"mutation", "mutationCreateSpace"},
		{"query", "queryActiveSpaces"},
		{"query", "queryArchivedSpaces"},
		{"query", "queryCopresentBoard"},
	}
	if len(merged) != len(wantOrder) {
		t.Fatalf("merged count = %d, want %d: %+v", len(merged), len(wantOrder), merged)
	}
	for i, w := range wantOrder {
		if merged[i].Kind != w.kind || merged[i].Name != w.name {
			t.Errorf("merged[%d] = (%s %s), want (%s %s)", i, merged[i].Kind, merged[i].Name, w.kind, w.name)
		}
	}

	// Reversing root order must produce the identical merged slice --
	// the determinism guarantee the drift gate relies on.
	reversed, err := CollectAndMerge([]string{bff, core})
	if err != nil {
		t.Fatalf("CollectAndMerge (reversed): %v", err)
	}
	if len(reversed) != len(merged) {
		t.Fatalf("reversed count = %d, want %d", len(reversed), len(merged))
	}
	for i := range merged {
		if reversed[i].Kind != merged[i].Kind || reversed[i].Name != merged[i].Name {
			t.Errorf("root order changed result at [%d]: %s/%s vs %s/%s",
				i, reversed[i].Kind, reversed[i].Name, merged[i].Kind, merged[i].Name)
		}
	}
}

// TestCollectAndMerge_DuplicateNameErrors confirms a construct name
// declared in two different roots is rejected with a clear, traceable
// error rather than silently last-wins. A core/BFF collision is a real
// bug.
func TestCollectAndMerge_DuplicateNameErrors(t *testing.T) {
	core := t.TempDir()
	bff := t.TempDir()

	writeFixture(t, core, "queries/spaces.memql", `
@description("Core list spaces")
query space querySpaces {
  args {
    ownerId  string  @required
  }
  filter  payload.ownerId==args.ownerId
  shape   spaceCard
}
`)
	// Same construct name in the BFF root -- collision.
	writeFixture(t, bff, "copresent/spaces.memql", `
@description("BFF list spaces")
query space querySpaces {
  args {
    boardId  string  @required
  }
  filter  payload.boardId==args.boardId
  shape   boardCard
}
`)

	_, err := CollectAndMerge([]string{core, bff})
	if err == nil {
		t.Fatalf("expected duplicate-name error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"duplicate construct", "querySpaces"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

// TestGenerate_MultiRootGoEmission exercises the full Generate path in
// Check mode end-to-end over two roots: first a write, then a re-check
// reports no drift (the byte-for-byte stability the gate depends on),
// and the merged Go output contains methods from BOTH roots.
func TestGenerate_MultiRootGoEmission(t *testing.T) {
	core := t.TempDir()
	bff := t.TempDir()
	out := t.TempDir()

	writeFixture(t, core, "queries/spaces.memql", `
@description("List active spaces")
query space queryActiveSpaces {
  args {
    ownerId  string  @required
  }
  filter  payload.ownerId==args.ownerId
  shape   spaceCard
}
`)
	writeFixture(t, bff, "copresent/board.memql", `
@description("Copresent board view")
query board queryCopresentBoard {
  args {
    boardId  string  @required
  }
  filter  payload.boardId==args.boardId
  shape   boardCard
}
`)

	// Write.
	if _, err := Generate(Options{Roots: []string{core, bff}, GoOut: out}); err != nil {
		t.Fatalf("Generate write: %v", err)
	}

	// Re-check: must report no drift.
	if _, err := Generate(Options{Roots: []string{core, bff}, GoOut: out, Check: true}); err != nil {
		t.Fatalf("Generate check after write reported drift: %v", err)
	}

	// Reversed root order must also be drift-free against the same
	// output -- proves root order does not affect bytes.
	if _, err := Generate(Options{Roots: []string{bff, core}, GoOut: out, Check: true}); err != nil {
		t.Fatalf("Generate check with reversed roots reported drift: %v", err)
	}

	queries, err := os.ReadFile(filepath.Join(out, "generated_queries.go"))
	if err != nil {
		t.Fatalf("read generated_queries.go: %v", err)
	}
	body := string(queries)
	for _, want := range []string{
		"func (qc *QueryClient) QueryActiveSpaces(",
		"func (qc *QueryClient) QueryCopresentBoard(",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("merged Go output missing %q", want)
		}
	}
}
