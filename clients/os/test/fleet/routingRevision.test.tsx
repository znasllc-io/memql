import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// ONE REVISION FOR ROUTES AND RULES.
//
// The engine keeps routes and rules in one revisioned document
// (component/memql/policy_customization.go, PolicyDocument.Revision): a route
// save or reset and a rule validate, save or removal all name the revision
// they were read at, and every write bumps it. So a write in one Routing tab
// makes the OTHER tab's copy stale -- and a surface that re-read only the tab
// it wrote from sends the next write from the other tab at a revision the
// engine has already left, and is refused every time.
//
// The double (routingFixtures.ts) enforces the revision exactly as the engine
// does, so these drive one tab, write, and then write in the other.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { renderRouting, routingConnection, routeRow, ruleRow, settle, shippedRules } = await import("./routingHarness");
const { retiredWordsIn } = await import("../../src/apps/fleet/routing/vocabulary");

afterEach(cleanup);

function use() {
  const made = routingConnection({
    routes: [routeRow("mine", "fleet:strongest", ["app:*"], { shipped: false })],
    rules: [...shippedRules(), ruleRow({ name: "a", when: { tag: "x" }, precedence: 30 }), ruleRow({ name: "b", when: { tag: "y" }, precedence: 10 })],
  });
  h.connection = made.connection;
  return made;
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const tabs = () => within(screen.getByRole("navigation", { name: "Routing views" }));
const tray = (name: RegExp) => within(screen.getByLabelText("Sources you can add")).getByRole("button", { name });

async function toTab(name: string) {
  fireEvent.click(tabs().getByRole("button", { name }));
  await settle();
}

async function moveTaggedXDown() {
  const line = screen.getAllByRole("button").find((b) => /Tagged x/.test(b.textContent ?? ""))!;
  line.focus();
  fireEvent.keyDown(line, { key: "ArrowDown", altKey: true });
  await settle();
}

async function backToRoutes() {
  if (screen.queryByRole("list", { name: "Routes" }) !== null) return;
  fireEvent.click(screen.getByRole("button", { name: "Back to Routes" }));
  await settle();
}

async function addVendorAndSave() {
  fireEvent.click(await screen.findByRole("button", { name: /^Open Mine,/ }));
  await settle();
  fireEvent.click(tray(/^Cheapest vendor:/));
  fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
  await settle();
}

describe("a write in one Routing tab, then a write in the other", () => {
  it("reorders a rule, then saves a route at the revision the reorder left", async () => {
    const made = use();
    await renderRouting("owner");
    await toTab("Rules");
    await moveTaggedXDown();
    expect(made.calls.routingRuleSave).toHaveBeenCalledTimes(1);
    expect(made.state.revision).toBe(8);

    await toTab("Routes");
    await addVendorAndSave();
    expect(made.calls.routingPolicySave).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: 8 }));
    expect(screen.queryByText("The route was not saved.")).toBeNull();
    expect(screen.queryByText(/Routing changed since/)).toBeNull();
    expect(made.state.revision).toBe(9);
  });

  it("saves a route, then reorders a rule at the revision the save left", async () => {
    const made = use();
    await renderRouting("owner");
    await toTab("Rules");
    await toTab("Routes");
    await addVendorAndSave();
    expect(made.state.revision).toBe(8);

    await backToRoutes();
    await toTab("Rules");
    await moveTaggedXDown();
    expect(made.calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: 8 }));
    expect(screen.queryByText("That did not go through.")).toBeNull();
    expect(made.state.revision).toBe(9);
  });

  it("adds a rule after a route save without a refusal", async () => {
    const made = use();
    await renderRouting("owner");
    await addVendorAndSave();
    await backToRoutes();
    await toTab("Rules");
    fireEvent.click(screen.getByRole("button", { name: "Add a rule" }));
    fireEvent.click(screen.getByRole("radio", { name: "A tag" }));
    fireEvent.change(screen.getByLabelText("Tag"), { target: { value: "nightly" } });
    fireEvent.click(within(bar()).getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("radio", { name: /^Mine/ }));
    fireEvent.click(within(bar()).getByRole("button", { name: "Add rule" }));
    await settle();
    expect(made.calls.routingRuleValidate).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: 8 }));
    expect(made.calls.routingRuleSave).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: 8 }));
    expect(screen.getByRole("list", { name: "Rules, in the order they are tried" })).toBeTruthy();
  });
});

describe("a route page whose routing changed elsewhere", () => {
  it("says so in product words, reads again, and a second save goes through", async () => {
    const made = use();
    await renderRouting("owner");
    fireEvent.click(await screen.findByRole("button", { name: /^Open Mine,/ }));
    await settle();
    // Somebody else wrote since this page read the routes.
    made.state.revision = 12;
    const reads = made.calls.routerListPolicies.mock.calls.length;
    fireEvent.click(tray(/^Cheapest vendor:/));
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();

    const notice = screen.getByText("Routing changed since this page read it.").closest(".os-notice") ?? document.body;
    expect(retiredWordsIn(notice.textContent ?? "")).toEqual([]);
    expect(document.body.textContent).not.toMatch(/routing policies changed/);
    // It read again, and the draft is still on screen.
    expect(made.calls.routerListPolicies.mock.calls.length).toBeGreaterThan(reads);
    expect(within(bar()).getByText("Unsaved")).toBeTruthy();

    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    expect(made.calls.routingPolicySave).toHaveBeenLastCalledWith(expect.objectContaining({ expectedRevision: 12 }));
    expect(made.state.revision).toBe(13);
  });

  it("Cancel after a refusal shows the route as it is saved now", async () => {
    const made = use();
    await renderRouting("owner");
    fireEvent.click(await screen.findByRole("button", { name: /^Open Mine,/ }));
    await settle();
    // Somebody else changed THIS route.
    made.state.revision = 12;
    made.state.routes = [routeRow("mine", "app:claude-code", [], { shipped: false })];
    fireEvent.click(tray(/^Cheapest vendor:/));
    fireEvent.click(within(bar()).getByRole("button", { name: "Save route" }));
    await settle();
    fireEvent.click(within(bar()).getByRole("button", { name: "Cancel" }));
    await settle();
    const names = [...document.querySelectorAll(".fleet-slot-body .fleet-slot-name")].map((n) => n.textContent);
    expect(names).toEqual(["Claude Code"]);
    expect(screen.queryByText("Routing changed since this page read it.")).toBeNull();
  });
});
