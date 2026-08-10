// The collect screen's defaults and its secret hygiene (memql#3473).
//
// Separate from addClusterState.test.ts, which owns the machine's transitions.
// These two properties are about what the FORM does rather than where the
// wizard goes, and both are the kind that pass silently while being wrong:
//
//   - a default that disagrees with the scripts it is about to run produces an
//     install that completes and then cannot be reached;
//   - a validation message that quotes the value it rejected is how a path --
//     or one day a secret -- ends up in a log nobody meant to write it to.

import test from "node:test";
import assert from "node:assert/strict";

import {
  AddClusterState,
  DEFAULT_INPUTS,
  requiredFields,
  type InputField,
} from "../src/state/addCluster.js";

// DERIVED, never hand-listed. A hand-maintained array leaves any field added
// later silently uncovered by the secret-hygiene test below -- which is the one
// test whose whole value is that it keeps holding for fields nobody has written
// yet. Reading the keys off the defaults means a new field is covered the
// moment it exists.
const ALL_FIELDS = Object.keys(DEFAULT_INPUTS) as InputField[];

// ---------------------------------------------------------------------------
// defaults
// ---------------------------------------------------------------------------

test("the form opens pre-filled with the installer's own domain default", () => {
  // Not invented here: scripts/install/hosts-entries.sh writes
  // cockpit.local.znas.io,identity.local.znas.io,local.znas.io when given no
  // --hostnames, and verify-frontdoor.sh checks the same hosts. A different
  // value would make the form disagree with the scripts it is about to run.
  const state = new AddClusterState();
  assert.equal(state.inputs.domain, "local.znas.io");
  assert.equal(DEFAULT_INPUTS.domain, "local.znas.io");
});

test("a repair can be started without typing anything at all", () => {
  // A repair needs only the domain, and the domain now has a default -- so the
  // whole form is acceptable as-is. This is the concrete form of "the operator
  // can accept them all without typing".
  const state = new AddClusterState();
  state.chooseAction("repair");
  assert.deepEqual(state.validate(), []);
  assert.equal(state.beginRun(), true);
  assert.equal(state.screen, "running");
});

test("an install still refuses to start on the fields only a person can supply", () => {
  // The default must not become a way to skip the four fields that genuinely
  // cannot be guessed. A wizard that started an install with a blank owner
  // would spend nine minutes reaching seed-bootstrap.sh's exit 2.
  const state = new AddClusterState();
  state.chooseAction("install");
  assert.equal(state.beginRun(), false);
  const missing = new Set(state.errors.map((e) => e.field));
  assert.ok(!missing.has("domain"), "the pre-filled domain should not be reported missing");
  for (const field of ["ownerFirstName", "ownerLastName", "ownerEmail", "providerKeyFile"] as const) {
    assert.ok(missing.has(field), `${field} was not required`);
  }
});

test("the four personal fields are deliberately blank", () => {
  // Guessing a name, an email or where somebody keeps an API key is either
  // wrong or -- on a shared machine -- somebody else's details.
  assert.equal(DEFAULT_INPUTS.ownerFirstName, "");
  assert.equal(DEFAULT_INPUTS.ownerLastName, "");
  assert.equal(DEFAULT_INPUTS.ownerEmail, "");
  assert.equal(DEFAULT_INPUTS.providerKeyFile, "");
});

test("the default is a starting value, not a floor -- it can be replaced or cleared", () => {
  // "Offered rather than skippable" (design D5) cuts both ways: the operator
  // must be able to overwrite it, and clearing it must still fail validation
  // rather than silently snapping back to the default.
  const state = new AddClusterState();
  state.chooseAction("repair");
  state.setInput("domain", "memql.example.com");
  assert.equal(state.inputs.domain, "memql.example.com");

  state.setInput("domain", "");
  assert.equal(state.beginRun(), false);
  assert.ok(state.errors.some((e) => e.field === "domain"));
});

// ---------------------------------------------------------------------------
// secret hygiene
// ---------------------------------------------------------------------------

test("no validation message ever quotes the value it rejected", () => {
  // THE ONE THAT MATTERS. providerKeyFile is a path today, but it is the field
  // that sits next to an API key, and the natural way to write a friendlier
  // error -- `That path (${value}) does not exist` -- is exactly how a value
  // reaches a log, a telemetry payload or a screenshot. Refusing to interpolate
  // values at all is a rule that cannot be got wrong by degrees.
  //
  // BOTH MESSAGE SOURCES ARE DRIVEN, and separately, because filling every
  // field makes the required-field branch unreachable -- an earlier version of
  // this test filled everything and so only ever exercised one shape error.
  const sentinel = "ZZ-do-not-echo-me-9f3b7c2a";

  // (a) shape errors: a value that is present and wrong. The trailing space
  // trips the domain rule as well as the email one, so more than a single
  // field produces a message.
  const shaped = new AddClusterState();
  shaped.chooseAction("install");
  for (const field of ALL_FIELDS) shaped.setInput(field, `${sentinel} x`);
  assert.ok(shaped.errors.length > 0, "no shape errors were produced to check");
  for (const error of shaped.errors) {
    assert.ok(
      !error.message.includes(sentinel),
      `${error.field}'s shape message echoed the value: ${error.message}`,
    );
  }

  // (b) required-field errors: the branch (a) cannot reach. Every field left
  // empty, then forced through validate() by attempting to start.
  const empty = new AddClusterState();
  empty.chooseAction("install");
  for (const field of ALL_FIELDS) empty.setInput(field, "");
  assert.equal(empty.beginRun(), false);
  assert.ok(empty.errors.length > 0, "no required-field errors were produced to check");
  for (const error of empty.errors) {
    assert.ok(
      !error.message.includes(sentinel),
      `${error.field}'s required message echoed a value: ${error.message}`,
    );
  }
});

test("a rejected email is refused without repeating the address", () => {
  // The specific case the general rule above is protecting: an invalid email
  // is the message most likely to be written as "X is not valid".
  const state = new AddClusterState();
  state.chooseAction("install");
  state.setInput("ownerEmail", "not-an-address");

  const error = state.errors.find((e) => e.field === "ownerEmail");
  assert.ok(error !== undefined, "an invalid email produced no error");
  assert.ok(!error.message.includes("not-an-address"));
});

test("the provider key field carries a PATH, and the schema says so", () => {
  // The structural half of "the key never reaches a log, an error message or
  // the receipt": nothing in this module ever holds the key. SessionOptions
  // documents the same constraint for the same reason -- argv is world-readable
  // in `ps`.
  assert.ok(requiredFields("install").includes("providerKeyFile"));
  assert.ok(!Object.keys(DEFAULT_INPUTS).some((k) => /secret|token|apiKey|password/i.test(k)));
});
