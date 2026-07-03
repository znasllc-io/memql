package memql_test

// cluster_register_node_dryrun_test.go -- regression coverage for memql#1643(a)
// and memql#1671:
//
// registerNode must bind its event input fields through to BOTH the
// createNode step (nodeRecord) and the createSpawnEvent step
// (spawnEvent) via explicit keyed args with coalesce fallbacks.
//
// #1643(a) fixed the nodeRecord step:
//
//   The original call mixed bare-shorthand args with a multi-segment path
//   (args.event.payload.node.id / .address) and carried an unknown `flavor` key:
//
//     createNode({
//       args.event.payload.node.id,                       // dotted -> NOT a valid shorthand
//       nodeType: args.event.payload.node.type,
//       flavor:   coalesce(args.event.payload.node.flavor, ""),  // not a createNode arg
//       args.event.payload.node.address                   // dotted -> NOT a valid shorthand
//     })
//
//   Dotted paths are not eligible for the `args.X` object-literal shorthand
//   (only simple identifiers are -- see tryParseShorthandCtx), so the two bare
//   entries fell through to the key:value parser and corrupted the bound arg map:
//   the recorded mutation came back as
//
//     map[address:<nil> flavor: id:<nil> nodeType:<nil>]
//
//   i.e. an extra `flavor` key and untyped-nil values -- exactly the
//   "required argument \"nodeType\" is missing" failure QA hit on the live
//   run_automation path.
//
// #1671 found that the spawnEvent step was STILL broken after #1643(a):
//
//   spawnEvent := createSpawnEvent({
//     nodeId:   coalesce(args.event.payload.node.id, ""),
//     nodeType: args.event.payload.node.type,   // <-- bare path, no coalesce!
//     ...
//   })
//
//   args.event.payload.node.type evaluates to nil when the sandbox does not
//   thread the trigger event into the logic step's args.event envelope. The live
//   executor resolves it for real, but a nil nodeType fails the @required
//   validation: "function createSpawnEvent: argument validation failed:
//   required argument nodeType is missing".
//
// Both fixes bind every @required field with an explicit key + coalesce, the
// same shape deregisterNode uses.
//
// Driven through the Gate-2 dry-run sandbox (no Postgres). Each createNode /
// createSpawnEvent mutation step reads event.payload.node.* directly and wraps
// every value in coalesce(...,""), so a @required arg is always bound to a
// non-nil string even when a nested field is absent -- the structural guarantee
// that retired the #1643(a)/#1671 shorthand-corruption + bare-path bugs.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"

	// Side-effect import: registers the behavioral dry-run runner + compile hook.
	_ "github.com/znasllc-io/memql/component/automations/steps"
)

// registerNodeAutomation mirrors dsl/cluster/automations.memql's registerNode.
// Since #2235 registerNode is no longer a pass-through logic: the createNode +
// createSpawnEvent writes are direct mutation steps on the automation, so the
// binding hygiene these tests guard is now verified on the real authored shape.
const registerNodeAutomation = `@enabled
@trigger(event="system.startup")
@description("Register this node in the database on startup")
automation registerNode {
  args {
    deploymentId any
    environment any
    node any
    provider any
    region any
  }
  step record {
    createNode {
      id:           coalesce(node.id, ""),
      nodeType:     coalesce(node.type, ""),
      address:      coalesce(node.address, ""),
      deploymentId: coalesce(deploymentId, ""),
      provider:     coalesce(provider, ""),
      environment:  coalesce(environment, ""),
      region:       coalesce(region, "")
    }
  }
  step spawn {
    createSpawnEvent {
      nodeId:   coalesce(node.id, ""),
      nodeType: coalesce(node.type, ""),
      action:   "spawned",
      reason:   "system.startup"
    }
  }
}`

// TestDryRun_RegisterNode_BindsCleanMutationArgs: a dry-run of the registerNode
// automation must record a createNode write whose argument map contains
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
	// createNode schema) must be gone.
	if _, ok := pl["flavor"]; ok {
		t.Errorf("createNode args must not carry a stray 'flavor' key: %+v", pl)
	}

	// Regression guard 2: the keyed args must bind to real string values
	// (coalesce(...,\"\")), never the untyped-nil the corrupted shorthand
	// produced. nodeType is @required on createNode -- an untyped nil
	// here is exactly the "required argument missing" failure.
	for _, k := range []string{"id", "nodeType", "address"} {
		v, ok := pl[k]
		if !ok {
			t.Errorf("expected createNode arg %q to be bound, missing from %+v", k, pl)
			continue
		}
		if v == nil {
			t.Errorf("createNode arg %q bound to untyped nil (shorthand-corruption regression): %+v", k, pl)
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("expected createNode arg %q to bind a string, got %T (%v)", k, v, v)
		}
	}
}

// TestDryRun_RegisterNode_SpawnEventBindsNodeType is the #1671 regression test.
//
// The #1643(a) fix corrected the nodeRecord step but left the spawnEvent step
// with a bare (uncoalesced) path:
//
//	createSpawnEvent {
//	  nodeId:   coalesce(event.payload.node.id, ""),
//	  nodeType: event.payload.node.type,   // <-- the pre-#1671 bare path, no coalesce!
//	  ...
//	}
//
// Passing nil for a @required field causes the live executor to reject the call
// with "required argument 'nodeType' is missing". Because the createSpawnEvent
// step now binds nodeType: coalesce(event.payload.node.type, ""), the recorded
// payload is always a non-nil string (at minimum "").
func TestDryRun_RegisterNode_SpawnEventBindsNodeType(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		AutomationName:   "registerNode",
		AutomationSource: registerNodeAutomation,
		TriggerEvent: &memql.DryRunTriggerEvent{
			Topic: "system.startup",
			Payload: map[string]any{
				"node": map[string]any{
					"id":      "node-test-2",
					"type":    "bff",
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

	// Locate the v1:cluster:spawnEvent mutation (the spawnEvent step).
	var spawnMut *memql.RecordedMutation
	for i := range report.SideEffectManifest.Mutations {
		if report.SideEffectManifest.Mutations[i].Concept == "v1:cluster:spawnEvent" {
			spawnMut = &report.SideEffectManifest.Mutations[i]
			break
		}
	}
	if spawnMut == nil {
		t.Fatalf("expected a recorded v1:cluster:spawnEvent mutation, got none; all mutations: %+v",
			report.SideEffectManifest.Mutations)
	}

	pl := spawnMut.Payload

	// #1671 regression: nodeType is @required on createSpawnEvent.
	// Before the fix, `nodeType: args.event.payload.node.type` evaluated to nil
	// in the sandbox (event not threaded), so the recorded payload had
	// nodeType=nil -- exactly the value that fails @required validation in the
	// live executor. After the fix (coalesce(...,"")), nodeType is a non-nil
	// string (at minimum ""), so @required is satisfied.
	for _, k := range []string{"nodeId", "nodeType", "action"} {
		v, ok := pl[k]
		if !ok {
			t.Errorf("createSpawnEvent arg %q missing from recorded payload %+v (#1671 regression)", k, pl)
			continue
		}
		if v == nil {
			t.Errorf("createSpawnEvent arg %q is nil (#1671 regression: bare path without coalesce): %+v", k, pl)
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("createSpawnEvent arg %q expected string, got %T (%v)", k, v, v)
		}
	}
}
