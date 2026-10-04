import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../../src/apps/deployables/settings";
import { WORK_STEP_CONCEPT } from "../../../src/apps/deployables/pipelines/rows";
import { click, emit, fakeConnection, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { VIEWER, minutesAgo, packageRow, pipelineRow, runRow, stepRow } from "./fixtures";

// The run page, layout A (epic memql#5479, issue memql#5500), opened the way a
// check run's details link opens it: an intent naming the run.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

function mount(connection: FakeConnection, runId = "r-1", role = "owner") {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId="runs" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} intent={{ id: `open-${runId}`, payload: { runId } }} consumeIntent={vi.fn()} />,
      { role, userId: VIEWER },
    ),
  );
}

const STEPS = [
  stepRow("checks.vet", { seq: 0, runId: "wr-1" }),
  stepRow("tests.unit", { seq: 1, runId: "wr-1", status: "failed", errorMessage: "The step's command exited with status 1.", logFileId: "file-log-unit" }),
  stepRow("tests.lint", { seq: 2, runId: "wr-1", artifactFileIds: ["file-cov"] }),
  stepRow("tests.os", { seq: 3, runId: "wr-1", status: "skipped", errorCode: "pipeline_not_affected", result: { reason: "No change under bucket os.", code: "pipeline_not_affected" }, binding: {} }),
  stepRow("deploy.verify", { seq: 4, runId: "wr-1", status: "skipped", errorCode: "pipeline_stage_blocked", result: { reason: "Not run: stage tests failed." }, binding: {} }),
];

const FILES = [
  { id: "artifact-log", sourceConceptRef: "v1:library:file:file-log-unit", title: "tests.unit.log", format: "text", producedByRunId: "wr-1" },
  { id: "artifact-cov", sourceConceptRef: "v1:library:file:file-cov", title: "dist__coverage.html", format: "other", producedByRunId: "wr-1" },
];

function seed(over: Partial<FakeSeed> = {}): FakeSeed {
  return {
    packages: [packageRow()],
    pipelines: [pipelineRow()],
    pipelineRuns: [runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10) })],
    workSteps: { "wr-1": STEPS },
    runArtifacts: { "wr-1": FILES },
    ...over,
  };
}

function barActs(): string[] {
  return [...document.querySelectorAll(".os-actbar-acts button")].map((b) => b.textContent ?? "");
}

describe("the run page", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("--- FAIL: TestRunLease (0.31s)\n    lease_test.go:88: lease not renewed\nFAIL\n", {
      status: 206,
      headers: { "Content-Range": "bytes 100-180/181" },
    })));
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("draws the stages as stops across the top, a failure not dimming what follows", async () => {
    mount(fakeConnection(seed()));
    expect(await screen.findByRole("heading", { name: "Show the cart" })).toBeTruthy();
    const stops = await screen.findAllByRole("tab");
    expect(stops.map((s) => s.querySelector(".pipeline-stop-name")?.textContent)).toEqual(["checks", "tests", "deploy"]);
    expect(stops.map((s) => s.closest(".os-rail-stage")?.getAttribute("data-state"))).toEqual(["done", "stopped", "skipped"]);
    // The stop the run stopped at is the open one.
    expect(stops[1]!.getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText("Pull request #42")).toBeTruthy();
    expect(screen.getByText("Affected")).toBeTruthy();
  });

  it("shows a failed step's last lines, the way to its whole log, and that it is already in the Library", async () => {
    mount(fakeConnection(seed()));
    const log = await screen.findByLabelText("The last lines of tests.unit's log");
    expect(log.textContent).toContain("lease not renewed");
    expect(screen.getByText("The step's command exited with status 1.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open the full log" })).toBeTruthy();
    expect(screen.getByText("Saved to your Library")).toBeTruthy();
    // The tail was asked for as a suffix range of the log's Library file.
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(String(url)).toContain("/_memql/artifacts/artifact-log/content");
    expect((init as RequestInit).headers).toMatchObject({ Range: "bytes=-16384" });
  });

  it("says a skipped step's reason in words, and lists the artifacts", async () => {
    mount(fakeConnection(seed()));
    await screen.findByLabelText("The last lines of tests.unit's log");
    expect(screen.getByText("No change under bucket os.")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Artifacts" })).getByText("dist/coverage.html")).toBeTruthy();
  });

  it("draws no log box for a step whose log never reached the Library, and says why", async () => {
    const failedNoLog = stepRow("tests.unit", { seq: 1, runId: "wr-1", status: "failed", errorMessage: "exit 1", logFileId: "",
      result: { status: "failed", metadata: { notes: [{ code: "pipeline_artifact_missing", message: "The Library refused the log." }] } } });
    mount(fakeConnection(seed({ workSteps: { "wr-1": [STEPS[0]!, failedNoLog] }, runArtifacts: { "wr-1": [] } })));
    expect(await screen.findByText("The Library refused the log.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open the full log" })).toBeNull();
    expect(document.querySelector(".pipeline-log")).toBeNull();
  });

  it("renders a refused fork's reason in place of stops, with nothing to re-run", async () => {
    const fork = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(5), conclusion: "refused", refusalCode: "pipeline_fork_refused",
      refusalMessage: "The pull request's head is in acme-fork/shop.", stages: [], workRunId: "", checkRunState: "written" });
    mount(fakeConnection(seed({ pipelineRuns: [fork] })));
    expect(await screen.findByText("This cluster does not run pull requests from forks")).toBeTruthy();
    expect(screen.queryAllByRole("tab")).toHaveLength(0);
    expect(barActs()).toEqual(["Open on GitHub"]);
  });

  it("offers Re-run failed on a failed run, and opens the attempt it starts", async () => {
    const connection = fakeConnection(seed({ pipelinesRerunResult: { runId: "r-2", attempt: 2, status: "queued" } }));
    mount(connection);
    await screen.findByLabelText("The last lines of tests.unit's log");
    expect(barActs()).toEqual(["Open on GitHub", "Re-run", "Re-run failed"]);
    await click(screen.getByRole("button", { name: "Re-run failed" }));
    expect(connection.calls).toContain('builtin pipelinesRerun(runId: "r-1", failedOnly: true)');
  });

  it("asks before cancelling a running run", async () => {
    const running = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(2), status: "in_progress", conclusion: "", durationMs: 0 });
    const connection = fakeConnection(seed({ pipelineRuns: [running], workSteps: { "wr-1": [stepRow("checks.vet", { seq: 0, runId: "wr-1", status: "running", binding: {} })] } }));
    mount(connection);
    await screen.findByRole("heading", { name: "Show the cart" });
    await waitFor(() => expect(barActs()).toEqual(["Open on GitHub", "Cancel"]));
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Cancel this run?")).toBeTruthy();
    expect(connection.calls.some((c) => c.startsWith("builtin pipelinesCancel("))).toBe(false);
    await click(screen.getByRole("button", { name: "Cancel the run" }));
    expect(connection.calls).toContain('builtin pipelinesCancel(runId: "r-1")');
  });

  it("offers no re-run while a newer attempt of the key is still running", async () => {
    const newer = runRow({ id: "r-2", ownerUserId: VIEWER, attempt: 2, queuedAt: minutesAgo(1), status: "in_progress", conclusion: "" });
    mount(fakeConnection(seed({ pipelineRuns: [runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10) }), newer] })));
    await screen.findByLabelText("The last lines of tests.unit's log");
    expect(barActs()).toEqual(["Open on GitHub"]);
    expect(screen.getByText(/attempt 2 is running/)).toBeTruthy();
  });

  it("keeps another run's steps out of this run's stages", async () => {
    const connection = fakeConnection(seed());
    mount(connection);
    await screen.findByLabelText("The last lines of tests.unit's log");
    await emit(connection, WORK_STEP_CONCEPT, stepRow("tests.intruder", { id: "step-x", seq: 9, runId: "v1:work:run:wr-OTHER" }), "NODE_CREATED");
    expect(screen.queryByText("intruder")).toBeNull();
  });

  it("leaves out the acts a session does not hold", async () => {
    mount(fakeConnection(seed()), "r-1", "user");
    await screen.findByRole("heading", { name: "Show the cart" });
    await waitFor(() => expect(barActs()).toEqual(["Open on GitHub"]));
  });
});
