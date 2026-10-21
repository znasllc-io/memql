import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readTheBarNow } from "../readTheBar";

// Fleet > Routing > Routes (design record 2026-10-18-routes-design.md, "Fleet,
// Routing, Routes" and "Who can do what"): the list of routes a person picks
// from in Ask, a route's page with its jobs and the sources that serve them,
// the New route wizard and Auto's page -- each as an owner, who may change the
// built-ins and Auto's balance, and as a member, who may change only their own
// routes. An act a person may not take is absent, never disabled.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { renderRouting, routingConnection, routeJobs, routeModeRow, settle, shippedRouteModes } = await import("./routingHarness");

afterEach(cleanup);
// A bar that has just changed takes no press for a moment (kit ActionBar): each
// press reads the bar first, on a fake Date so reading costs nothing.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
});
afterEach(() => {
  vi.useRealTimers();
});

type Seed = Parameters<typeof routingConnection>[0];

function use(seed: Seed = {}) {
  const made = routingConnection(seed);
  h.connection = made.connection;
  return made;
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });

function pressBar(name: string | RegExp) {
  readTheBarNow();
  fireEvent.click(within(bar()).getByRole("button", { name }));
}

/** The acts a bar offers, by name. */
const barActs = () => within(bar()).queryAllByRole("button").map((b) => b.textContent?.trim() ?? "");

const job = (title: string) => screen.getByRole("group", { name: title });

/** A job's sources, in the order they are tried. */
function sourcesOf(title: string): string[] {
  const list = within(job(title)).queryByRole("list");
  if (list === null) return [];
  return within(list)
    .getAllByRole("listitem")
    .map((item) => item.querySelector(".fleet-route-chip-name")?.textContent ?? "");
}

const mine = (over: Row = {}) =>
  routeModeRow({ id: "r3", name: "Out of credits", description: "Local models first, then Claude Code", jobs: routeJobs(["fleet:fastest"], ["fleet:strongest", "app:claude-code"], ["app:claude-code"]), ...over });

const changedLocal = () =>
  shippedRouteModes().map((r) => (r.id === "local" ? { ...r, customized: true, jobs: routeJobs(["fleet:fastest"], ["fleet:strongest", "app:claude-code"], ["fleet:strongest"]) } : r));

const openRoute = (id: string) => ({ intent: { id: `open-${id}`, payload: { routingRoute: id } } });

/** The studio machine with its one chat model at every slot. */
const busy = [{ machineId: "studio", memory: { totalBytes: 64e9, freeBytes: 9e9 }, loaded: ["qwen3.8:27b"], models: [{ modelId: "qwen3.8:27b", inFlight: 2, slots: 2 }] }];

const ROUTES_BUILTINS = ["routesForActor", "routeSave", "routeDelete", "routeReset", "autoBalance", "autoBalanceSave", "autoBalanceReset", "myBudget"];

describe("the routes list", () => {
  it("lists Auto, Local and Apps, then the person's own under Yours, each saying whether it is ready", async () => {
    use({
      modes: [
        ...shippedRouteModes(),
        mine(),
        routeModeRow({ id: "r4", name: "Emergency", ready: false, reason: "No signed-in app on your machines", jobs: routeJobs(["app:codex"], ["app:codex"], ["app:codex"]) }),
      ],
    });
    await renderRouting("owner");
    const names = (list: HTMLElement) => within(list).getAllByRole("button").map((b) => b.getAttribute("aria-label"));
    expect(names(screen.getByRole("list", { name: "Built-in routes" }))).toEqual(["Open Auto, Ready", "Open Local, Ready", "Open Apps, Ready"]);
    expect(names(screen.getByRole("list", { name: "Your routes" }))).toEqual(["Open Emergency, No signed-in app on your machines", "Open Out of credits, Ready"]);
    expect(screen.getByRole("button", { name: "New route" })).toBeTruthy();
  });

  it("says a person has no routes of their own yet, and offers the Add", async () => {
    use();
    await renderRouting("owner");
    expect(screen.getByText("Routes you make appear here.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "New route" })).toBeTruthy();
  });

  it("marks a changed built-in, and a route left with unsaved changes", async () => {
    use({ modes: changedLocal() });
    await renderRouting("owner");
    const local = screen.getByRole("button", { name: /^Open Local,/ });
    expect(within(local).getByText("Changed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Open Apps,/ }));
    fireEvent.click(screen.getByRole("button", { name: /^Codex: / }));
    fireEvent.click(within(job("Quick tasks")).getByRole("button", { name: "Add Codex" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Routes" })[0]!);
    expect(within(screen.getByRole("button", { name: /^Open Apps,/ })).getByText("Unsaved")).toBeTruthy();
    // Opened again, the change is still there.
    fireEvent.click(screen.getByRole("button", { name: /^Open Apps,/ }));
    expect(sourcesOf("Quick tasks")).toEqual(["Any signed-in app", "Codex"]);
  });

  it("gives a member the routes, their own and the Add, without Rules or History", async () => {
    use({ modes: [...shippedRouteModes(), mine()] });
    await renderRouting("writer");
    expect(within(screen.getByRole("navigation", { name: "Routing views" })).getAllByRole("button").map((b) => b.textContent)).toEqual(["Routes", "Machines"]);
    expect(screen.getByRole("button", { name: /^Open Out of credits,/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "New route" })).toBeTruthy();
  });

  it("on a cluster without the routes builtins, shows the shipped routes and nothing to save", async () => {
    const made = use();
    made.state.missing = new Set(ROUTES_BUILTINS);
    await renderRouting("owner");
    expect(screen.getByText("This cluster does not save routes yet.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Open Local,/ })).toBeTruthy();
    expect(screen.queryByText("Yours")).toBeNull();
    expect(screen.queryByRole("button", { name: "New route" })).toBeNull();
    // Local opens as shipped, read-only, and says why.
    fireEvent.click(screen.getByRole("button", { name: /^Open Local,/ }));
    expect(sourcesOf("Main work")).toEqual(["Strongest local model"]);
    expect(screen.queryByRole("region", { name: "Sources" })).toBeNull();
    expect(within(bar()).getByText("This cluster cannot save routes yet")).toBeTruthy();
    expect(barActs()).toEqual([]);
  });
});

describe("a route's page", () => {
  it("adds a source by selecting it and then Add on a job, and saves a built-in at the revision it read", async () => {
    const made = use();
    await renderRouting("owner", openRoute("local"));
    expect(within(bar()).getByText("Shipped")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Claude Code: / }));
    fireEvent.click(within(job("Quick tasks")).getByRole("button", { name: "Add Claude Code" }));
    expect(sourcesOf("Quick tasks")).toEqual(["Fastest local model", "Claude Code"]);
    expect(within(bar()).getByText("Unsaved changes")).toBeTruthy();
    expect(barActs()).toEqual(["Discard changes", "Save route"]);
    pressBar("Save route");
    await settle();
    expect(made.calls.routeSave.mock.calls[0]![0]).toEqual({ id: "local", fast: ["fleet:fastest", "app:claude-code"], strong: ["fleet:strongest"], reasoning: ["fleet:strongest"], revision: 7 });
    expect(within(bar()).getByText("Changed from shipped")).toBeTruthy();
    expect(barActs()).toEqual(["Restore default"]);
  });

  it("says why a job cannot take the selected source, and holds four at most", async () => {
    use();
    await renderRouting("owner", openRoute("local"));
    fireEvent.click(screen.getByRole("button", { name: /^Strongest local model: / }));
    expect(within(job("Main work")).getByText("Already in this job")).toBeTruthy();
    expect(within(job("Main work")).queryByRole("button", { name: /^Add / })).toBeNull();
    for (const source of [/^Any signed-in app: /, /^Claude Code: /, /^Cheapest vendor: /]) {
      fireEvent.click(screen.getByRole("button", { name: source }));
      fireEvent.click(within(job("Quick tasks")).getByRole("button", { name: /^Add / }));
    }
    expect(sourcesOf("Quick tasks")).toHaveLength(4);
    fireEvent.click(screen.getByRole("button", { name: /^Strongest vendor: / }));
    expect(within(job("Quick tasks")).getByText("A job holds 4 sources at most")).toBeTruthy();
  });

  it("moves a source with the arrow keys, and removes one with Delete or its x", async () => {
    use({ modes: changedLocal() });
    await renderRouting("owner", openRoute("local"));
    expect(sourcesOf("Main work")).toEqual(["Strongest local model", "Claude Code"]);
    fireEvent.keyDown(within(job("Main work")).getByRole("button", { name: /^1\. Strongest local model,/ }), { key: "ArrowDown" });
    expect(sourcesOf("Main work")).toEqual(["Claude Code", "Strongest local model"]);
    // Focus followed the source it was on.
    expect(document.activeElement?.getAttribute("aria-label")).toMatch(/^2\. Strongest local model,/);
    fireEvent.keyDown(within(job("Main work")).getByRole("button", { name: /^1\. Claude Code,/ }), { key: "Delete" });
    expect(sourcesOf("Main work")).toEqual(["Strongest local model"]);
    fireEvent.click(screen.getByRole("button", { name: "Remove Strongest local model from Main work" }));
    expect(within(job("Main work")).getByText("No source yet")).toBeTruthy();
    // A job with no source cannot be saved.
    expect(within(bar()).getByText("Every job needs a source")).toBeTruthy();
    expect(barActs()).toEqual(["Discard changes"]);
    pressBar("Discard changes");
    expect(sourcesOf("Main work")).toEqual(["Strongest local model", "Claude Code"]);
  });

  it("takes a source dragged from the tray onto a job", async () => {
    use();
    await renderRouting("owner", openRoute("local"));
    const reasoning = job("Deep reasoning");
    fireEvent.dragStart(screen.getByRole("button", { name: /^Any signed-in app: / }));
    fireEvent.dragOver(reasoning);
    fireEvent.drop(reasoning);
    expect(sourcesOf("Deep reasoning")).toEqual(["Strongest local model", "Any signed-in app"]);
  });

  it("asks before restoring a changed built-in, then restores it at the revision it read", async () => {
    const made = use({ modes: changedLocal() });
    await renderRouting("owner", openRoute("local"));
    pressBar("Restore default");
    expect(within(bar()).getByText("Restore the shipped sources?")).toBeTruthy();
    pressBar("Keep");
    expect(made.calls.routeReset).not.toHaveBeenCalled();
    pressBar("Restore default");
    pressBar("Restore default");
    await settle();
    expect(made.calls.routeReset.mock.calls[0]![0]).toEqual({ id: "local", revision: 7 });
    expect(sourcesOf("Main work")).toEqual(["Strongest local model"]);
    expect(within(bar()).getByText("Shipped")).toBeTruthy();
  });

  it("renames a route of yours and saves it without a revision, then deletes it after asking", async () => {
    const made = use({ modes: [...shippedRouteModes(), mine()] });
    await renderRouting("writer", openRoute("r3"));
    expect(within(bar()).getByText("Saved")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Out of tokens" } });
    pressBar("Save route");
    await settle();
    expect(made.calls.routeSave.mock.calls[0]![0]).toEqual({
      id: "r3",
      name: "Out of tokens",
      fast: ["fleet:fastest"],
      strong: ["fleet:strongest", "app:claude-code"],
      reasoning: ["app:claude-code"],
    });
    pressBar("Delete");
    expect(within(bar()).getByText("Delete this route?")).toBeTruthy();
    pressBar("Delete route");
    await settle();
    expect(made.calls.routeDelete.mock.calls[0]![0]).toEqual({ id: "r3" });
    expect(screen.getByRole("list", { name: "Built-in routes" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Open Out of tokens,/ })).toBeNull();
  });

  it("refuses a name a route of yours already has", async () => {
    use({ modes: [...shippedRouteModes(), mine(), routeModeRow({ id: "r4", name: "Emergency", jobs: routeJobs(["app:*"], ["app:*"], ["app:*"]) })] });
    await renderRouting("owner", openRoute("r3"));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "emergency" } });
    expect(screen.getByText("You have a route with that name")).toBeTruthy();
    expect(barActs()).toEqual(["Discard changes"]);
  });

  it("shows a member a built-in's jobs and what serves them, with nothing to change", async () => {
    use({ modes: changedLocal() });
    await renderRouting("writer", openRoute("local"));
    expect(sourcesOf("Main work")).toEqual(["Strongest local model", "Claude Code"]);
    expect(within(job("Main work")).getByText("Serves with qwen3.8:27b")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Sources" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Remove / })).toBeNull();
    expect(within(bar()).getByText("Changed from shipped")).toBeTruthy();
    expect(within(bar()).getByText("An owner or developer can change it")).toBeTruthy();
    expect(barActs()).toEqual([]);
  });

  it("names the cluster's embedding model for Search and memory, and takes nothing there", async () => {
    use();
    await renderRouting("owner", openRoute("apps"));
    const embeddings = job("Search and memory");
    expect(within(embeddings).getByText("nomic-embed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Claude Code: / }));
    expect(within(embeddings).queryByRole("button", { name: /^Add / })).toBeNull();
  });

  it("offers the way to an embedding model when the cluster has none", async () => {
    use({ embedder: "" });
    const onOpenModels = vi.fn();
    await renderRouting("owner", { ...openRoute("apps"), onOpenModels });
    expect(within(job("Search and memory")).getByText("No embedding model yet")).toBeTruthy();
    fireEvent.click(within(job("Search and memory")).getByRole("button", { name: "Add one in Models" }));
    expect(onOpenModels).toHaveBeenCalled();
  });

  it("says when routing changed under the page, and keeps the change to save again", async () => {
    const made = use();
    await renderRouting("owner", openRoute("local"));
    fireEvent.click(screen.getByRole("button", { name: /^Claude Code: / }));
    fireEvent.click(within(job("Quick tasks")).getByRole("button", { name: "Add Claude Code" }));
    made.state.revision = 8;
    pressBar("Save route");
    await settle();
    expect(screen.getByText("Routing changed since this page read it.")).toBeTruthy();
    expect(sourcesOf("Quick tasks")).toEqual(["Fastest local model", "Claude Code"]);
    expect(within(bar()).getByText("Unsaved changes")).toBeTruthy();
  });

  it("says why a local model cannot take calls, when the cluster measures it", async () => {
    use({ capacity: busy });
    await renderRouting("owner", openRoute("local"));
    expect(screen.getByRole("button", { name: "Strongest local model: Busy, 2 of 2" })).toBeTruthy();
    expect(within(job("Main work")).getByText("Busy, 2 of 2")).toBeTruthy();
  });

  it("reads readiness as before on a cluster that does not measure capacity", async () => {
    const made = use({ capacity: busy });
    made.state.missing = new Set(["fleetCapacity"]);
    await renderRouting("owner", openRoute("local"));
    expect(screen.getByRole("button", { name: "Strongest local model: qwen3.8:27b" })).toBeTruthy();
    expect(within(job("Main work")).getByText("Serves with qwen3.8:27b")).toBeTruthy();
  });
});

describe("New route", () => {
  it("names the route, starts from Apps, saves it and opens it", async () => {
    const made = use({ modes: [...shippedRouteModes(), mine()] });
    await renderRouting("writer");
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    expect(barActs()).toEqual(["Cancel"]);
    const name = screen.getByLabelText("Route name");
    fireEvent.change(name, { target: { value: "Local" } });
    expect(within(bar()).getByText("That name is a built-in route")).toBeTruthy();
    fireEvent.change(name, { target: { value: "out of credits" } });
    expect(within(bar()).getByText("You have a route with that name")).toBeTruthy();
    expect(barActs()).toEqual(["Cancel"]);
    fireEvent.change(name, { target: { value: "Night shift" } });
    pressBar("Continue");
    // One choice, answered by choosing.
    fireEvent.click(screen.getByRole("radio", { name: /^Apps/ }));
    expect(sourcesOf("Quick tasks")).toEqual(["Any signed-in app"]);
    pressBar("Save route");
    await settle();
    expect(made.calls.routeSave.mock.calls[0]![0]).toEqual({ name: "Night shift", fast: ["app:*"], strong: ["app:*"], reasoning: ["app:*"] });
    expect(screen.getByRole("heading", { name: "Night shift" })).toBeTruthy();
    expect(within(bar()).getByText("Saved")).toBeTruthy();
  });

  it("copies the cluster's Local as it is now", async () => {
    use({ modes: changedLocal() });
    await renderRouting("owner");
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "Mostly local" } });
    pressBar("Continue");
    fireEvent.click(screen.getByRole("radio", { name: /^Local/ }));
    expect(sourcesOf("Main work")).toEqual(["Strongest local model", "Claude Code"]);
  });

  it("starts empty, and offers Save only once every job has a source", async () => {
    use();
    await renderRouting("owner");
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "Hand picked" } });
    pressBar("Continue");
    fireEvent.click(screen.getByRole("radio", { name: /^Empty/ }));
    expect(within(job("Quick tasks")).getByText("No source yet")).toBeTruthy();
    expect(barActs()).toEqual(["Cancel"]);
    fireEvent.click(screen.getByRole("button", { name: /^Claude Code: / }));
    for (const title of ["Quick tasks", "Main work", "Deep reasoning"]) fireEvent.click(within(job(title)).getByRole("button", { name: "Add Claude Code" }));
    expect(barActs()).toEqual(["Cancel", "Save route"]);
  });
});

describe("Auto", () => {
  it("lets an owner move a weight and save the balance at the revision it read, then reset it", async () => {
    const made = use();
    await renderRouting("owner", openRoute("auto"));
    const speed = screen.getByRole("slider", { name: "Speed" }) as HTMLInputElement;
    expect(speed.value).toBe("50");
    expect(screen.queryByRole("button", { name: "Reset to recommended" })).toBeNull();
    fireEvent.change(speed, { target: { value: "70" } });
    fireEvent.click(screen.getByRole("button", { name: "Save balance" }));
    await settle();
    expect(made.calls.autoBalanceSave.mock.calls[0]![0]).toEqual({ speed: 70, cost: 50, quality: 50, revision: 7 });
    expect((screen.getByRole("slider", { name: "Speed" }) as HTMLInputElement).value).toBe("70");
    fireEvent.click(screen.getByRole("button", { name: "Reset to recommended" }));
    await settle();
    expect(made.calls.autoBalanceReset.mock.calls[0]![0]).toEqual({ revision: 8 });
    expect((screen.getByRole("slider", { name: "Speed" }) as HTMLInputElement).value).toBe("50");
  });

  it("discards a moved weight", async () => {
    const made = use();
    await renderRouting("owner", openRoute("auto"));
    fireEvent.change(screen.getByRole("slider", { name: "Cost" }), { target: { value: "20" } });
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect((screen.getByRole("slider", { name: "Cost" }) as HTMLInputElement).value).toBe("50");
    expect(made.calls.autoBalanceSave).not.toHaveBeenCalled();
  });

  it("shows a member the balance, and who can change it", async () => {
    use({ balance: { speed: 70, cost: 40, quality: 55, recommended: { speed: 50, cost: 50, quality: 50 }, customized: true } });
    await renderRouting("writer", openRoute("auto"));
    const speed = screen.getByRole("slider", { name: "Speed" }) as HTMLInputElement;
    expect(speed.value).toBe("70");
    expect(speed.disabled).toBe(true);
    expect(screen.getByText("An owner or developer can change the balance.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Reset to recommended" })).toBeNull();
  });

  it("states the budget, and what Auto does once it is reached", async () => {
    use({ budget: { monthlyLimitUSD: 50, spentThisMonthUSD: 51.2, resetsAt: "2026-11-01T00:00:00Z", state: "over" } });
    await renderRouting("owner", openRoute("auto"));
    const budget = screen.getByRole("region", { name: "Budget" });
    expect(within(budget).getByText("$50.00")).toBeTruthy();
    expect(within(budget).getByText("$51.20")).toBeTruthy();
    expect(within(budget).getByText("Budget reached. Auto uses only your machines and apps until it resets.")).toBeTruthy();
  });

  it("has no Budget section on a cluster that does not track budgets", async () => {
    const made = use();
    made.state.missing = new Set(["myBudget"]);
    await renderRouting("owner", openRoute("auto"));
    expect(screen.getByRole("region", { name: "Balance" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Budget" })).toBeNull();
  });

  it("says what Auto picks for each job now, and why not the local model when it cannot take the call", async () => {
    use({ capacity: busy });
    await renderRouting("owner", openRoute("auto"));
    const picks = screen.getByRole("region", { name: "What Auto picks now" });
    const valueOf = (label: string) => within(picks).getByText(label).nextElementSibling?.textContent;
    expect(valueOf("Quick tasks")).toBe("Claude CodeFastest local model: busy, 2 of 2");
    expect(valueOf("Main work")).toBe("Claude CodeStrongest local model: busy, 2 of 2");
    expect(valueOf("Search and memory")).toBe("nomic-embed");
  });

  it("picks the local model when it can take the call, with nothing to explain", async () => {
    use();
    await renderRouting("owner", openRoute("auto"));
    const picks = screen.getByRole("region", { name: "What Auto picks now" });
    expect(within(picks).getByText("Main work").nextElementSibling?.textContent).toBe("qwen3.8:27b");
  });
});
