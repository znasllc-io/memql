import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { readTheBar } from "../../readTheBar";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../../src/apps/deployables/DeployablesApp";
import { useOs } from "../../../src/chrome/state";
import { LocalDeployablesSettingsStore } from "../../../src/apps/deployables/settings";
import { LIBRARY_ARTIFACT_CONCEPT, WORK_STEP_CONCEPT } from "../../../src/apps/deployables/pipelines/rows";
import { MachinesProvider } from "../../../src/live/machines";
import { click, emit, fakeConnection, rowsResult, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { UNCERTAIN_MESSAGE, VIEWER, minutesAgo, packageRow, pipelineRow, runRow, stepRow, uncertainRunRow, uncertainStepRows } from "./fixtures";

// The run page, layout A (epic memql#5479, issue memql#5500), opened the way a
// check run's details link opens it: an intent naming the run.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

function FileDestinationProbe() {
  const { state } = useOs();
  const window = Object.values(state.shell.windows).find(item => item.appId === "files");
  return <output aria-label="Files destination">{JSON.stringify(window?.intent?.payload ?? null)}</output>;
}

function mount(connection: FakeConnection, runId = "r-1", role = "owner") {
  h.connection = connection;
  return render(
    withSession(
      <><DeployablesApp surface="pipelines" sectionId="runs" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} intent={{ id: `open-${runId}`, payload: { runId } }} consumeIntent={vi.fn()} /><FileDestinationProbe /></>,
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
    expect(screen.getByText("Only what changed")).toBeTruthy();
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

  it("opens the exact indexed log without asking Files to search for its backing file", async () => {
    mount(fakeConnection(seed()));
    await screen.findByLabelText("The last lines of tests.unit's log");
    await click(screen.getByRole("button", { name: "Open the full log" }));
    expect(JSON.parse(screen.getByLabelText("Files destination").textContent!)).toEqual({ artifactId: "artifact-log" });
  });

  it("shows confirmed expiry without changing the failed verdict or offering a dead download", async () => {
    mount(fakeConnection(seed({ runArtifacts: { "wr-1": [] }, expiredRunFiles: { "r-1": [
      { fileId: "file-log-unit", name: "tests.unit.log" }, { fileId: "file-cov", name: "dist__coverage.html" },
    ] } })));
    expect(await screen.findByText("Log expired.")).toBeTruthy();
    const artifacts = within(screen.getByRole("region", { name: "Artifacts" }));
    expect(artifacts.getByText("Expired")).toBeTruthy();
    expect(artifacts.getByText("dist/coverage.html")).toBeTruthy();
    expect(artifacts.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("button", { name: "Open the full log" })).toBeNull();
    expect(screen.getByText("The step's command exited with status 1.")).toBeTruthy();
    expect(document.querySelector(".pipeline-log")).toBeNull();
  });

  it.each([undefined, "receipt read failed"])("does not infer expiry from missing files when receipts are %s", async error => {
    mount(fakeConnection(seed({ runArtifacts: { "wr-1": [] }, expiredRunFilesError: error })));
    expect(await screen.findByText("Unavailable")).toBeTruthy();
    expect(screen.queryByText("Expired")).toBeNull();
    expect(screen.queryByText("Log expired.")).toBeNull();
    if (error) expect(screen.getByText("Artifact availability could not be checked.")).toBeTruthy();
  });

  it("removes an already displayed log tail when a confirmed cleanup arrives on the live feed", async () => {
    const connection = fakeConnection(seed({ expiredRunFiles: { "r-1": [{ fileId: "file-log-unit", name: "tests.unit.log" }] } }));
    mount(connection);
    await screen.findByLabelText("The last lines of tests.unit's log");
    await emit(connection, LIBRARY_ARTIFACT_CONCEPT, FILES[0]!, "NODE_DELETED");
    expect(await screen.findByText("Log expired.")).toBeTruthy();
    expect(screen.queryByLabelText("The last lines of tests.unit's log")).toBeNull();
    expect(screen.queryByRole("button", { name: "Open the full log" })).toBeNull();
    expect(screen.getByText("dist/coverage.html")).toBeTruthy();
  });

  it.each(["done", "failed"] as const)("explains a %s step's unsaved log without inventing a manifest error", async (status) => {
    const noLog = stepRow("tests.unit", { seq: 1, runId: "wr-1", status, errorMessage: status === "failed" ? "exit 1" : "", logFileId: "",
      result: { status: status === "done" ? "passed" : "failed", metadata: { notes: [{ code: "pipeline_artifact_missing", message: "The Library refused the log." }] } } });
    mount(fakeConnection(seed({ workSteps: { "wr-1": [STEPS[0]!, noLog] }, runArtifacts: { "wr-1": [] } })));
    expect(await screen.findByText("The Library refused the log.")).toBeTruthy();
    expect(screen.getByText("A log or artifact was not saved")).toBeTruthy();
    expect(screen.queryByText("Check the path in memql-package.yaml.")).toBeNull();
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

  // A run can fail with every stage passed: its workflow erred after them, as
  // the independent review that reached no model did on memql pull requests
  // 6312, 6313 and 6318. The page states the run's own reason off its row.
  it("says why a run failed outside its steps when every stage passed", async () => {
    const reason = "Pipeline workflow failed: every_door_shut: no door to a model is open for this call";
    const outside = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10), failureCode: "pipeline_executor_error", failureMessage: reason,
      stages: [
        { name: "checks", status: "passed", durationMs: 42000, steps: 1, failed: 0 },
        { name: "tests", status: "passed", durationMs: 42000, steps: 2, failed: 0 },
      ] });
    const passed = [stepRow("checks.vet", { seq: 0, runId: "wr-1" }), stepRow("tests.unit", { seq: 1, runId: "wr-1" }), stepRow("tests.lint", { seq: 2, runId: "wr-1" })];
    mount(fakeConnection(seed({ pipelineRuns: [outside], workSteps: { "wr-1": passed }, runArtifacts: { "wr-1": [] } })));
    const notice = (await screen.findByText("The run failed outside its steps.")).closest(".os-notice") as HTMLElement;
    expect(within(notice).getByText(reason)).toBeTruthy();
    expect(notice.getAttribute("data-tone")).toBe("error");
    expect((await screen.findAllByRole("tab")).map((s) => s.closest(".os-rail-stage")?.getAttribute("data-state"))).toEqual(["done", "done"]);
    expect(document.querySelector(".os-actbar-word")?.textContent).toBe("Failed");
    // Nothing failed, so there is no failed step to re-run: the whole run is.
    await waitFor(() => expect(barActs()).toEqual(["Open on GitHub", "Re-run"]));
  });

  it("says why where no stage ran, rather than that there was nothing to run", async () => {
    const reason = "The pipeline DSL selected no stages for a run with an existing work plan, so its original checks cannot be proven. Re-run the pipeline.";
    const outside = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10), stages: [], failureCode: "pipeline_node_lost", failureMessage: reason });
    mount(fakeConnection(seed({ pipelineRuns: [outside], workSteps: { "wr-1": [] }, runArtifacts: { "wr-1": [] } })));
    expect(await screen.findByText("The run failed outside its steps.")).toBeTruthy();
    expect(screen.getByText(reason)).toBeTruthy();
    expect(screen.queryByText("This run had nothing to run.")).toBeNull();
  });

  it("states no reason of its own for a run that failed in a step, or one that did not fail", async () => {
    const { unmount } = mount(fakeConnection(seed()));
    await screen.findByLabelText("The last lines of tests.unit's log");
    expect(screen.queryByText("The run failed outside its steps.")).toBeNull();
    unmount();

    // A reason is read only off a run that completed failed.
    const passed = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10), conclusion: "success", failureMessage: "not a failure",
      stages: [{ name: "checks", status: "passed", durationMs: 42000, steps: 1, failed: 0 }] });
    mount(fakeConnection(seed({ pipelineRuns: [passed], workSteps: { "wr-1": [stepRow("checks.vet", { seq: 0, runId: "wr-1" })] }, runArtifacts: { "wr-1": [] } })));
    await screen.findAllByRole("tab");
    expect(screen.queryByText("The run failed outside its steps.")).toBeNull();
    expect(screen.queryByText("not a failure")).toBeNull();
  });

  it("offers Re-run failed on a failed run, and opens the attempt it starts", async () => {
    const connection = fakeConnection(seed({ pipelinesRerunResult: { runId: "r-2", attempt: 2, status: "queued" } }));
    mount(connection);
    await screen.findByLabelText("The last lines of tests.unit's log");
    expect(barActs()).toEqual(["Open on GitHub", "Re-run", "Re-run failed"]);
    // The acts joined the bar as the run was read, and a bar that has just changed takes no press for a moment.
    await readTheBar();
    await click(screen.getByRole("button", { name: "Re-run failed" }));
    expect(connection.calls).toContain('builtin pipelinesRerun(runId: "r-1", failedOnly: true)');
  });

  it("asks before cancelling a running run", async () => {
    const running = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(2), status: "in_progress", conclusion: "", durationMs: 0 });
    const connection = fakeConnection(seed({ pipelineRuns: [running], workSteps: { "wr-1": [stepRow("checks.vet", { seq: 0, runId: "wr-1", status: "running", binding: {} })] } }));
    mount(connection);
    await screen.findByRole("heading", { name: "Show the cart" });
    await waitFor(() => expect(barActs()).toEqual(["Open on GitHub", "Cancel"]));
    await readTheBar();
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Cancel this run?")).toBeTruthy();
    expect(screen.getByText("Request cancellation. Finished steps keep their results.")).toBeTruthy();
    expect(connection.calls.some((c) => c.startsWith("builtin pipelinesCancel("))).toBe(false);
    await readTheBar();
    await click(screen.getByRole("button", { name: "Cancel the run" }));
    expect(connection.calls).toContain('builtin pipelinesCancel(runId: "r-1")');
  });

  it("offers no re-run while a newer attempt of the key is still running", async () => {
    const newer = runRow({ id: "r-2", ownerUserId: VIEWER, attempt: 2, queuedAt: minutesAgo(1), status: "in_progress", conclusion: "" });
    mount(fakeConnection(seed({ pipelineRuns: [runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10) }), newer] })));
    await screen.findByLabelText("The last lines of tests.unit's log");
    expect(barActs()).toEqual(["Open on GitHub"]);
    expect(screen.getByText(/attempt 2 is in progress/)).toBeTruthy();
  });

  it("keeps a dispatch in progress without inferring executor admission", async () => {
    const dispatched = runRow({ id: "r-1", ownerUserId: VIEWER, status: "in_progress", conclusion: "", finishedAt: "", durationMs: 0,
      stages: [{ name: "checks", status: "running", durationMs: 0, steps: 1, failed: 0 }] });
    mount(fakeConnection(seed({ pipelineRuns: [dispatched], runArtifacts: {}, workSteps: { "wr-1": [
      stepRow("checks.vet", { runId: "wr-1", status: "running", durationMs: 0, logFileId: "", binding: { surface: "cluster", jobName: "suspended-job" } }),
    ] } })));
    await screen.findByText("vet");
    expect(document.querySelector(".os-actbar-word")?.textContent).toBe("In progress");
    expect(within(screen.getByRole("tabpanel")).getByText("In progress")).toBeTruthy();
    expect(screen.queryByText("Running")).toBeNull();
    expect(screen.queryByText(/waiting for capacity/i)).toBeNull();
    expect(barActs()).toEqual(["Open on GitHub", "Cancel"]);
  });

  it("keeps an uncertain receipt visible outside the selected stage without confirming cancellation or offering a re-run", async () => {
    const connection = fakeConnection(seed({ pipelineRuns: [uncertainRunRow()], workSteps: { "wr-uncertain": uncertainStepRows() }, runArtifacts: {} }));
    mount(connection, "r-uncertain");
    const warning = await screen.findByText("The previous step’s outcome is unknown");
    const tabs = screen.getAllByRole("tab");
    expect(tabs[2]?.getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: "deploy Waiting , current stage" })).toBeTruthy();
    expect(screen.getByRole("tabpanel").contains(warning)).toBe(false);
    expect(document.querySelector(".os-actbar-word")?.textContent).toBe("Cancel requested");
    expect(barActs()).toEqual(["Open on GitHub"]);
    expect(screen.queryByText("Cancelled")).toBeNull();
    const details = screen.getByText("Details for tests / os-image").closest("details")!;
    expect(details.open).toBe(false);
    await click(within(details).getByText("Details for tests / os-image"));
    expect(details.open).toBe(true);
    expect(within(details).getByText(UNCERTAIN_MESSAGE)).toBeTruthy();
    await click(tabs[1]!);
    expect(screen.getAllByText("The previous step’s outcome is unknown")).toHaveLength(1);
    expect(screen.getByText("os-image")).toBeTruthy();
    expect(connection.calls.some((c) => c.startsWith("builtin pipelinesRerun(") || c.startsWith("builtin pipelinesCancel("))).toBe(false);
  });

  it("uses the typed receipt on the owning live feed, not error-message text or another run", async () => {
    const steps = uncertainStepRows();
    const ordinary = { ...steps[1]!, errorCode: "pipeline_executor_error" };
    const connection = fakeConnection(seed({ pipelineRuns: [uncertainRunRow()], workSteps: { "wr-uncertain": [steps[0]!, ordinary, steps[2]!] }, runArtifacts: {} }));
    mount(connection, "r-uncertain");
    await screen.findByText("verify");
    expect(screen.queryByText("The previous step’s outcome is unknown")).toBeNull();
    await emit(connection, WORK_STEP_CONCEPT, { ...steps[1]!, id: "foreign-step", runId: "wr-other" }, "NODE_CREATED");
    expect(screen.queryByText("The previous step’s outcome is unknown")).toBeNull();
    await emit(connection, WORK_STEP_CONCEPT, steps[1]!, "NODE_UPDATED");
    expect(await screen.findByText("The previous step’s outcome is unknown")).toBeTruthy();
    expect(barActs()).toEqual(["Open on GitHub"]);
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

// A step that took more than one attempt (the fleet-first retries, epic
// memql#6294): its row's binding holds each attempt, and the page lists them
// under the step -- where each ran by the machine's name, why it ended, and its
// own log -- in the page's existing step detail.
describe("a step's attempts on the run page", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("--- FAIL: TestRunLease (0.31s)\nFAIL\n", { status: 206, headers: { "Content-Range": "bytes 100-130/131" } })));
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const machine = (id: string, name: string): Row => ({
    id: `v1:worker:registration:${id}`, ownerUserId: VIEWER, name: `${id}-host`, displayName: name, platformInfo: {}, labels: {}, operatorLabels: {},
    capabilityDescriptor: {}, concurrency: {}, activeCount: 0, registeredAt: "2026-09-01T00:00:00Z", lastSeenAt: new Date().toISOString(),
  } as unknown as Row);
  const MACHINES = [machine("m-raider", "MSI Raider"), machine("m-cyber", "CyberPC")];

  function mountWithMachines(seedRows: FakeSeed) {
    const connection = fakeConnection(seedRows);
    const query = connection.query as unknown as { executeNamed: (name: string, call: string, opts?: unknown) => Promise<unknown> };
    const inner = query.executeNamed;
    query.executeNamed = vi.fn(async (name: string, call: string, opts?: unknown) => {
      if (call === "query myWorkersWithStatus()") return rowsResult(MACHINES);
      return inner(name, call, opts);
    });
    h.connection = connection;
    return render(withSession(
      <MachinesProvider>
        <DeployablesApp surface="pipelines" sectionId="runs" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} intent={{ id: "open-r-1", payload: { runId: "r-1" } }} consumeIntent={vi.fn()} />
        <FileDestinationProbe />
      </MachinesProvider>,
      { role: "owner", userId: VIEWER },
    ));
  }

  const cloneFailed = { attempt: 1, class: "nothing_ran", code: "pipeline_clone_failed", message: "The machine's git fetch failed.", surface: "fleet", workerId: "m-raider", logFileId: "file-a1" };
  const passedOnCyber = { attempt: 2, surface: "fleet", workerId: "m-cyber", logFileId: "file-a2" };
  const LOGS = [
    { id: "artifact-a1", sourceConceptRef: "v1:library:file:file-a1", title: "tests.unit.log", format: "text", producedByRunId: "wr-1" },
    { id: "artifact-a2", sourceConceptRef: "v1:library:file:file-a2", title: "tests.unit.log", format: "text", producedByRunId: "wr-1" },
  ];
  const stepsOf = (unit: Row): Row[] => [stepRow("checks.vet", { seq: 0, runId: "wr-1" }), unit];
  const passedRun = () => runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10), conclusion: "success",
    stages: [{ name: "checks", status: "passed", durationMs: 42000, steps: 1, failed: 0 }, { name: "tests", status: "passed", durationMs: 42000, steps: 1, failed: 0 }] });
  const retriedUnit = () => stepRow("tests.unit", { seq: 1, runId: "wr-1", logFileId: "file-a2", binding: { surface: "fleet", workerId: "m-cyber", attempts: [cloneFailed, passedOnCyber] } });

  function attemptLines(): string[] {
    return [...document.querySelectorAll(".pipeline-attempt")].map((li) => (li.querySelector("span")?.textContent ?? ""));
  }

  it("lists each attempt of a step that passed on a retry, by machine name, with why it ended and its own log", async () => {
    mountWithMachines({ packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [passedRun()], workSteps: { "wr-1": stepsOf(retriedUnit()) }, runArtifacts: { "wr-1": LOGS } });
    await screen.findByText("Attempt 2 on CyberPC: Passed.");
    expect(attemptLines()).toEqual([
      "Attempt 1 on MSI Raider: The repository could not be fetched at this commit.",
      "Attempt 2 on CyberPC: Passed.",
    ]);
    // The step shows its count beside where it ran last.
    const row = screen.getByText("unit").closest(".os-record-row") as HTMLElement;
    expect(await within(row).findByText("Fleet: CyberPC, 2 attempts")).toBeTruthy();
    expect(within(row).getByText("Passed")).toBeTruthy();
    // The list is the step's own, under its name.
    expect(screen.getByRole("list", { name: "Attempts of unit" })).toBeTruthy();
  });

  it("opens an attempt's own log, and offers none for a file the Library does not hold", async () => {
    mountWithMachines({ packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [passedRun()], workSteps: { "wr-1": stepsOf(retriedUnit()) }, runArtifacts: { "wr-1": [LOGS[0]!] } });
    await screen.findByText("Attempt 2 on CyberPC: Passed.");
    expect(screen.queryByRole("button", { name: "Open the log of attempt 2" })).toBeNull();
    await click(await screen.findByRole("button", { name: "Open the log of attempt 1" }));
    expect(JSON.parse(screen.getByLabelText("Files destination").textContent!)).toEqual({ artifactId: "artifact-a1" });
  });

  it("lists the attempts of a step that failed before the failure it led to, which keeps its own account", async () => {
    const exited = { attempt: 2, message: "The step's command exited with status 1.", surface: "fleet", workerId: "m-cyber", logFileId: "file-a2" };
    const failed = stepRow("tests.unit", { seq: 1, runId: "wr-1", status: "failed", errorCode: "pipeline_executor_error", errorMessage: "The step's command exited with status 1.",
      logFileId: "file-a2", binding: { surface: "fleet", workerId: "m-cyber", attempts: [cloneFailed, exited] } });
    mountWithMachines({ packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(10) })],
      workSteps: { "wr-1": stepsOf(failed) }, runArtifacts: { "wr-1": LOGS } });
    await screen.findByText("Attempt 2 on CyberPC: Failed.");
    // The last attempt says only that the step failed: why, and its log, are the step's account below.
    expect(attemptLines()).toEqual([
      "Attempt 1 on MSI Raider: The repository could not be fetched at this commit.",
      "Attempt 2 on CyberPC: Failed.",
    ]);
    const list = screen.getByRole("list", { name: "Attempts of unit" });
    const failure = document.querySelector(".pipeline-step-failure");
    expect(failure).toBeTruthy();
    expect(list.compareDocumentPosition(failure!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(failure as HTMLElement).getByText("The step's command exited with status 1.")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Open the full log" })).toBeTruthy();
    // One way to the failing attempt's log, not two; the earlier attempt keeps its own.
    expect(screen.queryByRole("button", { name: "Open the log of attempt 2" })).toBeNull();
    expect(screen.getByRole("button", { name: "Open the log of attempt 1" })).toBeTruthy();
  });

  it("says a running step is retrying, on the attempt after the one that failed", async () => {
    const waiting = stepRow("tests.unit", { seq: 1, runId: "wr-1", status: "running", durationMs: 0, logFileId: "", binding: { attempts: [cloneFailed], nextAttemptAt: "2026-10-04T13:05:39Z" } });
    const running = runRow({ id: "r-1", ownerUserId: VIEWER, queuedAt: minutesAgo(2), status: "in_progress", conclusion: "", durationMs: 0, finishedAt: "",
      stages: [{ name: "checks", status: "passed", durationMs: 42000, steps: 1, failed: 0 }, { name: "tests", status: "running", durationMs: 0, steps: 1, failed: 0 }] });
    mountWithMachines({ packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [running], workSteps: { "wr-1": stepsOf(waiting) }, runArtifacts: { "wr-1": LOGS } });
    await screen.findByText("Attempt 1 on MSI Raider: The repository could not be fetched at this commit.");
    const row = screen.getByText("unit").closest(".os-record-row") as HTMLElement;
    expect(within(row).getByText("Retrying")).toBeTruthy();
    expect(within(row).getByText("Attempt 2")).toBeTruthy();
    expect(within(row).queryByText("In progress")).toBeNull();
  });

  it("says nothing of attempts for a step that ran once, or a step with no history", async () => {
    const once = stepRow("tests.unit", { seq: 1, runId: "wr-1", logFileId: "file-a1", binding: { surface: "fleet", workerId: "m-raider", attempts: [{ attempt: 1, surface: "fleet", workerId: "m-raider", logFileId: "file-a1" }] } });
    mountWithMachines({ packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [passedRun()], workSteps: { "wr-1": stepsOf(once) }, runArtifacts: { "wr-1": LOGS } });
    const row = (await screen.findByText("unit")).closest(".os-record-row") as HTMLElement;
    expect(await within(row).findByText("Fleet: MSI Raider")).toBeTruthy();
    expect(document.querySelector(".pipeline-attempts")).toBeNull();
    expect(screen.queryByText(/attempts?\b/i)).toBeNull();
  });

  it("names a machine it cannot find by its place, never by its id", async () => {
    const stranger = stepRow("tests.unit", { seq: 1, runId: "wr-1", logFileId: "file-a2", binding: { surface: "fleet", workerId: "m-removed",
      attempts: [{ ...cloneFailed, workerId: "m-removed" }, { ...passedOnCyber, workerId: "m-removed" }] } });
    mountWithMachines({ packages: [packageRow()], pipelines: [pipelineRow()], pipelineRuns: [passedRun()], workSteps: { "wr-1": stepsOf(stranger) }, runArtifacts: { "wr-1": LOGS } });
    await screen.findByText("Attempt 2 on your fleet: Passed.");
    expect(document.body.textContent).not.toContain("m-removed");
  });
});
