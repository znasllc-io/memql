// Whether the run log is disclosed, held in state and not only in the DOM
// (memql#4455).
//
// The page opens and closes the disclosure itself; the state module records
// which way, so the next document the page is given (a reload, a theme change)
// draws it the same. These assertions are about STATE transitions rather than
// about a click.
//
// AND WHY FAILURE IS THE EXCEPTION. Collapsing the log is for the twelve
// minutes an install is going well. At the moment something breaks, the log IS
// the product, and making the operator find a toggle first would be design
// spite -- so the state module opens it.
//
// Refs: #4455 #4194 #4452

import test from "node:test";
import assert from "node:assert/strict";

import { AddClusterState } from "../src/state/addCluster.js";
import { UninstallRunState } from "../src/state/uninstallRun.js";

/** The executor events the state folds, in the shape `apply` reads. */
function started(id: string, description: string): never {
  return { type: "stepStarted", step: { id, description } } as never;
}
function finished(id: string, description: string, status: string, exitCode: number): never {
  return {
    type: "stepFinished",
    step: { id, description },
    outcome: { status, exitCode, reason: "", envelope: {} },
  } as never;
}

test("the log starts closed, because a run going well is not a log", () => {
  const state = new AddClusterState();
  assert.equal(state.logsOpen, false);
});

test("the page's open and close are recorded, so a repaint draws them the same", () => {
  const state = new AddClusterState();
  state.setLogsOpen(true);
  assert.equal(state.logsOpen, true);
  state.setLogsOpen(false);
  assert.equal(state.logsOpen, false);
});

test("A STEP FAILING OPENS THE LOG", () => {
  const state = new AddClusterState();
  assert.equal(state.logsOpen, false);
  state.apply(started("clusterUp", "Creating the cluster"));
  assert.equal(state.logsOpen, false, "a step merely running discloses nothing");
  state.apply(finished("clusterUp", "Creating the cluster", "failed", 5));
  assert.equal(state.logsOpen, true, "at failure the log is the product");
});

test("a new run starts closed, rather than on the last run's output", () => {
  const state = new AddClusterState();
  state.apply(finished("clusterUp", "Creating the cluster", "failed", 5));
  assert.equal(state.logsOpen, true);

  state.chooseAction("install");
  for (const [field, value] of [
    ["domain", "memql.localhost"],
    ["ownerFirstName", "A"],
    ["ownerLastName", "B"],
    ["ownerEmail", "a@b.test"],
    ["version", "v0.19.1"],
  ] as const) {
    state.setInput(field, value);
  }
  assert.equal(state.beginRun(), true, "the form is complete");
  assert.equal(state.logsOpen, false, "the disclosure belonged to the failure, not to the operator");
});

test("an uninstall holds its own disclosure, and discloses on its own failure", () => {
  // A SECOND FIELD, not a shared one: they are two different runs with two
  // different step lists, and an operator who opened the log on a failed
  // install should meet a closed one when they start a removal.
  const uninstall = new UninstallRunState();
  assert.equal(uninstall.logsOpen, false);
  uninstall.apply(finished("removeCluster", "Removing the cluster", "failed", 3));
  assert.equal(uninstall.logsOpen, true);

  uninstall.begin();
  assert.equal(uninstall.logsOpen, false, "a fresh removal starts closed");
});
