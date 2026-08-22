// Moving a local cluster to another release tag (memql#3739).
//
// Three things are asserted here and each one has a failure mode worth naming:
//
//   THE TAG LIST NEVER PICKS FOR YOU, and it degrades to typing. A picker that
//   auto-selected the newest would make "which version is this cluster on"
//   something the extension decided silently, and an operator with no network
//   still has a cluster to move.
//
//   THE FORECAST IS HONEST ABOUT WHAT IT IS. A run that reports fifteen steps
//   and does work in two looks like a full reinstall to whoever is watching it,
//   so the page says which two -- and says the rest are EXPECTED to skip rather
//   than promising they will.
//
//   THE RECORD IS WRITTEN AS THE RUN PROCEEDS, and `preserved` survives it
//   untranslated. A history that rounded `preserved` to "ok" would report a
//   developer's own k3d cluster as removed while it is still there.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import type { ExecEvent } from "../src/install/executor.js";
import type { Graph } from "../src/install/graph.js";
import {
  compareSemverDesc,
  isReleaseTag,
  listReleaseTags,
  parseLsRemoteTags,
  tagProblem,
} from "../src/install/tags.js";
import { listRuns, readRun, runFilePath } from "../src/state/runLog.js";
import { RunRecorder } from "../src/state/runRecorder.js";
import { isSameVersion, upgradePlan, upgradeSummary } from "../src/state/upgradePlan.js";
import { renderChooseTag } from "../src/webview/deploymentScreens.js";
import { refusedPlatformGuidance } from "../src/state/installProgress.js";

function tmpdir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-upgrade-"));
}

// -----------------------------------------------------------------------------
// the tag list
// -----------------------------------------------------------------------------

const LS_REMOTE = [
  "9c1f\trefs/tags/v0.16.1",
  "aa02\trefs/tags/v0.17.0",
  "aa02\trefs/tags/v0.17.0^{}",
  "bb03\trefs/tags/v0.9.2",
  "cc04\trefs/tags/v1.0.0-rc1",
  "dd05\trefs/tags/nightly",
  "ee06\trefs/heads/main",
].join("\n");

test("tags come back newest first, by number and not by string", () => {
  // `v0.9.2` sorts AFTER `v0.17.0` lexicographically, so a string sort would
  // put a nine-month-old release at the top and call it the newest.
  assert.deepEqual(parseLsRemoteTags(LS_REMOTE), ["v0.17.0", "v0.16.1", "v0.9.2"]);
});

test("an annotated tag is offered once, not twice", () => {
  // `^{}` is the commit the tag dereferences to. Both lines name one release.
  assert.equal(parseLsRemoteTags(LS_REMOTE).filter((t) => t === "v0.17.0").length, 1);
});

test("a pre-release, a nightly and a branch are not versions to deploy to", () => {
  const tags = parseLsRemoteTags(LS_REMOTE);
  assert.equal(tags.includes("v1.0.0-rc1"), false);
  assert.equal(tags.includes("nightly"), false);
  assert.equal(tags.includes("main"), false);
});

test("the comparator orders every component numerically", () => {
  assert.deepEqual(
    ["v0.2.0", "v0.10.0", "v1.0.0", "v0.2.10"].sort(compareSemverDesc),
    ["v1.0.0", "v0.10.0", "v0.2.10", "v0.2.0"],
  );
});

test("a listing that fails is an empty list with a reason, never a rejection", async () => {
  // The caller's alternative to a list is a text box, not an error dialog.
  const listing = await listReleaseTags({
    cwd: "/nowhere",
    run: async () => ({ stdout: "", error: "could not run git: ENOENT" }),
  });
  assert.deepEqual(listing.tags, []);
  assert.match(listing.error, /could not run git/);
});

test("a listing that throws is caught, for the same reason", async () => {
  const listing = await listReleaseTags({
    cwd: "/nowhere",
    run: async () => {
      throw new Error("boom");
    },
  });
  assert.deepEqual(listing.tags, []);
  assert.equal(listing.error, "boom");
});

test("an origin with no releases says so rather than reading as a failure", async () => {
  const listing = await listReleaseTags({
    cwd: "/nowhere",
    run: async () => ({ stdout: "aa\trefs/heads/main\n", error: "" }),
  });
  assert.deepEqual(listing.tags, []);
  assert.match(listing.error, /no release tags/);
});

test("an offline operator can still name a tag, and a mistyped one is caught", () => {
  assert.equal(isReleaseTag("v0.18.0"), true);
  assert.equal(tagProblem("v0.18.0"), undefined);
  assert.equal(tagProblem("  v0.18.0  "), undefined);
  assert.match(tagProblem("") ?? "", /Name the release tag/);
  // The failure this catches is a checkout that cannot find the ref, which
  // surfaces deep in the run rather than under the box that produced it.
  assert.match(tagProblem("0.18.0") ?? "", /vMAJOR\.MINOR\.PATCH/);
  assert.match(tagProblem("latest") ?? "", /vMAJOR\.MINOR\.PATCH/);
  assert.match(tagProblem("v1.0.0-rc1") ?? "", /vMAJOR\.MINOR\.PATCH/);
});

// -----------------------------------------------------------------------------
// the forecast
// -----------------------------------------------------------------------------

function graphOf(steps: { id: string; readOnly?: boolean }[]): Graph {
  return {
    name: "install",
    kind: "install",
    description: "test",
    steps: steps.map((s) => ({
      id: s.id,
      description: `${s.id} description`,
      capability: `install.${s.id}`,
      dependsOn: [],
      ...(s.readOnly === true ? { readOnly: true } : {}),
    })),
  } as unknown as Graph;
}

const GRAPH = graphOf([
  { id: "detect", readOnly: true },
  { id: "toolK3d" },
  { id: "hostsBlock" },
  { id: "stackCheckout" },
  { id: "providerKey", readOnly: true },
  { id: "clusterUp" },
]);

test("the forecast names the two steps a tag change is for", () => {
  const plan = upgradePlan({ graph: GRAPH, from: "v0.17.0", to: "v0.18.0" });
  const runs = plan.filter((s) => s.effect === "runs");
  assert.deepEqual(runs.map((s) => s.id), ["stackCheckout", "clusterUp"]);
  assert.equal(runs[0].detail, "v0.17.0 -> v0.18.0");
  assert.equal(runs[1].detail, "reconcile the local overlay");
});

test("a read-only step is a check, not a skip -- they ask different things of the reader", () => {
  const plan = upgradePlan({ graph: GRAPH, from: "v0.17.0", to: "v0.18.0" });
  const byId = new Map(plan.map((s) => [s.id, s]));
  assert.equal(byId.get("detect")?.effect, "verifyOnly");
  assert.equal(byId.get("detect")?.detail, "verify only");
  assert.equal(byId.get("toolK3d")?.effect, "skip");
  assert.equal(byId.get("toolK3d")?.detail, "already satisfied - skip");
});

test("the forecast is in graph order, not wave order", () => {
  // Waves are how the executor SCHEDULES. A list that reordered itself to match
  // would be a display of the executor rather than of the install.
  assert.deepEqual(
    upgradePlan({ graph: GRAPH, from: "a", to: "b" }).map((s) => s.id),
    ["detect", "toolK3d", "hostsBlock", "stackCheckout", "providerKey", "clusterUp"],
  );
});

test("an unknown current version is drawn as the word, never as a blank arrow", () => {
  const plan = upgradePlan({ graph: GRAPH, from: "", to: "v0.18.0" });
  assert.equal(plan.find((s) => s.id === "stackCheckout")?.detail, "unknown -> v0.18.0");
});

test("the summary counts the list it sits above rather than restating it", () => {
  const plan = upgradePlan({ graph: GRAPH, from: "v0.17.0", to: "v0.18.0" });
  assert.equal(
    upgradeSummary(plan),
    "2 steps change something, 2 only check the machine, and 2 should already be satisfied.",
  );
});

test("deploying to the version already installed is allowed, and said", () => {
  // It is a perfectly good way to reconcile a drifted overlay -- which is what
  // a repair is -- but an operator who picked it by accident should find out
  // before the run rather than from a list of skips.
  assert.equal(isSameVersion("v0.17.0", "v0.17.0"), true);
  assert.equal(isSameVersion("v0.17.0", "v0.18.0"), false);
  // An unknown current version is not "the same as" anything.
  assert.equal(isSameVersion("", ""), false);
});

// -----------------------------------------------------------------------------
// the record
// -----------------------------------------------------------------------------

function step(id: string): { id: string; description: string } {
  return { id, description: `${id} description` };
}

function finished(id: string, status: "ok" | "failed" | "skipped" | "preserved", reason = ""): ExecEvent {
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
      startedAt: "2026-08-14T12:00:00Z",
      finishedAt: "2026-08-14T12:00:01Z",
    },
  } as ExecEvent;
}

test("a run is on disk before its first step reports", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir,
    instance: "local",
    kind: "upgrade",
    fromVersion: "v0.17.0",
    toVersion: "v0.18.0",
    now: () => "2026-08-14T12:00:00Z",
    entropy: "abcd1234",
  });
  // The tree reads the run log, so a deployment appears the moment it starts
  // rather than when its first capability answers.
  const onDisk = await readRun(runFilePath(dir, recorder.current.id));
  assert.equal(onDisk?.status, "running");
  assert.equal(onDisk?.fromVersion, "v0.17.0");
  assert.equal(onDisk?.toVersion, "v0.18.0");
});

test("runStarted seeds every step, so an abandoned run names what never ran", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "local", kind: "upgrade", now: () => "t0", entropy: "aaaa1111",
  });
  await recorder.apply({ type: "runStarted", steps: [step("detect"), step("clusterUp")] });
  await recorder.apply({ type: "stepStarted", step: { id: "detect" } as never, params: {} });
  await recorder.apply(finished("detect", "ok"));
  // ...and the editor is killed here.
  const onDisk = await readRun(runFilePath(dir, recorder.current.id));
  assert.deepEqual(
    onDisk?.items.map((i) => `${i.label}:${i.status}`),
    ["detect:ok", "clusterUp:pending"],
  );
});

test("preserved reaches the record untranslated", async () => {
  // THE ONE THAT MATTERS MOST. `preserved` means the uninstall KEPT something
  // the operator already had -- their own k3d cluster, their own mkcert CA.
  // Rounding it to "ok" in the history reports it as removed while it is still
  // on the machine, and rounding it to "failed" invents an incident.
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "local", kind: "uninstall", now: () => "t0", entropy: "bbbb2222",
  });
  await recorder.apply({ type: "runStarted", steps: [step("removeCluster"), step("removeBinary")] });
  await recorder.apply(finished("removeCluster", "preserved", "you created this k3d cluster"));
  await recorder.apply(finished("removeBinary", "ok"));
  const run = await recorder.finish();

  const onDisk = await readRun(runFilePath(dir, run.id));
  const kept = onDisk?.items.find((i) => i.label === "removeCluster");
  assert.equal(kept?.status, "preserved");
  assert.equal(kept?.detail, "you created this k3d cluster");
  // And it does not fail the run: keeping something is the uninstall working.
  assert.equal(onDisk?.status, "succeeded");
});

test("a failed step fails the run and keeps the step's own sentence", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "local", kind: "upgrade", now: () => "t0", entropy: "cccc3333",
  });
  await recorder.apply({ type: "runStarted", steps: [step("stackCheckout")] });
  await recorder.apply(finished("stackCheckout", "failed", "tag v0.18.0 does not exist"));
  const run = await recorder.finish();
  assert.equal(run.status, "failed");
  // The step's own sentence, not the exit-code guidance: the guidance can only
  // be about a CLASS of failure, and this one is about this machine.
  assert.equal(run.items[0].detail, "tag v0.18.0 does not exist");
});

test("logs and wave boundaries are not history", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "local", kind: "upgrade", now: () => "t0", entropy: "dddd4444",
  });
  await recorder.apply({ type: "runStarted", steps: [step("detect")] });
  await recorder.apply({ type: "waveStarted", index: 0, ids: ["detect"] });
  await recorder.apply({ type: "stepLog", step: { id: "detect" } as never, line: "checking..." });
  assert.deepEqual(recorder.current.items.map((i) => i.status), ["pending"]);
});

test("a provider key given as a step param never reaches the record", async () => {
  const dir = await tmpdir();
  const recorder = await RunRecorder.begin({
    dir, instance: "local", kind: "install", now: () => "t0", entropy: "eeee5555",
  });
  await recorder.apply({
    type: "stepStarted",
    step: { id: "providerKey" } as never,
    params: { provider: "anthropic", "key-file": "sk-ant-api03-notarealkey" },
  });
  const text = await fs.readFile(runFilePath(dir, recorder.current.id), "utf8");
  assert.equal(text.includes("sk-ant"), false);
});

test("a write failure never stops the run", async () => {
  // The record is an account of an install. Losing the account is a smaller
  // harm than aborting an install half-way through changing the machine.
  //
  // Unwritable by being a FILE where the directory should be, which fails with
  // ENOTDIR everywhere. An unwritable system path is the obvious alternative
  // and a bad one: `fs.mkdir` under /proc does not fail on this machine, it
  // BLOCKS, and a test that hangs is worse than the one it was replacing.
  const parent = await tmpdir();
  const dir = path.join(parent, "runs");
  await fs.writeFile(dir, "not a directory", "utf8");
  const recorder = await RunRecorder.begin({
    dir,
    instance: "local",
    kind: "install",
    now: () => "t0",
    entropy: "ffff6666",
  });
  await recorder.apply({ type: "runStarted", steps: [step("detect")] });
  const run = await recorder.finish();
  assert.equal(run.id, "t0-install-ffff6666");
  assert.notEqual(recorder.lastError, "");
});

test("a run's record joins the history the tree lists", async () => {
  const dir = await tmpdir();
  for (const [kind, entropy] of [["install", "1111aaaa"], ["upgrade", "2222bbbb"]] as const) {
    const recorder = await RunRecorder.begin({
      dir, instance: "local", kind, now: () => `2026-08-1${kind === "install" ? "1" : "3"}T00:00:00Z`, entropy,
    });
    await recorder.finish("succeeded");
  }
  const runs = await listRuns(dir);
  assert.deepEqual(runs.map((r) => r.kind), ["upgrade", "install"]);
});

test("Create deployment on a refused platform does not offer a tag field", () => {
  const g = refusedPlatformGuidance();
  const html = renderChooseTag({
    instance: {
      name: "local",
      kind: "local",
      presence: "absent",
      connected: false,
    },
    listing: { tags: ["v0.19.6"], error: `${g.headline} ${g.advice}`, refusedPlatform: true },
    target: "",
    tagError: "",
    sameVersion: false,
    plan: [],
    summary: "",
  });
  assert.match(html, /linux\/amd64/);
  assert.match(html, /make up/);
  assert.doesNotMatch(html, /data-field="tag"/);
  assert.doesNotMatch(html, /Type the tag/);
  assert.doesNotMatch(html, /<select/);
});
