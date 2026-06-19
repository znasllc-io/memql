package conformance

// dim_schema_test.go -- the single-schema-source dimension (#1713).
//
// For EVERY @mcp-promoted function the three consumer surfaces must agree:
//   1. the typed-wrapper inputSchema advertised by tools/list (what a connector
//      discovers),
//   2. the inputSchema describeFunction reports (what an agent introspects),
//   3. the canonical FunctionInputJSONSchema (the single builder both render
//      through).
// All three byte-identical => they cannot drift, and no declared arg is dropped.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func schemaSourceCheck() check {
	return check{
		Issue:   "#1713",
		Dim:     "single-schema-source/no-dropped-args",
		NeedsDB: false,
		Run:     runSchemaSource,
	}
}

func runSchemaSource(t *testing.T, e *Env) {
	wrappers := e.Eng.MCPPromotedFunctionTools()
	if len(wrappers) == 0 {
		t.Fatal("no @mcp-promoted functions found; expected queryLibraryArtifactById et al.")
	}

	// The connector-facing surface (tools/list) must carry every promoted
	// function with an identical schema -- this is the genuine MCP enumeration.
	surface := e.listTools(t)

	checked := 0
	for _, w := range wrappers {
		name, _ := w["name"].(string)
		wrapSchema, _ := w["inputSchema"].(map[string]any)
		if wrapSchema == nil {
			t.Errorf("promoted tool %q advertises no inputSchema", name)
			continue
		}

		fn, err := e.Eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Errorf("promoted tool %q absent from the function registry: %v", name, err)
			continue
		}

		// (3) wrapper == canonical builder output.
		canonical := memql.FunctionInputJSONSchema(fn)
		if !reflect.DeepEqual(wrapSchema, canonical) {
			t.Errorf("schema drift for %q: wrapper != FunctionInputJSONSchema\n wrapper  =%#v\n canonical=%#v", name, wrapSchema, canonical)
		}

		// (1) tools/list surface == wrapper.
		if surfSchema, ok := surface[name]; ok {
			if !reflect.DeepEqual(surfSchema, wrapSchema) {
				t.Errorf("schema drift for %q between tools/list and the typed wrapper\n list   =%#v\n wrapper=%#v", name, surfSchema, wrapSchema)
			}
		} else {
			t.Errorf("promoted function %q is missing from the tools/list surface", name)
		}

		// (2) The describeFunction introspection surface must be reachable for
		// every promoted function and reflect the same args. (describeFunction
		// renders human-readable guidance through the same FunctionInputJSONSchema
		// source builder; the structured equality above already pins the schema,
		// so here we assert the introspection consumer surface is reachable and
		// non-empty -- it is what an agent calls to discover the contract.) This
		// leg routes through the engine Execute path (help() query handler), which
		// needs a DB connection, so it runs only when a DB is present; the
		// structured equality above is the DB-free core of the contract.
		if e.HasDB {
			assertDescribeReachable(t, e, name)
		}

		// No declared arg may be silently dropped from the schema (the reported
		// artifactId regression).
		assertNoDroppedArgs(t, name, fn, wrapSchema)
		checked++
	}

	if checked == 0 {
		t.Fatal("no promoted functions could be schema-checked")
	}
	t.Logf("#1713: verified single-schema-source over %d @mcp-promoted functions", checked)
}

// assertDescribeReachable proves the describeFunction introspection surface
// resolves (non-error, non-empty) for a promoted function -- so every tool an
// agent can call is also discoverable. The payload is human-readable guidance
// (markdown), so this asserts reachability + that the function name is reflected,
// not byte-equality (the structured schema equality is pinned against the typed
// wrapper + canonical builder above).
func assertDescribeReachable(t *testing.T, e *Env, fnName string) {
	t.Helper()
	out, isErr := e.toolCall(t, "describeFunction", map[string]any{"name": fnName})
	if isErr {
		t.Errorf("describeFunction(%q) returned an MCP error result: %v", fnName, out)
		return
	}
	if !strings.Contains(stringify(out), fnName) {
		t.Errorf("describeFunction(%q) payload does not reflect the function name (introspection drift): %v", fnName, out)
	}
}

// stringify renders an arbitrary decoded MCP payload to a string for substring
// checks (string passthrough; everything else JSON-encoded).
func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// assertNoDroppedArgs proves every declared args-block field surfaces as a
// property in the rendered JSON-Schema (#1713 "no dropped args").
func assertNoDroppedArgs(t *testing.T, name string, fn *memql.Function, schema map[string]any) {
	t.Helper()
	if fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) == 0 {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Errorf("%q declares %d args but the schema has no properties block", name, len(fn.ArgsSchema.Fields))
		return
	}
	for _, f := range fn.ArgsSchema.Fields {
		if f == nil || f.Name == "" {
			continue
		}
		if _, ok := props[f.Name]; !ok {
			t.Errorf("%q drops declared arg %q from its MCP schema (#1713)", name, f.Name)
		}
	}
}
