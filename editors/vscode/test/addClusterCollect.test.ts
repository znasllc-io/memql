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
  // api.memql.localhost, identity.memql.localhost, portal.memql.localhost and
  // the apex when given no --hostnames, and verify-frontdoor.sh probes the
  // first two. A different value would make the form disagree with the scripts
  // it is about to run.
  const state = new AddClusterState();
  assert.equal(state.inputs.domain, "memql.localhost");
  assert.equal(DEFAULT_INPUTS.domain, "memql.localhost");
});

test("a repair can be started without typing anything, once the receipt is in", () => {
  // A repair asks for the domain (defaulted) and the owner -- and the panel
  // pre-fills the owner from the receipt before the operator sees the form, so
  // in practice nothing is typed (memql#3544).
  //
  // The OWNER is the category this module cannot default (znasllc-io#3888):
  // three values the panel pre-fills from the receipt. A repair that reached
  // `seedBootstrap` without them died at `exit 2` naming values no box offered.
  //
  // THE KEY PATH IS NO LONGER IN THAT LIST (epic memql#4440, removed outright
  // by epic memql#5088). It used to be the one thing standing between a fresh
  // state and a startable repair, on the reasoning that the run could not pass
  // wave 2 without it. It can now: `providerFederation` skips satisfied, always,
  // because a local cluster cannot federate. Keeping it required would have
  // made repair unreachable for every cluster there now is.
  const state = new AddClusterState();
  state.chooseAction("repair");
  assert.deepEqual(
    state.validate().map((e) => e.field),
    ["ownerFirstName", "ownerLastName", "ownerEmail"],
    "the domain is defaulted; the owner is not",
  );

  state.setInput("ownerFirstName", "Ada");
  state.setInput("ownerLastName", "Lovelace");
  state.setInput("ownerEmail", "owner@example.com");
  assert.deepEqual(state.validate(), [], "the owner is all a repair asks for");
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
  for (const field of ["ownerFirstName", "ownerLastName", "ownerEmail"] as const) {
    assert.ok(missing.has(field), `${field} was not required`);
  }
  // AND NOTHING ELSE IS REQUIRED, which is where a vendor key would reappear
  // (epic memql#4440, epic memql#5088). The three above genuinely cannot be
  // guessed and seed-bootstrap.sh genuinely refuses without them. Asserted as a
  // SET rather than as the absence of one field name, so a credential re-added
  // under any name fails here.
  assert.deepEqual(
    [...missing].sort(),
    ["ownerEmail", "ownerFirstName", "ownerLastName"],
    "an install refused to start over a field it does not need",
  );
});

test("the three personal fields are deliberately blank", () => {
  // Guessing a name or an email is either wrong or -- on a shared machine --
  // somebody else's details. (It was FOUR until epic memql#5088; the fourth
  // was where somebody keeps an API key, and there is no such key now.)
  assert.equal(DEFAULT_INPUTS.ownerFirstName, "");
  assert.equal(DEFAULT_INPUTS.ownerLastName, "");
  assert.equal(DEFAULT_INPUTS.ownerEmail, "");
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
  // THE RULE OUTLIVES THE FIELD THAT MOTIVATED IT. It was written for
  // `providerKeyFile`, the box that sat next to an API key, where the natural
  // friendlier error -- `That path (${value}) does not exist` -- is exactly how
  // a value reaches a log, a telemetry payload or a screenshot. That field is
  // gone (epic memql#5088) and the rule is not: an owner email is a personal
  // detail and a domain can carry a company name. Refusing to interpolate
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

test("no collected field is an AI credential, and the schema says so", () => {
  // THE STRUCTURAL CLAIM, WIDENED (epic memql#4440 -> epic memql#5088).
  //
  // It used to be "the key field carries a PATH, never the key" -- nothing in
  // this module ever holds a secret, because argv is world-readable in `ps`.
  // There is no key field now, so the claim it becomes is stronger and simpler:
  // this module holds no AI credential of any kind, under any name.
  //
  // ASSERTED OVER `Inputs` ITSELF rather than over a list of banned field
  // names. `DEFAULT_INPUTS` has a key for every field the wizard collects, so a
  // field re-added tomorrow is caught whatever it is called -- which a
  // hard-coded absence check (`!includes("providerKeyFile")`) would not be.
  const fields = Object.keys(DEFAULT_INPUTS);
  assert.ok(!fields.some((k) => /secret|token|apiKey|password|provider|vendor|key/i.test(k)),
    `a collected field looks like an AI credential: ${fields.join(", ")}`);
  // And the same over the two lists a screen actually renders from, since a
  // field could be offered without a default.
  for (const action of ["install", "repair"] as const) {
    assert.ok(!requiredFields(action).some((f) => /provider|key|vendor|secret/i.test(f)),
      `${action} requires a field that looks like an AI credential`);
  }
});

// -----------------------------------------------------------------------------
// THE KEY-FILE SECTION IS DELETED (epic memql#5088)
// -----------------------------------------------------------------------------
//
// Four cases lived here and all four were about one field, `providerKeyFile`:
// that pasting an Anthropic key into it was refused, that an OpenAI key was
// refused too, that an ordinary path was accepted, and that a repair collected
// the field again so a bad recorded value could be corrected (memql#3544,
// memql#3545).
//
// There is no such field. Both cloud vendors are reached by workload identity
// federation, and a cluster this wizard builds could not use even that -- a k3d
// cluster's OIDC issuer is not publicly reachable, so no vendor can verify a
// token minted by it.
//
// THE REFUSAL THEY TESTED IS NOT ENTIRELY GONE, and it is worth knowing where
// it went. memql#3545 built two walls: the validator here, which could tell a
// person what to do instead, and `redactSecrets` on the receipt and run-log
// WRITE, which covers every other way a param can reach a file. The second
// stands, and `receiptSecrets.test.ts` and `runLogSecrets.test.ts` still hold
// it. Only the wall with a field behind it has been removed.
//
// What replaces the structural half is the case above: no collected field is
// an AI credential, asserted over `Inputs` rather than over a name.
