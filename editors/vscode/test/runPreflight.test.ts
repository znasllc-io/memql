// The write confirmation, and what "once" means.
//
// The prompt only works if it is rare. Firing on every run of an
// iterate-fix-rerun loop trains the developer to dismiss it unread, at which
// point it protects nothing -- so the acknowledgement is remembered. But
// remembering it too broadly is the opposite failure: acknowledging "yes,
// write to staging" for one mutation must not silently pre-authorise a
// different mutation the developer has not thought about.

import test from "node:test";
import assert from "node:assert/strict";

import { WriteConfirmationGate, writeConfirmationMessage } from "../src/run/preflight.js";

test("required -- a read never prompts", () => {
  const gate = new WriteConfirmationGate();
  assert.equal(gate.required("query", false, "staging", "spaceParticipants"), false);
  assert.equal(gate.required("logic", false, "staging", "sweep"), false);
  assert.equal(gate.required("tool", false, "staging", "searchUsers"), false);
});

test("required -- a mutation against a LOCAL cluster never prompts", () => {
  const gate = new WriteConfirmationGate();
  assert.equal(gate.required("mutate", true, "local", "createSpace"), false);
});

test("required -- a mutation against a non-local cluster prompts ONCE", () => {
  const gate = new WriteConfirmationGate();
  assert.equal(gate.required("mutate", false, "staging", "createSpace"), true);
  gate.acknowledge("staging", "createSpace");
  assert.equal(
    gate.required("mutate", false, "staging", "createSpace"),
    false,
    "a second run of the same mutation on the same cluster must not re-prompt",
  );
});

test("required -- acknowledging one mutation does not pre-authorise another", () => {
  const gate = new WriteConfirmationGate();
  gate.acknowledge("staging", "createSpace");
  assert.equal(gate.required("mutate", false, "staging", "deleteSpace"), true);
});

test("required -- acknowledging on one cluster does not carry to another", () => {
  const gate = new WriteConfirmationGate();
  gate.acknowledge("staging", "createSpace");
  assert.equal(gate.required("mutate", false, "prod", "createSpace"), true);
});

test("required -- an ABSENT local flag means not local", () => {
  // Every cluster already in an operator's clusters.yaml predates the field.
  // Defaulting those to "local" would silently disable the confirmation on
  // exactly the clusters -- staging, production -- it exists for.
  const gate = new WriteConfirmationGate();
  assert.equal(gate.required("mutate", false, "unmarked", "createSpace"), true);
});

test("acknowledgement keys cannot collide across a separator", () => {
  // A plain ":" join would let ("a:b", "c") and ("a", "b:c") share one
  // acknowledgement, so confirming a write on one cluster would silently
  // authorise a differently-named one.
  const gate = new WriteConfirmationGate();
  gate.acknowledge("a:b", "c");
  assert.equal(gate.required("mutate", false, "a", "b:c"), true);
});

test("reset -- clears acknowledgements when the cluster registry changes", () => {
  // An operator who re-points a cluster NAME at a different endpoint would
  // otherwise carry an acknowledgement over to a cluster they confirmed
  // nothing about.
  const gate = new WriteConfirmationGate();
  gate.acknowledge("staging", "createSpace");
  gate.reset();
  assert.equal(gate.required("mutate", false, "staging", "createSpace"), true);
});

test("writeConfirmationMessage -- names both the cluster and the construct", () => {
  // "Are you sure?" tells the developer nothing they can check. The claim has
  // to be one they can immediately recognise as right or wrong.
  const message = writeConfirmationMessage({
    clusterName: "staging",
    clusterLabel: "memQL Staging",
    constructName: "createSpace",
    constructKind: "mutate",
  });
  assert.match(message, /createSpace/);
  assert.match(message, /memQL Staging/);
  assert.match(message, /not marked local/);
});
