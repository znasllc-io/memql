import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { FakeSeed } from "../test/deployables/harness";
import { SHA_A, SHA_B, VIEWER, minutesAgo, packageRow, pipelineRow, runRow, stepRow, uncertainRunRow, uncertainStepRows } from "../test/deployables/pipelines/fixtures";

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
  uncertainRunRow(),
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
  // Every stage passed, then the workflow failed reaching a model for the
  // independent review: the run's own reason is on the row.
  runRow({
    id: "r-10", ownerUserId: VIEWER, pullRequest: 43, headBranch: "delivery-estimate", title: "Show the delivery estimate",
    sha: "4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6071", conclusion: "failure", workRunId: "wr-10",
    failureCode: "pipeline_executor_error", failureMessage: "Pipeline workflow failed: every_door_shut: no door to a model is open for this call",
    stages: [
      { name: "checks", status: "passed", durationMs: 50000, steps: 2, failed: 0 },
      { name: "tests", status: "passed", durationMs: 395000, steps: 3, failed: 0 },
    ],
    ...finishedAfter(90, 452000),
  }),
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

const outside = (key: string, over: Record<string, unknown>) => stepRow(key, { runId: "v1:work:run:wr-10", logFileId: "", ...over });

/** The attempt that failed outside its steps: every step passed. */
const OUTSIDE_STEPS: Row[] = [
  outside("checks.vet", { seq: 0, durationMs: 22000 }),
  outside("checks.lint", { seq: 1, durationMs: 30000 }),
  outside("tests.go-tests#1", { seq: 2, durationMs: 395000 }),
  outside("tests.go-tests#2", { seq: 3, durationMs: 371000 }),
  outside("tests.os", { seq: 4, durationMs: 91000 }),
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

// ---- steps that took more than one attempt (the fleet-first retries) -----------

/** The viewer's machines the attempts ran on, named as the Fleet app names them. */
export const RETRY_MACHINES: Row[] = [
  { id: "v1:worker:registration:m-raider", ownerUserId: VIEWER, name: "msi-raider", displayName: "MSI Raider", platformInfo: { os: "linux", arch: "amd64", hostname: "msi-raider" },
    labels: { pipelines: "allowed" }, operatorLabels: {}, concurrency: { HEADLESS: 2 }, activeCount: 0, registeredAt: "2026-09-01T00:00:00Z", connectedNodeId: "agent-0", lastSeenAt: new Date().toISOString() } as unknown as Row,
  { id: "v1:worker:registration:m-cyber", ownerUserId: VIEWER, name: "cyberpc", displayName: "CyberPC", platformInfo: { os: "linux", arch: "amd64", hostname: "cyberpc" },
    labels: { pipelines: "allowed" }, operatorLabels: {}, concurrency: { HEADLESS: 2 }, activeCount: 0, registeredAt: "2026-09-01T00:00:00Z", connectedNodeId: "agent-0", lastSeenAt: new Date().toISOString() } as unknown as Row,
];

const attemptOf = (attempt: number, over: Record<string, unknown>) => ({ attempt, surface: "fleet", workerId: "m-raider", logFileId: `file-r11-${attempt}`, ...over });
const retried = (key: string, over: Record<string, unknown>) => stepRow(key, { runId: "v1:work:run:wr-11", ...over });

/**
 * A run whose steps needed more than their first attempt: one passed on its
 * second, one failed on its third, one ran once. The failed step's last attempt
 * is the step's log, so its tail is the content route's.
 */
const RETRIED_STEPS: Row[] = [
  retried("checks.vet", { seq: 0, durationMs: 21000, logFileId: "" }),
  retried("tests.go-tests#1", {
    seq: 1, durationMs: 398000, logFileId: "file-r11-2",
    binding: { surface: "fleet", workerId: "m-raider", nodeId: "agent-0", attempts: [
      attemptOf(1, { class: "nothing_ran", code: "pipeline_clone_failed", message: "The machine's git fetch failed before the command ran." }),
      attemptOf(2, {}),
    ] },
  }),
  retried("tests.go-tests#2", {
    seq: 2, status: "failed", durationMs: 388000, errorMessage: "The step's command exited with status 1.", logFileId: "file-r11-3",
    binding: { surface: "fleet", workerId: "m-cyber", nodeId: "agent-0", attempts: [
      attemptOf(1, { class: "nothing_ran", code: "pipeline_clone_failed", message: "The machine's git fetch failed before the command ran.", logFileId: "file-r11-1" }),
      attemptOf(2, { class: "nothing_ran", code: "pipeline_clone_failed", message: "The machine's git fetch failed before the command ran.", logFileId: "file-r11-1b" }),
      attemptOf(3, { class: "final", workerId: "m-cyber", message: "The step's command exited with status 1." }),
    ] },
  }),
  retried("tests.os", { seq: 3, durationMs: 96000, logFileId: "file-r11-os", binding: { surface: "fleet", workerId: "m-cyber", attempts: [attemptOf(1, { workerId: "m-cyber", logFileId: "file-r11-os" })] } }),
  retried("deploy.ship", { seq: 4, status: "skipped", errorCode: "pipeline_stage_blocked", result: { reason: "Not run: stage tests failed." }, binding: {} }),
];

const RETRIED_FILES: Row[] = ["file-r11-1", "file-r11-1b", "file-r11-2", "file-r11-3"].map((id) => (
  { id: `artifact-${id}`, sourceConceptRef: `v1:library:file:${id}`, title: "log", format: "text", producedByRunId: "wr-11" } as unknown as Row
));

const waiting = (key: string, over: Record<string, unknown>) => stepRow(key, { runId: "v1:work:run:wr-12", ...over });

/** A step waiting for its next attempt: the first ended, the row says when the second may start. */
const RETRYING_STEPS: Row[] = [
  waiting("checks.vet", { seq: 0, durationMs: 19000, logFileId: "" }),
  waiting("tests.go-tests#1", {
    seq: 1, status: "running", durationMs: 0, logFileId: "",
    binding: { attempts: [attemptOf(1, { class: "nothing_ran", code: "pipeline_clone_failed", message: "The machine's git fetch failed before the command ran.", logFileId: "file-r12-1" })],
      nextAttemptAt: new Date(Date.now() + 30_000).toISOString() },
  }),
  waiting("tests.go-tests#2", { seq: 2, status: "running", durationMs: 0, logFileId: "", binding: { surface: "fleet", workerId: "m-cyber" } }),
  waiting("deploy.ship", { seq: 3, status: "pending", durationMs: 0, binding: {}, logFileId: "" }),
];

const RETRY_RUNS: Row[] = [
  runRow({
    id: "r-11", ownerUserId: VIEWER, pullRequest: 46, headBranch: "flaky-fetch", title: "Retry a failed fetch on the same machine",
    sha: "8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b", conclusion: "failure", workRunId: "wr-11",
    stages: [
      { name: "checks", status: "passed", durationMs: 21000, steps: 1, failed: 0 },
      { name: "tests", status: "failed", durationMs: 398000, steps: 3, failed: 1 },
      { name: "deploy", status: "blocked", durationMs: 0, steps: 1, failed: 0 },
    ],
    ...finishedAfter(14, 440000),
  }),
  runRow({
    id: "r-12", ownerUserId: VIEWER, pullRequest: 47, headBranch: "retry-wait", title: "Wait between a step's attempts",
    sha: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d", status: "in_progress", conclusion: "", workRunId: "wr-12", ...at(3), finishedAt: "", durationMs: 0,
    stages: [
      { name: "checks", status: "passed", durationMs: 19000, steps: 1, failed: 0 },
      { name: "tests", status: "running", durationMs: 0, steps: 2, failed: 0 },
      { name: "deploy", status: "waiting", durationMs: 0, steps: 1, failed: 0 },
    ],
  }),
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
  workSteps: { "wr-7": FAILED_STEPS, "wr-8": RUNNING_STEPS, "wr-10": OUTSIDE_STEPS, "wr-uncertain": uncertainStepRows() },
  runArtifacts: { "wr-7": FAILED_FILES },
};

/** Runs whose steps took more than one attempt, with the machines that ran them. */
export const PIPELINES_RETRIES_SEED: FakeSeed = {
  ...PIPELINES_SEED,
  pipelineRuns: [...RETRY_RUNS, ...(PIPELINES_SEED.pipelineRuns ?? [])],
  workSteps: { ...PIPELINES_SEED.workSteps, "wr-11": RETRIED_STEPS, "wr-12": RETRYING_STEPS },
  runArtifacts: { ...PIPELINES_SEED.runArtifacts, "wr-11": RETRIED_FILES },
};

/** A pipeline connected, nothing run yet. */
export const PIPELINES_NO_RUNS_SEED: FakeSeed = {
  packages: PACKAGES.slice(0, 1),
  pipelines: PIPELINES.slice(0, 1),
  pipelineRuns: [],
};

const previewStep = (name: string, over: Record<string, unknown> = {}) => ({
  name, packages: "", only: "", shards: 0, bucket: "", needs: [] as string[], secrets: [] as string[], services: [] as string[], ...over,
});

/** What `pipelinesPreview` reads from memql-package.yaml for the shop: no step needs a machine. */
export const PREVIEW = {
  repository: "acme/shop",
  defaultBranch: "main",
  sha: SHA_A,
  name: "shop",
  checkName: "MemQL / shop",
  stages: [
    { name: "checks", on: [], channel: "", steps: [previewStep("vet"), previewStep("lint")] },
    { name: "tests", on: [], channel: "", steps: [previewStep("go-tests", { packages: "affected", shards: 2 }), previewStep("os", { bucket: "os" })] },
    { name: "deploy", on: ["push"], channel: "", steps: [previewStep("ship", { secrets: ["SHOP_TOKEN"] })] },
  ],
  needs: [] as string[],
  secrets: ["SHOP_TOKEN"],
  suggestedDelivery: "webhook",
  existing: null,
  refusal: null,
} as unknown as Row;

/** One of the viewer's machines, its own policy allowing pipeline steps. */
export const ALLOWED_MACHINE = {
  id: "v1:worker:registration:m-studio",
  ownerUserId: VIEWER,
  name: "studio-mac",
  displayName: "",
  platformInfo: { os: "darwin", arch: "arm64", hostname: "studio-mac" },
  labels: { pipelines: "allowed" },
  operatorLabels: {},
  concurrency: { HEADLESS: 2 },
  activeCount: 0,
  registeredAt: "2026-09-01T00:00:00Z",
  connectedNodeId: "agent-0",
  lastSeenAt: new Date().toISOString(),
} as unknown as Row;

/** The shop's source with no pipeline yet: what Connect pipeline opens over. */
export const PIPELINES_UNCONNECTED_SEED: FakeSeed = {
  packages: PACKAGES.slice(0, 1),
  pipelines: [],
  pipelineRuns: [],
  pipelinePreview: PREVIEW,
};

/** Pipelines on two of the list seed's sources, for the Overview map's Checks nodes. */
export function mapPipelines(): Pick<FakeSeed, "pipelines" | "pipelineRuns"> {
  return {
    pipelines: [
      pipelineRow({ id: "pl-acme", packageId: "pkg-acme", name: "acme", repository: "acme/storefront" }),
      pipelineRow({ id: "pl-widgets", packageId: "pkg-widgets", name: "widgets-co", repository: "acme/widgets" }),
    ],
    pipelineRuns: [
      runRow({ id: "rm-1", ownerUserId: VIEWER, pipelineId: "pl-acme", repository: "acme/storefront", event: "push", pullRequest: 0, headBranch: "main", conclusion: "success", workRunId: "", stages: [{ name: "checks", status: "passed", durationMs: 40000, steps: 1, failed: 0 }], ...finishedAfter(30, 40000) }),
      runRow({ id: "rm-2", ownerUserId: VIEWER, pipelineId: "pl-widgets", repository: "acme/widgets", event: "push", pullRequest: 0, headBranch: "main", conclusion: "failure", workRunId: "", ...finishedAfter(90, 460000) }),
    ],
  };
}
