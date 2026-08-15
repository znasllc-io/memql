// Claiming a cluster with the recovered magic link (znasllc-io/memql#3884).
//
// THE BUG THESE PIN. The `magicLink` step recovers the owner's sign-in link and
// puts it on `result.link`; nothing in the extension read it. That is not a
// missing convenience -- a cluster is claimed by its first sign-in, so the
// discarded link was the only thing that could create the owner account, and
// the install ended by telling the operator to use it.
//
// The second half matters as much as the first, and for the reason the
// enrolment tests give: the value comes from a shell script's stdout, and "open
// whatever URL the script printed" is a browser-navigation primitive handed to
// anything that can write to that pipe. This link authenticates as the cluster
// OWNER, so the validation is the load-bearing part.

import test from "node:test";
import assert from "node:assert/strict";

import {
  CLAIM_RESULT_FIELD,
  CLAIM_STATE_FIELD,
  CLAIM_STEP_ID,
  ClaimError,
  claimUrlFrom,
  claimWasRecovered,
  completeClaim,
  isClaimUrl,
  openClaimLink,
} from "../src/install/claim.js";
import type { ExecutionReport, StepOutcome } from "../src/install/executor.js";

const LINK = "https://identity.memql.localhost/auth/complete?ml=" + "a".repeat(43);

function outcome(over: Partial<StepOutcome> = {}): StepOutcome {
  return {
    id: CLAIM_STEP_ID,
    script: "install.magicLink",
    status: "ok",
    exitCode: 0,
    envelope: {
      ok: true,
      capability: "install.magicLink",
      changed: false,
      result: { [CLAIM_RESULT_FIELD]: LINK, [CLAIM_STATE_FIELD]: "recovered" },
      error: null,
    },
    verified: true,
    preExisting: false,
    params: {},
    startedAt: "2026-08-15T00:00:00.000Z",
    finishedAt: "2026-08-15T00:00:01.000Z",
    ...over,
  };
}

function report(outcomes: StepOutcome[]): ExecutionReport {
  return { graph: "install", ok: true, waves: [[CLAIM_STEP_ID]], outcomes };
}

function recorder() {
  const opened: string[] = [];
  return {
    opened,
    deps: {
      resolveExternalUri: (url: string) => url,
      openExternal: (url: string) => {
        opened.push(url);
      },
    },
  };
}

// ---------------------------------------------------------------------------
// reading it off the report -- the half that did not exist at all
// ---------------------------------------------------------------------------

test("the link is read off the magicLink step's envelope", () => {
  assert.equal(claimUrlFrom(report([outcome()])), LINK);
});

test("a step that did not run yields no link rather than a stale one", () => {
  assert.equal(claimUrlFrom(report([])), "");
  assert.equal(claimUrlFrom(report([outcome({ status: "failed", envelope: null })])), "");
  assert.equal(claimUrlFrom(report([outcome({ status: "skipped" })])), "");
});

test("recovering nothing is an ordinary outcome, distinguishable from not running", () => {
  // The step reports success with linkState=none when the log window held no
  // link -- a rotated log, or a cluster somebody already claimed. A caller has
  // to be able to tell that from a step that never ran, because the two ask the
  // operator for completely different next steps.
  const none = outcome({
    envelope: {
      ok: true,
      capability: "install.magicLink",
      changed: false,
      result: { [CLAIM_RESULT_FIELD]: "", [CLAIM_STATE_FIELD]: "none" },
      error: null,
    },
  });
  assert.equal(claimUrlFrom(report([none])), "");
  assert.equal(claimWasRecovered(report([none])), false);
  assert.equal(claimWasRecovered(report([outcome()])), true);
  assert.equal(claimWasRecovered(report([])), false);
});

// ---------------------------------------------------------------------------
// validation -- an owner credential is not opened on a script's say-so
// ---------------------------------------------------------------------------

test("a minted magic link validates", () => {
  assert.equal(isClaimUrl(LINK), true);
});

test("http is refused, because the link carries a bearer in its query", () => {
  assert.equal(isClaimUrl(LINK.replace("https:", "http:")), false);
});

test("some other URL is refused even over https", () => {
  // Not "a magic link we do not recognise" -- a value from somewhere this
  // module has no model of.
  assert.equal(isClaimUrl("https://example.com/"), false);
  assert.equal(isClaimUrl("https://identity.memql.localhost/enroll?code=x"), false);
  assert.equal(isClaimUrl("https://identity.memql.localhost/auth/complete"), false);
  assert.equal(isClaimUrl("https://identity.memql.localhost/auth/complete?ml="), false);
  assert.equal(isClaimUrl("not a url at all"), false);
});

test("a malformed value is refused before any browser is asked to open it", async () => {
  const r = recorder();
  await assert.rejects(
    () => openClaimLink("https://example.com/", r.deps),
    (err: unknown) => err instanceof ClaimError && err.reason === "malformed",
  );
  assert.deepEqual(r.opened, [], "nothing may be opened when validation refused the value");
});

// ---------------------------------------------------------------------------
// the whole step
// ---------------------------------------------------------------------------

test("a recovered link is opened, and returned so the caller can say so", async () => {
  const r = recorder();
  const returned = await completeClaim(report([outcome()]), r.deps);
  assert.equal(returned, LINK);
  assert.deepEqual(r.opened, [LINK]);
});

test("the two empty cases are told apart, because their remedies differ", async () => {
  const r = recorder();
  await assert.rejects(
    () => completeClaim(report([]), r.deps),
    (err: unknown) => err instanceof ClaimError && err.reason === "notRun",
  );
  const none = outcome({
    envelope: {
      ok: true,
      capability: "install.magicLink",
      changed: false,
      result: { [CLAIM_RESULT_FIELD]: "", [CLAIM_STATE_FIELD]: "none" },
      error: null,
    },
  });
  await assert.rejects(
    () => completeClaim(report([none]), r.deps),
    (err: unknown) => err instanceof ClaimError && err.reason === "noneRecovered",
  );
});

test("a host with no browser fails as browserUnavailable, not as a generic error", async () => {
  // A recoverable state: the caller's fallback is to show the operator the
  // link, and it can only choose that if the reason survives.
  const deps = {
    resolveExternalUri: (url: string) => url,
    openExternal: () => {
      throw new Error("no browser on this host");
    },
  };
  await assert.rejects(
    () => completeClaim(report([outcome()]), deps),
    (err: unknown) => err instanceof ClaimError && err.reason === "browserUnavailable",
  );
});

test("a failure never carries the credential back to the caller", async () => {
  // completeClaim returns the link on success only. A caller that needs it for
  // a fallback asks claimUrlFrom deliberately, rather than reaching into an
  // error that happened to close over a cluster-owner credential.
  const deps = {
    resolveExternalUri: (url: string) => url,
    openExternal: () => {
      throw new Error("no browser on this host");
    },
  };
  try {
    await completeClaim(report([outcome()]), deps);
    assert.fail("expected the open to fail");
  } catch (err) {
    const text = `${(err as Error).message} ${JSON.stringify((err as ClaimError).underlying ?? "")}`;
    assert.equal(text.includes("ml="), false, "the error text must not carry the token");
  }
});
