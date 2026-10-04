import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { runFromRow, stepFromRow, type RunRow, type StepRow } from "../../../src/apps/deployables/pipelines/rows";

// Wire rows as the engine answers them (dsl/pipelines/shapes.memql,
// dsl/work/shapes.memql), projected through the app's own readers so a test
// asserts what the surfaces read rather than a hand-built projection.

export const OWNER = "v1:identity:user:owner-1";
export const PIPELINE_ID = "pl-shop";
export const PACKAGE_ID = "pkg-shop";
export const SHA_A = "3f9c2ab0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6";
export const SHA_B = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678";

export function runRow(over: Record<string, unknown> = {}): Row {
  return {
    id: "run-1",
    ownerUserId: OWNER,
    pipelineId: PIPELINE_ID,
    repository: "acme/shop",
    sha: SHA_A,
    mode: "affected",
    event: "pull_request",
    runKey: `acme/shop@${SHA_A}:affected:pull_request`,
    attempt: 1,
    trigger: "webhook",
    pullRequest: 42,
    headBranch: "cart",
    title: "Show the cart\n\nLonger body",
    version: SHA_A,
    status: "completed",
    conclusion: "failure",
    checkRunId: "9001",
    checkRunState: "written",
    workRunId: "wr-1",
    stages: [
      { name: "checks", status: "passed", durationMs: 52000, steps: 1, failed: 0 },
      { name: "tests", status: "failed", durationMs: 460000, steps: 3, failed: 1 },
      { name: "deploy", status: "blocked", durationMs: 0, steps: 1, failed: 0 },
    ],
    queuedAt: "2026-10-04T13:05:00Z",
    startedAt: "2026-10-04T13:05:02Z",
    finishedAt: "2026-10-04T13:12:42Z",
    durationMs: 460000,
    ...over,
  } as Row;
}

export function run(over: Record<string, unknown> = {}): RunRow {
  return runFromRow(runRow(over));
}

export function stepRow(key: string, over: Record<string, unknown> = {}): Row {
  const [stage, rest] = key.split(".");
  const name = (rest ?? "").split("#")[0];
  return {
    id: `step-${key}`,
    runId: "v1:work:run:wr-1",
    key,
    seq: 0,
    status: "done",
    call: { construct: "pipeline", stage, name },
    durationMs: 42000,
    binding: { surface: "cluster", nodeId: "workbench-0", jobName: `job-${key}` },
    logFileId: `file-log-${key}`,
    artifactFileIds: [],
    ...over,
  } as Row;
}

export function step(key: string, over: Record<string, unknown> = {}): StepRow {
  return stepFromRow(stepRow(key, over));
}
