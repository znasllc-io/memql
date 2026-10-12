import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { readTheBar } from "../../readTheBar";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../../src/apps/email/settings");
const { saveEditorPreference } = await import("../../../src/items/editorPreference");
const { VSCODE_NO_ANSWER_MESSAGE, browserHandoffPorts } = await import("../../../src/items/vscode");
const { fakeConnection, templateRow, withSession } = await import("../harness");

// CREATE NEW TEMPLATE (memql#6033, Ruling 9): nothing is written here. The
// wizard opens the editor's New email page for the organization and waits for
// the template the person publishes there, which reaches the OS the way every
// template does: on the live feed, from another node, as a draft first.

type Conn = ReturnType<typeof fakeConnection>;

const TEMPLATE = "v1:campaigns:template";

function mount(connection: Conn) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  return render(
    withSession(<EmailApp sectionId="templates" navigate={vi.fn()} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />),
  );
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const barDetail = () => bar().querySelector(".os-actbar-detail")?.textContent ?? "";
const barActs = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));
const openStep = () => document.querySelector('.os-rail-stage[data-open="true"] .os-rail-label')?.textContent ?? "";
/** A step's line on the rail: its name, then its answer. */
const railLine = (name: string) => within(screen.getByRole("list", { name: "Creating a template" })).getByRole("button", { name: new RegExp(`^${name}`) });

afterEach(() => {
  localStorage.clear();
  vi.useRealTimers();
});

describe("Create new template", () => {
  it("opens the editor for the organization and opens the template once it appears", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    try {
      const conn = fakeConnection({
        accounts: [
          { id: "self", name: "Operator organization", status: "active" },
          { id: "acme", name: "Acme", status: "active" },
        ],
        templates: [templateRow({ id: "t1", name: "Welcome 2" }), templateRow({ id: "t-acme", name: "Acme welcome", accountId: "acme" })],
      });
      mount(conn);
      await screen.findByText("Welcome 2");
      fireEvent.click(screen.getByRole("button", { name: "Create new template" }));

      // The Add control's own words, and the orb names what is made.
      expect(await screen.findByRole("heading", { level: 3, name: "Create new template" })).toBeTruthy();
      expect(screen.getByRole("button", { name: "Back to Templates" })).toBeTruthy();

      // Two organizations: asked first, and already answered with where this person starts.
      await waitFor(() => expect(openStep()).toBe("Write"));
      expect(railLine("Organization").textContent).toContain("Operator organization");
      expect(screen.getByText("The editor opens a new email for Operator organization. When you publish it there, it appears here.")).toBeTruthy();
      // Another one is a choice away, and answered by choosing.
      fireEvent.click(railLine("Organization"));
      expect(openStep()).toBe("Organization");
      expect(barWord()).toBe("Choose an organization");
      fireEvent.click(screen.getByRole("radio", { name: /^Acme/ }));
      expect(openStep()).toBe("Write");
      expect(screen.getByText("The editor opens a new email for Acme. When you publish it there, it appears here.")).toBeTruthy();
      expect(barActs()).toEqual(["Cancel", "Open the editor"]);
      // Nothing is written: no template is created from the OS.
      expect(conn.query.campaignSaveTemplate).not.toHaveBeenCalled();

      await readTheBar();
      press("Open the editor");
      // The editor opened inside the click, on the New email page for Acme.
      expect(open).toHaveBeenCalledOnce();
      expect(new URL(String(open.mock.calls[0]![0])).searchParams.get("resource")).toBe("memql-file://memql.example.com/new-email/acme/New%20email");

      // Somebody else's turn: a visible wait, measured.
      expect(bar().getAttribute("data-tone")).toBe("busy");
      expect(barWord()).toBe("Waiting for the new template");
      expect(barDetail()).toBe("Publish it in the editor to continue.");
      expect(bar().querySelector(".os-actbar-meta")?.textContent).toMatch(/^\d+:\d\d$/);
      expect(barActs()).toEqual(["Stop waiting"]);

      // A template that was already there, saved again, is not the new one; nor is another organization's.
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t-acme", name: "Acme welcome", accountId: "acme", createdAt: "2026-10-11T09:00:00Z" })));
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t-other", name: "Not this one", accountId: "self", status: "draft" }), "NODE_CREATED"));
      expect(barWord()).toBe("Waiting for the new template");

      // The first new template in Acme, whatever its state, opens its page.
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t-new", name: "Welcome 3", accountId: "acme", status: "draft" }), "NODE_CREATED"));
      expect(await screen.findByRole("heading", { level: 3, name: "Welcome 3" })).toBeTruthy();
      await waitFor(() => expect(barWord()).toBe("Draft"));
      expect(conn.query.campaignSaveTemplate).not.toHaveBeenCalled();
    } finally {
      open.mockRestore();
    }
  });

  it("says so in the step when the editor does not answer, and the act returns", async () => {
    saveEditorPreference("vscode");
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
    mount(conn);
    await screen.findByText("Welcome 2");
    fireEvent.click(screen.getByRole("button", { name: "Create new template" }));
    // One organization: nothing to ask, and the wizard opens on Write.
    await waitFor(() => expect(openStep()).toBe("Write"));
    expect(screen.queryByRole("radio")).toBeNull();
    expect(screen.getByText("The editor opens a new email for Operator organization. When you publish it there, it appears here.")).toBeTruthy();
    await readTheBar();

    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    // An installed editor is opened by its own scheme, which a test's browser
    // cannot follow: the page goes nowhere, exactly as when no editor answers.
    const navigate = vi.spyOn(browserHandoffPorts, "navigate").mockImplementation(() => {});
    try {
      press("Open the editor");
      expect(navigate.mock.calls[0]![0]).toMatch(/^vscode:\/\/znasllc\.memql-productivity-tools\/open\?resource=memql-file%3A%2F%2Fmemql\.example\.com%2Fnew-email%2Fself%2FNew%2520email/);
      expect(barWord()).toBe("Waiting for the new template");
      act(() => { vi.advanceTimersByTime(3_000); });
    } finally {
      navigate.mockRestore();
      vi.useRealTimers();
    }
    // The wait stops, the step says what to check, and the act is back.
    expect(screen.getByText(VSCODE_NO_ANSWER_MESSAGE)).toBeTruthy();
    expect(bar().getAttribute("data-tone")).not.toBe("busy");
    expect(barActs()).toEqual(["Cancel", "Open the editor"]);
  });

  it("stops waiting when asked, and Cancel goes back to the list", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    try {
      const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
      mount(conn);
      await screen.findByText("Welcome 2");
      fireEvent.click(screen.getByRole("button", { name: "Create new template" }));
      await waitFor(() => expect(barActs()).toEqual(["Cancel", "Open the editor"]));
      await readTheBar();
      press("Open the editor");
      expect(barActs()).toEqual(["Stop waiting"]);
      await readTheBar();
      press("Stop waiting");
      expect(barActs()).toEqual(["Cancel", "Open the editor"]);
      // A template that arrives now is nobody's answer.
      act(() => conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t-new", name: "Welcome 3", status: "draft" }), "NODE_CREATED"));
      expect(screen.queryByRole("heading", { level: 3, name: "Welcome 3" })).toBeNull();
      await readTheBar();
      press("Cancel");
      expect(await screen.findByRole("heading", { level: 3, name: "Templates" })).toBeTruthy();
    } finally {
      open.mockRestore();
    }
  });

  it("begins a wait only from a templates list that has finished loading", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.templates.getMockImplementation()!;
    conn.query.templates.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Create new template" }));
    await waitFor(() => expect(openStep()).toBe("Write"));
    // A row that was always there and arrives late would look new: not yet.
    expect(barActs()).toEqual(["Cancel"]);
    await act(async () => { release(); });
    await waitFor(() => expect(barActs()).toEqual(["Cancel", "Open the editor"]));
  });
});
