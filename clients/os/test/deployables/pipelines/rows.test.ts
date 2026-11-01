import { describe, expect, it } from "vitest";

import { stepFromRow } from "../../../src/apps/deployables/pipelines/rows";
import { stepRow } from "./fixtures";

// A step's attempts, as its row's binding records them
// (component/pipelinerun's attemptRecord.asMap): the history the run page lists
// when a step took more than one attempt. Read like every other field of a step
// row -- by what a field IS, and never trusting what it holds.

const readAttempts = (attempts: unknown) => stepFromRow(stepRow("build.mac", { binding: { surface: "fleet", workerId: "m-cyber", attempts } })).attempts;

describe("a step's attempts", () => {
  it("reads binding.attempts, one entry to an attempt, in the order they ran", () => {
    expect(readAttempts([
      {
        attempt: 1, class: "nothing_ran", code: "pipeline_clone_failed", message: "The machine's git fetch failed.",
        surface: "fleet", workerId: "m-raider", nodeId: "agent-0", jobName: "", startedAt: "2026-10-04T13:05:02Z",
        finishedAt: "2026-10-04T13:05:09Z", logFileId: "v1:library:file:file-a1",
      },
      { attempt: 2, surface: "fleet", workerId: "m-cyber", logFileId: "file-a2" },
    ])).toEqual([
      { attempt: 1, code: "pipeline_clone_failed", message: "The machine's git fetch failed.", surface: "fleet", workerId: "m-raider", logFileId: "file-a1" },
      { attempt: 2, code: "", message: "", surface: "fleet", workerId: "m-cyber", logFileId: "file-a2" },
    ]);
  });

  it("puts them in attempt order whatever order they arrive in, and keeps one entry to an attempt", () => {
    expect(readAttempts([
      { attempt: 3, code: "pipeline_node_lost" },
      { attempt: 1, code: "pipeline_clone_failed" },
      { attempt: 3, code: "a later copy of attempt 3" },
      { attempt: 2, code: "pipeline_clone_failed" },
    ]).map((a) => [a.attempt, a.code])).toEqual([[1, "pipeline_clone_failed"], [2, "pipeline_clone_failed"], [3, "pipeline_node_lost"]]);
  });

  it("ignores an entry that is not an attempt of a step", () => {
    expect(readAttempts([
      null, "attempt 1", 7, true, [], [{ attempt: 1 }],
      {}, { code: "pipeline_clone_failed" },
      { attempt: 0 }, { attempt: -1 }, { attempt: 1.5 }, { attempt: "2" }, { attempt: null }, { attempt: Number.POSITIVE_INFINITY },
      { attempt: 4, code: "pipeline_node_lost" },
    ]).map((a) => a.attempt)).toEqual([4]);
  });

  it("reads a field it cannot trust as empty, and keeps the attempt", () => {
    expect(readAttempts([{ attempt: 2, code: 7, message: { text: "x" }, surface: "elsewhere", workerId: null, logFileId: ["file"] }])).toEqual([
      { attempt: 2, code: "", message: "", surface: "", workerId: "", logFileId: "" },
    ]);
  });

  it("reads none from a step that never reached the executor, or whose history is not a list", () => {
    expect(stepFromRow(stepRow("checks.vet", { binding: {} })).attempts).toEqual([]);
    expect(stepFromRow(stepRow("checks.vet", { binding: undefined })).attempts).toEqual([]);
    expect(stepFromRow(stepRow("checks.vet", { binding: { surface: "cluster", jobName: "job-1" } })).attempts).toEqual([]);
    for (const attempts of ["attempt 1", 3, { attempt: 1 }, null]) expect(readAttempts(attempts)).toEqual([]);
  });

  it("leaves the step's own place and receipt as they were", () => {
    const s = stepFromRow(stepRow("build.mac", {
      status: "done", logFileId: "file-final",
      binding: { surface: "fleet", workerId: "m-cyber", nodeId: "agent-0", attempts: [{ attempt: 1, code: "pipeline_clone_failed" }, { attempt: 2 }] },
    }));
    expect(s.where).toEqual({ surface: "fleet", nodeId: "agent-0", jobName: "", workerId: "m-cyber" });
    expect(s.logFileId).toBe("file-final");
    expect(s.attempts).toHaveLength(2);
  });
});
