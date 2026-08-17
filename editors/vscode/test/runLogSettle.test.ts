// A run's DERIVED status must be settled over the items being written, not
// over the caller's snapshot.
//
// THE BUG THIS HOLDS DOWN. An uninstall that removed six artifacts and
// correctly preserved a seventh closed like this:
//
//     status: "running"
//     finishedAt: "2026-08-17T17:10:19.884Z"
//     items: 6x ok, 1x preserved          <- every one terminal
//
// A record that says it finished, lists nothing outstanding, and calls itself
// running. `runRowIcon` maps "running" to `loading~spin`, the activation sweep
// only closes runs with NO verdict (this one has a timestamp), and
// `runsToPrune` never prunes a non-terminal run -- so a successful uninstall
// renders a live spinner for as long as the file exists, and reloading does not
// help.
//
// The cause is concurrency the persistence layer already handles and the close
// bypassed. Steps are recorded concurrently; each `recordRunItem` starts from
// the recorder's in-memory run and assigns the merged result back, so
// overlapping records lose one another's update IN MEMORY while the file
// converges -- which is exactly why `mutateRun` re-reads and merges. Settling
// the status over that snapshot saw an item still `pending` and wrote
// "running" beside the on-disk items, all of them finished.
//
// So the assertion is not "succeeded is computed correctly" -- `settleRunStatus`
// was always right about the items it was shown. It is that the close is shown
// the RIGHT ITEMS.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { finishRunSettled, readRun, recordRunItem, runFilePath, writeRun } from "../src/state/runLog.js";
import { runIsTerminal, settleRunStatus, type Run } from "../src/state/deployments.js";

async function tmpdir(): Promise<string> {
  return await fs.mkdtemp(path.join(os.tmpdir(), "memql-runsettle-"));
}

const STARTED = "2026-08-17T17:10:14.441Z";
const FINISHED = "2026-08-17T17:10:19.884Z";

/** The uninstall above, as the recorder first opened it: every step pending. */
function openedRun(): Run {
  return {
    id: "20260817T171014441Z-uninstall-fd88c372",
    instance: "local",
    kind: "uninstall",
    startedAt: STARTED,
    status: "running",
    items: [
      { label: "removeCluster", status: "pending", at: STARTED },
      { label: "removeCheckout", status: "pending", at: STARTED },
      { label: "removeHostsBlock", status: "pending", at: STARTED },
    ],
  };
}

test("a close settles over the items on disk, not the caller's stale snapshot", async () => {
  const dir = await tmpdir();
  const opened = openedRun();
  await writeRun(dir, opened);

  // Every step lands on disk -- including `preserved`, which is the uninstall
  // correctly declining to remove something the operator already had.
  for (const item of [
    { label: "removeCluster", status: "ok" as const, at: FINISHED },
    { label: "removeCheckout", status: "preserved" as const, at: FINISHED },
    { label: "removeHostsBlock", status: "ok" as const, at: FINISHED },
  ]) {
    await recordRunItem(dir, opened, item);
  }

  // ...and the caller closes holding `opened`, whose items still all say
  // pending. This is the lost update, reproduced exactly: a real recorder gets
  // here by two concurrent records overwriting each other's assignment.
  assert.equal(settleRunStatus(opened), "running", "the stale snapshot must still look unfinished, or this test proves nothing");

  const closed = await finishRunSettled(dir, opened, FINISHED, settleRunStatus);

  assert.equal(closed.status, "succeeded");
  assert.ok(runIsTerminal(closed.status), "a closed run must be terminal or it renders a spinner forever");

  const onDisk = await readRun(runFilePath(dir, opened.id));
  assert.ok(onDisk !== null, "the closed record must be readable back");
  assert.equal(onDisk.status, "succeeded", "the FILE is what the tree renders from");
  assert.equal(onDisk.finishedAt, FINISHED);
  assert.equal(onDisk.items.length, 3);
  assert.ok(
    !onDisk.items.some((i) => i.status === "pending" || i.status === "running"),
    "no item may still be open in a record that closed",
  );
});

test("a record never says finished and running at the same time", async () => {
  const dir = await tmpdir();
  const opened = openedRun();
  await writeRun(dir, opened);
  for (const label of ["removeCluster", "removeCheckout", "removeHostsBlock"]) {
    await recordRunItem(dir, opened, { label, status: "ok", at: FINISHED });
  }

  const onDisk = await readRun(
    runFilePath(dir, (await finishRunSettled(dir, opened, FINISHED, settleRunStatus)).id),
  );
  assert.ok(onDisk !== null, "the closed record must be readable back");

  // The self-contradiction is the observable symptom, so it gets its own
  // assertion: whatever the status ends up being, a finishedAt without a
  // terminal status is the shape that spins forever.
  if (onDisk.finishedAt !== undefined) {
    assert.ok(
      runIsTerminal(onDisk.status),
      `record carries finishedAt=${onDisk.finishedAt} but status=${onDisk.status}`,
    );
  }
});

test("a failure still fails the run, settled over the merged items", async () => {
  const dir = await tmpdir();
  const opened = openedRun();
  await writeRun(dir, opened);
  await recordRunItem(dir, opened, { label: "removeCluster", status: "ok", at: FINISHED });
  await recordRunItem(dir, opened, { label: "removeCheckout", status: "failed", at: FINISHED });
  await recordRunItem(dir, opened, { label: "removeHostsBlock", status: "ok", at: FINISHED });

  const closed = await finishRunSettled(dir, opened, FINISHED, settleRunStatus);
  assert.equal(closed.status, "failed", "a failure on disk must not be lost by settling over a snapshot");
});

test("an explicitly supplied status is still the caller's to give", async () => {
  // The abort path knows something the items cannot say. Deriving there would
  // report a cancelled run as succeeded.
  const dir = await tmpdir();
  const opened = openedRun();
  await writeRun(dir, opened);
  for (const label of ["removeCluster", "removeCheckout", "removeHostsBlock"]) {
    await recordRunItem(dir, opened, { label, status: "ok", at: FINISHED });
  }

  const { finishRun } = await import("../src/state/runLog.js");
  const closed = await finishRun(dir, opened, "cancelled", FINISHED);
  assert.equal(closed.status, "cancelled");
});
