// Getting back to a local cluster that is already here (memql#3741).
//
// `memql.clusters.remove` takes the ROW, not the cluster -- correctly, and
// nothing said so. Until this action the only way back was the add-a-cluster
// form: four boxes about a cluster this editor installed itself, whose answers
// are all written down on the machine already.
//
// The property every test here protects is that NOTHING IS TYPED. A path that
// asked for one field would be the form this exists to remove, so the tests
// assert the composed entry, the fallback when the receipt is gone, and -- in
// the panel -- that the action never lands on a screen with an input on it.

import test from "node:test";
import assert from "node:assert/strict";

import { addClusterMenu } from "../src/clusters/presence.js";
import { offersReconnect, planLocalReconnect } from "../src/clusters/reconnect.js";
import { installedClusterEntry } from "../src/install/handoff.js";
import { emptyReceipt, type Receipt, type ReceiptEntry } from "../src/install/receipt.js";
import { DEFAULT_LOCAL_DOMAIN } from "../src/install/stackPin.js";
import { requiredFields } from "../src/state/addCluster.js";

function receiptWith(params: Record<string, string>): Receipt {
  const entry: ReceiptEntry = {
    stepId: "seedBootstrap",
    script: "install/seed-bootstrap.sh",
    receipt: "",
    preExisting: false,
    params,
    result: {},
    changed: true,
    recordedAt: "2026-08-01T00:00:00Z",
  };
  return { ...emptyReceipt("install", "2026-08-01T00:00:00Z"), entries: [entry] };
}

// -----------------------------------------------------------------------------
// the plan
// -----------------------------------------------------------------------------

test("the domain comes off the receipt, and the entry is the install's own", () => {
  const plan = planLocalReconnect(receiptWith({ domain: "lab.example.com" }));
  assert.equal(plan.domain, "lab.example.com");
  assert.equal(plan.fromReceipt, true);
  // IDENTICAL to what an install writes, including `local: true` -- two
  // spellings of that entry would be two answers to what a local cluster's row
  // looks like, and only one of them earns the uninstall action.
  assert.deepEqual(plan.entry, installedClusterEntry({ domain: "lab.example.com" }));
  assert.equal(plan.entry.local, true);
  assert.equal(plan.entry.endpoint, "api.lab.example.com:443");
});

test("a machine with no receipt still reconnects, on the installer's own default", () => {
  // The hand-built `make up` case: a cluster that answers, built by an operator
  // rather than by this wizard, so there is no receipt to read. The default is
  // not a guess -- it is what `make up` serves with no DOMAIN= override.
  const plan = planLocalReconnect(null);
  assert.equal(plan.domain, DEFAULT_LOCAL_DOMAIN);
  assert.equal(plan.fromReceipt, false);
  assert.equal(plan.entry.endpoint, `api.${DEFAULT_LOCAL_DOMAIN}:443`);
});

test("a receipt that recorded no domain falls back too", () => {
  const plan = planLocalReconnect(receiptWith({ tag: "v0.17.0" }));
  assert.equal(plan.domain, DEFAULT_LOCAL_DOMAIN);
  assert.equal(plan.fromReceipt, false);
});

// -----------------------------------------------------------------------------
// when it is offered
// -----------------------------------------------------------------------------

test("offered for both installed verdicts, and never for absent", () => {
  assert.equal(offersReconnect("installed-healthy", false), true);
  // A cluster that is here and not answering is still one the operator wants
  // listed -- that is where the repair action lives.
  assert.equal(offersReconnect("installed-unreachable", false), true);
  // Nothing installed: the card would register a row pointing at an address
  // nothing serves.
  assert.equal(offersReconnect("absent", false), false);
});

test("never offered when the cluster is already in the list", () => {
  for (const verdict of ["absent", "installed-healthy", "installed-unreachable"] as const) {
    assert.equal(offersReconnect(verdict, true), false, verdict);
  }
});

test("the card says what it will do without being clicked", () => {
  const card = addClusterMenu("installed-healthy", false).find((c) => c.action === "reconnect");
  assert.notEqual(card, undefined);
  assert.match(card?.label ?? "", /Connect to the local cluster/);
  // The promise the action has to keep, made on the card itself.
  assert.match(card?.detail ?? "", /nothing to type/);
});

// -----------------------------------------------------------------------------
// nothing is typed
// -----------------------------------------------------------------------------

test("reconnect collects no fields at all", () => {
  // A single field here would be the form this action exists to remove. The
  // other no-field actions are `connect` (its own form) and `uninstall` (its
  // own preview); reconnect has neither, which is the point.
  assert.deepEqual(requiredFields("reconnect"), []);
});
