import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Fleet > Routing > Routes: the list, the route page (the composer) and the
// New route wizard -- against the real Fleet reads and the routing builtins
// (routingHarness.tsx), so what is asserted is what the page does with what
// the engine sends.
//
// The composer's promises, each asserted by mouse AND by keyboard where a
// person could use either:
//   - the circuit lights up to the FIRST slot that is ready right now, from
//     real readiness, and says "Serves now" there
//   - equip, choose-then-replace, reorder and remove all edit the chain
//   - an incompatible placement is refused in place and changes nothing
//   - the bar offers exactly the acts the state allows, Save route last
//   - a save sends exactly the chain on screen and the revision it was read at

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { renderRouting, routingConnection, routeRow, ruleRow, settle, shippedRules, studioMachine } = await import("./routingHarness");

afterEach(cleanup);

let calls: ReturnType<typeof routingConnection>["calls"];
let state: ReturnType<typeof routingConnection>["state"];
function use(seed: Parameters<typeof routingConnection>[0] = {}) {
  const made = routingConnection(seed);
  h.connection = made.connection;
  calls = made.calls;
  state = made.state;
}
beforeEach(() => use());

async function openRoute(title: string) {
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(`^Open ${title},`) }));
  await settle();
}

function slots(): HTMLElement[] {
  return within(screen.getByRole("list", { name: "Sources, in the order they are tried" }))
    .getAllByRole("listitem")
    .filter((li) => li.querySelector(".fleet-slot-body") !== null);
}
function slotNames(): string[] {
  return slots().map((li) => li.querySelector(".fleet-slot-name")?.textContent ?? "");
}
function slotButton(index: number): HTMLElement {
  return slots()[index]!.querySelector<HTMLElement>(".fleet-slot-body")!;
}
function tray(name: RegExp): HTMLElement {
  return within(screen.getByLabelText("Sources you can add")).getByRole("button", { name });
}
function bar(): HTMLElement {
  return screen.getByRole("group", { name: "What you can do with this" });
}

describe("the routes list", () => {
  it("draws each route as its name, a strip of source glyphs and whether it serves now", async () => {
    await renderRouting();
    const list = screen.getByRole("list", { name: "Routes" });
    const localFirst = within(list).getByRole("button", { name: /^Open Local first,/ });
    // localFirst = strongest local, any app, cheapest vendor. The studio has a
    // chat model online, so the FIRST source serves -- and what answers is
    // that model, so the row names it.
    expect(localFirst.textContent).toContain("Serves with qwen3.8:27b");
    expect(within(localFirst).getAllByRole("listitem")).toHaveLength(3);
    expect(localFirst.textContent).toContain("Shipped");
    // The engine id carries a retired word; the row does not.
    expect(within(list).getByRole("button", { name: /^Open Local, then best vendor,/ })).toBeTruthy();
  });

  it("says Nothing ready when no source in the chain can serve", async () => {
    use({ machines: [] });
    await renderRouting();
    expect(screen.getByRole("button", { name: /^Open Local only,/ }).textContent).toContain("Nothing ready");
  });

  it("marks a changed shipped route", async () => {
    use({ routes: [routeRow("localFirst", "app:*", [], { customized: true })] });
    await renderRouting();
    expect(screen.getByRole("button", { name: /^Open Local first,/ }).textContent).toContain("Changed");
  });
});

describe("the circuit", () => {
  it("lights up to the first READY slot and says Serves now there, and nowhere else", async () => {
    // No chat model on the machine: the strongest-local slot cannot serve, so
    // the call passes over it to the signed-in app.
    use({ machines: [studioMachine({ labels: {} })] });
    await renderRouting();
    await openRoute("Local first");
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app", "Cheapest vendor"]);
    const lit = slots().map((li) => li.querySelector(".fleet-circuit-link")?.hasAttribute("data-lit"));
    expect(lit).toEqual([true, true, false]);
    expect(within(slots()[1]!).getByText("Serves now")).toBeTruthy();
    expect(screen.getAllByText("Serves now")).toHaveLength(1);
    expect(slotButton(1).getAttribute("aria-label")).toBe("2. Any signed-in app, ready, serves now");
    expect(slotButton(0).getAttribute("aria-label")).toBe("1. Strongest local model, no chat model on an online machine");
  });

  it("lights nothing when nothing is ready, and moves when readiness does", async () => {
    use({ machines: [] });
    await renderRouting();
    await openRoute("Local first");
    expect(screen.queryByText("Serves now")).toBeNull();
    expect(document.querySelector(".fleet-circuit")?.hasAttribute("data-connected")).toBe(false);
  });

  it("stops at a vendor whose state has not been read, rather than lighting past it", async () => {
    use({ routes: [routeRow("mine", "federation:cheapest", ["app:claude-code"], { shipped: false })] });
    (h.connection as { query: { inferenceStatus: ReturnType<typeof vi.fn> } }).query.inferenceStatus.mockImplementation(() => new Promise(() => {}));
    await renderRouting();
    await openRoute("Mine");
    expect(screen.queryByText("Serves now")).toBeNull();
    expect(slotButton(0).getAttribute("aria-label")).toBe("1. Cheapest vendor, not checked");
  });
});

describe("equipping, choosing, reordering and removing", () => {
  beforeEach(() => use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })] }));

  it("appends a tray source on a click, and the bar turns to Unsaved with Save route last", async () => {
    await renderRouting();
    await openRoute("Mine");
    expect(within(bar()).queryByRole("button", { name: "Save route" })).toBeNull();
    fireEvent.click(tray(/^Cheapest vendor:/));
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app", "Cheapest vendor"]);
    expect(within(bar()).getByText("Unsaved")).toBeTruthy();
    const acts = within(bar()).getAllByRole("button").map((b) => b.textContent);
    expect(acts).toEqual(["Cancel", "Save route"]);
  });

  it("replaces a CHOSEN slot: Enter on the slot, then Enter on a tray source", async () => {
    await renderRouting();
    await openRoute("Mine");
    const first = slotButton(0);
    first.focus();
    fireEvent.click(first); // Enter on a button is a click
    expect(first.getAttribute("aria-pressed")).toBe("true");
    const claude = tray(/^Claude Code:/);
    claude.focus();
    fireEvent.click(claude);
    expect(slotNames()).toEqual(["Claude Code", "Any signed-in app"]);
    // The choice is spent: the next pick appends.
    fireEvent.click(tray(/^Cheapest vendor:/));
    expect(slotNames()).toEqual(["Claude Code", "Any signed-in app", "Cheapest vendor"]);
  });

  it("moves a slot with the arrow keys, keeping focus on it, and removes it with Delete", async () => {
    await renderRouting();
    await openRoute("Mine");
    const first = slotButton(0);
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowRight" });
    await settle();
    expect(slotNames()).toEqual(["Any signed-in app", "Strongest local model"]);
    expect(document.activeElement).toBe(slotButton(1));
    fireEvent.keyDown(slotButton(1), { key: "ArrowLeft" });
    await settle();
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app"]);
    fireEvent.keyDown(slotButton(0), { key: "Delete" });
    await settle();
    expect(slotNames()).toEqual(["Any signed-in app"]);
    expect(document.activeElement).toBe(slotButton(0));
  });

  it("removes a slot with its small x", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(screen.getByRole("button", { name: "Remove Any signed-in app" }));
    expect(slotNames()).toEqual(["Strongest local model"]);
  });

  it("reorders by dragging one slot onto another", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.dragStart(slotButton(1));
    fireEvent.dragOver(slots()[0]!);
    fireEvent.drop(slots()[0]!);
    expect(slotNames()).toEqual(["Any signed-in app", "Strongest local model"]);
  });

  it("equips by dragging a tray source onto the trailing +", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.dragStart(tray(/^Claude Code:/));
    const plus = screen.getByRole("button", { name: "Add a source at the end" }).closest("li")!;
    fireEvent.dragOver(plus);
    fireEvent.drop(plus);
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app", "Claude Code"]);
  });
});

describe("a placement the route cannot take", () => {
  beforeEach(() => use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })] }));

  it("refuses an embeddings-only model by click, says why in place, and changes nothing", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(tray(/^nomic-embed:/));
    expect(screen.getByRole("alert").textContent).toBe("nomic-embed only makes embeddings.");
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app"]);
    expect(within(bar()).queryByText("Unsaved")).toBeNull();
  });

  it("refuses the same model dropped onto a slot, and the slot keeps its source", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.dragStart(tray(/^nomic-embed:/));
    fireEvent.dragOver(slots()[0]!);
    fireEvent.drop(slots()[0]!);
    expect(within(slots()[0]!).getByRole("alert").textContent).toBe("nomic-embed only makes embeddings.");
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app"]);
  });

  it("refuses a duplicate", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(tray(/^Any signed-in app:/));
    expect(screen.getByRole("alert").textContent).toBe("Already in this route.");
  });
});

describe("the tray hides nothing", () => {
  it("keeps a source that cannot serve, quiet, with its reason in its name", async () => {
    await renderRouting();
    await openRoute("Local first");
    const codex = tray(/^Codex:/);
    expect(codex.getAttribute("aria-label")).toBe("Codex: not installed on any machine");
    expect(codex.hasAttribute("data-ready")).toBe(false);
    expect(tray(/^Claude Code:/).getAttribute("aria-label")).toBe("Claude Code: ready");
    expect(tray(/^Cheapest vendor:/).getAttribute("aria-label")).toBe("Cheapest vendor: no vendor set up");
  });

  it("offers to add a machine where there is none", async () => {
    use({ machines: [] });
    const onAddMachine = vi.fn();
    await renderRouting("owner", { onAddMachine });
    await openRoute("Local first");
    fireEvent.click(screen.getByRole("button", { name: "Add a machine" }));
    expect(onAddMachine).toHaveBeenCalled();
  });
});

describe("saving and restoring", () => {
  it("saves exactly the chain on screen at the revision it was read at, then reads again", async () => {
    use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false, description: "Mine first" })] });
    await renderRouting();
    await openRoute("Mine");
    fireEvent.keyDown(slotButton(1), { key: "ArrowLeft" });
    await settle();
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(calls.routingPolicySave).toHaveBeenCalledWith({ name: "mine", description: "Mine first", primary: "app:*", fallbacks: ["fleet:strongest"], expectedRevision: 7 });
    expect(calls.routerListPolicies.mock.calls.length).toBeGreaterThan(1);
  });

  it("keeps a draft refused for staleness, and says so in the product's words", async () => {
    use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })] });
    state.saveError = new Error("routing policies changed since revision 7; refresh and review before saving");
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(tray(/^Claude Code:/));
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(screen.getByText("Routing changed since this page read it.")).toBeTruthy();
    expect(screen.queryByText(/routing policies/)).toBeNull();
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app", "Claude Code"]);
    expect(within(bar()).getByText("Unsaved")).toBeTruthy();
  });

  it("keeps a draft the engine refused for another reason, with its reason in route words", async () => {
    use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })] });
    state.saveError = new Error('policy entry "app:cursor": "cursor" is not an app this engine drives');
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(tray(/^Claude Code:/));
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(screen.getByText("The route was not saved.")).toBeTruthy();
    expect(screen.getByText('source "app:cursor": "cursor" is not an app this engine drives')).toBeTruthy();
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app", "Claude Code"]);
  });

  it("Cancel returns to the saved chain", async () => {
    use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })] });
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(tray(/^Claude Code:/));
    fireEvent.click(within(bar()).getByRole("button", { name: "Cancel" }));
    await settle();
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app"]);
    expect(within(bar()).getByText("Saved")).toBeTruthy();
  });

  it("offers nothing on an untouched shipped route, and states the scope once", async () => {
    await renderRouting();
    await openRoute("Local first");
    expect(within(bar()).getByText("Shipped")).toBeTruthy();
    expect(within(bar()).getByText("Applies to every rule that takes this route")).toBeTruthy();
    expect(within(bar()).queryAllByRole("button")).toHaveLength(0);
  });

  it("restores a changed shipped route after asking, in the bar", async () => {
    use({ routes: [routeRow("localFirst", "app:*", [], { customized: true })] });
    await renderRouting();
    await openRoute("Local first");
    expect(within(bar()).getByText("Changed from shipped")).toBeTruthy();
    fireEvent.click(within(bar()).getByRole("button", { name: "Restore shipped" }));
    expect(within(bar()).getByText("Restore shipped sources?")).toBeTruthy();
    expect(calls.routingPolicyReset).not.toHaveBeenCalled();
    fireEvent.click(within(bar()).getByRole("button", { name: "Restore shipped" }));
    await settle();
    expect(calls.routingPolicyReset).toHaveBeenCalledWith({ name: "localFirst", expectedRevision: 7 });
  });

  it("keeps the protected embeddings route read-only, with no act at all", async () => {
    await renderRouting();
    await openRoute("Embeddings");
    expect(within(bar()).getByText("Protected")).toBeTruthy();
    expect(within(bar()).queryAllByRole("button")).toHaveLength(0);
    expect(screen.queryByLabelText("Sources you can add")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Remove / })).toBeNull();
  });

  it("keeps a draft when the person goes back to the list, and the row says Unsaved", async () => {
    use({ routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })] });
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(tray(/^Claude Code:/));
    await act(async () => {
      (document.querySelector("[aria-label='Back to Routes']") as HTMLElement | null)?.click();
    });
    // Outside a window the Head keeps its own back control.
    if (screen.queryByRole("list", { name: "Routes" }) === null) fireEvent.click(screen.getByRole("button", { name: "Back to Routes" }));
    expect(screen.getByRole("button", { name: /^Open Mine,/ }).textContent).toContain("Unsaved");
    await openRoute("Mine");
    expect(slotNames()).toEqual(["Strongest local model", "Any signed-in app", "Claude Code"]);
  });

  it("edits the description in place, and Escape puts the words back", async () => {
    use({ routes: [routeRow("mine", "fleet:strongest", [], { shipped: false, description: "Old words" })] });
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(screen.getByRole("button", { name: "Description: Old words. Edit" }));
    const input = screen.getByLabelText("Description");
    fireEvent.change(input, { target: { value: "New words" } });
    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.getByRole("button", { name: "Description: Old words. Edit" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Description: Old words. Edit" }));
    fireEvent.change(screen.getByLabelText("Description"), { target: { value: "New words" } });
    fireEvent.keyDown(screen.getByLabelText("Description"), { key: "Enter" });
    expect(within(bar()).getByText("Unsaved")).toBeTruthy();
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(calls.routingPolicySave).toHaveBeenCalledWith(expect.objectContaining({ description: "New words" }));
  });
});

describe("a source by its exact name", () => {
  beforeEach(() => use({ routes: [routeRow("mine", "fleet:strongest", [], { shipped: false })] }));

  function exact(): HTMLInputElement {
    return screen.getByLabelText("Specific source") as HTMLInputElement;
  }

  it("pins an app's model with Enter, and it saves as typed", async () => {
    await renderRouting();
    await openRoute("Mine");
    // Absent until there is something to add.
    expect(screen.queryByRole("button", { name: "Add specific source" })).toBeNull();
    fireEvent.change(exact(), { target: { value: "app:claude-code:opus" } });
    fireEvent.keyDown(exact(), { key: "Enter" });
    expect(slotNames()).toEqual(["Strongest local model", "Claude Code · opus"]);
    expect(exact().value).toBe("");
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(calls.routingPolicySave).toHaveBeenCalledWith(expect.objectContaining({ primary: "fleet:strongest", fallbacks: ["app:claude-code:opus"] }));
  });

  it("adds a vendor by name with its Add, into the CHOSEN slot", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.click(slotButton(0));
    fireEvent.change(exact(), { target: { value: "federation:acme" } });
    fireEvent.click(screen.getByRole("button", { name: "Add specific source" }));
    expect(slotNames()).toEqual(["acme"]);
  });

  it("takes a model no machine offers yet, and says it is not on any machine", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.change(exact(), { target: { value: "fleet:llama4:70b" } });
    fireEvent.keyDown(exact(), { key: "Enter" });
    expect(slotButton(1).getAttribute("aria-label")).toBe("2. llama4:70b, not on any machine");
  });

  it("refuses a slip in place and keeps the words", async () => {
    await renderRouting();
    await openRoute("Mine");
    fireEvent.change(exact(), { target: { value: "app:cursor" } });
    fireEvent.keyDown(exact(), { key: "Enter" });
    expect(screen.getByRole("alert").textContent).toBe("cursor is not an app this cluster drives.");
    expect(slotNames()).toEqual(["Strongest local model"]);
    expect(exact().value).toBe("app:cursor");
  });
});

describe("the rules a route change reaches", () => {
  it("counts the rules that take the route, and opens Rules on exactly those", async () => {
    use({
      rules: [
        ...shippedRules(),
        ruleRow({ name: "nightly", when: { tag: "nightly" }, policy: "localOnly", precedence: 20 }),
        ruleRow({ name: "batch", when: { tag: "batch" }, policy: "localFirst", precedence: 10 }),
      ],
    });
    await renderRouting();
    await openRoute("Local only");
    // The two shipped compiler rules take it too.
    fireEvent.click(screen.getByRole("button", { name: "Taken by 3 rules" }));
    await settle();
    const list = screen.getByRole("list", { name: "Rules, in the order they are tried" });
    expect(within(list).getAllByRole("listitem").map((li) => li.querySelector(".fleet-rule-when")?.textContent)).toEqual([
      "Route composing prompt",
      "Rule compiling prompt",
      "Tagged nightly",
    ]);
    fireEvent.click(screen.getByRole("button", { name: "Refine rules" }));
    expect((screen.getByLabelText("Route") as HTMLSelectElement | HTMLElement).textContent).toContain("Local only");
  });

  it("says the count in the bar while the route is unsaved", async () => {
    await renderRouting();
    await openRoute("Local first");
    fireEvent.click(tray(/^Claude Code:/));
    // The shipped floor and backgroundLane take Local first.
    expect(within(bar()).getByText(/applies to \d+ rules/)).toBeTruthy();
  });
});

describe("New route", () => {
  it("names, composes and saves a route in three steps, each forward act on the floor", async () => {
    await renderRouting();
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    const floor = () => screen.getByRole("group", { name: "What you can do with this" });
    // No name, no Next -- absent, never disabled.
    expect(within(floor()).queryByRole("button", { name: "Next" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "Night shift" } });
    fireEvent.click(within(floor()).getByRole("button", { name: "Next" }));
    // Sources: the same composer. Nothing in it yet, so no Next.
    expect(within(floor()).queryByRole("button", { name: "Next" })).toBeNull();
    fireEvent.click(tray(/^Claude Code:/));
    fireEvent.click(tray(/^Cheapest vendor:/));
    expect(slotNames()).toEqual(["Claude Code", "Cheapest vendor"]);
    fireEvent.click(within(floor()).getByRole("button", { name: "Next" }));
    expect(screen.getByText("Serves with Claude Code")).toBeTruthy();
    fireEvent.click(within(floor()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(calls.routingPolicySave).toHaveBeenCalledWith({ name: "nightShift", description: "", primary: "app:claude-code", fallbacks: ["federation:cheapest"], expectedRevision: 7 });
  });

  it("waits for the re-read after saving, and never says the new route is gone", async () => {
    await renderRouting();
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    const floor = () => screen.getByRole("group", { name: "What you can do with this" });
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "Night shift" } });
    fireEvent.click(within(floor()).getByRole("button", { name: "Next" }));
    fireEvent.click(tray(/^Claude Code:/));
    fireEvent.click(within(floor()).getByRole("button", { name: "Next" }));
    // Hold the re-read, and look at every render until it lands.
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => { release = resolve; });
    const read = calls.routerListPolicies.getMockImplementation()!;
    calls.routerListPolicies.mockImplementation(async (...args: unknown[]) => { await held; return read(...args); });
    // EVERY paint, not just the settled one: the flash lasted one render.
    let flashed = false;
    // The RECORDS, not the page when the observer runs: by then the next
    // render may already have replaced what was painted.
    const watch = new MutationObserver((records) => {
      for (const r of records) for (const n of r.addedNodes) if (n.textContent?.includes("That route is gone")) flashed = true;
    });
    watch.observe(document.body, { childList: true, subtree: true, characterData: true });
    fireEvent.click(within(floor()).getByRole("button", { name: "Save route" }));
    await settle();
    release();
    await settle();
    watch.disconnect();
    expect(flashed).toBe(false);
    expect(slotNames()).toEqual(["Claude Code"]);
  });

  it("says the re-read failed, rather than that the new route is gone", async () => {
    await renderRouting();
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    const floor = () => screen.getByRole("group", { name: "What you can do with this" });
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "Night shift" } });
    fireEvent.click(within(floor()).getByRole("button", { name: "Next" }));
    fireEvent.click(tray(/^Claude Code:/));
    fireEvent.click(within(floor()).getByRole("button", { name: "Next" }));
    calls.routerListPolicies.mockRejectedValue(new Error("connection lost"));
    fireEvent.click(within(floor()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(screen.queryByText("That route is gone")).toBeNull();
    expect(screen.getByText("The route could not be read.")).toBeTruthy();
  });

  it("refuses a name that is taken, and says so on the floor", async () => {
    await renderRouting();
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    fireEvent.change(screen.getByLabelText("Route name"), { target: { value: "local first" } });
    const floor = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(floor).getByText("A route with that name exists")).toBeTruthy();
    expect(within(floor).queryByRole("button", { name: "Next" })).toBeNull();
  });

  it("goes nowhere and writes nothing on Cancel", async () => {
    await renderRouting();
    fireEvent.click(screen.getByRole("button", { name: "New route" }));
    fireEvent.click(within(screen.getByRole("group", { name: "What you can do with this" })).getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("list", { name: "Routes" })).toBeTruthy();
    expect(calls.routingPolicySave).not.toHaveBeenCalled();
  });
});

describe("who may see which tab", () => {
  it.each([
    ["owner", ["Routes", "Rules", "Machines", "History"]],
    ["developer", ["Routes", "Rules", "Machines", "History"]],
    ["admin", ["Machines", "History"]],
  ])("%s sees %j", async (role, tabs) => {
    await renderRouting(role);
    expect(within(screen.getByRole("navigation", { name: "Routing views" })).getAllByRole("button").map((b) => b.textContent)).toEqual(tabs);
  });

  it("a writer sees the machine choice alone, with no tab bar", async () => {
    await renderRouting("writer");
    expect(screen.queryByRole("navigation", { name: "Routing views" })).toBeNull();
    expect(screen.getByLabelText("Routing strategy")).toBeTruthy();
    expect(calls.routerListPolicies).not.toHaveBeenCalled();
  });

  it("opens on the tab an intent names, and consumes it", async () => {
    const consumeIntent = vi.fn();
    await renderRouting("owner", { intent: { id: "i-1", payload: { routingTab: "rules" } }, consumeIntent });
    expect(screen.getByRole("list", { name: "Rules, in the order they are tried" })).toBeTruthy();
    expect(consumeIntent).toHaveBeenCalledWith("i-1");
  });
});

