import { describe, expect, it } from "vitest";

import {
  attemptWords, dayLabel, durationWords, refusalPhrase, runOutcome, runTitle, stepLabel, stepWord, triggerWords, whereWords,
} from "../../../src/apps/deployables/pipelines/words";
import { run, step } from "./fixtures";

describe("the pipelines vocabulary", () => {
  it("spells a duration the way the check run does", () => {
    expect(durationWords(0)).toBe("");
    expect(durationWords(400)).toBe("1s");
    expect(durationWords(52000)).toBe("52s");
    expect(durationWords(460000)).toBe("7m 40s");
    expect(durationWords(120000)).toBe("2m");
    expect(durationWords(3780000)).toBe("1h 3m");
  });

  it("reads every outcome in words, never a code", () => {
    expect(runOutcome(run())).toMatchObject({ word: "Failed", detail: "at tests, 7m 40s", mark: "stopped", tone: "warn" });
    expect(runOutcome(run({ conclusion: "success", stages: [{ name: "checks", status: "passed", durationMs: 1, steps: 1, failed: 0 }] })))
      .toMatchObject({ word: "Passed", detail: "1 stage, 7m 40s", mark: "done" });
    expect(runOutcome(run({ status: "queued", conclusion: "", durationMs: 0 }))).toMatchObject({ word: "Queued", detail: "" });
    expect(runOutcome(run({ status: "in_progress", conclusion: "", stages: [{ name: "checks", status: "passed" }, { name: "tests", status: "waiting" }] })))
      .toMatchObject({ word: "Running", detail: "tests", mark: "current" });
    expect(runOutcome(run({ status: "in_progress", conclusion: "", cancelRequested: true, stages: [] }))).toMatchObject({ word: "Cancelling" });
    expect(runOutcome(run({ conclusion: "cancelled", stages: [{ name: "tests", status: "cancelled" }] }))).toMatchObject({ word: "Cancelled", detail: "at tests, 7m 40s" });
  });

  it("reads a refused fork run as an outcome in words (memql#5499)", () => {
    const fork = run({ conclusion: "refused", refusalCode: "pipeline_fork_refused", stages: [], durationMs: 0, workRunId: "" });
    expect(runOutcome(fork)).toEqual({ word: "Refused", detail: "pull request from a fork", tone: "warn", mark: "stopped" });
    expect(refusalPhrase("pipeline_step_invalid")).toBe("the pipeline block is not valid");
    expect(refusalPhrase("credential_revoked")).toBe("the repository could not be read");
  });

  it("says a deploy-plus-notify push that could not notify did not, beside Passed (memql#5499)", () => {
    const push = run({
      event: "push", headBranch: "main", conclusion: "success",
      stages: [
        { name: "checks", status: "passed" }, { name: "tests", status: "passed" },
        { name: "deploy", status: "passed" }, { name: "notify", status: "skipped" },
      ],
    });
    expect(runOutcome(push).detail).toBe("3 stages, notify skipped, 7m 40s");
  });

  it("names what opened the run, and which attempt it is", () => {
    expect(triggerWords(run())).toBe("Pull request #42");
    expect(triggerWords(run({ event: "merge_group" }))).toBe("Merge queue");
    expect(triggerWords(run({ event: "push" }))).toBe("Push");
    expect(triggerWords(run({ event: "release", version: "v1.2.0" }))).toBe("Release v1.2.0");
    expect(attemptWords(run())).toBe("");
    expect(attemptWords(run({ attempt: 2 }))).toBe("Attempt 2");
    expect(attemptWords(run({ attempt: 3, rerunFailedOnly: true }))).toBe("Attempt 3, failed steps only");
    expect(runTitle(run())).toBe("Show the cart");
    expect(runTitle(run({ title: "" }))).toBe("Commit 3f9c2ab");
  });

  it("words a step's state, its shard and where it ran", () => {
    expect(stepWord(step("tests.unit"))).toBe("Passed");
    expect(stepWord(step("tests.unit", { status: "failed", result: { status: "refused" } }))).toBe("Refused");
    expect(stepWord(step("tests.unit", { status: "skipped", errorCode: "pipeline_passed_earlier" }))).toBe("Passed earlier");
    expect(stepWord(step("deploy.verify", { status: "skipped", errorCode: "pipeline_stage_blocked" }))).toBe("Not run");
    expect(stepWord(step("tests.os", { status: "skipped", errorCode: "pipeline_not_affected" }))).toBe("Skipped");
    expect(stepWord(step("tests.unit", { status: "pending" }))).toBe("Waiting");
    expect(stepLabel(step("tests.go#2"), 4)).toBe("go 2 of 4");
    expect(stepLabel(step("tests.unit"), 0)).toBe("unit");
    expect(whereWords(step("tests.unit"))).toBe("Cluster");
    expect(whereWords(step("tests.os", { binding: { surface: "fleet", workerId: "w-1" } }), () => "studio-mac")).toBe("Fleet: studio-mac");
    expect(whereWords(step("tests.os", { binding: {} }))).toBe("");
  });

  it("labels a day the way the Runs list groups it", () => {
    const now = new Date(2026, 9, 4, 15, 0);
    expect(dayLabel(new Date(2026, 9, 4, 1, 0).toISOString(), now)).toBe("Today");
    expect(dayLabel(new Date(2026, 9, 3, 23, 0).toISOString(), now)).toBe("Yesterday");
    expect(dayLabel(new Date(2026, 8, 30, 12, 0).toISOString(), now)).toContain("September");
    expect(dayLabel(new Date(2025, 8, 30, 12, 0).toISOString(), now)).toContain("2025");
  });
});
