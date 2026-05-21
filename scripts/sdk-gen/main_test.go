package main

import (
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

	out := string(emitTSMethods([]Construct{c}, "query"))

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

	out := string(emitTSMethods([]Construct{c}, "query"))

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
	out := string(emitTSMethods(nil, "query"))
	if !strings.Contains(out, "export {};") {
		t.Errorf("expected placeholder `export {};` for empty construct list\n--- output ---\n%s", out)
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
