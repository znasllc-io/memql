import { describe, expect, it } from "vitest";

import {
  attemptCountWords, attemptSentence, attemptWords, dayLabel, durationWords, isAccountedBelow, refusalPhrase, runOutcome, runTitle, shownAttempts,
  stepLabel, stepWord, triggerWords, whereWords,
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
    expect(runOutcome(run({ status: "queued", conclusion: "", durationMs: 0 }))).toMatchObject({ word: "Queued", detail: "", mark: "waiting" });
    expect(runOutcome(run({ status: "in_progress", conclusion: "", stages: [{ name: "checks", status: "passed" }, { name: "tests", status: "waiting" }] })))
      .toMatchObject({ word: "In progress", detail: "at tests", mark: "current" });
    expect(runOutcome(run({ status: "in_progress", conclusion: "", cancelRequested: true, stages: [] }))).toMatchObject({ word: "Cancel requested" });
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
    expect(triggerWords(run({ event: "schedule" }))).toBe("Scheduled security scan");
    expect(triggerWords(run({ event: "manual", trigger: "manual" }))).toBe("Manual run");
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
    expect(stepWord(step("tests.unit", { status: "running" }))).toBe("In progress");
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


describe("a step's attempts in words", () => {
  const names: Record<string, string> = { "m-raider": "MSI Raider", "m-cyber": "CyberPC" };
  const machineName = (workerId: string) => names[workerId] ?? "";
  const failedOnRaider = { attempt: 1, code: "pipeline_clone_failed", surface: "fleet", workerId: "m-raider", logFileId: "file-a1" };
  const retried = step("build.mac", {
    binding: { surface: "fleet", workerId: "m-cyber", attempts: [failedOnRaider, { attempt: 2, surface: "fleet", workerId: "m-cyber", logFileId: "file-a2" }] },
  });
  const retrying = step("build.mac", { status: "running", durationMs: 0, binding: { attempts: [failedOnRaider], nextAttemptAt: "2026-10-04T13:05:39Z" } });

  it("counts the attempts beside where the step ran, once there is more than one", () => {
    expect(attemptCountWords(retried)).toBe("2 attempts");
    expect(attemptCountWords(step("build.mac", { binding: { surface: "cluster", attempts: [{ attempt: 1 }] } }))).toBe("");
    expect(attemptCountWords(step("build.mac"))).toBe("");
    // A step still going is on the attempt after the last that ended.
    expect(attemptCountWords(retrying)).toBe("Attempt 2");
    expect(attemptCountWords(step("build.mac", { status: "running", binding: {} }))).toBe("");
  });

  it("calls a running step with a failed attempt behind it retrying, and no other step", () => {
    expect(stepWord(retrying)).toBe("Retrying");
    expect(stepWord(step("build.mac", { status: "running", binding: { surface: "cluster" } }))).toBe("In progress");
    // Settled, the history is not a state: the word is how the step ended.
    expect(stepWord(retried)).toBe("Passed");
    expect(stepWord(step("build.mac", { status: "failed", binding: { attempts: [failedOnRaider, { attempt: 2 }] } }))).toBe("Failed");
  });

  it("lists attempts only for a history worth reading", () => {
    expect(shownAttempts(retried).map((a) => a.attempt)).toEqual([1, 2]);
    expect(shownAttempts(retrying).map((a) => a.attempt)).toEqual([1]);
    // A step that ran once has one record and no story, passed or failed.
    expect(shownAttempts(step("build.mac", { binding: { attempts: [{ attempt: 1 }] } }))).toEqual([]);
    expect(shownAttempts(step("build.mac", { status: "failed", binding: { attempts: [{ attempt: 1, code: "pipeline_clone_failed" }] } }))).toEqual([]);
    expect(shownAttempts(step("build.mac"))).toEqual([]);
  });

  it("says each attempt: where it ran, and how it ended in the page's own words", () => {
    expect(attemptSentence(retried, retried.attempts[0]!, machineName)).toBe("Attempt 1 on MSI Raider: The repository could not be fetched at this commit.");
    expect(attemptSentence(retried, retried.attempts[1]!, machineName)).toBe("Attempt 2 on CyberPC: Passed.");
    // The cluster, a machine nobody can name, and no place recorded.
    const odd = step("build.mac", { status: "running", binding: { attempts: [
      { attempt: 1, code: "pipeline_node_lost", surface: "cluster" },
      { attempt: 2, code: "pipeline_clone_failed", surface: "fleet", workerId: "m-gone" },
      { attempt: 3, code: "pipeline_clone_failed", surface: "fleet" },
      { attempt: 4, code: "pipeline_node_lost" },
    ] } });
    expect(odd.attempts.map((a) => attemptSentence(odd, a, machineName))).toEqual([
      "Attempt 1 on the cluster: The node running the step went away before it finished.",
      "Attempt 2 on your fleet: The repository could not be fetched at this commit.",
      "Attempt 3 on your fleet: The repository could not be fetched at this commit.",
      "Attempt 4: The node running the step went away before it finished.",
    ]);
  });

  it("leaves the account of how a step ended to the step: its last attempt says only the step's word", () => {
    const histories = {
      failed: step("build.mac", { status: "failed", errorMessage: "The step's command exited with status 1.", binding: { surface: "fleet", workerId: "m-cyber", attempts: [
        failedOnRaider, { attempt: 2, message: "The step's command exited with status 1.", surface: "fleet", workerId: "m-cyber" }] } }),
      refused: step("build.mac", { status: "failed", result: { status: "refused" }, binding: { surface: "fleet", workerId: "m-cyber", attempts: [
        failedOnRaider, { attempt: 2, code: "pipeline_no_machine_for_need", surface: "fleet", workerId: "m-cyber" }] } }),
      cancelled: step("build.mac", { status: "cancelled", errorMessage: "The run was cancelled before this step finished.", binding: { surface: "fleet", workerId: "m-cyber", attempts: [
        failedOnRaider, { attempt: 2, message: "The run was cancelled before this step finished.", surface: "fleet", workerId: "m-cyber" }] } }),
    };
    expect(histories.failed.attempts.map((a) => [attemptSentence(histories.failed, a, machineName), isAccountedBelow(histories.failed, a)])).toEqual([
      ["Attempt 1 on MSI Raider: The repository could not be fetched at this commit.", false],
      ["Attempt 2 on CyberPC: Failed.", true],
    ]);
    expect(attemptSentence(histories.refused, histories.refused.attempts[1]!, machineName)).toBe("Attempt 2 on CyberPC: Refused.");
    expect(attemptSentence(histories.cancelled, histories.cancelled.attempts[1]!, machineName)).toBe("Attempt 2 on CyberPC: Cancelled.");
    // A cancelled step with no reason has no account below it, so its last attempt says its own.
    const bare = step("build.mac", { status: "cancelled", errorMessage: "", binding: { attempts: [failedOnRaider, { attempt: 2, message: "Stopped by a cancel.", surface: "fleet", workerId: "m-cyber" }] } });
    expect(isAccountedBelow(bare, bare.attempts[1]!)).toBe(false);
    // A step that passed, or is still going, has no account: every attempt says its own.
    expect(retried.attempts.some((a) => isAccountedBelow(retried, a))).toBe(false);
    expect(retrying.attempts.some((a) => isAccountedBelow(retrying, a))).toBe(false);
  });

  it("never invents a reason: a code this build has no copy for says the server's own words, and none says Failed", () => {
    const unknown = step("build.mac", { status: "failed", binding: { surface: "fleet", workerId: "m-cyber", attempts: [
      { attempt: 1, code: "pipeline_from_the_future", message: "A new kind of fault.", surface: "fleet", workerId: "m-raider" },
      { attempt: 2, message: "The step's command exited with status 75", surface: "fleet", workerId: "m-raider" },
      { attempt: 3, surface: "fleet", workerId: "m-cyber" },
    ] } });
    expect(unknown.attempts.map((a) => attemptSentence(unknown, a, machineName))).toEqual([
      "Attempt 1 on MSI Raider: A new kind of fault.",
      "Attempt 2 on MSI Raider: The step's command exited with status 75.",
      // The step failed, so its last attempt did too, whatever it carried.
      "Attempt 3 on CyberPC: Failed.",
    ]);
    // A code alone, with no copy and no message, is not shown as a word.
    const bare = step("build.mac", { status: "running", binding: { attempts: [{ attempt: 1, code: "pipeline_from_the_future" }] } });
    expect(attemptSentence(bare, bare.attempts[0]!, machineName)).toBe("Attempt 1: Failed.");
  });
});
