// Runs the editor never finished, and what a finished run's record holds
// (znasllc-io/memql#3886).
//
// TWO DEFECTS, ONE FILE, because they are the two halves of the same record.
//
//  1. A run whose extension host exited mid-flight stayed `running` forever.
//     Nothing was left to write the close, `runsToPrune` deliberately never
//     prunes a non-terminal run, so the file survived every sweep and the tree
//     drew a live spinner for work that ended hours earlier. An operator reads
//     that as "still going" and waits instead of retrying.
//
//  2. A COMPLETED run's items carried nothing but {at, label, status}. The
//     writer replaces the item at a label wholesale, so `stepFinished`
//     overwrote the parameter detail `stepStarted` had just written -- and only
//     put something back on a failure. The deployment view had nothing to show
//     because there was nothing in the file to show.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  reconcileOrphanedRuns,
  readRun,
  runFilePath,
  runsToPrune,
  writeRun,
} from "../src/state/runLog.js";
import { runIsTerminal, type Run, type RunStatus } from "../src/state/deployments.js";

async function tmpdir(): Promise<string> {
  return await fs.mkdtemp(path.join(os.tmpdir(), "memql-runlog-"));
}

function run(id: string, status: RunStatus, over: Partial<Run> = {}): Run {
  return {
    id,
    instance: "local",
    kind: "uninstall",
    startedAt: "2026-08-15T07:39:00.000Z",
    status,
    items: [
      { label: "detect", status: "ok", at: "2026-08-15T07:39:01.000Z" },
      { label: "removeCluster", status: "running", at: "2026-08-15T07:39:02.000Z" },
    ],
    ...over,
  };
}

const AT = "2026-08-15T15:00:00.000Z";

// ---------------------------------------------------------------------------
// closing out what the last host left behind
// ---------------------------------------------------------------------------

test("a run left mid-flight is closed as interrupted, not left spinning", async () => {
  const dir = await tmpdir();
  await writeRun(dir, run("r-running", "running"));

  const closed = await reconcileOrphanedRuns(dir, AT);
  assert.deepEqual(closed.map((r) => r.id), ["r-running"]);

  const onDisk = await readRun(runFilePath(dir, "r-running"));
  assert.equal(onDisk?.status, "interrupted");
  assert.equal(onDisk?.finishedAt, AT);
  assert.equal(runIsTerminal(onDisk!.status), true, "it must never be swept again");
});

test("interrupted is not failed and not cancelled", async () => {
  // The distinction is the whole point. `failed` would file a healthy run in
  // the list an operator scans for defects; `cancelled` would claim somebody
  // decided to stop, when the defining fact is that nobody decided anything.
  const dir = await tmpdir();
  await writeRun(dir, run("r", "running"));
  await reconcileOrphanedRuns(dir, AT);
  const onDisk = await readRun(runFilePath(dir, "r"));
  assert.notEqual(onDisk?.status, "failed");
  assert.notEqual(onDisk?.status, "cancelled");
});

test("interrupted survives a round trip through the parser", async () => {
  // The parse allowlist is separate from the type, and an unrecognised status
  // reads back as `failed` by design. A status this module writes but cannot
  // read would silently become a failure on the next activation.
  const dir = await tmpdir();
  await writeRun(dir, run("r", "running"));
  await reconcileOrphanedRuns(dir, AT);
  const first = await readRun(runFilePath(dir, "r"));
  await writeRun(dir, first!);
  const second = await readRun(runFilePath(dir, "r"));
  assert.equal(second?.status, "interrupted");
});

test("every run kind the recorder can mint reads back as itself", async () => {
  // The parse allowlist is separate from the type, exactly as the status case
  // above records -- and an unrecognised KIND reads back as `install`. A rebuild
  // run (memql#4246) written by the Deployments page and read back as an install
  // would file "I ran my own code" under "I installed a cluster", in the one
  // list an operator scans to answer which of the two happened.
  const dir = await tmpdir();
  const kinds: Run["kind"][] = ["install", "upgrade", "repair", "uninstall", "rebuild", "rollout"];
  for (const kind of kinds) await writeRun(dir, run(`r-${kind}`, "succeeded", { kind }));
  for (const kind of kinds) {
    assert.equal((await readRun(runFilePath(dir, `r-${kind}`)))?.kind, kind);
  }
});

test("runs that already reached a verdict are left exactly alone", async () => {
  const dir = await tmpdir();
  const settled: RunStatus[] = ["succeeded", "failed", "cancelled", "superseded", "rolled_back"];
  for (const status of settled) await writeRun(dir, run(`r-${status}`, status));

  const closed = await reconcileOrphanedRuns(dir, AT);
  assert.deepEqual(closed, [], "nothing terminal may be rewritten");
  for (const status of settled) {
    const onDisk = await readRun(runFilePath(dir, `r-${status}`));
    assert.equal(onDisk?.status, status);
    assert.equal(onDisk?.finishedAt, undefined, "a settled run's timestamps must not move");
  }
});

test("the step the run died in keeps saying so", async () => {
  // Item statuses are deliberately untouched. Read together -- run
  // `interrupted`, step `running` -- the record says exactly what happened, and
  // rewriting the step to `failed` would assert something untrue about work
  // that may well have completed on the machine.
  const dir = await tmpdir();
  await writeRun(dir, run("r", "running"));
  await reconcileOrphanedRuns(dir, AT);
  const onDisk = await readRun(runFilePath(dir, "r"));
  assert.deepEqual(
    onDisk?.items.map((i) => `${i.label}:${i.status}`),
    ["detect:ok", "removeCluster:running"],
  );
});

test("every orphan is closed, not just the newest", async () => {
  // The reported case had three, hours apart, accumulated across sessions.
  const dir = await tmpdir();
  await writeRun(dir, run("r1", "running"));
  await writeRun(dir, run("r2", "running"));
  await writeRun(dir, run("r3", "succeeded"));

  const closed = await reconcileOrphanedRuns(dir, AT);
  assert.deepEqual(closed.map((r) => r.id).sort(), ["r1", "r2"]);
});

test("an empty or absent directory is not an error", async () => {
  const dir = await tmpdir();
  assert.deepEqual(await reconcileOrphanedRuns(dir, AT), []);
});

test("once interrupted, an orphan becomes prunable", async () => {
  // This is what stops the pile-up. `runsToPrune` never prunes a non-terminal
  // run -- correctly, since the one record that must survive a prune is the one
  // being written to -- so before this fix an orphan was immortal.
  const dir = await tmpdir();
  await writeRun(dir, run("r", "running"));
  const before = runsToPrune([run("r", "running")], 0);
  assert.deepEqual(before, [], "a running run is never prunable");

  await reconcileOrphanedRuns(dir, AT);
  const after = runsToPrune([(await readRun(runFilePath(dir, "r")))!], 0);
  assert.deepEqual(after.map((r) => r.id), ["r"]);
});
