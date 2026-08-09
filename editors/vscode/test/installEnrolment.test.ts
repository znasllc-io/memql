// The install wizard's enrolment step, host side (memql#3408).
//
// What these tests are really pinning is that the wizard OPENS the link
// instead of printing it, and that it refuses to open anything that is not a
// minted enrolment link. The second half matters more than it looks: the value
// comes from a shell script's stdout, and "open whatever URL the script
// printed" is a browser-navigation primitive handed to anything that can write
// to that pipe.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ENROLMENT_RESULT_FIELD,
  ENROLMENT_STEP_ID,
  EnrolmentError,
  completeEnrolment,
  enrolmentUrlFrom,
  isEnrolmentUrl,
  openEnrolmentLink,
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
      result: { [ENROLMENT_RESULT_FIELD]: LINK },
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
// Reading the link off the report
// ---------------------------------------------------------------------------

test("the link is read off the enrolment step's envelope", () => {
  assert.equal(enrolmentUrlFrom(report([outcome()])), LINK);
});

test("a step that did not run yields no link rather than a stale one", () => {
  assert.equal(enrolmentUrlFrom(report([])), "");
  assert.equal(enrolmentUrlFrom(report([outcome({ status: "failed", envelope: null })])), "");
  assert.equal(enrolmentUrlFrom(report([outcome({ status: "skipped" })])), "");
});

test("a step whose envelope carries no link yields empty, not undefined-as-string", () => {
  const empty = outcome();
  empty.envelope = { ...empty.envelope!, result: {} };
  assert.equal(enrolmentUrlFrom(report([empty])), "");
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
  const opened = await completeEnrolment(report([outcome()]), rec.deps);
  assert.equal(opened, LINK);
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

test("a skipped enrolment step is reported as notRun, a barren one as missing", async () => {
  const rec = recorder();
  await assert.rejects(
    () => completeEnrolment(report([]), rec.deps),
    (err: unknown) => err instanceof EnrolmentError && err.reason === "notRun",
  );
  const barren = outcome();
  barren.envelope = { ...barren.envelope!, result: {} };
  await assert.rejects(
    () => completeEnrolment(report([barren]), rec.deps),
    (err: unknown) => err instanceof EnrolmentError && err.reason === "missing",
  );
});
