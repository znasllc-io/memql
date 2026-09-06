// A failing step's own output is kept where the run record points at it
// (znasllc-io/memql#5059).
//
// The record said:
//
//   rebuildFromCheckout  failed  exit 5: building the voice image
//                        (memql-voice:local) failed -- the docker build output
//                        above is the account of it
//
// There was no output above. A run record is a JSON document holding one detail
// line per step, and it is the only thing that survives the run; the sentence
// was true of a terminal and false of the artifact it was written into. The
// actual cause was one line of BuildKit output that nothing kept.
//
// So: the recorder writes a failing step's stderr to a `.log` beside the record
// and names it in the detail. These tests pin that, the bound on its size, the
// redaction on the way in, and the two negatives -- a successful step writes
// nothing, and the log never lands as a `.json` the Deployments tree watches.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import type { ExecEvent } from "../src/install/executor.js";
import { REDACTED, redactLogText } from "../src/install/secrets.js";
import { readRun, runFilePath } from "../src/state/runLog.js";
import { RunRecorder } from "../src/state/runRecorder.js";

function tmpdir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-steplog-"));
}

function step(id: string): { id: string; description: string } {
  return { id, description: `${id} description` };
}

function finished(
  id: string,
  status: "ok" | "failed" | "skipped" | "preserved",
  reason = "",
  log = "",
): ExecEvent {
  return {
    type: "stepFinished",
    step: { id, description: "", capability: "", dependsOn: [] } as never,
    outcome: {
      id,
      script: "s.sh",
      status,
      exitCode: status === "failed" ? 5 : 0,
      envelope: null,
      verified: status === "ok",
      preExisting: status === "preserved",
      params: {},
      ...(reason !== "" ? { reason } : {}),
      ...(log !== "" ? { log } : {}),
      startedAt: "2026-09-06T22:16:31Z",
      finishedAt: "2026-09-06T22:23:14Z",
    },
  } as ExecEvent;
}

// The line that was the whole answer, and that nothing kept.
const BUILDKIT_ERROR =
  'ERROR: failed to build: failed to solve: target stage "voice-runtime" could not be found';

// ---------------------------------------------------------------------------
// the record now points at something that exists
// ---------------------------------------------------------------------------

test("a failed step's output is written beside the record and named in the detail", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "aaaa1111",
  });
  await recorder.apply({ type: "runStarted", steps: [step("rebuildFromCheckout")] });
  await recorder.apply(
    finished(
      "rebuildFromCheckout",
      "failed",
      "exit 5: building the voice image (memql-voice:local) failed",
      "#12 [builder 5/9] RUN go build\n" + BUILDKIT_ERROR + "\n",
    ),
  );
  const run = await recorder.finish();

  const onDisk = await readRun(runFilePath(dir, run.id));
  const item = onDisk?.items.find((i) => i.label === "rebuildFromCheckout");
  assert.ok(item?.detail?.includes("log="), `detail names no log: ${item?.detail}`);

  // The name in the detail resolves to a file, and that file holds the answer.
  const named = /log=(\S+)/.exec(item?.detail ?? "")?.[1] ?? "";
  assert.notEqual(named, "");
  const body = await fs.readFile(path.join(dir, named), "utf8");
  assert.ok(
    body.includes(BUILDKIT_ERROR),
    "the log beside the record does not contain the error that caused the failure",
  );
});

test("the step's own sentence still leads the detail", async () => {
  // The log is ADDITIONAL evidence, not a replacement for the sentence: a
  // reader scanning the history must still see what happened without opening a
  // file.
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "bbbb2222",
  });
  await recorder.apply({ type: "runStarted", steps: [step("rebuildFromCheckout")] });
  await recorder.apply(
    finished("rebuildFromCheckout", "failed", "building the voice image failed", BUILDKIT_ERROR),
  );
  const run = await recorder.finish();

  assert.ok(run.items[0].detail?.startsWith("building the voice image failed"));
});

test("the log is not a .json, so the Deployments watcher does not repaint on it", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "cccc3333",
  });
  await recorder.apply({ type: "runStarted", steps: [step("rebuildFromCheckout")] });
  await recorder.apply(finished("rebuildFromCheckout", "failed", "failed", BUILDKIT_ERROR));
  await recorder.finish();

  const entries = await fs.readdir(dir);
  const jsons = entries.filter((e) => e.endsWith(".json"));
  assert.equal(jsons.length, 1, `expected one .json run record, got ${jsons.join(", ")}`);
  assert.equal(entries.filter((e) => e.endsWith(".log")).length, 1);
});

// ---------------------------------------------------------------------------
// the negatives
// ---------------------------------------------------------------------------

test("a successful step writes no log", async () => {
  // Every run would otherwise pay for output nobody needs.
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "dddd4444",
  });
  await recorder.apply({ type: "runStarted", steps: [step("updateCheckout")] });
  await recorder.apply(finished("updateCheckout", "ok", "", "lots of chatter on stderr"));
  const run = await recorder.finish();

  assert.equal((await fs.readdir(dir)).filter((e) => e.endsWith(".log")).length, 0);
  assert.equal((run.items[0].detail ?? "").includes("log="), false);
});

test("a failed step with no output names no log", async () => {
  // A pointer to a file that was never written is the bug this fixes, spelled
  // the other way round.
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "eeee5555",
  });
  await recorder.apply({ type: "runStarted", steps: [step("rebuildFromCheckout")] });
  await recorder.apply(finished("rebuildFromCheckout", "failed", "it broke"));
  const run = await recorder.finish();

  assert.equal((await fs.readdir(dir)).filter((e) => e.endsWith(".log")).length, 0);
  assert.equal((run.items[0].detail ?? "").includes("log="), false);
});

// ---------------------------------------------------------------------------
// the bound, and what must not reach the file
// ---------------------------------------------------------------------------

test("a huge build log is kept as a bounded tail, and the tail is the end", async () => {
  // BuildKit puts the resolved error LAST, which is why the tail is the half
  // worth keeping. The run directory is not a build cache.
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "ffff6666",
  });
  const huge = "x".repeat(400 * 1024) + "\n" + BUILDKIT_ERROR;
  await recorder.apply({ type: "runStarted", steps: [step("rebuildFromCheckout")] });
  await recorder.apply(finished("rebuildFromCheckout", "failed", "failed", huge));
  const run = await recorder.finish();

  const named = /log=(\S+)/.exec(run.items[0].detail ?? "")?.[1] ?? "";
  const body = await fs.readFile(path.join(dir, named), "utf8");
  assert.ok(Buffer.byteLength(body, "utf8") < 200 * 1024, "the log is not bounded");
  assert.ok(body.includes(BUILDKIT_ERROR), "the bound dropped the end, which is where the error is");
  assert.ok(body.includes("earlier output dropped"), "a truncated log does not say it was truncated");
});

test("a credential echoed into the build output never reaches the file", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "memql", kind: "update", now: () => "t0", entropy: "00007777",
  });
  const key = "sk-" + "a".repeat(40);
  const enrol = "mql_enr_" + "b".repeat(43);
  await recorder.apply({ type: "runStarted", steps: [step("rebuildFromCheckout")] });
  await recorder.apply(
    finished("rebuildFromCheckout", "failed", "failed", `+ curl -H 'Bearer ${key}'\nlink ${enrol}\n`),
  );
  const run = await recorder.finish();

  const named = /log=(\S+)/.exec(run.items[0].detail ?? "")?.[1] ?? "";
  const body = await fs.readFile(path.join(dir, named), "utf8");
  assert.equal(body.includes(key), false, "a provider key reached the run directory");
  assert.equal(body.includes(enrol), false, "an enrolment token reached the run directory");
  assert.ok(body.includes(REDACTED));
});

// ---------------------------------------------------------------------------
// the redaction itself, on the shape that made it necessary
// ---------------------------------------------------------------------------

test("redactLogText finds credentials MID-LINE, which the param scan cannot", () => {
  // `looksLikeProviderKey` is anchored to a whole value because a param IS the
  // value. Free text is the different shape this exists for.
  const key = "sk-" + "z".repeat(32);
  const out = redactLogText(`docker build --secret id=k,env=KEY  # ${key} leaked here`);
  assert.equal(out.includes(key), false);
  assert.ok(out.includes(REDACTED));
  // ...and it leaves the readable part readable, which is the point of keeping
  // the log at all.
  assert.ok(out.includes("docker build --secret"));
});

test("redactLogText leaves ordinary build output untouched", () => {
  const line = "#12 [builder 5/9] RUN go build -tags voice -o /app/bin/memql .";
  assert.equal(redactLogText(line), line);
});
