package memql_test

// cluster_register_node_dryrun_test.go -- regression coverage for memql#1643(a):
// logicRegisterNode must bind its event input fields through to the
// mutationCreateNode step via explicit keyed args (mirroring logicDeregisterNode).
//
// The original call mixed bare-shorthand args with a multi-segment path
// (args.event.payload.node.id / .address) and carried an unknown `flavor` key:
//
//	mutationCreateNode({
//	  args.event.payload.node.id,                       // dotted -> NOT a valid shorthand
//	  nodeType: args.event.payload.node.type,
//	  flavor:   coalesce(args.event.payload.node.flavor, ""),  // not a mutationCreateNode arg
//	  args.event.payload.node.address                   // dotted -> NOT a valid shorthand
//	})
//
// Dotted paths are not eligible for the `args.X` object-literal shorthand
// (only simple identifiers are -- see tryParseShorthandCtx), so the two bare
// entries fell through to the key:value parser and corrupted the bound arg map:
// the recorded mutation came back as
//
//	map[address:<nil> flavor: id:<nil> nodeType:<nil>]
//
// i.e. an extra `flavor` key and untyped-nil values -- exactly the
// "required argument \"nodeType\" is missing" failure QA hit on the live
// run_automation path. The fix binds every field with an explicit key +
// coalesce, the same shape logicDeregisterNode uses.
//
// Driven through the Gate-2 dry-run sandbox (no Postgres). NB: the sandbox
// executor does not thread the trigger event into a logic step's `args.event`
// envelope, so this asserts the *binding shape* of the call (the regression the
// harness faithfully reproduces), not the resolved field values. The live
// executor resolves args.event.payload.node.* for real -- the identical access
// in logicBootstrapCluster is the production-proven precedent.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"

	// Side-effect import: registers the behavioral dry-run runner + compile hook.
	_ "github.com/znasllc-io/memql/component/automations/steps"
)

// registerNodeAutomation mirrors dsl/cluster/automations.memql's registerNode:
// a single step invoking the logicRegisterNode construct with the trigger event.
const registerNodeAutomation = `@enabled
@trigger(event="system.startup")
@description("Register this node in the database on startup")
automation registerNode {
  step run {
    logic registerNode { event: event }
  }
}`

// TestDryRun_RegisterNode_BindsCleanMutationArgs: a dry-run of the registerNode
// automation must record a mutationCreateNode write whose argument map contains
// EXACTLY the valid keyed args (id / nodeType / address) -- no stray `flavor`
// key and no untyped-nil values from a corrupted shorthand parse. Before the
// #1643(a) fix this recorded `map[address:<nil> flavor: id:<nil> nodeType:<nil>]`
// (the binding QA saw fail with "required argument nodeType is missing").
func TestDryRun_RegisterNode_BindsCleanMutationArgs(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		AutomationName:   "registerNode",
		AutomationSource: registerNodeAutomation,
		TriggerEvent: &memql.DryRunTriggerEvent{
			Topic: "system.startup",
			Payload: map[string]any{
				"node": map[string]any{
					"id":      "node-test-1",
					"type":    "worker",
					"address": "127.0.0.1:50051",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RunBundleDryRun returned error: %v", err)
	}
	if !report.OK {
		t.Fatalf("expected dry-run OK, got failure: %s\ntrace=%+v", report.FailureReason, report.Trace)
	}

	var nodeMut *memql.RecordedMutation
	for i := range report.SideEffectManifest.Mutations {
		if report.SideEffectManifest.Mutations[i].Concept == "v1:cluster:node" {
			nodeMut = &report.SideEffectManifest.Mutations[i]
			break
		}
	}
	if nodeMut == nil {
		t.Fatalf("expected a recorded v1:cluster:node mutation, got none: %+v",
			report.SideEffectManifest.Mutations)
	}

	pl := nodeMut.Payload

	// Regression guard 1: the unknown `flavor` arg (not part of the
	// mutationCreateNode schema) must be gone.
	if _, ok := pl["flavor"]; ok {
		t.Errorf("mutationCreateNode args must not carry a stray 'flavor' key: %+v", pl)
	}

	// Regression guard 2: the keyed args must bind to real string values
	// (coalesce(...,\"\")), never the untyped-nil the corrupted shorthand
	// produced. nodeType is @required on mutationCreateNode -- an untyped nil
	// here is exactly the "required argument missing" failure.
	for _, k := range []string{"id", "nodeType", "address"} {
		v, ok := pl[k]
		if !ok {
			t.Errorf("expected mutationCreateNode arg %q to be bound, missing from %+v", k, pl)
			continue
		}
		if v == nil {
			t.Errorf("mutationCreateNode arg %q bound to untyped nil (shorthand-corruption regression): %+v", k, pl)
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("expected mutationCreateNode arg %q to bind a string, got %T (%v)", k, v, v)
		}
	}
}
