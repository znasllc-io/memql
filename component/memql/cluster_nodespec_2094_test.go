package memql

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestClusterNodeSpec2094 pins Epic 2.1 (#2094): the dsl/cluster
// deployment concept gains a per-node-type NodeSpec child
// (v1:cluster:deploymentNodeSpec) carrying per-node version + replicas
// (engine-as-spine), append-only/versioned like the parent deployment,
// with mutations to write it and a query to read a deployment's full
// NodeSpec set.
//
// It boots the full engine over the embedded DSL tree (no Postgres,
// exactly like TestEngineInitLoadsFullDSL) so the concept registry +
// function registry the cluster automations resolve against are
// populated for real. A regression -- a renamed field, a dropped
// mutation, a query that no longer loads -- fails here instead of
// silently breaking the E2.2+ deploy pack that depends on this shape.
func TestClusterNodeSpec2094(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after load")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the full DSL tree: %v", err)
	}

	// 1) The NodeSpec concept is registered under the cluster namespace.
	c, err := registry.Get("v1:cluster:deploymentNodeSpec")
	if err != nil {
		t.Fatalf("v1:cluster:deploymentNodeSpec concept not registered: %v", err)
	}

	// 2) It carries the engine-as-spine per-node fields. version is
	//    intentionally optional (empty = resolve against the deployment
	//    engine version); deploymentId + nodeType are the composite key.
	schemaRaw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		t.Fatalf("unmarshal definition schema: %v", err)
	}
	for _, field := range []string{"deploymentId", "nodeType", "version", "replicas", "imageDigest"} {
		if _, ok := schema.Properties[field]; !ok {
			t.Errorf("deploymentNodeSpec schema missing field %q (have %v)", field, keysOf(schema.Properties))
		}
	}

	// 3) The mutations (write a spec, re-pin a spec) and the read query
	//    (a deployment's full NodeSpec set) are registered as DSL
	//    functions. Construct names are bare post-#2036.
	fns := eng.Functions()
	for _, name := range []string{
		"createDeploymentNodeSpec",
		"updateDeploymentNodeSpec",
		"nodeSpecsForDeployment",
	} {
		if !fns.Has(name) {
			t.Errorf("expected DSL function %q to be registered after Init", name)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
