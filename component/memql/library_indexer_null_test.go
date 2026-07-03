package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/dsl"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// These tests pin the fix for memql#1626: the six sibling Library
// indexer logics (logicIndex{Document,Note,Todo,CalendarEvent,Memory,
// LiveSource}) passed RAW nullable fields straight into
// createArtifact. When an optional field was ABSENT on the
// backing row it resolved to an explicit null, and the
// v1:library:artifact string fields reject null
// ("expected string, but got null"), so artifact auto-indexing failed
// for every record whose optional fields were absent.
//
// #1605 fixed exactly this on indexGeneratedOutput with
// `coalesce(field, "")`; #1626 is the same fix applied to the six
// siblings it missed. The systemic alternative -- making the concept's
// optional string fields nullable -- is not expressible in the concept
// schema (the JSON-Schema builder emits a bare {"type":"string"} for an
// optional string and there is no @nullable annotation), and would not
// help the @enum fields anyway (null is not a member of an enum), so
// the coalesce approach is the fix.

// compileArtifactSchema loads the REAL v1:library:artifact concept from
// the embedded DSL tree and returns its compiled definition schema --
// exactly the schema the production Concept.Create path validates the
// indexer's mutation payload against.
func compileArtifactSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	concept, err := memoryNodes.DefaultRegistry().Get("v1:library:artifact")
	require.NoError(t, err, "v1:library:artifact concept must be loadable from the embedded DSL")

	raw, err := concept.DefinitionSchema()
	require.NoError(t, err)
	require.NotEmpty(t, raw, "artifact definition schema must be present")

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	require.NoError(t, compiler.AddResource("artifact.json", bytes.NewReader(raw)))
	schema, err := compiler.Compile("artifact.json")
	require.NoError(t, err)
	return schema
}

// renderArtifactPayloadFromMemoryIndexer renders the REAL
// createArtifact (loaded from the embedded DSL) with the exact
// args indexMemory passes for a memory whose OPTIONAL fields
// (summary / agentId / partitionId) are absent. `coalesced` selects whether
// those absent fields arrive as "" (the post-#1626 indexer output) or as
// null (the pre-#1626 raw-passthrough that failed validation).
func renderArtifactPayloadFromMemoryIndexer(t *testing.T, coalesced bool) map[string]any {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	fn, err := registry.Get("createArtifact")
	require.NoError(t, err, "createArtifact must register from dsl/library/mutations.memql")
	require.NotNil(t, fn.MutationTemplate, "createArtifact must carry a mutation template")

	// The optional-field value the memory indexer hands the mutation: ""
	// when coalesced (post-#1626), nil when raw (pre-#1626).
	var optional any
	if coalesced {
		optional = ""
	} else {
		optional = nil
	}

	// Required artifact args are always present (they map to @required
	// fields on v1:library:memory); the optionals are the ones #1626 is
	// about.
	args := map[string]any{
		"sourceConceptRef": "v1:library:memory:mem-1626",
		"ownerUserId":      "user-1626",
		"lens":             "record",
		"kind":             "memory",
		"source":           "agent_generated",
		"title":            "Prefers concise replies",
		"summary":          optional,
		"agentId":          optional,
		"partitionId":      optional,
		"live":             false,
	}

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, args)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))
	return payload
}

// TestArtifactIndexerAbsentOptionalFieldsValidate is the acceptance test
// for memql#1626: a sibling indexer (memory) creating an artifact with
// ABSENT optional fields must produce a payload that validates against
// the real v1:library:artifact schema. The coalesced ("") output lands;
// the raw (null) output -- the pre-fix behaviour -- is rejected.
func TestArtifactIndexerAbsentOptionalFieldsValidate(t *testing.T) {
	schema := compileArtifactSchema(t)

	// Post-#1626: absent optionals arrive as "" and the artifact row lands.
	coalesced := renderArtifactPayloadFromMemoryIndexer(t, true)
	require.NoError(t, schema.Validate(coalesced),
		"memory indexer payload with coalesced (\"\") optional fields must validate against v1:library:artifact (memql#1626)")
	require.Equal(t, "", coalesced["summary"])
	require.Equal(t, "", coalesced["agentId"])
	require.Equal(t, "", coalesced["partitionId"])

	// Pre-#1626: raw nullable passthrough produced explicit nulls, which
	// the artifact string fields reject. This is the exact failure the
	// connector test hit live on 0.9.67.
	raw := renderArtifactPayloadFromMemoryIndexer(t, false)
	err := schema.Validate(raw)
	require.Error(t, err,
		"raw null optional fields must be rejected by v1:library:artifact -- the bug #1626 fixes")
	require.Contains(t, err.Error(), "null",
		"rejection must be the 'expected string, but got null' failure mode")
}

// TestLibraryIndexersCoalesceNullableOptionalFields guards against the
// memql#1626 "1 of 7" regression directly at the fix site. #2235 moved
// the createArtifact write out of the (now-deleted) index* promotion
// logics and into the index*OnCreate automation steps (dsl/library/
// automations.memql) -- the cluster registerNode/deregisterNode shape.
// This reads the REAL automations.memql from the embedded tree and
// asserts every indexer step wraps its nullable optional fields in
// coalesce(...) rather than binding the raw `field: event.payload.X`
// form that resolves to null and fails artifact validation.
//
// An indexer that reverts any of these fields to the raw form -- exactly
// how #1605 left 6 of 7 indexers broken -- fails here.
func TestLibraryIndexersCoalesceNullableOptionalFields(t *testing.T) {
	data, err := fs.ReadFile(dsl.Tree(), "library/automations.memql")
	require.NoError(t, err, "library/automations.memql must be readable from the embedded DSL tree")
	blocks := splitAutomationBlocks(string(data))

	// Each entry: the indexer's mutation-call binding for a nullable
	// optional field that MUST be coalesced. The `want` substring is the
	// coalesced form that must be present; the `rawBad` substring is the
	// raw passthrough that must be absent (it is the null-producing bug).
	cases := []struct {
		automation string
		field      string
		want       string
		rawBad     string
	}{
		// Post-G5 (#2367) the indexers read their typed args bare -- the
		// coalesce guard is unchanged, only the read form migrated from
		// event.payload.X to the bare field.
		// indexNoteOnCreate
		{"indexNoteOnCreate", "summary", `summary:          coalesce(body`, `summary:          body`},
		// indexMemoryOnCreate (the indexer failing live on 0.9.67)
		{"indexMemoryOnCreate", "summary", `summary:          coalesce(summary`, `summary:          summary`},
		{"indexMemoryOnCreate", "agentId", `agentId:          coalesce(agentId`, `agentId:          agentId`},
		{"indexMemoryOnCreate", "partitionId", `partitionId:      coalesce(partitionId`, `partitionId:      partitionId`},
	}

	for _, tc := range cases {
		t.Run(tc.automation+"/"+tc.field, func(t *testing.T) {
			block, ok := blocks[tc.automation]
			require.True(t, ok, "%s must exist in library/automations.memql", tc.automation)
			require.Contains(t, block, tc.want,
				"%s must coalesce its nullable %s field (memql#1626)", tc.automation, tc.field)
			// The coalesced form is `field: coalesce(event...)`, so the raw
			// `field: event...` passthrough is never a substring of it -- a
			// plain NotContains over THIS automation's block cleanly catches a
			// revert to the raw form.
			require.NotContains(t, block, tc.rawBad,
				"%s must NOT bind %s raw (resolves to null -> artifact validation fails, memql#1626)", tc.automation, tc.field)
		})
	}
}

// splitAutomationBlocks carves library/automations.memql into a map keyed
// by automation name, each value the source text from that `automation
// NAME {` declaration up to (but not including) the next one. Lets the
// coalesce assertions scope to a single indexer.
func splitAutomationBlocks(src string) map[string]string {
	const marker = "\nautomation "
	out := map[string]string{}
	starts := []int{}
	for i := 0; ; {
		at := strings.Index(src[i:], marker)
		if at < 0 {
			break
		}
		abs := i + at
		starts = append(starts, abs+1) // +1 to drop the leading newline
		i = abs + len(marker)
	}
	for idx, s := range starts {
		end := len(src)
		if idx+1 < len(starts) {
			end = starts[idx+1]
		}
		block := src[s:end]
		// name := the token after "automation " up to the next space / brace / newline.
		rest := block[len("automation "):]
		name := rest
		if cut := strings.IndexAny(rest, " {\n"); cut >= 0 {
			name = rest[:cut]
		}
		out[name] = block
	}
	return out
}
