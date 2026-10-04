import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { FakeSeed } from "../test/deployables/harness";
import { SHA_A, SHA_B, VIEWER, minutesAgo, packageRow, pipelineRow, runRow, stepRow } from "../test/deployables/pipelines/fixtures";

// THE PIPELINES QA SEEDS (epic memql#5479): one cluster's worth of runs, shaped
// as the engine answers them, so the Runs tab, a run page, a source's Checks
// and the connect rail are judged against the same rows. Built from the
// suite's own fixtures, so a capture cannot disagree with what the tests
// assert.
//
// Times are relative to NOW because the Runs list groups by the reader's own
// day: a fixed date would put every run under one label from the past.

const SHOP = "pl-shop";
const DOCS = "pl-docs";

const at = (minutes: number) => ({ queuedAt: minutesAgo(minutes), startedAt: minutesAgo(minutes - 0.05) });

function finishedAfter(minutes: number, durationMs: number) {
  return { ...at(minutes), finishedAt: new Date(Date.now() - minutes * 60_000 + durationMs).toISOString(), durationMs };
}

const RUNS: Row[] = [
  runRow({
    id: "r-9", ownerUserId: VIEWER, pullRequest: 45, headBranch: "webhook-retry", title: "Retry failed webhook deliveries",
    sha: "9e1d7c3b5a4f2e0d1c9b8a7f6e5d4c3b2a1f0e9d", status: "queued", conclusion: "", stages: [], workRunId: "",
    checkRunState: "written", ...at(1), startedAt: "", finishedAt: "", durationMs: 0,
  }),
  runRow({
    id: "r-8", ownerUserId: VIEWER, pullRequest: 44, headBranch: "saved-card", title: "Pay with a saved card",
    sha: SHA_B, status: "in_progress", conclusion: "", workRunId: "wr-8", ...at(6), finishedAt: "", durationMs: 0,
    stages: [
      { name: "checks", status: "passed", durationMs: 48000, steps: 2, failed: 0 },
      { name: "tests", status: "running", durationMs: 0, steps: 5, failed: 0 },
      { name: "deploy", status: "waiting", durationMs: 0, steps: 1, failed: 0 },
    ],
  }),
  runRow({ id: "r-7", ownerUserId: VIEWER, workRunId: "wr-7", ...finishedAfter(52, 460000) }),
  runRow({
    id: "r-6", ownerUserId: VIEWER, event: "push", pullRequest: 0, headBranch: "main", title: "Merge pull request #40 from acme/receipts",
    sha: "5c4b3a29180716f5e4d3c2b1a09f8e7d6c5b4a39", mode: "full", conclusion: "success", workRunId: "wr-6",
    stages: [
      { name: "checks", status: "passed", durationMs: 51000, steps: 2, failed: 0 },
      { name: "tests", status: "passed", durationMs: 402000, steps: 5, failed: 0 },
      { name: "deploy", status: "passed", durationMs: 95000, steps: 1, failed: 0 },
    ],
    ...finishedAfter(128, 548000),
  }),
  runRow({
    id: "r-5", ownerUserId: VIEWER, pullRequest: 41, headBranch: "patch-1", title: "Fix a typo in the README",
    sha: "0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6", conclusion: "refused", refusalCode: "pipeline_fork_refused",
    refusalMessage: "The pull request's head is in octo-fork/shop, and a fork never runs on this cluster.",
    stages: [], workRunId: "", checkRunState: "written", ...finishedAfter(60 * 26, 0),
  }),
  runRow({
    id: "r-4", ownerUserId: VIEWER, pullRequest: 39, headBranch: "search", title: "Search the catalog by tag",
    sha: "7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a4", conclusion: "cancelled", workRunId: "wr-4",
    stages: [
      { name: "checks", status: "passed", durationMs: 47000, steps: 2, failed: 0 },
      { name: "tests", status: "cancelled", durationMs: 130000, steps: 5, failed: 0 },
      { name: "deploy", status: "blocked", durationMs: 0, steps: 1, failed: 0 },
    ],
    ...finishedAfter(60 * 29, 177000),
  }),
  runRow({
    id: "r-3", ownerUserId: VIEWER, pullRequest: 38, headBranch: "receipts", title: "Email a receipt after checkout",
    sha: "2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e", attempt: 2, trigger: "rerun", rerunOf: "r-2", rerunFailedOnly: true,
    conclusion: "success", workRunId: "wr-3",
    stages: [
      { name: "checks", status: "passed", durationMs: 1000, steps: 2, failed: 0 },
      { name: "tests", status: "passed", durationMs: 188000, steps: 5, failed: 0 },
      { name: "deploy", status: "skipped", durationMs: 0, steps: 1, failed: 0 },
    ],
    ...finishedAfter(60 * 50, 189000),
  }),
  runRow({
    id: "r-2", ownerUserId: VIEWER, pullRequest: 38, headBranch: "receipts", title: "Email a receipt after checkout",
    sha: "2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e", conclusion: "failure", workRunId: "wr-2",
    stages: [
      { name: "checks", status: "passed", durationMs: 49000, steps: 2, failed: 0 },
      { name: "tests", status: "failed", durationMs: 311000, steps: 5, failed: 1 },
      { name: "deploy", status: "blocked", durationMs: 0, steps: 1, failed: 0 },
    ],
    ...finishedAfter(60 * 51, 360000),
  }),
  runRow({
    id: "r-1", ownerUserId: VIEWER, pipelineId: DOCS, repository: "acme/docs", event: "push", pullRequest: 0, headBranch: "main",
    title: "Document the refund window", sha: "c0ffee0123456789abcdef0123456789abcdef01", mode: "full", conclusion: "success",
    workRunId: "wr-1",
    stages: [
      { name: "build", status: "passed", durationMs: 34000, steps: 1, failed: 0 },
      { name: "publish", status: "passed", durationMs: 12000, steps: 1, failed: 0 },
    ],
    ...finishedAfter(60 * 52, 46000),
  }),
];

const PACKAGES: Row[] = [
  packageRow({ latestKnownVersion: SHA_A, deployedVersion: "5c4b3a29180716f5e4d3c2b1a09f8e7d6c5b4a39" }),
  packageRow({ id: "pkg-docs", name: "docs", repoUrl: "https://github.com/acme/docs", deployedVersion: "c0ffee0123456789abcdef0123456789abcdef01", latestKnownVersion: "c0ffee0123456789abcdef0123456789abcdef01" }),
];

const PIPELINES: Row[] = [
  pipelineRow({ id: SHOP }),
  pipelineRow({ id: DOCS, packageId: "pkg-docs", name: "docs", repository: "acme/docs", compute: "fleet", secretNames: [] }),
];

const unit = (key: string, over: Record<string, unknown>) => stepRow(key, { runId: "v1:work:run:wr-7", ...over });

/** The failed attempt's steps: one failure with its log, one artifact, a skip and the blocked deploy. */
const FAILED_STEPS: Row[] = [
  unit("checks.vet", { seq: 0, durationMs: 21000 }),
  unit("checks.lint", { seq: 1, durationMs: 31000 }),
  unit("tests.go-tests#1", { seq: 2, durationMs: 402000 }),
  unit("tests.go-tests#2", { seq: 3, status: "failed", durationMs: 388000, errorMessage: "The step's command exited with status 1.", logFileId: "file-log-go2" }),
  unit("tests.os", { seq: 4, durationMs: 96000, artifactFileIds: ["file-cov"] }),
  unit("tests.e2e", { seq: 5, status: "skipped", errorCode: "pipeline_not_affected", result: { reason: "No change under bucket e2e.", code: "pipeline_not_affected" }, binding: {} }),
  unit("deploy.ship", { seq: 6, status: "skipped", errorCode: "pipeline_stage_blocked", result: { reason: "Not run: stage tests failed." }, binding: {} }),
];

const FAILED_FILES: Row[] = [
  { id: "artifact-log-go2", sourceConceptRef: "v1:library:file:file-log-go2", title: "tests.go-tests#2.log", format: "text", producedByRunId: "wr-7" } as unknown as Row,
  { id: "artifact-cov", sourceConceptRef: "v1:library:file:file-cov", title: "dist__coverage.html", format: "other", producedByRunId: "wr-7" } as unknown as Row,
];

const running = (key: string, over: Record<string, unknown>) => stepRow(key, { runId: "v1:work:run:wr-8", ...over });

/**
 * The running attempt: checks done, tests under way across two shards, deploy
 * waiting. `queue()` writes every step pending before any runs, so the deploy
 * stage's step exists, unstarted, from the first moment.
 */
const RUNNING_STEPS: Row[] = [
  running("checks.vet", { seq: 0, durationMs: 19000 }),
  running("checks.lint", { seq: 1, durationMs: 29000 }),
  running("tests.go-tests#1", { seq: 2, status: "running", durationMs: 0, binding: { surface: "fleet", workerId: "studio-mac", jobName: "" } }),
  running("tests.go-tests#2", { seq: 3, status: "running", durationMs: 0 }),
  running("tests.os", { seq: 4, durationMs: 88000 }),
  running("deploy.ship", { seq: 5, status: "pending", durationMs: 0, binding: {}, logFileId: "" }),
];

/** The last 40 lines a failing Go shard prints, as the content route's suffix range answers them. */
export const FAILED_LOG = [
  "=== RUN   TestCartTotals",
  "=== RUN   TestCartTotals/discount_applies_once",
  "=== RUN   TestCartTotals/discount_and_gift_card",
  "    cart_test.go:118: total = 41.50, want 39.50",
  "    cart_test.go:119: the gift card was applied before the discount",
  "--- FAIL: TestCartTotals (0.02s)",
  "    --- PASS: TestCartTotals/discount_applies_once (0.00s)",
  "    --- FAIL: TestCartTotals/discount_and_gift_card (0.01s)",
  "=== RUN   TestCheckoutIdempotency",
  "--- PASS: TestCheckoutIdempotency (0.31s)",
  "FAIL",
  "FAIL\tgithub.com/acme/shop/cart\t1.204s",
  "ok  \tgithub.com/acme/shop/catalog\t0.811s",
  "ok  \tgithub.com/acme/shop/checkout\t2.017s",
  "FAIL",
].join("\n") + "\n";

/**
 * Answers the Library's content route with `body` and passes everything else
 * through. The run page reads a failed step's last lines with a suffix Range
 * on that route; the harness has no bff behind it.
 */
export function installContentRoute(body: string): void {
  const real = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url.includes("/_memql/artifacts/") && url.includes("/content")) {
      return new Response(body, { status: 206, headers: { "Content-Type": "text/plain" } });
    }
    return real(input, init);
  };
}

/** The populated cluster: two pipelines, runs over three days in every state. */
export const PIPELINES_SEED: FakeSeed = {
  packages: PACKAGES,
  pipelines: PIPELINES,
  pipelineRuns: RUNS,
  workSteps: { "wr-7": FAILED_STEPS, "wr-8": RUNNING_STEPS },
  runArtifacts: { "wr-7": FAILED_FILES },
};

/** A pipeline connected, nothing run yet. */
export const PIPELINES_NO_RUNS_SEED: FakeSeed = {
  packages: PACKAGES.slice(0, 1),
  pipelines: PIPELINES.slice(0, 1),
  pipelineRuns: [],
};
