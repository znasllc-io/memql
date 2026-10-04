import { describe, expect, it } from "vitest";

import { runBarFor } from "../../../src/apps/deployables/pipelines/acts";
import { problemFrom, previewFromRow } from "../../../src/apps/deployables/pipelines/calls";
import { runGithubUrl } from "../../../src/apps/deployables/pipelines/github";
import { tailOf } from "../../../src/apps/deployables/pipelines/logTail";
import { readRunOpen, scrubbedRunSearch } from "../../../src/apps/deployables/pipelines/openRun";
import { pipelineFromRow } from "../../../src/apps/deployables/pipelines/rows";
import {
  filterRuns, groupByDay, lastRunPerBranch, NO_RUN_FILTER, newerAttemptOf, upstreamChecks,
} from "../../../src/apps/deployables/pipelines/runs";
import { openStopFor, stopsForRun } from "../../../src/apps/deployables/pipelines/stops";
import { PACKAGE_ID, PIPELINE_ID, SHA_A, SHA_B, run, step } from "./fixtures";

const ALL = { rerun: true, cancel: true };
const pipeline = pipelineFromRow({ id: PIPELINE_ID, packageId: PACKAGE_ID, repository: "acme/shop", defaultBranch: "main", status: "active", delivery: "webhook" });

describe("the Runs list's readings", () => {
  const runs = [
    run({ id: "r1", queuedAt: "2026-10-04T13:05:00Z", headBranch: "cart", conclusion: "failure" }),
    run({ id: "r2", queuedAt: "2026-10-04T12:41:00Z", headBranch: "main", event: "push", conclusion: "success", sha: SHA_B, title: "Fix shard balance" }),
    run({ id: "r3", queuedAt: "2026-10-03T09:10:00Z", headBranch: "main", event: "push", status: "in_progress", conclusion: "" }),
    run({ id: "r4", queuedAt: "2026-10-04T14:00:00Z", pipelineId: "pl-other", headBranch: "main", conclusion: "success" }),
  ];

  it("orders newest first and narrows by source, branch, outcome and text", () => {
    expect(filterRuns(runs, NO_RUN_FILTER).map((r) => r.id)).toEqual(["r4", "r1", "r2", "r3"]);
    expect(filterRuns(runs, { ...NO_RUN_FILTER, pipelineId: PIPELINE_ID }).map((r) => r.id)).toEqual(["r1", "r2", "r3"]);
    expect(filterRuns(runs, { ...NO_RUN_FILTER, branch: "main", outcome: "passed" }).map((r) => r.id)).toEqual(["r4", "r2"]);
    expect(filterRuns(runs, { ...NO_RUN_FILTER, search: "shard" }).map((r) => r.id)).toEqual(["r2"]);
    expect(filterRuns(runs, { ...NO_RUN_FILTER, search: "a1b2c3" }).map((r) => r.id)).toEqual(["r2"]);
  });

  it("groups by the day a run was opened, newest day first", () => {
    const days = groupByDay(runs, new Date("2026-10-04T18:00:00Z"));
    expect(days.map((d) => d.runs.map((r) => r.id))).toEqual([["r4", "r1", "r2"], ["r3"]]);
  });

  it("finds the newest attempt of a key, and only a newer one", () => {
    const first = run({ id: "a1", attempt: 1 });
    const second = run({ id: "a2", attempt: 2, status: "in_progress", conclusion: "" });
    expect(newerAttemptOf(first, [first, second])?.id).toBe("a2");
    expect(newerAttemptOf(second, [first, second])).toBeNull();
  });

  it("reads the newest run per branch, the default branch first, the merge queue as one row", () => {
    const rows = lastRunPerBranch([
      run({ id: "c-old", headBranch: "cart", queuedAt: "2026-10-04T10:00:00Z" }),
      run({ id: "c-new", headBranch: "cart", queuedAt: "2026-10-04T13:00:00Z" }),
      run({ id: "m", headBranch: "main", event: "push", queuedAt: "2026-10-04T09:00:00Z" }),
      run({ id: "q", headBranch: "gh-readonly-queue/main/pr-42", event: "merge_group", queuedAt: "2026-10-04T12:00:00Z" }),
      run({ id: "tag", headBranch: "", event: "release", queuedAt: "2026-10-04T14:00:00Z" }),
    ], "main");
    expect(rows.map((r) => r.id)).toEqual(["m", "c-new", "q"]);
  });

  it("says what Latest upstream's checks say, and only what a run says", () => {
    const passed = run({ sha: SHA_A, conclusion: "success", event: "push", headBranch: "main" });
    expect(upstreamChecks([passed], pipeline, SHA_A, "")).toBe("checks passed, not yet deployed");
    expect(upstreamChecks([passed], pipeline, SHA_A.slice(0, 7), SHA_A)).toBe("checks passed");
    expect(upstreamChecks([run({ sha: SHA_A, conclusion: "failure" })], pipeline, SHA_A, "")).toBe("checks failed");
    expect(upstreamChecks([run({ sha: SHA_A, status: "in_progress", conclusion: "" })], pipeline, SHA_A, "")).toBe("checks running");
    expect(upstreamChecks([passed], pipeline, SHA_B, "")).toBe("");
    expect(upstreamChecks([passed], null, SHA_A, "")).toBe("");
  });
});

describe("a run's stops", () => {
  const failedRun = run();
  const steps = [
    step("checks.vet", { seq: 0 }),
    step("tests.unit", { seq: 1, status: "failed", errorCode: "pipeline_executor_error", errorMessage: "exit 1" }),
    step("tests.lint", { seq: 2 }),
    step("tests.os", { seq: 3, status: "skipped", errorCode: "pipeline_not_affected", result: { reason: "No change under bucket os." } }),
    step("deploy.verify", { seq: 4, status: "skipped", errorCode: "pipeline_stage_blocked", result: { reason: "Not run: stage tests failed." } }),
  ];

  it("reads the stages from the step rows, in plan order, a failure not dimming what follows", () => {
    const stops = stopsForRun(failedRun, steps);
    expect(stops.map((s) => [s.name, s.state, s.word])).toEqual([
      ["checks", "done", "Passed"], ["tests", "stopped", "Failed"], ["deploy", "skipped", "Not run"],
    ]);
    expect(openStopFor(stops, failedRun)).toBe("tests");
  });

  it("reads a running run's current stop from the steps, even while the table says waiting", () => {
    const going = run({ status: "in_progress", conclusion: "", stages: [{ name: "checks", status: "passed" }, { name: "tests", status: "waiting" }, { name: "deploy", status: "waiting" }] });
    const stops = stopsForRun(going, [step("checks.vet", { seq: 0 }), step("tests.unit", { seq: 1, status: "running" }), step("deploy.verify", { seq: 2, status: "pending" })]);
    expect(stops.map((s) => s.state)).toEqual(["done", "current", "ahead"]);
    expect(openStopFor(stops, going)).toBe("tests");
  });

  it("gives a stage still running no time: its longest finished step is not its elapsed time", () => {
    const going = run({ status: "in_progress", conclusion: "", stages: [] });
    const stops = stopsForRun(going, [
      step("checks.vet", { seq: 0, durationMs: 29000 }),
      step("tests.unit", { seq: 1, status: "running", durationMs: 0 }),
      step("tests.os", { seq: 2, durationMs: 88000 }),
      step("deploy.verify", { seq: 3, status: "pending", durationMs: 0 }),
    ]);
    expect(stops.map((s) => [s.name, s.word, s.took])).toEqual([
      ["checks", "Passed", "29s"], ["tests", "Running", ""], ["deploy", "Waiting", ""],
    ]);
  });

  it("carries a failed-only re-run's earlier passes as passes", () => {
    const stops = stopsForRun(run({ conclusion: "success" }), [step("checks.vet", { seq: 0, status: "skipped", errorCode: "pipeline_passed_earlier" })]);
    expect(stops[0]).toMatchObject({ state: "done", word: "Passed" });
  });

  it("has no stops for a run refused before any plan", () => {
    const fork = run({ conclusion: "refused", refusalCode: "pipeline_fork_refused", stages: [], workRunId: "" });
    expect(stopsForRun(fork, [])).toEqual([]);
  });
});

describe("the run page's bar", () => {
  const failedSteps = [step("tests.unit", { status: "failed" })];

  it("offers what the engine allows, at most three, one button, primary last", () => {
    expect(runBarFor({ run: run(), steps: failedSteps, newer: null, pipelineActive: true, can: ALL, onGithub: true }).acts)
      .toEqual([{ name: "Open on GitHub", primary: false }, { name: "Re-run", primary: false }, { name: "Re-run failed", primary: true }]);
    expect(runBarFor({ run: run({ conclusion: "success" }), steps: [step("tests.unit")], newer: null, pipelineActive: true, can: ALL, onGithub: true }).acts.map((a) => a.name))
      .toEqual(["Open on GitHub", "Re-run"]);
    const going = runBarFor({ run: run({ status: "in_progress", conclusion: "" }), steps: [], newer: null, pipelineActive: true, can: ALL, onGithub: true });
    expect(going.acts.map((a) => a.name)).toEqual(["Open on GitHub", "Cancel"]);
    expect(going.tone).toBe("busy");
  });

  it("leaves out what is not legal rather than disabling it", () => {
    const fork = run({ conclusion: "refused", refusalCode: "pipeline_fork_refused", workRunId: "" });
    expect(runBarFor({ run: fork, steps: [], newer: null, pipelineActive: true, can: ALL, onGithub: true }).acts.map((a) => a.name)).toEqual(["Open on GitHub"]);
    expect(runBarFor({ run: run(), steps: failedSteps, newer: null, pipelineActive: false, can: ALL, onGithub: true }).acts.map((a) => a.name)).toEqual(["Open on GitHub"]);
    expect(runBarFor({ run: run(), steps: failedSteps, newer: null, pipelineActive: true, can: { rerun: false, cancel: true }, onGithub: false }).acts).toEqual([]);
    const compileRefused = run({ refusalCode: "pipeline_step_invalid", workRunId: "", stages: [] });
    expect(runBarFor({ run: compileRefused, steps: [], newer: null, pipelineActive: true, can: ALL, onGithub: true }).acts.map((a) => a.name)).toEqual(["Open on GitHub", "Re-run"]);
    const cancelling = run({ status: "in_progress", conclusion: "", cancelRequested: true });
    expect(runBarFor({ run: cancelling, steps: [], newer: null, pipelineActive: true, can: ALL, onGithub: true }).acts.map((a) => a.name)).toEqual(["Open on GitHub"]);
  });

  it("offers no re-run while a newer attempt of the key is running, and says why", () => {
    const bar = runBarFor({ run: run(), steps: failedSteps, newer: run({ id: "r2", attempt: 2, status: "in_progress", conclusion: "" }), pipelineActive: true, can: ALL, onGithub: true });
    expect(bar.acts.map((a) => a.name)).toEqual(["Open on GitHub"]);
    expect(bar.detail).toContain("attempt 2 is running");
  });
});

describe("the wire and the address", () => {
  it("opens the check run on GitHub, else the pull request, else the commit", () => {
    expect(runGithubUrl(run())).toBe("https://github.com/acme/shop/runs/9001");
    expect(runGithubUrl(run({ checkRunState: "refused" }))).toBe("https://github.com/acme/shop/pull/42");
    expect(runGithubUrl(run({ checkRunState: "", event: "push" }))).toBe(`https://github.com/acme/shop/commit/${SHA_A}`);
  });

  it("reads a refusal's code, scope and sentence back out of an error", () => {
    expect(problemFrom(new Error("builtin pipelinesRerun: pipeline_nothing_to_rerun: This run has no failed step to run again. Re-run it whole instead.")))
      .toEqual({ code: "pipeline_nothing_to_rerun", scope: "", message: "This run has no failed step to run again. Re-run it whole instead." });
    expect(problemFrom(new Error("pipeline_fleet_not_consented (tests/os-checks): needs docker"))).toEqual({ code: "pipeline_fleet_not_consented", scope: "tests/os-checks", message: "needs docker" });
    expect(problemFrom(new Error("credential_revoked: the grant was revoked"))).toMatchObject({ code: "credential_revoked" });
    expect(problemFrom(new Error("the network went away"))).toEqual({ code: "", scope: "", message: "the network went away" });
  });

  it("reads a preview, its refusal and its existing pipeline", () => {
    const p = previewFromRow({
      repository: "acme/shop", defaultBranch: "main", sha: SHA_A, name: "shop", checkName: "MemQL / shop",
      stages: [{ name: "tests", on: [], channel: "", steps: [{ name: "os", needs: ["docker"], secrets: [], services: [], shards: 0, bucket: "os", packages: "", only: "" }] }],
      needs: ["docker"], secrets: [], suggestedDelivery: "webhook",
      existing: { pipelineId: "pl-1", status: "disconnected", delivery: "poll", compute: "cluster_and_fleet", secretNames: ["A"] },
      refusal: null,
    });
    expect(p.stages[0]?.steps[0]?.needs).toEqual(["docker"]);
    expect(p.existing).toEqual({ pipelineId: "pl-1", status: "disconnected", delivery: "poll", compute: "cluster_and_fleet", secretNames: ["A"] });
    expect(previewFromRow({ refusal: { code: "pipeline_not_declared", message: "none", scope: "" } }).refusal?.code).toBe("pipeline_not_declared");
  });

  it("keeps the last lines of a log, and says when earlier ones exist", () => {
    expect(tailOf("a\nb\nc\n", false)).toEqual({ lines: ["a", "b", "c"], truncated: false });
    expect(tailOf("artial\nb\nc", true)).toEqual({ lines: ["b", "c"], truncated: true });
    const many = Array.from({ length: 60 }, (_, i) => `line ${i}`).join("\n");
    expect(tailOf(many, false).lines).toHaveLength(40);
    expect(tailOf(many, false).truncated).toBe(true);
  });

  it("reads a run named in the address and scrubs only its own parameter", () => {
    expect(readRunOpen("?pipelineRun=run-1&x=1")).toBe("run-1");
    expect(readRunOpen("?pipelineRun=")).toBeNull();
    expect(scrubbedRunSearch("?pipelineRun=run-1&x=1")).toBe("?x=1");
  });
});
