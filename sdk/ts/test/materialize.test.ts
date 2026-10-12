// The Materializer front door: the one client entry point for creating a file
// (docs/superpowers/specs/2026-10-11-materializer-front-door-design.md, section 2).
//
// Every test runs against a fake `execute`, the way the OS and the editors
// will supply it, and asserts on the CALL STRINGS it receives. The properties
// worth pinning are the ones a green happy path hides:
//
//   * the hand-off request ids must satisfy the Library's own rules
//     (integrations/library/review.go reviewRequestID, ^[a-zA-Z0-9_-]{8,120}$),
//     or every real hand-off is refused while every fake-backed test passes;
//   * a follow must not wait forever on a run that has died, and must not
//     leave an abort listener behind per poll;
//   * the hand-off never throws, because the file exists and is good.

import test from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { getEventListeners } from "node:events";

import {
  followComposition,
  handOffDescription,
  isDescribedMarkdown,
  materialize,
  startMaterialize,
  MaterializeCancelled,
  MaterializeFailed,
  type MaterializeExecute,
  type MaterializeRequest,
  type MaterializeSource,
} from "../src/client/materialize.js";
import {
  buildComposeMaterialize,
  buildLibraryAddDocumentComment,
  buildLibraryRequestDocumentRevision,
} from "../src/client/generated_builtins.js";
import * as client from "../src/client/index.js";

type Rows = Record<string, unknown>[];
// A queue answers call after call (the last entry repeats); a function answers
// from the call string, so a test need not depend on the order calls are made.
type Answer = Rows[] | ((call: string) => Rows);

function fake(script: Record<string, Answer>, limit = 300) {
  const calls: { name: string; call: string }[] = [];
  const execute: MaterializeExecute = async (name, call) => {
    calls.push({ name, call });
    // A loop that never ends is the failure several tests exist to catch; end
    // it with a message instead of hanging the run.
    if (calls.length > limit) throw new Error(`more than ${limit} calls: the code under test polls without end`);
    const answer = script[name];
    if (answer === undefined) return [];
    if (typeof answer === "function") return answer(call);
    return answer.length > 1 ? (answer.shift() ?? []) : (answer[0] ?? []);
  };
  return { calls, execute, named: (name: string) => calls.filter(c => c.name === name) };
}

const fileIdIn = (call: string) => /fileId:\s*"([^"]*)"/.exec(call)?.[1] ?? "";
const requestIdIn = (call: string) => /requestId:\s*"([^"]*)"/.exec(call)?.[1] ?? "";

// What the Library accepts as a request id (integrations/library/review.go).
const LIBRARY_REQUEST_ID = /^[a-zA-Z0-9_-]{8,120}$/;

// A refusal AS IT REALLY ARRIVES. The server wraps an internal engine error in
// "MemQL engine failed to execute query. Details: " (buildEngineErrorMessage,
// component/grpc/server.go), and executeNamed puts the call's name in front
// (query.ts). A fake that threw the bare reason would pass code that shows a
// person both prefixes.
const ENGINE = "MemQL engine failed to execute query. Details: ";
const refusal = (construct: string, reason: string): Error => new Error(`${construct}: ${ENGINE}${reason}`);

// ---------------------------------------------------------------------------
// Starting and following a composition
// ---------------------------------------------------------------------------

test("materialize starts once, follows to ready, and resolves the artifact", async () => {
  const f = fake({
    composeMaterialize: [[{ compositionId: "c1" }]],
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Notes" }]],
    libraryArtifactForFile: [[], [{ id: "a1", title: "Notes.md" }]],
  });
  const seen: string[] = [];
  const out = await materialize({ execute: f.execute, onStatus: s => seen.push(s), intervalMs: 1 }, { name: "Notes", format: "markdown", requestId: "r1" });
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Notes.md" });
  assert.equal(f.named("composeMaterialize").length, 1);
  assert.match(f.calls[0]?.call ?? "", /requestId:\s*"r1"/);
  assert.ok(seen.includes("composing") && seen.includes("Saved; waiting for its Files entry"));
});

test("a failed composition throws its reason", async () => {
  const f = fake({ compositionById: [[{ status: "failed", failureReason: "No access to notes.md" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), (e: Error) => e instanceof MaterializeFailed && e.message === "No access to notes.md");
});

test("a failed composition with no reason still says something", async () => {
  const f = fake({ compositionById: [[{ status: "failed" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), (e: Error) => e instanceof MaterializeFailed && e.message === "Materializer could not finish this file.");
});

test("a composition cancelled elsewhere ends the follow without cancelling it again", async () => {
  const f = fake({ compositionById: [[{ status: "cancelled" }]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"), MaterializeCancelled);
  assert.equal(f.named("composeCancel").length, 0);
});

test("a composition that can no longer be read is an explicit failure", async () => {
  const f = fake({ compositionById: [[]] });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"),
    (e: Error) => e instanceof MaterializeFailed && /no longer readable/.test(e.message));
});

test("no compositionId in the reply is an explicit error", async () => {
  const f = fake({ composeMaterialize: [[{}]] });
  await assert.rejects(materialize({ execute: f.execute, intervalMs: 1 }, { name: "x", format: "txt", requestId: "r" }), /did not confirm a composition/);
});

test("followComposition resolves a ready composition and never hands anything off", async () => {
  // The composition keeps the person's statement, but handing it to the
  // Library is the caller's business: the OS follows its own live feed and
  // calls handOffDescription itself.
  const f = fake({
    compositionById: [[{ status: "ready", outputFileId: "f1", name: "Brief", statement: "A one page brief" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Brief.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
  assert.deepEqual(f.calls.map(c => c.name), ["compositionById", "libraryArtifactForFile"]);
});

// ---------------------------------------------------------------------------
// When it looks
// ---------------------------------------------------------------------------
//
// A blank file can be ready, with its Files entry, well inside two seconds, and
// the upload it replaces was instant. So the first look comes 300 ms after the
// start; the rest keep the whole interval, so a slow composition is not read
// more often than before.

const FIRST_LOOK_MS = 300;

// Lets every promise that is ready settle, past the follow's chain of awaits.
const settle = async () => { for (let n = 0; n < 5; n++) await new Promise<void>(resolve => setImmediate(resolve)); };

// A clock the test moves by hand. It stands in for the global setTimeout and
// clearTimeout (which the follow's waits use) while a test runs, so the schedule
// is seen exactly and without waiting it out. `now` is the time the clock reads.
function handClock() {
  const real = { setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout };
  const timers = new Map<number, { at: number; fire: () => void }>();
  let now = 0;
  let nextId = 1;
  globalThis.setTimeout = ((fire: () => void, ms = 0) => {
    const id = nextId++;
    timers.set(id, { at: now + ms, fire });
    return id;
  }) as unknown as typeof setTimeout;
  globalThis.clearTimeout = ((id: number) => { timers.delete(id); }) as unknown as typeof clearTimeout;
  return {
    get now() { return now; },
    // Moves the clock on by `ms`, firing every timer that comes due on the way, in order.
    async advance(ms: number) {
      const until = now + ms;
      for (;;) {
        const due = [...timers.entries()].filter(([, timer]) => timer.at <= until).sort((a, b) => a[1].at - b[1].at)[0];
        if (!due) break;
        timers.delete(due[0]);
        now = due[1].at;
        due[1].fire();
        await settle();
      }
      now = until;
      await settle();
    },
    restore() { Object.assign(globalThis, real); },
  };
}

// The times, on the hand clock, at which the composition is read.
function looksAt(clock: ReturnType<typeof handClock>, f: ReturnType<typeof fake>): { looks: number[]; execute: MaterializeExecute } {
  const looks: number[] = [];
  return { looks, execute: (name, call) => { if (name === "compositionById") looks.push(clock.now); return f.execute(name, call); } };
}

test("the first look comes 300 ms after the start, and each one after it a whole interval later", async () => {
  const clock = handClock();
  try {
    const f = fake({
      compositionById: [[{ status: "composing" }], [{ status: "composing" }], [{ status: "ready", outputFileId: "f1" }]],
      libraryArtifactForFile: [[{ id: "a1", title: "Notes.md" }]],
    });
    const { looks, execute } = looksAt(clock, f);
    const run = followComposition({ execute }, "c1");
    await settle();
    assert.deepEqual(looks, [], "the composition was read before the first wait");
    await clock.advance(FIRST_LOOK_MS - 1);
    assert.deepEqual(looks, []);
    await clock.advance(1);
    assert.deepEqual(looks, [FIRST_LOOK_MS], "the first look did not come at 300 ms");
    await clock.advance(1999);
    assert.deepEqual(looks, [FIRST_LOOK_MS], "a later look came before the whole interval");
    await clock.advance(1);
    assert.deepEqual(looks, [FIRST_LOOK_MS, FIRST_LOOK_MS + 2000]);
    await clock.advance(2000);
    assert.deepEqual(looks, [FIRST_LOOK_MS, FIRST_LOOK_MS + 2000, FIRST_LOOK_MS + 4000]);
    assert.equal((await run).artifactId, "a1");
  } finally {
    clock.restore();
  }
});

test("a caller's own interval keeps its length, and a shorter one is never stretched to 300 ms", async () => {
  for (const [interval, first, second] of [[5000, FIRST_LOOK_MS, FIRST_LOOK_MS + 5000], [100, 100, 200]] as const) {
    const clock = handClock();
    try {
      const f = fake({ compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1" }]], libraryArtifactForFile: [[{ id: "a1" }]] });
      const { looks, execute } = looksAt(clock, f);
      const run = followComposition({ execute, intervalMs: interval }, "c1");
      await clock.advance(second);
      assert.deepEqual(looks, [first, second], `interval ${interval}`);
      assert.equal((await run).artifactId, "a1");
    } finally {
      clock.restore();
    }
  }
});

// ---------------------------------------------------------------------------
// Stopping
// ---------------------------------------------------------------------------

test("aborting cancels the composition once and stops polling", async () => {
  const controller = new AbortController();
  const f = fake({ compositionById: [[{ status: "composing" }]] });
  const run = followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5 }, "c1");
  setTimeout(() => controller.abort(), 12);
  await assert.rejects(run, (e: Error) => e instanceof MaterializeCancelled);
  const before = f.calls.length;
  await new Promise(r => setTimeout(r, 20));
  assert.equal(f.calls.filter(c => c.name === "composeCancel").length, 1);
  assert.equal(f.calls.length, before, "polling continued after abort");
});

test("a stop that lands while the composition is being read ends the wait at once, with one cancel", { timeout: 5000 }, async () => {
  // The signal is already aborted by the time the wait for the next look begins,
  // so no abort event is left to arrive. Without a check of the signal itself the
  // wait would run its whole interval (a minute here) and cancel only after it.
  const controller = new AbortController();
  const f = fake({ compositionById: () => { controller.abort(); return [{ status: "composing" }]; } });
  const started = Date.now();
  await assert.rejects(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 60_000 }, "c1"), MaterializeCancelled);
  assert.ok(Date.now() - started < 1000, "the stop waited out the poll interval");
  assert.equal(f.named("composeCancel").length, 1);
  assert.equal(f.named("compositionById").length, 1);
});

// ---------------------------------------------------------------------------
// A stop once the file exists does not take the file away
// ---------------------------------------------------------------------------

test("a stop while a ready composition waits for its Files entry takes one last look and keeps the file", async () => {
  // The file exists, so there is nothing left to cancel. The entry may well have
  // appeared while the wait ran, and then the person gets what was made.
  const controller = new AbortController();
  const f = fake({ compositionById: [[{ status: "ready", outputFileId: "f1", name: "Brief" }]], libraryArtifactForFile: [[], [{ id: "a1", title: "Brief.md" }]] });
  const run = followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1");
  setTimeout(() => controller.abort(), FIRST_LOOK_MS + 12);
  assert.deepEqual(await run, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
  assert.equal(f.named("composeCancel").length, 0, "a ready composition is not cancelled");
  assert.equal(f.named("libraryArtifactForFile").length, 2);
});

test("a stop while a ready composition's Files entry is still missing ends the follow without cancelling it", async () => {
  // Nothing can be handed back without the entry (the artifact is how the file is
  // opened), so this one is the only stop after the file exists that throws. The
  // composition stays ready and its file is kept.
  const controller = new AbortController();
  const f = fake({ compositionById: [[{ status: "ready", outputFileId: "f1" }]], libraryArtifactForFile: [[]] });
  const run = followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1");
  setTimeout(() => controller.abort(), FIRST_LOOK_MS + 12);
  await assert.rejects(run, MaterializeCancelled);
  assert.equal(f.named("composeCancel").length, 0, "a ready composition is not cancelled");
  assert.equal(f.named("libraryArtifactForFile").length, 2, "one look when it was found ready, one last look at the stop");
});

test("a stop already in effect when the Files entry was looked for does not look again", async () => {
  const controller = new AbortController();
  const f = fake({ compositionById: () => { controller.abort(); return [{ status: "ready", outputFileId: "f1" }]; }, libraryArtifactForFile: [[]] });
  await assert.rejects(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 60_000 }, "c1"), MaterializeCancelled);
  assert.equal(f.named("libraryArtifactForFile").length, 1);
  assert.equal(f.named("composeCancel").length, 0);
});

// ---------------------------------------------------------------------------
// A stop that races the composition finishing
// ---------------------------------------------------------------------------
//
// The look that saw "composing" can be up to a whole interval old when the stop arrives. If the
// composition finished in that time the file exists, and composeCancel, which cancels nothing that
// has finished, says so: integrations/compose/handlers.go answers `cancelled: false` with the
// composition's own status. The stop must not throw the file away on the strength of a look that
// is out of date.
const alreadyFinished = (status: string): Rows => [{ compositionId: "c1", status, cancelled: false, note: "that composition had already finished, so nothing was stopped" }];
// The stop lands in the wait after the first look, which comes FIRST_LOOK_MS after the start.
const stoppedAfter = (run: Promise<unknown>, controller: AbortController) => { setTimeout(() => controller.abort(), FIRST_LOOK_MS + 12); return run; };

test("a composition that finishes while the stop is on its way keeps its file", async () => {
  const controller = new AbortController();
  const f = fake({
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Brief" }]],
    composeCancel: [alreadyFinished("ready")],
    libraryArtifactForFile: [[{ id: "a1", title: "Brief.md" }]],
  });
  const result = await stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller);
  assert.deepEqual(result, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
  assert.deepEqual(f.calls.map(c => c.name), ["compositionById", "composeCancel", "compositionById", "libraryArtifactForFile"]);
});

test("a stop that raced a finished composition, whose Files entry is not there yet, ends without a second cancel", async () => {
  // The same rule as a stop after the file exists: with no entry there is no artifact to hand back,
  // so this one throws, and the composition stays ready with its file.
  const controller = new AbortController();
  const f = fake({
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Brief" }]],
    composeCancel: [alreadyFinished("ready")],
    libraryArtifactForFile: [[]],
  });
  await assert.rejects(stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller), MaterializeCancelled);
  assert.deepEqual(f.calls.map(c => c.name), ["compositionById", "composeCancel", "compositionById", "libraryArtifactForFile"]);
});

test("a composition that failed while the stop was on its way reports why, as a poll would have", async () => {
  const controller = new AbortController();
  const f = fake({
    compositionById: [[{ status: "composing" }], [{ status: "failed", failureReason: "No access to notes.md" }]],
    composeCancel: [alreadyFinished("failed")],
  });
  await assert.rejects(stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller),
    (e: Error) => e instanceof MaterializeFailed && e.message === "No access to notes.md");
});

test("a composition stopped from elsewhere while the stop was on its way is a stop", async () => {
  const controller = new AbortController();
  const f = fake({
    compositionById: [[{ status: "composing" }], [{ status: "cancelled" }]],
    composeCancel: [alreadyFinished("cancelled")],
  });
  await assert.rejects(stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller), MaterializeCancelled);
});

test("a cancel that took effect is not followed by another look at the composition", async () => {
  const controller = new AbortController();
  const f = fake({
    compositionById: [[{ status: "composing" }]],
    composeCancel: [[{ compositionId: "c1", status: "cancelled", cancelled: true }]],
  });
  await assert.rejects(stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller), MaterializeCancelled);
  assert.deepEqual(f.calls.map(c => c.name), ["compositionById", "composeCancel"]);
});

test("a cancel the cluster answers with nothing is a stop, and is not followed by another look", async () => {
  const controller = new AbortController();
  const f = fake({ compositionById: [[{ status: "composing" }]], composeCancel: [[]] });
  await assert.rejects(stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller), MaterializeCancelled);
  assert.deepEqual(f.calls.map(c => c.name), ["compositionById", "composeCancel"]);
});

test("a cancel that says it finished a composition that still reads as unfinished is a stop, and does not loop", async () => {
  const controller = new AbortController();
  const f = fake({
    compositionById: [[{ status: "composing" }]],
    composeCancel: [alreadyFinished("ready")],
  });
  await assert.rejects(stoppedAfter(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, "c1"), controller), MaterializeCancelled);
  assert.equal(f.named("composeCancel").length, 1);
  assert.equal(f.named("compositionById").length, 2);
});

test("an already stopped signal cancels the composition without reading it", async () => {
  const controller = new AbortController();
  controller.abort();
  const f = fake({});
  await assert.rejects(followComposition({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, "c1"), MaterializeCancelled);
  assert.deepEqual(f.calls.map(c => c.name), ["composeCancel"]);
  assert.match(f.calls[0]?.call ?? "", /compositionId:\s*"c1"/);
  assert.match(f.calls[0]?.call ?? "", /reason:\s*"Stopped by the person"/);
});

test("stopping before the request is sent starts nothing", async () => {
  const controller = new AbortController();
  controller.abort();
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  await assert.rejects(materialize({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, { name: "Notes", format: "markdown", requestId: "req-0001" }), MaterializeCancelled);
  assert.deepEqual(f.calls, []);
});

test("a long follow leaves no abort listener behind: each lives only as long as its wait", async () => {
  const controller = new AbortController();
  const polls = 25;
  const queue: Rows[] = Array.from({ length: polls }, () => [{ status: "composing" }]);
  queue.push([{ status: "ready", outputFileId: "f1" }]);
  const base = fake({ compositionById: queue, libraryArtifactForFile: [[{ id: "a1", title: "Long.md" }]] });
  const heldAtCall: number[] = [];
  const execute: MaterializeExecute = (name, call) => {
    heldAtCall.push(getEventListeners(controller.signal, "abort").length);
    return base.execute(name, call);
  };
  const out = await followComposition({ execute, signal: controller.signal, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
  assert.ok(heldAtCall.length > polls, "the follow was too short to prove anything");
  assert.equal(Math.max(...heldAtCall), 0, "a listener outlived the wait that added it");
  assert.equal(getEventListeners(controller.signal, "abort").length, 0);
});

// ---------------------------------------------------------------------------
// A run that has stopped must not leave the follow waiting
// ---------------------------------------------------------------------------

function unfinished(extra: Record<string, unknown> = {}): Rows[] {
  return [[{ status: "composing", runId: "run1", ...extra }]];
}

test("a run that stopped before the file was finished fails the follow with the run's message", async () => {
  const f = fake({
    compositionById: unfinished(),
    workRunForOwner: [[{ id: "run1", status: "abandoned", errorMessage: "The cluster lost the node making this file." }]],
  });
  await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"),
    (e: Error) => e instanceof MaterializeFailed && e.message === "The cluster lost the node making this file.");
  assert.match(f.named("workRunForOwner")[0]?.call ?? "", /runId:\s*"run1"/);
});

for (const status of ["failed", "cancelled", "abandoned"]) {
  test(`a run in state "${status}" with no message still fails the follow, in plain words`, async () => {
    const f = fake({ compositionById: unfinished(), workRunForOwner: [[{ id: "run1", status }]] });
    await assert.rejects(followComposition({ execute: f.execute, intervalMs: 1 }, "c1"),
      (e: Error) => e instanceof MaterializeFailed && e.message === "The run stopped before the file was finished.");
  });
}

test("a run that is still working does not stop the follow", async () => {
  const f = fake({
    compositionById: [...unfinished(), ...unfinished(), [{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: [[{ id: "run1", status: "running" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Slow.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
  assert.equal(f.named("workRunForOwner").length, 2, "the run is read while the composition is unfinished, not after it is ready");
});

test("a ready composition keeps its file even when its run is recorded as stopped", async () => {
  const f = fake({
    compositionById: [[{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: [[{ id: "run1", status: "failed", errorMessage: "late failure" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Kept.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.fileId, "f1");
  assert.equal(f.named("workRunForOwner").length, 0);
});

test("only the run that carries this composition can explain its stop", async () => {
  const f = fake({
    compositionById: [...unfinished(), [{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: [[{ id: "some-other-run", status: "failed", errorMessage: "not ours" }]],
    libraryArtifactForFile: [[{ id: "a1", title: "Ours.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
});

test("a run that cannot be read does not stop the follow", async () => {
  // The run read is a safety net. If it is unavailable the composition's own
  // status still decides, as it did before the net existed.
  const f = fake({
    compositionById: [...unfinished(), [{ status: "ready", outputFileId: "f1", runId: "run1" }]],
    workRunForOwner: () => { throw new Error("not permitted"); },
    libraryArtifactForFile: [[{ id: "a1", title: "Quiet.md" }]],
  });
  const out = await followComposition({ execute: f.execute, intervalMs: 1 }, "c1");
  assert.equal(out.artifactId, "a1");
});

// ---------------------------------------------------------------------------
// What startMaterialize sends
// ---------------------------------------------------------------------------

test("startMaterialize sends only what the person set", async () => {
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  const id = await startMaterialize(f.execute, {
    name: "Plan", format: "docx", requestId: "req-0001", statement: "   ", sources: [], accountIds: [], templateId: "", folderId: "fold1",
  });
  assert.equal(id, "c1");
  assert.equal(f.calls[0]?.call, buildComposeMaterialize({ name: "Plan", format: "docx", requestId: "req-0001", folderId: "fold1" }));
});

test("startMaterialize carries every field the request declares", async () => {
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  await startMaterialize(f.execute, {
    name: "Welcome", format: "json", requestId: "req-0002", statement: "A welcome email", outputKind: "email_template",
    sources: [{ kind: "library_file", ref: "f9", label: "Brand notes", content: true }], templateId: "t1", folderId: "fold1",
    accountIds: ["acct1"], recipeId: "rc1", deployableKind: "spa",
  });
  assert.equal(f.calls[0]?.call, buildComposeMaterialize({
    name: "Welcome", format: "json", requestId: "req-0002", statement: "A welcome email", outputKind: "email_template",
    sources: [{ kind: "library_file", ref: "f9", label: "Brand notes", content: true }], templateId: "t1", folderId: "fold1",
    accountIds: ["acct1"], recipeId: "rc1", deployableKind: "spa",
  }));
});

test("startMaterialize never forwards a field the request type does not declare", async () => {
  // A draft or section keys would change what the server treats the request
  // as, and isDescribedMarkdown could no longer say what the server will do.
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  const sneaky = { name: "x", format: "markdown", requestId: "req-0003", draft: "text", sectionKeys: ["a"], ceilings: { maxModelCalls: 99 } } as MaterializeRequest;
  await startMaterialize(f.execute, sneaky);
  assert.equal(f.calls[0]?.call, buildComposeMaterialize({ name: "x", format: "markdown", requestId: "req-0003" }));
});

test("a refusal to start reaches the caller as the cluster's sentence alone", async () => {
  // As it really arrives: the call's name, the engine's frame, then the sentence the
  // Materializer wrote for a person. What a person is shown is the last part only.
  const cases: [string, string][] = [
    ["Enter a name without slashes.", "Enter a name without slashes."],
    ["That folder isn't available. Choose another folder.", "That folder isn't available. Choose another folder."],
    ["compose: the sources could not be read: no such query", "the sources could not be read: no such query"],
  ];
  for (const [reason, shown] of cases) {
    const f = fake({ composeMaterialize: refusedAs("composeMaterialize", reason) });
    await assert.rejects(startMaterialize(f.execute, { name: "Notes 10/11", format: "markdown", requestId: "req-0008" }), (e: Error) => e.message === shown && !(e instanceof MaterializeFailed), reason);
    await assert.rejects(materialize({ execute: f.execute, intervalMs: 1 }, { name: "Notes 10/11", format: "markdown", requestId: "req-0008" }), (e: Error) => e.message === shown, reason);
  }
});

test("a request with no request id is refused before anything is sent", async () => {
  // Without an id a double click makes two files; that is what the id is for.
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  await assert.rejects(startMaterialize(f.execute, { name: "x", format: "txt", requestId: "  " }), /request id/);
  assert.deepEqual(f.calls, []);
});

// ---------------------------------------------------------------------------
// The hand-off's request ids, checked before a file is made
// ---------------------------------------------------------------------------

const DESCRIBED_ASK = { name: "Brief", format: "markdown", statement: "A one page brief for the launch" } as const;

test("a described request whose id the hand-off would refuse is refused before the file is made", async () => {
  // The hand-off sends the description under <id>-comment and <id>-revision, and
  // the Library takes only letters, digits, - and _ in them. An id outside that
  // would still make the FILE, and then fail to send what the file was asked for.
  for (const requestId of ["campaign:42", "has space", "with/slash", "über-id", "a".repeat(112)]) {
    const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
    await assert.rejects(startMaterialize(f.execute, { ...DESCRIBED_ASK, requestId }), /request id .*letters, digits/, `"${requestId}" was accepted`);
    assert.deepEqual(f.calls, [], `"${requestId}" was sent`);
    await assert.rejects(materialize({ execute: f.execute, intervalMs: 1 }, { ...DESCRIBED_ASK, requestId }), /request id .*letters, digits/);
    assert.deepEqual(f.calls, [], `"${requestId}" was sent by materialize`);
  }
});

test("a described request may use the longest id the hand-off can carry", async () => {
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  const requestId = "a".repeat(111);
  assert.equal(await startMaterialize(f.execute, { ...DESCRIBED_ASK, requestId }), "c1");
  assert.match(f.calls[0]?.call ?? "", new RegExp(`requestId:\\s*"${requestId}"`));
});

test("an id the hand-off never uses is not held to its pattern", async () => {
  // A blank file, a template, a spreadsheet: nothing is sent under the id but the
  // request itself, and the server asks only that it is not blank.
  const f = fake({ composeMaterialize: [[{ compositionId: "c1" }]] });
  for (const request of [
    { name: "x", format: "markdown" as const, requestId: "campaign:42" },
    { name: "x", format: "markdown" as const, statement: "Brief", templateId: "t1", requestId: "campaign:42" },
    { name: "x", format: "csv" as const, statement: "Brief", requestId: "a".repeat(200) },
  ]) {
    assert.equal(await startMaterialize(f.execute, request), "c1");
  }
  assert.equal(f.calls.length, 3);
});

test("the id is trimmed once, and the file and its description are asked for under the same one", async () => {
  // The server trims it. If the hand-off kept the spaces it would be refused
  // under a pattern the trimmed id passes.
  const f = fake(describedScript());
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, { ...described, requestId: "  req-0004  " });
  assert.equal(out.revisionId, "req-0004-revision");
  assert.equal(requestIdIn(f.calls[0]?.call ?? ""), "req-0004");
  assert.deepEqual(f.calls.filter(c => /^library(Add|Request)/.test(c.name)).map(c => requestIdIn(c.call)), ["req-0004-comment", "req-0004-revision"]);
  const direct = fake(handOffScript());
  await handOffDescription(direct.execute, { artifactId: "a1", statement: "A one page brief", requestId: " req-0007 " });
  assert.deepEqual(direct.calls.map(c => requestIdIn(c.call)), ["req-0007-comment", "req-0007-revision"]);
});

// ---------------------------------------------------------------------------
// handOffDescription: the person's client gives the description to the Library
// ---------------------------------------------------------------------------

const REVISION_REPLY = { runId: "run9", goalId: "goal9", requestId: "ignored", proposal: { artifactId: "a1" } };

function handOffScript(extra: Record<string, Answer> = {}): Record<string, Answer> {
  return {
    libraryAddDocumentComment: [[{ saved: true, commentId: "cm1" }]],
    libraryRequestDocumentRevision: [[REVISION_REPLY]],
    ...extra,
  };
}

test("handOffDescription comments on the whole document, then asks the Library to write it", async () => {
  const f = fake(handOffScript());
  const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "A one page brief for the launch", requestId: "req-0001" });
  assert.deepEqual(out, { revisionId: "req-0001-revision" });
  assert.deepEqual(f.calls.map(c => c.name), ["libraryAddDocumentComment", "libraryRequestDocumentRevision"]);
  assert.equal(f.calls[0]?.call, buildLibraryAddDocumentComment({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    anchor: { kind: "document", quote: "Entire document" },
    body: "A one page brief for the launch", requestId: "req-0001-comment", attachments: [],
  }));
  assert.equal(f.calls[1]?.call, buildLibraryRequestDocumentRevision({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    commentIds: ["cm1"], instruction: "", requestId: "req-0001-revision",
  }));
});

test("the request ids it derives are ones the Library accepts", async () => {
  // integrations/library/review.go refuses any id outside [a-zA-Z0-9_-]{8,120}.
  // A separator such as ":" would make every real hand-off fail.
  const f = fake(handOffScript());
  const requestId = randomUUID();
  const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "Describe it", requestId });
  assert.equal(out.revisionFailure, undefined);
  const sent = f.calls.map(c => requestIdIn(c.call));
  assert.equal(sent.length, 2);
  for (const id of sent) assert.match(id, LIBRARY_REQUEST_ID);
  assert.equal(out.revisionId, sent[1]);
  assert.notEqual(sent[0], sent[1]);
});

test("a retried hand-off reuses its request ids, so the Library returns the first receipts", async () => {
  const f = fake(handOffScript());
  const request = { artifactId: "a1", statement: "A one page brief", requestId: "req-0001" };
  const first = await handOffDescription(f.execute, request);
  const second = await handOffDescription(f.execute, request);
  assert.deepEqual(second, first);
  assert.deepEqual(f.calls.map(c => requestIdIn(c.call)), ["req-0001-comment", "req-0001-revision", "req-0001-comment", "req-0001-revision"]);
});

const FAILURE_PREFIX = "The description could not be sent for writing: ";

const failures: { label: string; script: Record<string, Answer>; request?: Partial<Parameters<typeof handOffDescription>[1]>; message: RegExp; calls: string[] }[] = [
  {
    label: "the Library refuses the comment",
    script: handOffScript({ libraryAddDocumentComment: () => { throw refusal("libraryAddDocumentComment", "the document changed; reopen its current revision before adding feedback"); } }),
    message: /^the document changed; reopen its current revision/,
    calls: ["libraryAddDocumentComment"],
  },
  {
    label: "the comment is not confirmed",
    script: handOffScript({ libraryAddDocumentComment: [[{ saved: false }]] }),
    message: /did not confirm the description/,
    calls: ["libraryAddDocumentComment"],
  },
  {
    label: "the Library cannot start the revision",
    script: handOffScript({ libraryRequestDocumentRevision: () => { throw refusal("libraryRequestDocumentRevision", "document review jobs are unavailable on this node"); } }),
    message: /^document review jobs are unavailable/,
    calls: ["libraryAddDocumentComment", "libraryRequestDocumentRevision"],
  },
  {
    label: "the revision is not confirmed",
    script: handOffScript({ libraryRequestDocumentRevision: [[{ requestId: "req-0003-revision" }]] }),
    message: /did not confirm the writing request/,
    calls: ["libraryAddDocumentComment", "libraryRequestDocumentRevision"],
  },
  {
    label: "the request id is not one the Library accepts",
    script: handOffScript(),
    request: { requestId: "campaign:42" },
    message: /request ids .* letters, digits/,
    calls: [],
  },
  {
    label: "there is nothing to send",
    script: handOffScript(),
    request: { statement: "   " },
    message: /no description to send/,
    calls: [],
  },
];

for (const { label, script, request, message, calls } of failures) {
  test(`handOffDescription reports a failure instead of throwing: ${label}`, async () => {
    const f = fake(script);
    const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "A one page brief", requestId: "req-0003", ...request });
    assert.equal(out.revisionId, undefined);
    const failure = out.revisionFailure ?? "";
    assert.ok(failure.startsWith(FAILURE_PREFIX), `got ${JSON.stringify(out)}`);
    assert.match(failure.slice(FAILURE_PREFIX.length), message);
    assert.equal("revisionId" in out, false);
    assert.deepEqual(f.calls.map(c => c.name), calls);
  });
}

// A refusal arrives as "<construct name>: MemQL engine failed to execute query.
// Details: <reason>" (see `refusal`). Neither the name nor the engine's frame is
// something a person can act on, and revisionFailure is shown to people.
const refusedAs = (name: string, reason: string) => () => { throw refusal(name, reason); };

test("a refusal that arrives with the construct's name and the engine's frame in front is shown without either", async () => {
  for (const [construct, calls] of [
    ["libraryAddDocumentComment", ["libraryAddDocumentComment"]],
    ["libraryRequestDocumentRevision", ["libraryAddDocumentComment", "libraryRequestDocumentRevision"]],
  ] as const) {
    const reason = "document review jobs are unavailable on this node";
    const f = fake(handOffScript({ [construct]: refusedAs(construct, reason) }));
    const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "A one page brief", requestId: "req-0003" });
    assert.deepEqual(out, { revisionFailure: FAILURE_PREFIX + reason });
    assert.deepEqual(f.calls.map(c => c.name), calls);
  }
});

test("the lookups it makes are worded the same way", async () => {
  for (const construct of ["libraryArtifactForFile", "libraryFileById"]) {
    const f = fake(handOffScript({ ...library({}), [construct]: refusedAs(construct, "you cannot read that file") }));
    const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "Summarise this", requestId: "req-0003", sources: [reference("f1", "Notes")] });
    assert.deepEqual(out, { revisionFailure: FAILURE_PREFIX + "you cannot read that file" }, construct);
  }
});

test("the retry without attachments words its own refusal the same way", async () => {
  let attempts = 0;
  const f = fake(handOffScript({
    ...library({ "f-notes": stored("att-notes", "notes.md", KiB) }),
    libraryAddDocumentComment: () => { throw refusal("libraryAddDocumentComment", ++attempts === 1 ? "attach a nonempty UTF-8 Markdown file up to 32 KiB" : "the document changed; reopen its current revision before adding feedback"); },
  }));
  const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "Summarise these", requestId: "req-0010", sources: [reference("f-notes", "Notes")] });
  assert.deepEqual(out, { revisionFailure: FAILURE_PREFIX + "the document changed; reopen its current revision before adding feedback" });
  assert.equal(attempts, 2, "an attachment refusal that arrives prefixed is still retried without them");
});

test("only the construct's own name is taken off, and only from the front", async () => {
  const cases: [string, string][] = [
    ["Note: the document changed", "Note: the document changed"],                                       // some other word and a colon
    ["the libraryAddDocumentComment: call failed", "the libraryAddDocumentComment: call failed"],       // not at the front
    ["libraryAddDocumentComment:no space", "libraryAddDocumentComment:no space"],                       // not how executeNamed words it
    ["libraryAddDocumentComment: libraryAddDocumentComment: twice", "libraryAddDocumentComment: twice"], // the one prefix, once
  ];
  for (const [message, shown] of cases) {
    const f = fake(handOffScript({ libraryAddDocumentComment: () => { throw new Error(message); } }));
    const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "A one page brief", requestId: "req-0003" });
    assert.equal(out.revisionFailure, FAILURE_PREFIX + shown, message);
  }
});

// ---------------------------------------------------------------------------
// References: attach what the Library accepts, report the rest
// ---------------------------------------------------------------------------
//
// The Library takes Markdown (.md, up to 32 KiB) and images (PNG, JPEG, GIF,
// WebP, up to 8 MiB) as review attachments, eight at most, and refuses the
// WHOLE comment when one reference is anything else
// (integrations/library/review_attachments.go). A hand-off must not fail for a
// reference it could have left off, and must not leave one off without saying
// so: what is not attached comes back by label in `unattached`.

const KiB = 1024;
const MiB = 1024 * KiB;

type Stored = { artifact?: Record<string, unknown>; file?: Record<string, unknown> };

// A Library holding these files, answering the two reads by file id as the real reads do.
function library(files: Record<string, Stored>): Record<string, Answer> {
  return {
    libraryArtifactForFile: call => { const artifact = files[fileIdIn(call)]?.artifact; return artifact ? [artifact] : []; },
    libraryFileById: call => { const file = files[fileIdIn(call)]?.file; return file ? [file] : []; },
  };
}

// An ordinary stored file: its Files entry and its file row. A size of undefined is a row that does not say.
const stored = (artifactId: string, name: string, size?: number, row: Record<string, unknown> = {}): Stored =>
  ({ artifact: { id: artifactId }, file: { name, ...(size === undefined ? {} : { size }), ...row } });
const reference = (ref: string, label: string): MaterializeSource => ({ kind: "library_file", ref, label });
const attachmentOf = (n: number | string, version = 1) => ({ artifactId: `att-${n}`, version, revision: `file:${version}` });

async function handOffWith(files: Record<string, Stored>, sources: MaterializeSource[], extra: Record<string, Answer> = {}) {
  const f = fake(handOffScript({ ...library(files), ...extra }));
  const out = await handOffDescription(f.execute, { artifactId: "a1", statement: "Summarise these", requestId: "req-0010", sources });
  return { f, out };
}

const commentWith = (attachments: { artifactId: string; version: number; revision: string }[]) => buildLibraryAddDocumentComment({
  artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
  anchor: { kind: "document", quote: "Entire document" }, body: "Summarise these", requestId: "req-0010-comment", attachments,
});

test("handOffDescription attaches every reference the Library accepts, and reports nothing", async () => {
  const { f, out } = await handOffWith(
    { "f-notes": stored("att-notes", "notes.md", 2 * KiB, { versionNumber: 3 }), "f-logo": stored("att-logo", "logo.png", 40 * KiB) },
    [reference("f-notes", "Notes"), reference("f-logo", "Logo")]);
  assert.deepEqual(out, { revisionId: "req-0010-revision" });
  assert.equal("unattached" in out, false, "nothing left over is an absent field, not an empty list");
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([
    { artifactId: "att-notes", version: 3, revision: "file:3" }, { artifactId: "att-logo", version: 1, revision: "file:1" },
  ]));
});

test("a PDF is reported rather than attached, and the image still goes with the revision", async () => {
  const { f, out } = await handOffWith(
    { "f-spec": stored("att-spec", "spec.pdf", 200 * KiB), "f-shot": stored("att-shot", "shot.jpg", 300 * KiB, { versionNumber: 2 }) },
    [reference("f-spec", "Spec"), reference("f-shot", "Screenshot")]);
  assert.deepEqual(out, { revisionId: "req-0010-revision", unattached: ["Spec"] });
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([{ artifactId: "att-shot", version: 2, revision: "file:2" }]));
  assert.deepEqual(f.calls.map(c => c.name).filter(n => /^library(Add|Request)/.test(n)), ["libraryAddDocumentComment", "libraryRequestDocumentRevision"]);
});

test("with only unsupported references the comment goes without attachments and the revision still opens", async () => {
  const { f, out } = await handOffWith(
    { "f-spec": stored("att-spec", "spec.pdf", 200 * KiB), "f-numbers": stored("att-numbers", "numbers.csv", 4 * KiB) },
    [reference("f-spec", "Spec"), reference("f-numbers", "Numbers")]);
  assert.deepEqual(out, { revisionId: "req-0010-revision", unattached: ["Spec", "Numbers"] });
  assert.deepEqual(f.named("libraryAddDocumentComment").map(c => c.call), [commentWith([])]);
  assert.equal(f.named("libraryRequestDocumentRevision").length, 1);
});

// What review_attachments.go accepts, as far as a file's name and size can show.
const ACCEPTED: [string, number | undefined][] = [
  ["notes.md", 1], ["notes.md", 32 * KiB], ["NOTES.MD", 10], ["draft.v2.md", 10],
  ["shot.png", 1], ["SHOT.PNG", 10], ["shot.jpg", 10], ["shot.jpeg", 10], ["anim.gif", 10], ["pic.webp", 10], ["pic.png", 8 * MiB],
  ["notes.md", undefined], // the row does not say how big it is: the Library decides
];
const NOT_ACCEPTED: [string, number | undefined][] = [
  ["notes.md", 32 * KiB + 1], ["notes.md", 0],            // Markdown is non-empty and up to 32 KiB
  ["notes.markdown", 10], ["notes.txt", 10],              // only ".md" is Markdown
  ["report.pdf", 10], ["table.csv", 10], ["page.html", 10],
  ["logo.svg", 10], ["scan.tiff", 10], ["pic.bmp", 10],   // only PNG, JPEG, GIF and WebP are images
  ["pic.png", 8 * MiB + 1], ["pic.png", 0],               // an image is non-empty and up to 8 MiB
  ["README", 10], ["bundle.png.zip", 10], ["", 10],       // the LAST extension counts, and there must be one
];

test("the Library's own type and size rules decide what is attached", async () => {
  for (const [name, size] of ACCEPTED) {
    const { f, out } = await handOffWith({ "f-1": stored("att-1", name, size) }, [reference("f-1", "Ref")]);
    assert.equal(out.unattached, undefined, `${name} (${size}) should be attached`);
    assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([attachmentOf(1)]), `${name} (${size})`);
  }
});

test("a reference of a type or size the Library refuses is reported by label", async () => {
  for (const [name, size] of NOT_ACCEPTED) {
    const { f, out } = await handOffWith({ "f-1": stored("att-1", name, size) }, [reference("f-1", "Ref")]);
    assert.deepEqual(out, { revisionId: "req-0010-revision", unattached: ["Ref"] }, `${name} (${size}) should not be attached`);
    assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([]), `${name} (${size})`);
  }
});

test("no more than eight references are attached, and the rest are reported", async () => {
  const files: Record<string, Stored> = {};
  const sources: MaterializeSource[] = [];
  for (let n = 1; n <= 10; n++) {
    files[`f-${n}`] = stored(`att-${n}`, `img${n}.png`, 10);
    sources.push(reference(`f-${n}`, `Image ${n}`));
  }
  const { f, out } = await handOffWith(files, sources);
  assert.deepEqual(out.unattached, ["Image 9", "Image 10"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith(Array.from({ length: 8 }, (_, n) => attachmentOf(n + 1))));
});

test("attachments may total exactly 16 MiB and no more", async () => {
  const { f, out } = await handOffWith(
    { "f-1": stored("att-1", "one.png", 8 * MiB), "f-2": stored("att-2", "two.png", 8 * MiB), "f-3": stored("att-3", "dot.png", 1) },
    [reference("f-1", "One"), reference("f-2", "Two"), reference("f-3", "One byte over")]);
  assert.deepEqual(out.unattached, ["One byte over"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([attachmentOf(1), attachmentOf(2)]));
});

test("a reference that would pass 16 MiB is reported, and later ones that still fit are attached", async () => {
  const { f, out } = await handOffWith({
    "f-1": stored("att-1", "one.png", 8 * MiB), "f-2": stored("att-2", "two.png", 7 * MiB + 512 * KiB),
    "f-3": stored("att-3", "three.png", MiB), "f-4": stored("att-4", "four.png", 256 * KiB),
  }, [reference("f-1", "One"), reference("f-2", "Two"), reference("f-3", "Three"), reference("f-4", "Four")]);
  // 8 + 7.5 = 15.5 MiB. Three (1 MiB) would make 16.5; Four (0.25 MiB) makes 15.75.
  assert.deepEqual(out.unattached, ["Three"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([attachmentOf(1), attachmentOf(2), attachmentOf(4)]));
});

test("Markdown attachments may total exactly 64 KiB and no more", async () => {
  const { f, out } = await handOffWith(
    { "f-1": stored("att-1", "one.md", 32 * KiB), "f-2": stored("att-2", "two.md", 32 * KiB), "f-3": stored("att-3", "three.md", 1) },
    [reference("f-1", "One"), reference("f-2", "Two"), reference("f-3", "Three")]);
  assert.deepEqual(out.unattached, ["Three"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([attachmentOf(1), attachmentOf(2)]));
});

test("Markdown over 64 KiB in all is reported, while a small one and an image after it still go", async () => {
  const { f, out } = await handOffWith({
    "f-1": stored("att-1", "one.md", 30 * KiB), "f-2": stored("att-2", "two.md", 30 * KiB), "f-3": stored("att-3", "three.md", 30 * KiB),
    "f-4": stored("att-4", "four.md", KiB), "f-5": stored("att-5", "chart.png", 100 * KiB),
  }, [reference("f-1", "One"), reference("f-2", "Two"), reference("f-3", "Three"), reference("f-4", "Four"), reference("f-5", "Chart")]);
  // 30 + 30 = 60 KiB of Markdown. Three would make 90; Four makes 61; an image is not counted as Markdown.
  assert.deepEqual(out.unattached, ["Three"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([attachmentOf(1), attachmentOf(2), attachmentOf(4), attachmentOf(5)]));
});

test("references that are not files, or do not resolve, are reported; a repeated one is attached once", async () => {
  const f = fake(handOffScript(library({
    "f-notes": stored("att-notes", "notes.md", KiB, { versionNumber: 3 }),
    "f-logo": stored("att-logo", "logo.png", 5 * KiB),
    "f-nofile": { artifact: { id: "att-nofile" } },
    "f-archived": { artifact: { id: "att-archived", archived: true }, file: { name: "old.md", size: 10, versionNumber: 2 } },
  })));
  const out = await handOffDescription(f.execute, {
    artifactId: "a1", statement: "Summarise these", requestId: "req-0010",
    sources: [
      reference("f-notes", "Notes"),
      { kind: "concept_row", ref: "v1:crm:contact#c1", label: "A contact" },
      { kind: "query", ref: "contacts()", label: "All contacts" },
      reference("f-logo", "Logo"),
      reference("f-notes", "Notes, listed twice"),
      reference("f-gone", "Nothing resolves"),
      reference("f-nofile", "No file row"),
      reference("f-archived", "Archived"),
    ],
  });
  assert.deepEqual(out, { revisionId: "req-0010-revision", unattached: ["A contact", "All contacts", "Nothing resolves", "No file row", "Archived"] });
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([
    { artifactId: "att-notes", version: 3, revision: "file:3" }, { artifactId: "att-logo", version: 1, revision: "file:1" },
  ]));
  // Sources that are not files cannot be attached and are never looked up.
  assert.equal(f.calls.some(c => /contact/.test(c.call)), false, "a source that is not a file was looked up");
});

test("a reference listed twice is attached once, or reported once, and the same file under two ids counts once", async () => {
  const notes = stored("att-notes", "notes.md", KiB);
  const { f, out } = await handOffWith(
    { "f-notes": notes, "v1:library:file:f-notes": notes, "f-spec": stored("att-spec", "spec.pdf", KiB) },
    [reference("f-notes", "Notes"), reference("f-notes", "Notes again"), reference("v1:library:file:f-notes", "Notes by its full id"),
      reference("f-spec", "Spec"), reference("f-spec", "Spec again")]);
  assert.deepEqual(out, { revisionId: "req-0010-revision", unattached: ["Spec"] });
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, commentWith([attachmentOf("notes")]));
});

test("a reference with no label is reported by what it points at", async () => {
  const { out } = await handOffWith({ "f-spec": stored("att-spec", "spec.pdf", KiB) }, [
    reference("f-spec", "  "), { kind: "concept_row", ref: "v1:crm:contact#c1", label: "" },
  ]);
  assert.deepEqual(out.unattached, ["f-spec", "v1:crm:contact#c1"]);
});

// A refusal the Library words about attachments (integrations/library/review_attachments.go).
const ATTACHMENT_REFUSAL = "attach a nonempty UTF-8 Markdown file up to 32 KiB";
const DOCUMENT_REFUSAL = "the document changed; reopen its current revision before adding feedback";

test("when the Library refuses an attachment the comment is sent once more without them, and they are reported", async () => {
  // Only the bytes show that a .md file is not UTF-8, or that a .png will not decode,
  // and the refusal does not say which attachment it meant: so none go the second time.
  const { f, out } = await handOffWith(
    { "f-notes": stored("att-notes", "notes.md", 2 * KiB), "f-spec": stored("att-spec", "spec.pdf", KiB), "f-shot": stored("att-shot", "shot.png", 40 * KiB) },
    [reference("f-notes", "Notes"), reference("f-spec", "Spec"), reference("f-shot", "Screenshot")],
    { libraryAddDocumentComment: call => {
      if (/attachments: \[\{/.test(call)) throw refusal("libraryAddDocumentComment", ATTACHMENT_REFUSAL);
      return [{ saved: true, commentId: "cm1" }];
    } });
  assert.deepEqual(out, { revisionId: "req-0010-revision", unattached: ["Notes", "Spec", "Screenshot"] });
  assert.deepEqual(f.named("libraryAddDocumentComment").map(c => c.call), [commentWith([attachmentOf("notes"), attachmentOf("shot")]), commentWith([])]);
  assert.equal(f.named("libraryRequestDocumentRevision")[0]?.call, buildLibraryRequestDocumentRevision({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1", commentIds: ["cm1"], instruction: "", requestId: "req-0010-revision",
  }));
});

test("a refusal that is not about an attachment is not retried without them", async () => {
  const { f, out } = await handOffWith({ "f-notes": stored("att-notes", "notes.md", KiB) }, [reference("f-notes", "Notes")],
    { libraryAddDocumentComment: () => { throw refusal("libraryAddDocumentComment", DOCUMENT_REFUSAL); } });
  assert.deepEqual(out, { revisionFailure: FAILURE_PREFIX + DOCUMENT_REFUSAL });
  assert.equal(f.named("libraryAddDocumentComment").length, 1);
  assert.equal(f.named("libraryRequestDocumentRevision").length, 0);
});

test("with no attachment to take off there is nothing to retry", async () => {
  const { f, out } = await handOffWith({ "f-spec": stored("att-spec", "spec.pdf", KiB) }, [reference("f-spec", "Spec")],
    { libraryAddDocumentComment: () => { throw refusal("libraryAddDocumentComment", ATTACHMENT_REFUSAL); } });
  assert.equal(out.revisionFailure, FAILURE_PREFIX + ATTACHMENT_REFUSAL);
  assert.equal(f.named("libraryAddDocumentComment").length, 1);
});

test("if the comment fails again without its attachments, that failure is the one reported", async () => {
  let attempts = 0;
  const { f, out } = await handOffWith({ "f-notes": stored("att-notes", "notes.md", KiB) }, [reference("f-notes", "Notes")],
    { libraryAddDocumentComment: () => { throw refusal("libraryAddDocumentComment", ++attempts === 1 ? ATTACHMENT_REFUSAL : DOCUMENT_REFUSAL); } });
  assert.deepEqual(out, { revisionFailure: FAILURE_PREFIX + DOCUMENT_REFUSAL });
  assert.equal(f.named("libraryAddDocumentComment").length, 2);
  assert.equal(f.named("libraryRequestDocumentRevision").length, 0);
});

// ---------------------------------------------------------------------------
// materialize: the hand-off for a described Markdown request
// ---------------------------------------------------------------------------

function describedScript(extra: Record<string, Answer> = {}): Record<string, Answer> {
  return handOffScript({
    composeMaterialize: [[{ compositionId: "c1" }]],
    compositionById: [[{ status: "ready", outputFileId: "f1", name: "Brief" }]],
    ...library({
      f1: { artifact: { id: "a1", title: "Brief.md" } },
      "f-notes": stored("att-notes", "notes.md", 1200, { versionNumber: 2 }),
      "f-spec": stored("att-spec", "spec.pdf", 5000),
    }),
    ...extra,
  });
}

const described: MaterializeRequest = {
  name: "Brief", format: "markdown", requestId: "req-0004", statement: "A one page brief for the launch",
  sources: [{ kind: "library_file", ref: "f-notes", label: "Notes" }],
};

test("materialize on a described Markdown request returns the revision once the file is in Files", async () => {
  const f = fake(describedScript());
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, described);
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md", revisionId: "req-0004-revision" });
  const names = f.calls.map(c => c.name);
  assert.deepEqual(names.slice(0, 3), ["composeMaterialize", "compositionById", "libraryArtifactForFile"]);
  assert.deepEqual(names.slice(-2), ["libraryAddDocumentComment", "libraryRequestDocumentRevision"]);
  assert.equal(f.named("libraryAddDocumentComment")[0]?.call, buildLibraryAddDocumentComment({
    artifactId: "a1", expectedVersion: 1, expectedRevision: "file:1",
    anchor: { kind: "document", quote: "Entire document" }, body: "A one page brief for the launch", requestId: "req-0004-comment",
    attachments: [{ artifactId: "att-notes", version: 2, revision: "file:2" }],
  }));
  // The request the server sees is the person's own words, filed blank there.
  assert.match(f.calls[0]?.call ?? "", /statement:\s*"A one page brief for the launch"/);
});

test("materialize puts the references the Library could not take on its result", async () => {
  const f = fake(describedScript());
  const sources: MaterializeSource[] = [reference("f-notes", "Notes"), reference("f-spec", "Spec")];
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, { ...described, sources });
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md", revisionId: "req-0004-revision", unattached: ["Spec"] });
});

test("materialize waits for the Files entry before it hands the description over", async () => {
  const f = fake(describedScript({
    libraryArtifactForFile: [[], [{ id: "a1", title: "Brief.md" }]],
    libraryFileById: [[]],
  }));
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, { ...described, sources: [] });
  assert.equal(out.revisionId, "req-0004-revision");
  const names = f.calls.map(c => c.name);
  assert.ok(names.indexOf("libraryAddDocumentComment") > names.lastIndexOf("libraryArtifactForFile"));
});

test("a failed hand-off leaves the file and says why, and materialize still resolves", async () => {
  const f = fake(describedScript({ libraryRequestDocumentRevision: () => { throw refusal("libraryRequestDocumentRevision", "document review jobs are unavailable on this node"); } }));
  const out = await materialize({ execute: f.execute, intervalMs: 1 }, described);
  assert.equal(out.artifactId, "a1");
  assert.equal(out.fileId, "f1");
  assert.equal(out.revisionId, undefined);
  assert.equal(out.revisionFailure, FAILURE_PREFIX + "document review jobs are unavailable on this node");
});

const STOPPED_BEFORE_SENT = "Stopped before the description was sent.";

test("stopping after the file exists keeps the file, sends nothing, and says the description was not sent", async () => {
  const controller = new AbortController();
  const f = fake(describedScript({
    libraryArtifactForFile: call => { controller.abort(); return fileIdIn(call) === "f1" ? [{ id: "a1", title: "Brief.md" }] : []; },
  }));
  const out = await materialize({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, described);
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md", revisionFailure: STOPPED_BEFORE_SENT });
  assert.equal(f.named("libraryAddDocumentComment").length, 0);
  assert.equal(f.named("libraryRequestDocumentRevision").length, 0);
  assert.equal(f.named("libraryFileById").length, 0, "the references were not even looked up");
  assert.equal(f.named("composeCancel").length, 0, "a ready composition is not cancelled");
});

test("stopping after a blank file exists returns the file as it is: there was no description to send", async () => {
  const controller = new AbortController();
  const f = fake(describedScript({
    libraryArtifactForFile: call => { controller.abort(); return fileIdIn(call) === "f1" ? [{ id: "a1", title: "Brief.md" }] : []; },
  }));
  const out = await materialize({ execute: f.execute, signal: controller.signal, intervalMs: 1 }, { name: "Brief", format: "markdown", requestId: "req-0006" });
  assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
  assert.equal(f.named("composeCancel").length, 0);
});

test("a stop during the wait for a described file's Files entry still returns the file once the entry is there", async () => {
  const controller = new AbortController();
  let looks = 0;
  const f = fake(describedScript({ libraryArtifactForFile: () => (++looks > 1 ? [{ id: "a1", title: "Brief.md" }] : []) }));
  const run = materialize({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, described);
  setTimeout(() => controller.abort(), FIRST_LOOK_MS + 12);
  assert.deepEqual(await run, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md", revisionFailure: STOPPED_BEFORE_SENT });
  assert.equal(f.named("libraryAddDocumentComment").length, 0);
  assert.equal(f.named("composeCancel").length, 0);
});

test("a stop before the file exists still ends materialize with nothing returned", async () => {
  const controller = new AbortController();
  const f = fake(describedScript({ compositionById: () => { controller.abort(); return [{ status: "composing" }]; } }));
  await assert.rejects(materialize({ execute: f.execute, signal: controller.signal, intervalMs: 60_000 }, described), MaterializeCancelled);
  assert.equal(f.named("composeCancel").length, 1);
  assert.equal(f.named("libraryAddDocumentComment").length, 0);
});

test("a described file that was finished while the stop was on its way is returned, and its description is not sent", async () => {
  const controller = new AbortController();
  const f = fake(describedScript({
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Brief" }]],
    composeCancel: [alreadyFinished("ready")],
  }));
  const result = await stoppedAfter(materialize({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, described), controller);
  assert.deepEqual(result, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md", revisionFailure: STOPPED_BEFORE_SENT });
  assert.equal(f.named("libraryAddDocumentComment").length, 0);
  assert.equal(f.named("libraryRequestDocumentRevision").length, 0);
  assert.equal(f.named("composeCancel").length, 1);
});

test("a blank file that was finished while the stop was on its way is returned as it is", async () => {
  const controller = new AbortController();
  const f = fake(describedScript({
    compositionById: [[{ status: "composing" }], [{ status: "ready", outputFileId: "f1", name: "Brief" }]],
    composeCancel: [alreadyFinished("ready")],
  }));
  const result = await stoppedAfter(materialize({ execute: f.execute, signal: controller.signal, intervalMs: 5000 }, { name: "Brief", format: "markdown", requestId: "req-0006" }), controller);
  assert.deepEqual(result, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
});

const notDescribed: [string, MaterializeRequest][] = [
  ["a blank Markdown file", { name: "Notes", format: "markdown", requestId: "req-0005" }],
  ["a statement of only spaces", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "   " }],
  ["Markdown through a template", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", templateId: "t1" }],
  ["Markdown from a recipe", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", recipeId: "rc1" }],
  ["a described email template", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", outputKind: "email_template" }],
  ["a package", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", deployableKind: "spa" }],
  ["a described file that is not Markdown", { name: "Notes", format: "txt", requestId: "req-0005", statement: "Brief" }],
  ["a described email JSON", { name: "Notes", format: "json", requestId: "req-0005", statement: "Brief", outputKind: "email_template" }],
  // The revision workflow reads Markdown and images and cannot read the graph, so
  // a request to write a document from records keeps the composer, which can.
  ["Markdown from a query", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", sources: [{ kind: "query", ref: "query openInvoices()", label: "Invoices" }] }],
  ["Markdown from a concept row", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", sources: [{ kind: "concept_row", ref: "inv1", label: "INV-1" }] }],
  ["Markdown from a file and a query", { name: "Notes", format: "markdown", requestId: "req-0005", statement: "Brief", sources: [
    { kind: "library_file", ref: "f-notes", label: "Notes" }, { kind: "query", ref: "query openInvoices()", label: "Invoices" },
  ] }],
];

for (const [label, request] of notDescribed) {
  test(`materialize makes no Library hand-off call for ${label}`, async () => {
    const f = fake(describedScript());
    const out = await materialize({ execute: f.execute, intervalMs: 1 }, request);
    assert.deepEqual(out, { compositionId: "c1", fileId: "f1", artifactId: "a1", name: "Brief.md" });
    assert.equal(isDescribedMarkdown(request), false);
    for (const name of ["libraryAddDocumentComment", "libraryRequestDocumentRevision", "libraryFileById", "libraryDocumentReview"]) {
      assert.equal(f.named(name).length, 0, `${name} was called`);
    }
  });
}

test("isDescribedMarkdown matches the server's rule for a request filed blank", () => {
  assert.equal(isDescribedMarkdown(described), true);
  assert.equal(isDescribedMarkdown({ format: "markdown", statement: " Write it " }), true);
  assert.equal(isDescribedMarkdown({ format: "markdown", statement: "Write it", templateId: "  " }), true, "a template of only spaces is no template, as on the server");
  assert.equal(isDescribedMarkdown({ format: "html", statement: "Write it" }), false);
});

test("isDescribedMarkdown holds for Library files and keeps the composer for anything else", () => {
  const base = { format: "markdown", statement: "Write it" } as const;
  assert.equal(isDescribedMarkdown({ ...base, sources: [] }), true, "no sources is still described");
  assert.equal(isDescribedMarkdown({ ...base, sources: [{ kind: "library_file", ref: "f1", label: "Notes", content: true }] }), true, "files are references the revision workflow can read");
  assert.equal(isDescribedMarkdown({ ...base, sources: [{ kind: "query", ref: "query q()", label: "Q" }] }), false);
  assert.equal(isDescribedMarkdown({ ...base, sources: [{ kind: "concept_row", ref: "r1", label: "R" }] }), false);
  assert.equal(isDescribedMarkdown({ ...base, sources: [{ kind: "library_file", ref: "f1", label: "F" }, { kind: "query", ref: "query q()", label: "Q" }] }), false, "one data source among the files is enough");
});

// ---------------------------------------------------------------------------
// The package surface
// ---------------------------------------------------------------------------

test("the client barrel exports the materialize surface", () => {
  for (const name of ["materialize", "startMaterialize", "followComposition", "handOffDescription", "isDescribedMarkdown", "fileNameProblem", "fileNameStem", "refusalSentence"] as const) {
    assert.equal(typeof client[name], "function", `${name} is not exported from the client barrel`);
  }
  assert.equal(client.MaterializeCancelled, MaterializeCancelled);
  assert.equal(client.MaterializeFailed, MaterializeFailed);
});
