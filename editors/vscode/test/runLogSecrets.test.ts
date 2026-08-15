// What a step's RESULT may leave behind in the run log (znasllc-io/memql#3886).
//
// WHY THIS FILE EXISTS SEPARATELY FROM THE PARAM REDACTION TESTS. Putting step
// results into the run log is what makes a deployment's detail view useful, and
// it is also a brand new route from a capability script's stdout into a
// plaintext file that is written once, kept for fifty runs and never rewritten.
//
// The existing guard does not cover it and never claimed to: `redactSecrets`
// answers "did an operator paste a provider key into a path field", matches
// `^sk-` and says in its own comment that it is not a general secret detector.
// Two install steps return single-use credentials as ordinary result fields --
// `magic-link.sh` returns the owner's sign-in link, `enrolment-link.sh` returns
// an `mql_enr_` enrolment URL -- and both would sail straight through it.
//
// So these tests pin the withholding, and they are deliberately written as
// "this exact value must not appear", not as "the function was called".

import test from "node:test";
import assert from "node:assert/strict";

import { WITHHELD, redactResult, redactSecrets, withholdsFromRunLog } from "../src/install/secrets.js";

const MAGIC = "https://identity.memql.localhost/auth/complete?ml=" + "a".repeat(43);
const ENROL = "https://identity.memql.localhost/enroll?code=mql_enr_" + "b".repeat(43);

// ---------------------------------------------------------------------------
// the gap this closes
// ---------------------------------------------------------------------------

test("redactSecrets alone would have published both credentials", () => {
  // Not a criticism of that function -- it is the demonstration of why results
  // needed their own guard rather than being routed through the param one.
  const asParams = redactSecrets({ link: MAGIC, enrolUrl: ENROL });
  assert.equal(asParams.link, MAGIC);
  assert.equal(asParams.enrolUrl, ENROL);
});

test("the magic link never reaches the record", () => {
  const out = redactResult({ link: MAGIC, linkState: "recovered" });
  assert.equal(out.link, WITHHELD);
  assert.equal(JSON.stringify(out).includes("ml="), false);
});

test("the enrolment link never reaches the record", () => {
  const out = redactResult({ enrolUrl: ENROL, enrolmentState: "minted" });
  assert.equal(out.enrolUrl, WITHHELD);
  assert.equal(JSON.stringify(out).includes("mql_enr_"), false);
});

// ---------------------------------------------------------------------------
// two independent gates, so either one alone is enough
// ---------------------------------------------------------------------------

test("a credential-shaped NAME is withheld whatever the value looks like", () => {
  // Catches a future script returning something secret in a shape no value
  // test would flag. The camelCase entries are the ones that matter: the
  // obvious regex for "the word key" does not match the `Key` in `apiKey`,
  // because the character before it is a letter -- so the whole camelCase half
  // of the namespace used to slip past this gate and get caught, if at all, by
  // the value test instead.
  for (const name of [
    "token",
    "secret",
    "password",
    "apiKey",
    "authCode",
    "callbackUrl",
    "enrolUrl",
    "magic_link",
    "ACCESS-TOKEN",
  ]) {
    assert.equal(withholdsFromRunLog(name, "plain-looking"), true, name);
  }
});

test("an ordinary field name is not swept up by the word list", () => {
  // The gate has to be narrow enough that the status fields survive it --
  // otherwise the detail view is empty for a different reason.
  for (const name of ["namespace", "context", "target", "email", "candidates", "state"]) {
    assert.equal(withholdsFromRunLog(name, "plain"), false, name);
  }
});

test("the HEAD noun decides, so a state that describes a credential survives", () => {
  // `linkState` is a state, not a link, and it is the single field that would
  // have told an operator what happened. Matching any word rather than the head
  // would withhold it to protect the word "link" and leave the detail view as
  // empty as it was before.
  assert.equal(withholdsFromRunLog("linkState", "recovered"), false);
  assert.equal(withholdsFromRunLog("enrolmentState", "awaitingFirstSignIn"), false);
  assert.equal(withholdsFromRunLog("tokenCount", 3), false);
  // ...while the head noun still decides the other way when it is the credential.
  assert.equal(withholdsFromRunLog("stateToken", "abc"), true);
});

test("a credential-shaped VALUE is withheld whatever it is called", () => {
  for (const value of [MAGIC, ENROL, "sk-ant-abc123", "mql_enr_xyz", "x".repeat(200)]) {
    assert.equal(withholdsFromRunLog("harmless", value), true, value.slice(0, 24));
  }
});

// ---------------------------------------------------------------------------
// and the fields an operator actually needs still get through
// ---------------------------------------------------------------------------

test("the status fields that would have explained the dead end survive", () => {
  // These are the exact values that would have told the operator what happened:
  // there is no account yet, so sign in with the link first. Withholding them
  // would make the detail view useless in the one case it is most needed.
  const out = redactResult({
    ownerClaimed: false,
    enrolmentState: "awaitingFirstSignIn",
    linkState: "recovered",
    namespace: "memql",
    candidates: 3,
  });
  assert.equal(out.ownerClaimed, "false");
  assert.equal(out.enrolmentState, "awaitingFirstSignIn");
  assert.equal(out.linkState, "recovered");
  assert.equal(out.namespace, "memql");
  assert.equal(out.candidates, "3");
});

test("an email address is not treated as a credential", () => {
  // A fixture domain, not a real one. `TestNoVendorDomainLiterals` sweeps every
  // tracked file for the vendor domain, this file included -- the domain is a
  // value the operator supplies as `--domain`, and a literal in the tree is how
  // it stops being one.
  assert.equal(redactResult({ email: "owner@example.com" }).email, "owner@example.com");
});

test("nested values are summarised rather than walked", () => {
  // A walker would be one more place a credential could be reached through a
  // shape nobody anticipated, and the run log is a history an operator skims.
  const out = redactResult({ items: [1, 2, 3], nested: { link: MAGIC } });
  assert.equal(out.items, "[3 items]");
  assert.equal(out.nested, "{...}");
  assert.equal(JSON.stringify(out).includes("ml="), false);
});

test("absent values are dropped rather than written as the string 'undefined'", () => {
  const out = redactResult({ a: null, b: undefined, c: "kept" });
  assert.equal("a" in out, false);
  assert.equal("b" in out, false);
  assert.equal(out.c, "kept");
});

test("an empty string is a value, not a credential", () => {
  // linkState=none comes with link="" -- the empty link must not be reported
  // as withheld, which would say a credential was produced when none was.
  assert.equal(withholdsFromRunLog("anything", ""), false);
});
