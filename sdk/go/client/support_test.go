package client

import (
	"reflect"
	"testing"
)

// TestResult_Rows_BundleFlattensPayload pins the documented
// flatten-on-Rows behavior for the bundle envelope. Anyone changing
// this is breaking metrics_fetcher and any consumer that reads
// `row["foo"]` directly assuming the SDK-side flattening already
// happened.
func TestResult_Rows_BundleFlattensPayload(t *testing.T) {
	r := &Result{payload: bundleFixture()}
	rows := r.Rows()
	if len(rows) != 1 {
		t.Fatalf("Rows: want 1 row, got %d", len(rows))
	}
	row := rows[0]
	// Intrinsics kept by the flattening pass:
	if got := row["id"]; got != "v1:agents:agent:abc" {
		t.Errorf("id = %v, want v1:agents:agent:abc", got)
	}
	if got := row["concept"]; got != "v1:agents:agent" {
		t.Errorf("concept = %v, want v1:agents:agent", got)
	}
	// Payload flattened to top-level:
	if got := row["name"]; got != "Faye" {
		t.Errorf("name = %v, want Faye (payload should be flattened)", got)
	}
	// Intrinsics dropped by the flattening pass:
	if _, ok := row["type"]; ok {
		t.Errorf("Rows() leaked 'type' intrinsic: %v -- the flat path intentionally drops it", row["type"])
	}
	if _, ok := row["provenance"]; ok {
		t.Errorf("Rows() leaked 'provenance': %v -- the flat path intentionally drops it", row["provenance"])
	}
}

// TestResult_RawNodes_PreservesFullShape pins the contract that
// RawNodes returns the engine's MemoryNode shape uncollapsed.
// Admin / concept-browser tools depend on every intrinsic +
// provenance + metadata being available.
func TestResult_RawNodes_PreservesFullShape(t *testing.T) {
	r := &Result{payload: bundleFixture()}
	rows := r.RawNodes()
	if len(rows) != 1 {
		t.Fatalf("RawNodes: want 1 row, got %d", len(rows))
	}
	row := rows[0]

	// All MemoryNode intrinsics must round-trip.
	wantIntrinsics := map[string]any{
		"id":        "v1:agents:agent:abc",
		"concept":   "v1:agents:agent",
		"type":      "memoryNode",
		"createdBy": "system",
		"createdAt": "2026-05-21T04:44:42Z",
	}
	for k, want := range wantIntrinsics {
		if got := row[k]; got != want {
			t.Errorf("RawNodes row[%q] = %v, want %v", k, got, want)
		}
	}

	// payload must STILL be nested -- not flattened.
	payload, ok := row["payload"].(map[string]any)
	if !ok {
		t.Fatalf("RawNodes row[\"payload\"] = %v, want nested map", row["payload"])
	}
	if got := payload["name"]; got != "Faye" {
		t.Errorf("RawNodes payload[\"name\"] = %v, want Faye", got)
	}
	if got := payload["description"]; got != "An agent" {
		t.Errorf("RawNodes payload[\"description\"] = %v, want \"An agent\"", got)
	}

	// provenance must survive.
	prov, ok := row["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("RawNodes row[\"provenance\"] = %v, want nested map", row["provenance"])
	}
	if got := prov["kind"]; got != "system" {
		t.Errorf("RawNodes provenance[\"kind\"] = %v, want system", got)
	}

	// metadata must survive.
	meta, ok := row["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("RawNodes row[\"metadata\"] = %v, want nested map", row["metadata"])
	}
	if got := meta["spaceId"]; got != "v1:cognition:space:xyz" {
		t.Errorf("RawNodes metadata[\"spaceId\"] = %v, want v1:cognition:space:xyz", got)
	}

	// schema must survive.
	schema, ok := row["schema"].(map[string]any)
	if !ok {
		t.Fatalf("RawNodes row[\"schema\"] = %v, want nested map", row["schema"])
	}
	if got := schema["version"]; got != "1" {
		t.Errorf("RawNodes schema[\"version\"] = %v, want \"1\"", got)
	}
}

// TestResult_RawNodes_NonBundle confirms RawNodes returns nil for
// shape-wrapped envelopes -- there are no MemoryNodes to preserve.
// Documented behavior; callers that need to handle both envelopes
// fall back to Rows().
func TestResult_RawNodes_NonBundle(t *testing.T) {
	cases := []struct {
		name string
		r    *Result
	}{
		{"nil result", nil},
		{"nil payload", &Result{payload: nil}},
		{"shape-wrapped data envelope", &Result{payload: map[string]any{
			"data": []any{
				map[string]any{"id": "v1:foo:bar:1", "name": "alice"},
			},
		}}},
		{"raw slice envelope", &Result{payload: []any{
			map[string]any{"id": "v1:foo:bar:1", "name": "alice"},
		}}},
		{"single-row envelope", &Result{payload: map[string]any{
			"id": "v1:foo:bar:1", "name": "alice",
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.RawNodes(); got != nil {
				t.Errorf("RawNodes = %v, want nil for non-bundle envelope", got)
			}
		})
	}
}

// TestResult_RawNodes_CopiesNodeMap verifies that mutating the
// returned Row does NOT reach back into the result's underlying
// payload. Catches the regression where RawNodes hands out direct
// references to the protojson-decoded map.
func TestResult_RawNodes_CopiesNodeMap(t *testing.T) {
	payload := bundleFixture()
	r := &Result{payload: payload}

	rows := r.RawNodes()
	if len(rows) != 1 {
		t.Fatalf("RawNodes: want 1 row, got %d", len(rows))
	}
	rows[0]["mutated"] = "yes"

	// Re-fetch from the same Result; mutation must not have leaked.
	again := r.RawNodes()
	if _, leaked := again[0]["mutated"]; leaked {
		t.Errorf("RawNodes returned a shared reference -- mutation leaked into underlying payload")
	}

	// Sanity-check the fixture map didn't change either.
	bundle := payload["bundle"].(map[string]any)
	nodes := bundle["nodes"].([]any)
	node := nodes[0].(map[string]any)
	if _, leaked := node["mutated"]; leaked {
		t.Errorf("RawNodes returned a shared reference to the bundle node map -- mutation leaked")
	}
}

// bundleFixture returns a representative bundle envelope as it
// would arrive from protojson.Unmarshal of a memqlv1.Result with
// a GraphBundle of MemoryNodes. Used by RawNodes / Rows tests.
func bundleFixture() map[string]any {
	return map[string]any{
		"bundle": map[string]any{
			"nodes": []any{
				map[string]any{
					"id":        "v1:agents:agent:abc",
					"concept":   "v1:agents:agent",
					"type":      "memoryNode",
					"createdBy": "system",
					"createdAt": "2026-05-21T04:44:42Z",
					"payload": map[string]any{
						"name":        "Faye",
						"description": "An agent",
						"role":        "specialist",
					},
					"schema": map[string]any{
						"version": "1",
					},
					"metadata": map[string]any{
						"spaceId": "v1:cognition:space:xyz",
					},
					"provenance": map[string]any{
						"kind": "system",
					},
				},
			},
		},
	}
}

// Compile-time sanity: Row must remain a map alias so tests can
// poke at fields without a constructor.
var _ Row = (map[string]any)(nil)

// Compile-time sanity: reflect.DeepEqual works on the Row type the
// rest of the SDK uses, so future test cases can switch to it
// without surprise.
var _ = reflect.DeepEqual
