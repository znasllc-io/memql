import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The Ask route picker (design brief "routing in MemQL OS", section 6): a pill
// in the composer names the conversation's route; activating it REPLACES the
// panel's content with Where and Effort; choosing applies to this
// conversation and returns.
//
// The connection seam is mocked at the MODULE, as the Fleet suites do, so the
// real readings run: the machines feed (MachinesProvider over
// myWorkersWithStatus) for the apps, and inferenceStatus for local models and
// vendors.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AskSurface, FLEET_ROUTING_SECTION } = await import("../../src/ask/AskSurface");
const { FLEET_SECTIONS } = await import("../../src/apps/fleet/settings");
const { ConversationSession } = await import("../../src/ask/conversationSession");
const { AUTO_ROUTING } = await import("../../src/ask/askRoute");
const { MachinesProvider } = await import("../../src/live/machines");
const { fakeConnection, machineRow, withSession } = await import("../fleet/harness");

import type { AskRouteStore, AskRouting } from "../../src/ask/askRoute";
import type { AskCallbacks, AskOptions, AskTransport } from "../../src/ask/askController";

afterEach(cleanup);

class MemoryRoutes implements AskRouteStore {
  readonly saved = new Map<string, AskRouting>();
  load(id: string) { return this.saved.get(id) ?? { ...AUTO_ROUTING }; }
  save(id: string, routing: AskRouting) { this.saved.set(id, routing); }
}

const DOORS: Row = {
  id: "v1:platform:inferenceStatus:self",
  eligible: true,
  localEligible: true,
  localModelCount: 2,
  eligibleModelIds: ["qwen3.5:4b", "qwen3.8:27b"],
  appEligible: false,
  runnableApps: [],
  federationConfigured: false,
  cloudConfigured: false,
  fleetInferenceInstalled: true,
};

const STUDIO = machineRow({
  id: "v1:worker:registration:studio",
  displayName: "Studio",
  apps: [
    { id: "claude-code", version: "2.1.283", allowed: true, signedIn: true, subscription: "present" },
    { id: "codex", version: "0.9", allowed: true, signedIn: false, subscription: "unknown" },
  ],
});

const ready = { state: "ready" as const, message: "", refresh: vi.fn() };

function mount({ machines = [STUDIO], doors = [DOORS], doorsPending = false, connected = true, routing, variant = "sheet", onManageRoutes = vi.fn() }: { machines?: Row[]; doors?: Row[]; doorsPending?: boolean; connected?: boolean; routing?: AskRouting; variant?: "sheet" | "widget"; onManageRoutes?: () => void } = {}) {
  const connection = fakeConnection({ myWorkersWithStatus: machines, inferenceStatus: doors });
  if (doorsPending) connection.query.inferenceStatus = vi.fn(() => new Promise(() => {}));
  h.connection = connected ? connection : null;
  const calls: { prompt: string; options?: AskOptions }[] = [];
  let callbacks!: AskCallbacks;
  const transport: AskTransport = {
    ask: (prompt, _context, on, options) => { calls.push({ prompt, options }); callbacks = on; return { cancel: vi.fn() }; },
    conversations: {
      list: vi.fn(async () => []),
      create: vi.fn(async () => ({ id: "c1", title: "New conversation" })),
      read: vi.fn(async () => []),
    },
  };
  const routes = new MemoryRoutes();
  const conversation = new ConversationSession(transport, routes);
  if (routing) conversation.setRouting(routing);
  const view = render(withSession(
    <MachinesProvider>
      <AskSurface transport={transport} conversation={conversation} availability={ready} variant={variant} onManageRoutes={onManageRoutes} />
    </MachinesProvider>,
  ));
  return { view, calls, routes, conversation, onManageRoutes, finish: () => act(() => { callbacks.delta("ok"); callbacks.done(); }) };
}

function pill() { return screen.getByRole("button", { name: /^Route: / }); }
function where() { return screen.getByRole("radiogroup", { name: "Where" }); }
function row(name: RegExp) { return within(where()).getByRole("radio", { name }); }
async function open() {
  await act(async () => { pill().click(); });
  // The readings settle: every row states its note (no skeleton left).
  await waitFor(() => expect(within(where()).queryAllByRole("status")).toHaveLength(0));
}

beforeEach(() => { h.connection = null; });

describe("the pill", () => {
  it("names the conversation's route: Auto by default", () => {
    mount();
    expect(pill().textContent).toBe("Auto");
    expect(pill().getAttribute("aria-label")).toBe("Route: Auto");
  });

  it("on Auto it reads nothing: the composer adds no read of its own", async () => {
    mount();
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    expect((h.connection as ReturnType<typeof fakeConnection>).query.inferenceStatus).not.toHaveBeenCalled();
    expect(pill().getAttribute("aria-describedby")).toBeNull();
  });

  it("a pinned app that cannot serve now says why, quietly, before a send fails", async () => {
    const laptop = machineRow({
      id: "v1:worker:registration:laptop",
      displayName: "Laptop",
      connectedNodeId: "",
      apps: [{ id: "claude-code", allowed: true, signedIn: true }],
    });
    mount({ machines: [laptop], routing: { source: "app:claude-code", level: "" } });
    await waitFor(() => expect(pill().getAttribute("aria-describedby")).toBeTruthy());
    // The words stay the choice; the reason is the pill's description.
    expect(pill().textContent).toBe("Claude Code");
    expect(document.getElementById(pill().getAttribute("aria-describedby")!)?.textContent).toBe("Laptop is offline");
    expect(pill().getAttribute("data-ready")).toBe("false");
  });

  it("a pinned local model that cannot serve now says why", async () => {
    mount({ doors: [{ ...DOORS, localEligible: false }], routing: { source: "fleet:strongest", level: "" } });
    await waitFor(() => expect(pill().getAttribute("aria-describedby")).toBeTruthy());
    expect(document.getElementById(pill().getAttribute("aria-describedby")!)?.textContent).toBe("No local model online");
  });

  it("a pinned source that can serve carries no cue", async () => {
    mount({ routing: { source: "app:claude-code", level: "" } });
    await open();
    await act(async () => { screen.getByRole("button", { name: "Back to the conversation" }).click(); });
    expect(pill().getAttribute("aria-describedby")).toBeNull();
    expect(pill().getAttribute("data-ready")).toBeNull();
  });
});

describe("the picker", () => {
  it("REPLACES the conversation with Where and Effort built from real readings", async () => {
    mount();
    await open();
    // In place of the conversation, not over it.
    expect(screen.queryByRole("log", { name: "Conversation" })).toBeNull();
    expect(screen.queryByRole("textbox", { name: "Ask" })).toBeNull();

    const names = within(where()).getAllByRole("radio").map(r => r.querySelector(".os-choice-card-name")?.textContent);
    expect(names).toEqual(["Auto", "Local", "Claude Code", "Codex", "Vendor"]);
    expect(row(/^Auto/).getAttribute("aria-checked")).toBe("true");
    expect(row(/^Auto/).textContent).toContain("Follows your rules");
    expect(row(/^Local/).textContent).toContain("Your machines");
    // The app's machine, from the machines feed.
    expect(row(/^Claude Code/).textContent).toContain("On Studio");
    // Not ready: visible, quiet, the reason in one line, and not choosable.
    expect(row(/^Codex/).textContent).toContain("Not signed in on Studio");
    expect(row(/^Codex/).getAttribute("aria-disabled")).toBe("true");
    expect(row(/^Vendor/).textContent).toContain("No vendor key");
    expect(row(/^Vendor/).getAttribute("aria-disabled")).toBe("true");

    const effort = screen.getByRole("radiogroup", { name: "Effort" });
    expect(within(effort).getAllByRole("radio").map(r => r.textContent)).toEqual(["Auto", "Fast", "Strong", "Reasoning"]);
    expect(within(effort).getByRole("radio", { name: "Auto" }).getAttribute("aria-checked")).toBe("true");
  });

  it("says what it changes in its accessible description, and nothing about routes on screen", async () => {
    mount();
    await open();
    const region = screen.getByRole("region", { name: "Route" });
    const describedBy = region.getAttribute("aria-describedby")!;
    expect(document.getElementById(describedBy)?.textContent).toMatch(/this conversation only/i);
    // Vocabulary (brief section 1): no engine nouns on a routing surface.
    const visible = region.textContent!.replace(document.getElementById(describedBy)!.textContent!, "");
    expect(visible).not.toMatch(/polic|door|lane|federation|task rule/i);
  });

  it("choosing a ready source applies to THIS conversation and returns to it", async () => {
    const { calls } = mount();
    await open();
    await act(async () => { row(/^Claude Code/).click(); });

    expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
    expect(screen.getByRole("log", { name: "Conversation" })).toBeTruthy();
    expect(pill().getAttribute("aria-label")).toBe("Route: Claude Code");
    expect(document.activeElement).toBe(pill());

    fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "hi" } });
    await act(async () => { screen.getByRole("button", { name: "Send" }).click(); });
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]!.options?.routing).toEqual({ source: "app:claude-code", level: "" });
  });

  it("choosing an Effort applies and returns, like Where; the pill carries the level when it is not Auto", async () => {
    const { calls } = mount();
    await open();
    await act(async () => { within(screen.getByRole("radiogroup", { name: "Effort" })).getByRole("radio", { name: "Strong" }).click(); });
    // Choosing IS the action (brief section 6): back to the conversation,
    // focus on the pill, the level in its words.
    expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
    expect(document.activeElement).toBe(pill());
    expect(pill().textContent).toBe("Auto · Strong");

    // The level is kept when the Where is chosen next.
    await open();
    expect(within(screen.getByRole("radiogroup", { name: "Effort" })).getByRole("radio", { name: "Strong" }).getAttribute("aria-checked")).toBe("true");
    await act(async () => { row(/^Local/).click(); });
    expect(pill().textContent).toBe("Local · Strong");
    expect(pill().getAttribute("aria-label")).toBe("Route: Local · Strong");

    fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "hi" } });
    await act(async () => { screen.getByRole("button", { name: "Send" }).click(); });
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]!.options?.routing).toEqual({ source: "fleet:strongest", level: "strong" });

    // Fast on Local asks for the fastest local model.
    await open();
    await act(async () => { within(screen.getByRole("radiogroup", { name: "Effort" })).getByRole("radio", { name: "Fast" }).click(); });
    expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
    expect(pill().textContent).toBe("Local · Fast");
  });

  it("an unready row cannot be chosen: nothing changes and the picker stays", async () => {
    const { conversation } = mount();
    await open();
    await act(async () => { row(/^Codex/).click(); });
    expect(screen.getByRole("region", { name: "Route" })).toBeTruthy();
    expect(conversation.getSnapshot().routing).toEqual(AUTO_ROUTING);
    // Still in the tab order, so its reason can be read.
    expect((row(/^Codex/) as HTMLButtonElement).disabled).toBe(false);
  });

  it("states why each app cannot serve, from the machines feed", async () => {
    const offline = machineRow({
      id: "v1:worker:registration:laptop",
      displayName: "Laptop",
      connectedNodeId: "",
      apps: [{ id: "claude-code", allowed: true, signedIn: true }],
    });
    const blocked = machineRow({
      id: "v1:worker:registration:mini",
      displayName: "Mini",
      apps: [{ id: "codex", allowed: false, signedIn: true }],
    });
    mount({ machines: [offline, blocked], doors: [{ ...DOORS, localEligible: false, federationConfigured: true }] });
    await open();
    expect(row(/^Claude Code/).textContent).toContain("Laptop is offline");
    expect(row(/^Codex/).textContent).toContain("Not allowed on Mini");
    expect(row(/^Local/).textContent).toContain("No local model online");
    expect(row(/^Local/).getAttribute("aria-disabled")).toBe("true");
    expect(row(/^Vendor/).textContent).toContain("Cheapest vendor");
    expect(row(/^Vendor/).getAttribute("aria-disabled")).toBeNull();
  });

  it("an app on no machine says so", async () => {
    mount({ machines: [machineRow({ id: "v1:worker:registration:bare", apps: [] })] });
    await open();
    expect(row(/^Claude Code/).textContent).toContain("Not installed on any machine");
    expect(row(/^Codex/).textContent).toContain("Not installed on any machine");
  });

  it("while readings are out, rows keep their shape and are not choosable yet", async () => {
    mount({ doorsPending: true });
    await act(async () => { pill().click(); });
    expect(within(row(/^Local/)).getByRole("status")).toBeTruthy();
    expect(row(/^Local/).getAttribute("aria-disabled")).toBe("true");
    // Auto needs no reading.
    expect(row(/^Auto/).getAttribute("aria-disabled")).toBeNull();
    // Loading is the shape of the content: the words are for screen readers only.
    const region = screen.getByRole("region", { name: "Route" }).cloneNode(true) as HTMLElement;
    region.querySelectorAll(".os-sr-only").forEach((el) => el.remove());
    expect(region.textContent).not.toMatch(/Loading|Checking|Reading/);
  });

  it("Escape and Back return to the conversation without changing the route", async () => {
    const { conversation } = mount();
    await open();
    // Focus lands on the current choice.
    expect(document.activeElement).toBe(row(/^Auto/));
    await act(async () => { fireEvent.keyDown(document.activeElement!, { key: "Escape" }); });
    expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
    expect(document.activeElement).toBe(pill());
    await open();
    await act(async () => { screen.getByRole("button", { name: "Back to the conversation" }).click(); });
    expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
    expect(conversation.getSnapshot().routing).toEqual(AUTO_ROUTING);
  });

  it("Escape unwinds the picker from inside it, and never reaches the sheet's own Escape (which closes Ask)", async () => {
    mount();
    await open();
    const sheetEscape = vi.fn();
    window.addEventListener("keydown", sheetEscape);
    try {
      await act(async () => { fireEvent.keyDown(document.activeElement!, { key: "Escape" }); });
      expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
      expect(sheetEscape).not.toHaveBeenCalled();

      // A click on the picker's own background keeps focus in the picker, so
      // Escape still unwinds it rather than closing Ask behind it.
      await open();
      const region = screen.getByRole("region", { name: "Route" });
      act(() => { region.focus(); });
      expect(document.activeElement).toBe(region);
      await act(async () => { fireEvent.keyDown(region, { key: "Escape" }); });
      expect(screen.queryByRole("region", { name: "Route" })).toBeNull();
      expect(sheetEscape).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener("keydown", sheetEscape);
    }
  });

  it("the desk widget's open picker leaves Escape elsewhere in the OS alone", async () => {
    mount({ variant: "widget" });
    await open();
    // Somewhere else on the desk: a launcher field, a dropdown, a menu.
    const elsewhere = document.createElement("button");
    document.body.appendChild(elsewhere);
    const own = vi.fn();
    const onDocument = vi.fn();
    elsewhere.addEventListener("keydown", own);
    document.addEventListener("keydown", onDocument);
    try {
      elsewhere.focus();
      await act(async () => { fireEvent.keyDown(elsewhere, { key: "Escape" }); });
      expect(own).toHaveBeenCalledOnce();
      expect(onDocument).toHaveBeenCalledOnce();
      // The widget's picker did not take it.
      expect(screen.getByRole("region", { name: "Route" })).toBeTruthy();
    } finally {
      document.removeEventListener("keydown", onDocument);
      elsewhere.remove();
    }
  });

  it("with no connection, the rows say so instead of a skeleton that never resolves", async () => {
    mount({ connected: false });
    await act(async () => { pill().click(); });
    expect(within(where()).queryAllByRole("status")).toHaveLength(0);
    for (const name of [/^Local/, /^Claude Code/, /^Codex/, /^Vendor/]) {
      expect(row(name).textContent).toContain("Not connected");
      expect(row(name).getAttribute("aria-disabled")).toBe("true");
    }
    expect(row(/^Auto/).getAttribute("aria-disabled")).toBeNull();
  });

  it("the current choice stays marked in the picker when it can no longer serve", async () => {
    const laptop = machineRow({
      id: "v1:worker:registration:laptop",
      displayName: "Laptop",
      connectedNodeId: "",
      apps: [{ id: "claude-code", allowed: true, signedIn: true }],
    });
    mount({ machines: [laptop], routing: { source: "app:claude-code", level: "" } });
    await open();
    expect(row(/^Claude Code/).getAttribute("aria-checked")).toBe("true");
    expect(row(/^Claude Code/).getAttribute("aria-disabled")).toBe("true");
    expect(row(/^Claude Code/).textContent).toContain("Laptop is offline");
  });

  it("Manage routes names a section Fleet has", () => {
    expect(FLEET_SECTIONS.map((section) => section.id)).toContain(FLEET_ROUTING_SECTION);
  });

  it("Manage routes opens Fleet's routing", async () => {
    const { onManageRoutes } = mount();
    await open();
    await act(async () => { screen.getByRole("button", { name: "Manage routes" }).click(); });
    expect(onManageRoutes).toHaveBeenCalledOnce();
  });

  it("the choice persists with the conversation; a new conversation starts on Auto", async () => {
    const { routes, finish, calls } = mount();
    await open();
    await act(async () => { row(/^Claude Code/).click(); });
    fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "hi" } });
    await act(async () => { screen.getByRole("button", { name: "Send" }).click(); });
    await waitFor(() => expect(calls).toHaveLength(1));
    finish();
    expect(routes.load("c1")).toEqual({ source: "app:claude-code", level: "" });

    await act(async () => { screen.getByRole("button", { name: "New conversation" }).click(); });
    expect(pill().getAttribute("aria-label")).toBe("Route: Auto");
  });
});
