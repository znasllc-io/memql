import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../../src/apps/deployables/settings";
import { RUN_CONCEPT } from "../../../src/apps/deployables/pipelines/rows";
import { chooseOption } from "../../selectControl";
import { click, emit, fakeConnection, withSession, type FakeConnection, type FakeSeed } from "../harness";
import { VIEWER, minutesAgo, packageRow, pipelineRow, runRow } from "./fixtures";

// The Runs tab (epic memql#5479, issue memql#5499), through the real app: the
// real feeds, projections and generated builders run, and the assertions are
// what a person sees.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

function mount(connection: FakeConnection | null, opts: { intent?: { id: string; payload: Record<string, unknown> }; navigate?: (id: string) => void } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId="runs" navigate={opts.navigate ?? vi.fn()} askContext={vi.fn()} store={memStore()} {...(opts.intent ? { intent: opts.intent, consumeIntent: vi.fn() } : {})} />,
      { role: "owner", userId: VIEWER },
    ),
  );
}

function seed(over: Partial<FakeSeed> = {}): FakeSeed {
  return {
    packages: [packageRow()],
    pipelines: [pipelineRow()],
    pipelineRuns: [
      runRow({ id: "r-failed", ownerUserId: VIEWER, queuedAt: minutesAgo(30), title: "Show the cart", headBranch: "cart" }),
      runRow({ id: "r-passed", ownerUserId: VIEWER, queuedAt: minutesAgo(50), title: "Fix shard balance", headBranch: "main", event: "push", conclusion: "success",
        stages: [{ name: "checks", status: "passed" }, { name: "tests", status: "passed" }, { name: "deploy", status: "passed" }, { name: "notify", status: "skipped" }] }),
      runRow({ id: "r-old", ownerUserId: VIEWER, queuedAt: minutesAgo(60 * 30), title: "Bump toolchain", headBranch: "main", event: "push", conclusion: "success" }),
    ],
    ...over,
  };
}

function rowFor(title: string): HTMLElement {
  return screen.getByText(title).closest(".os-livelist-row") as HTMLElement;
}

describe("the Runs tab", () => {
  beforeEach(() => {
    h.connection = null;
    // Keep the fixture's recent runs inside the reader's current day. Real
    // timers still drive the live feed and testing-library's async waits.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(2026, 9, 5, 12));
  });
  afterEach(() => vi.useRealTimers());

  it("lists every run newest first under its day, one line each", async () => {
    mount(fakeConnection(seed()));
    await screen.findByText("Show the cart");
    const days = [...document.querySelectorAll(".pipeline-runs-day-label")].map((el) => el.textContent);
    expect(days[0]).toBe("Today");
    expect(days.length).toBeGreaterThanOrEqual(2);
    const names = [...document.querySelectorAll(".pipeline-runs-day .os-row-name")].map((el) => el.textContent);
    expect(names).toEqual(["Show the cart", "Fix shard balance", "Bump toolchain"]);

    const failed = rowFor("Show the cart");
    expect(within(failed).getByText("shop")).toBeTruthy();
    expect(within(failed).getByText("cart")).toBeTruthy();
    expect(within(failed).getByText("3f9c2ab")).toBeTruthy();
    expect(within(failed).getByText("Pull request #42")).toBeTruthy();
    expect(within(failed).getByText("Failed")).toBeTruthy();
    expect(failed.querySelector(".pipeline-run-detail")?.textContent).toBe("at tests, 7m 40s");
    // A push that ran every stage but could not notify says so, in words, and
    // a narrow column wraps between its phrases, never inside one.
    const pushed = rowFor("Fix shard balance").querySelector(".pipeline-run-detail");
    expect(pushed?.textContent).toBe("3 stages, notify skipped, 7m 40s");
    expect([...(pushed?.querySelectorAll(".pipeline-nowrap") ?? [])].map((p) => p.textContent)).toEqual(["3 stages", "notify skipped", "7m 40s"]);
  });

  it("narrows by outcome behind Refine, with a chip that takes the question back", async () => {
    mount(fakeConnection(seed()));
    await screen.findByText("Show the cart");
    await click(screen.getByRole("button", { name: /Refine runs/i }));
    chooseOption(screen.getByLabelText("Outcome"), "Failed");
    await waitFor(() => expect(screen.queryByText("Fix shard balance")).toBeNull());
    expect(screen.getByText("Show the cart")).toBeTruthy();
    // The chip says what was asked, and removing it takes the question back.
    await click(screen.getByRole("button", { name: /Failed/ }));
    expect(await screen.findByText("Fix shard balance")).toBeTruthy();
  });

  it("groups recent runs under Yesterday just after the reader's midnight", async () => {
    vi.setSystemTime(new Date(2026, 9, 6, 0, 10));
    mount(fakeConnection(seed()));
    await screen.findByText("Show the cart");
    const days = [...document.querySelectorAll(".pipeline-runs-day-label")].map((el) => el.textContent);
    expect(days[0]).toBe("Yesterday");
    expect(days).not.toContain("Today");
  });

  it("rings a run when it has an answer, and not when it arrives or moves", async () => {
    const connection = fakeConnection(seed());
    mount(connection);
    await screen.findByText("Show the cart");
    const queued = runRow({ id: "r-new", ownerUserId: VIEWER, queuedAt: minutesAgo(1), title: "New change", status: "queued", conclusion: "", stages: [], durationMs: 0 });
    await emit(connection, RUN_CONCEPT, queued, "NODE_CREATED");
    await screen.findByText("New change");
    expect(rowFor("New change").dataset.arrival).toBeUndefined();
    await emit(connection, RUN_CONCEPT, { ...queued, status: "in_progress", stages: [{ name: "checks", status: "running" }] });
    expect(rowFor("New change").dataset.arrival).toBeUndefined();
    await emit(connection, RUN_CONCEPT, { ...queued, status: "completed", conclusion: "success", durationMs: 60000, stages: [{ name: "checks", status: "passed" }] });
    await waitFor(() => expect(rowFor("New change").dataset.arrival).toBe("updated"));
  });

  it("says there are no runs yet only after the read answered, and points at Sources", async () => {
    const navigate = vi.fn();
    mount(fakeConnection(seed({ pipelineRuns: [] })), { navigate });
    expect(await screen.findByText("No runs yet")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Open Sources" }));
    expect(navigate).toHaveBeenCalledWith("sources", { fromContent: true });
  });

  it("opens a run's page with the trail back to Runs", async () => {
    mount(fakeConnection(seed()));
    await click(await screen.findByRole("button", { name: /^Open Show the cart/ }));
    expect(await screen.findByRole("heading", { name: "Show the cart" })).toBeTruthy();
    await click(screen.getAllByRole("button", { name: /Runs/ })[0]!);
    expect(await screen.findByText("Fix shard balance")).toBeTruthy();
  });

  it("reads older runs on request, after the live window", async () => {
    // The live window walks four pages; the fifth is the past, read on request.
    const page = (n: number) => [runRow({ id: `r-p${n}`, ownerUserId: VIEWER, queuedAt: minutesAgo(60 * 24 * n), title: `Change ${n}` })];
    mount(fakeConnection(seed({ pipelineRunPages: [seed().pipelineRuns!, page(2), page(3), page(4),
      [runRow({ id: "r-older", ownerUserId: VIEWER, queuedAt: minutesAgo(60 * 24 * 40), title: "Ancient change" })]] })));
    await screen.findByText("Change 4");
    expect(screen.queryByText("Ancient change")).toBeNull();
    await click(screen.getByRole("button", { name: "Show older runs" }));
    expect(await screen.findByText("Ancient change")).toBeTruthy();
  });

  it("opens the run a check run's details link names", async () => {
    const navigate = vi.fn();
    mount(fakeConnection(seed()), { navigate, intent: { id: "i-1", payload: { runId: "r-failed" } } });
    expect(navigate).toHaveBeenCalledWith("runs");
    expect(await screen.findByRole("heading", { name: "Show the cart" })).toBeTruthy();
  });
});
