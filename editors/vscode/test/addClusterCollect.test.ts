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
  optionalFields,
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
  // THE KEY PATH IS NO LONGER IN THAT LIST (epic memql#4440). It used to be
  // the one thing standing between a fresh state and a startable repair, on
  // the reasoning that the run could not pass wave 2 without it. It can now:
  // `providerKey` skips, satisfied, when no key was supplied. Keeping it
  // required would have made repair unreachable for every cluster installed
  // the way this product now recommends -- with no key at all.
  const state = new AddClusterState();
  state.chooseAction("repair");
  assert.deepEqual(
    state.validate().map((e) => e.field),
    ["ownerFirstName", "ownerLastName", "ownerEmail"],
    "the domain and the vendor are defaulted; the owner is not",
  );

  state.setInput("ownerFirstName", "Ada");
  state.setInput("ownerLastName", "Lovelace");
  state.setInput("ownerEmail", "owner@example.com");
  assert.deepEqual(state.validate(), [], "a repair still wants a key path");
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
  // AND THE KEY PATH IS NOT ONE OF THEM (epic memql#4440). The three above
  // genuinely cannot be guessed and seed-bootstrap.sh genuinely refuses
  // without them; a vendor key is neither -- nothing in install, start,
  // repair, upgrade or uninstall reads it.
  assert.ok(
    !missing.has("providerKeyFile"),
    "an install refused to start over an AI provider key it does not need",
  );
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
  // COLLECTED, not required (epic memql#4440) -- the structural claim is
  // about what this module HOLDS, which is unchanged by the demotion. If
  // anything the claim got stronger: the field is now one an operator can
  // leave empty entirely.
  assert.ok(optionalFields("install").includes("providerKeyFile"));
  assert.ok(!Object.keys(DEFAULT_INPUTS).some((k) => /secret|token|apiKey|password/i.test(k)));
});

// -----------------------------------------------------------------------------
// The key FILE field must hold a path, and a repair must be able to fix it
// (memql#3544 / memql#3545)
// -----------------------------------------------------------------------------

test("pasting the provider key itself into the key-FILE box is refused", () => {
  // WHAT ACTUALLY HAPPENED. The field's hint says "A PATH to a file holding the
  // key, never the key itself", and nothing enforced it -- so an operator who
  // pasted their Anthropic key had it accepted, passed to the script as
  // `--key-file=sk-ant-...` (argv, which `ps` shows to every process on the
  // machine) and then RECORDED IN THE INSTALL RECEIPT, where it sat in
  // plaintext afterwards.
  //
  // A hint is not a control. The value that cannot be allowed to reach argv is
  // refused where it is typed.
  const state = new AddClusterState();
  state.chooseAction("install");
  state.setInput("providerKeyFile", "sk-ant-api03-EXAMPLE-not-a-real-key-aaaaaaaaaaaaaaaaaaaa");

  const problem = state.errors.find((e) => e.field === "providerKeyFile");
  assert.ok(problem !== undefined, "a pasted key must be refused, not accepted");
  assert.match(problem.message, /path/i, "and the refusal must say what is wanted instead");
  // The message must not quote the value back: this whole test is about a
  // secret, and a validation message is rendered into HTML and is the kind of
  // thing that ends up in a screenshot.
  assert.doesNotMatch(problem.message, /sk-ant/, "the refusal must not echo the secret");
});

test("an OpenAI key pasted into the same box is refused too", () => {
  // The vendor is collected separately, so the refusal cannot key off it. Both
  // supported vendors' key formats are recognised.
  const state = new AddClusterState();
  state.chooseAction("install");
  state.setInput("providerKeyFile", "sk-proj-EXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLE");
  assert.ok(state.errors.some((e) => e.field === "providerKeyFile"));
});

test("an ordinary path is accepted", () => {
  const state = new AddClusterState();
  state.chooseAction("install");
  state.setInput("providerKeyFile", "/home/someone/.memql/key");
  assert.deepEqual(state.errors, []);
});

test("a repair collects the provider key again, so a bad one can be corrected", () => {
  // THE DEAD END. `requiredFields("repair")` was `["domain"]`, on the reasoning
  // that a repair re-runs a graph over a machine that already answered these
  // questions -- and the answer it reads back is the one on the receipt. When
  // the recorded answer is the thing that is WRONG, that reasoning inverts: the
  // repair re-runs with the same bad value, fails at the same step, and offers
  // the operator no field in which to fix it. Every repair, forever.
  //
  // The receipt still supplies the DEFAULT (memql#3512's point stands -- nobody
  // should retype a good path), but it is now a starting value in a box rather
  // than a locked-in one.
  // COLLECTED, NOT REQUIRED (epic memql#4440). memql#3544's dead end was
  // "the box is not there to fix a bad value", and an OPTIONAL box fixes that
  // just as completely as a required one -- the operator can still edit the
  // pre-filled path. What a REQUIREMENT would additionally do is refuse a
  // repair on every cluster that was installed with no key at all, which
  // after this epic is most of them.
  assert.ok(
    optionalFields("repair").includes("providerKeyFile"),
    "a repair must be able to correct the key path it is about to reuse",
  );
  assert.ok(
    optionalFields("repair").includes("provider"),
    "and the vendor that path is verified against",
  );
  assert.ok(
    !requiredFields("repair").includes("providerKeyFile"),
    "a repair must not demand a key the install was never asked for",
  );
});
