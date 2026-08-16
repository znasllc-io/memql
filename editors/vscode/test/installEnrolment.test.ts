// The install wizard's enrolment step, host side (memql#3408, memql#3906).
//
// What these tests are really pinning is that the wizard OPENS a link instead
// of printing it, and that it refuses to open anything that is not a minted
// enrolment link. The second half matters more than it looks: the value comes
// from a shell script's stdout, and "open whatever URL the script printed" is a
// browser-navigation primitive handed to anything that can write to that pipe.
//
// The reader half changed in memql#3906. This module used to lift the run's
// minted link off the report so the done screen could replay it; a single-use,
// 15-minute credential was the wrong thing to hold on a screen that can stay
// open for days. What it reads now is the durable fact -- whether there is an
// owner ACCOUNT to enrol against -- and the link is minted at click time.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ENROLMENT_STEP_ID,
  EnrolmentError,
  OWNER_CLAIMED_FIELD,
  isEnrolmentUrl,
  openEnrolmentLink,
  ownerAccountExistsFrom,
} from "../src/install/enrolment.js";
import type { ExecutionReport, StepOutcome } from "../src/install/executor.js";

const LINK = "https://identity.example.com/enroll?code=mql_enr_" + "a".repeat(43);

function outcome(over: Partial<StepOutcome> = {}): StepOutcome {
  return {
    id: ENROLMENT_STEP_ID,
    script: "install.enrolmentLink",
    status: "ok",
    exitCode: 0,
    envelope: {
      ok: true,
      capability: "install.enrolmentLink",
      changed: true,
      result: { [OWNER_CLAIMED_FIELD]: true, enrolmentState: "minted" },
      error: null,
    },
    verified: true,
    preExisting: false,
    params: {},
    startedAt: "2026-08-09T00:00:00.000Z",
    finishedAt: "2026-08-09T00:00:01.000Z",
    ...over,
  };
}

function report(outcomes: StepOutcome[]): ExecutionReport {
  return { graph: "install", ok: true, waves: [[ENROLMENT_STEP_ID]], outcomes };
}

/** Records what was asked to open, so a test can assert the URL, not the call count. */
function recorder() {
  const opened: string[] = [];
  const resolved: string[] = [];
  return {
    opened,
    resolved,
    deps: {
      resolveExternalUri: (url: string) => {
        resolved.push(url);
        return url;
      },
      openExternal: (url: string) => {
        opened.push(url);
      },
    },
  };
}

// ---------------------------------------------------------------------------
// Reading the OWNER ACCOUNT off the report
// ---------------------------------------------------------------------------

test("ownerClaimed off the enrolment step is what says enrolment is possible", () => {
  assert.equal(ownerAccountExistsFrom(report([outcome()])), true);
});

test("a cluster with no owner yet reports false, and gets the claim route instead", () => {
  // `enrolment-link.sh` reports ownerClaimed=false / enrolmentState=
  // awaitingFirstSignIn when nothing has claimed the cluster. That is the
  // hand-rolled case: there is no account to enrol a passkey for, so the done
  // screen leads with the magic link instead.
  const unclaimed = outcome();
  unclaimed.envelope = {
    ...unclaimed.envelope!,
    result: { [OWNER_CLAIMED_FIELD]: false, enrolmentState: "awaitingFirstSignIn" },
  };
  assert.equal(ownerAccountExistsFrom(report([unclaimed])), false);
});

test("a step that did not run establishes nothing, and says so", () => {
  // Not an error. It costs the operator the OFFER, not the capability --
  // memql.clusters.takeOwnership is reachable from the Clusters tree whatever
  // any one run managed to find out.
  assert.equal(ownerAccountExistsFrom(report([])), false);
  assert.equal(ownerAccountExistsFrom(report([outcome({ status: "failed", envelope: null })])), false);
  assert.equal(ownerAccountExistsFrom(report([outcome({ status: "skipped" })])), false);
});

test("a missing field is false, not truthy-by-accident", () => {
  const empty = outcome();
  empty.envelope = { ...empty.envelope!, result: {} };
  assert.equal(ownerAccountExistsFrom(report([empty])), false);
  // The string "false" is what a shell that forgot cap_result_set_raw would
  // emit, and it must not read as an owner.
  const stringly = outcome();
  stringly.envelope = { ...stringly.envelope!, result: { [OWNER_CLAIMED_FIELD]: "false" } };
  assert.equal(ownerAccountExistsFrom(report([stringly])), false);
});

// ---------------------------------------------------------------------------
// What counts as an enrolment link
// ---------------------------------------------------------------------------

test("a minted https /enroll link is accepted", () => {
  assert.ok(isEnrolmentUrl(LINK));
});

test("http is refused -- the link carries a bearer in its query string", () => {
  assert.ok(!isEnrolmentUrl("http://identity.example.com/enroll?code=mql_enr_x"));
});

test("a different path is refused, however plausible the host", () => {
  assert.ok(!isEnrolmentUrl("https://identity.example.com/login?code=mql_enr_x"));
  assert.ok(!isEnrolmentUrl("https://identity.example.com/"));
});

test("an /enroll URL with no code is refused", () => {
  assert.ok(!isEnrolmentUrl("https://identity.example.com/enroll"));
  assert.ok(!isEnrolmentUrl("https://identity.example.com/enroll?code="));
});

test("a non-URL is refused rather than thrown on", () => {
  assert.ok(!isEnrolmentUrl(""));
  assert.ok(!isEnrolmentUrl("enrol here please"));
});

// ---------------------------------------------------------------------------
// Opening
// ---------------------------------------------------------------------------

test("the operator copies nothing -- the link is opened for them", async () => {
  const rec = recorder();
  await openEnrolmentLink(LINK, rec.deps);
  assert.deepEqual(rec.opened, [LINK]);
});

test("asExternalUri runs first, so the flow survives Remote-SSH and Codespaces", async () => {
  const seen: string[] = [];
  const forwarded = "https://forwarded.example.dev/enroll?code=mql_enr_x";
  await openEnrolmentLink(LINK, {
    resolveExternalUri: (url: string) => {
      seen.push(url);
      return forwarded;
    },
    openExternal: (url: string) => {
      seen.push(url);
    },
  });
  assert.deepEqual(seen, [LINK, forwarded], "the resolved URI must be what gets opened");
});

test("a value that is not an enrolment link is never opened", async () => {
  const rec = recorder();
  await assert.rejects(
    () => openEnrolmentLink("https://evil.example.com/pwn", rec.deps),
    (err: unknown) => err instanceof EnrolmentError && err.reason === "malformed",
  );
  assert.deepEqual(rec.opened, [], "nothing may reach the browser after a malformed verdict");
});

test("no browser on this host is browserUnavailable, not a generic failure", async () => {
  await assert.rejects(
    () =>
      openEnrolmentLink(LINK, {
        resolveExternalUri: (url: string) => url,
        openExternal: () => {
          throw new Error("no display");
        },
      }),
    (err: unknown) => err instanceof EnrolmentError && err.reason === "browserUnavailable",
  );
});

test("the opener is the only gate a minted link passes through", async () => {
  // Every route into ownership -- the palette command, the Clusters tree, the
  // install's done screen -- reaches the browser through openEnrolmentLink, so
  // the https + `/enroll?code=` check above cannot be bypassed by adding a
  // fourth entry point. A second opener is the regression worth naming.
  const rec = recorder();
  await assert.rejects(
    () => openEnrolmentLink("https://identity.example.com/enroll", rec.deps),
    (err: unknown) => err instanceof EnrolmentError && err.reason === "malformed",
  );
  assert.deepEqual(rec.opened, []);
});
