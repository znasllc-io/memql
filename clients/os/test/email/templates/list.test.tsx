import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { chooseOption } from "../../selectControl";

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
const { fakeConnection, layoutRow, templateRow, withSession } = await import("../harness");

// THE TEMPLATES SECTION'S LIST (memql#6033, Task 11): every template with its
// layout and its state in one word, asked questions of through Refine.

type Conn = ReturnType<typeof fakeConnection>;

const TEMPLATE = "v1:campaigns:template";
const ORGANIZATIONS = [
  { id: "self", name: "Operator organization", status: "active" },
  { id: "acme", name: "Acme", status: "active" },
];

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

function mount(connection: Conn, settings: Record<string, unknown> = {}, intent?: { id: string; payload: Record<string, unknown> }) {
  h.connection = connection;
  const view = render(
    withSession(
      <EmailApp sectionId="templates" navigate={vi.fn()} askContext={() => {}} store={memoryStore(settings)} uploads={{ upload: vi.fn() }} intent={intent} consumeIntent={() => {}} />,
    ),
  );
  return { view };
}

/** The list row a name is drawn in. */
const rowOf = (name: string) => screen.getByText(name).closest(".os-record-row") as HTMLElement;
/** The live-list item (where the arrival cue is drawn) a name is drawn in. */
const itemOf = (name: string) => screen.getByText(name).closest(".os-livelist-row") as HTMLElement;
const headCount = (view: ReturnType<typeof render>) => view.container.querySelector(".os-head-meta")?.textContent ?? null;

afterEach(() => {
  vi.useRealTimers();
});

describe("the templates list", () => {
  it("Templates has no Layouts tab", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
    const { view } = mount(conn);
    await screen.findByText("Welcome 2");
    await waitFor(() => expect(headCount(view)).toBe("1"));

    // One view, so no tab bar and no Layouts to switch to.
    expect(screen.queryByRole("navigation", { name: "Templates and layouts" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Layouts" })).toBeNull();
    // Templates are still added here: the editor writes them.
    expect(screen.getByRole("button", { name: "Create new template" })).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 3 }).map((heading) => heading.textContent)).toEqual(["Templates"]);
  });

  it("the list shows Writing for a template MemQL is writing", async () => {
    const conn = fakeConnection({
      templates: [
        templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: { state: "writing", startedAt: "2026-11-04T10:00:00Z", error: "", notes: ["One file was left out."] } }),
        templateRow({ id: "t2", name: "Done letter", status: "draft", composition: { state: "done", startedAt: "2026-11-04T10:00:00Z", error: "", notes: [] } }),
      ],
    });
    mount(conn);
    await screen.findByText("Autumn letter");
    const state = (name: string) => rowOf(name).querySelector(".os-record-status") as HTMLElement;
    expect(state("Autumn letter").textContent).toBe("Writing");
    expect(state("Autumn letter").getAttribute("data-tone")).toBe("accent");
    // A finished composition is just the template's own state, and notes never reach the list.
    expect(state("Done letter").textContent).toBe("Draft");
    expect(screen.queryByText("One file was left out.")).toBeNull();
    // No row actions.
    expect(within(rowOf("Autumn letter")).queryByRole("button", { name: /Cancel|Retry/ })).toBeNull();
  });

  it("the list shows Failed with the reason on hover", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: { state: "failed", startedAt: "2026-11-04T10:00:00Z", error: "Cancelled", notes: [] } })],
    });
    mount(conn);
    await screen.findByText("Autumn letter");
    const state = rowOf("Autumn letter").querySelector(".os-record-status") as HTMLElement;
    expect(state.textContent).toBe("Failed");
    expect(state.getAttribute("data-tone")).toBe("warn");
    expect(state.getAttribute("title")).toBe("Cancelled");
    expect(within(rowOf("Autumn letter")).queryByText("Cancelled")).toBeNull();
  });

  it("names each template's layout and state", async () => {
    const conn = fakeConnection({
      accounts: [ORGANIZATIONS[0]!],
      templates: [
        templateRow({ id: "t1", name: "Welcome 2", subject: "Glad you're here", layoutId: "builtin:article" }),
        templateRow({ id: "t2", name: "Plain note", layoutId: "", status: "draft" }),
        templateRow({ id: "t3", name: "Acme letter copy", layoutId: "org:l1" }),
        templateRow({ id: "t4", name: "Old copy", status: "archived", layoutId: "builtin:letter" }),
      ],
      layouts: { self: [layoutRow({ layoutId: "l1", name: "Our letter" })] },
    });
    mount(conn, { includeArchived: true });
    await screen.findByText("Welcome 2");

    // One reading of the organization's layouts names them all, built-in and its own.
    await waitFor(() => expect(within(rowOf("Welcome 2")).getByText("Article")).toBeTruthy());
    expect(conn.query.emailLayouts).toHaveBeenCalledWith({ accountId: "self", includeArchived: true }, expect.anything());
    expect(within(rowOf("Acme letter copy")).getByText("Our letter")).toBeTruthy();
    // A template with no layout is its own HTML.
    expect(within(rowOf("Plain note")).getByText("Custom")).toBeTruthy();
    // The subject is the line under the name.
    expect(within(rowOf("Welcome 2")).getByText("Glad you're here")).toBeTruthy();

    const state = (name: string) => rowOf(name).querySelector(".os-record-status")?.textContent ?? "";
    expect(state("Welcome 2")).toBe("Published");
    expect(state("Plain note")).toBe("Draft");
    expect(state("Old copy")).toBe("Archived");
    expect(rowOf("Old copy").getAttribute("data-dim")).toBe("true");
    expect(rowOf("Welcome 2").getAttribute("data-dim")).toBeNull();
    // The engine's own words, and an opaque layout id, are never shown.
    expect(screen.queryByText("ready")).toBeNull();
    expect(screen.queryByText(/builtin:|org:/)).toBeNull();
    // One organization: nothing to tell apart, so no organization on the rows.
    expect(screen.queryByText("Operator organization")).toBeNull();
  });

  it("refines by organization and state", async () => {
    const conn = fakeConnection({
      accounts: ORGANIZATIONS,
      templates: [
        templateRow({ id: "t1", name: "Welcome 2", subject: "Glad you're here" }),
        templateRow({ id: "t2", name: "Autumn draft", status: "draft" }),
        templateRow({ id: "t3", name: "Acme welcome", accountId: "acme", subject: "Hello from Acme" }),
        templateRow({ id: "t4", name: "Old copy", status: "archived" }),
      ],
    });
    const { view } = mount(conn);
    await screen.findByText("Welcome 2");
    // Archived templates are listed only with Include archived.
    expect(screen.queryByText("Old copy")).toBeNull();
    await waitFor(() => expect(headCount(view)).toBe("3"));
    // Two organizations are listed, so each row names its own.
    expect(within(rowOf("Acme welcome")).getByText("Acme")).toBeTruthy();
    expect(within(rowOf("Welcome 2")).getByText("Operator organization")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Refine templates" }));
    // By name and by subject.
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "acme" } });
    expect(screen.getByText("Acme welcome")).toBeTruthy();
    expect(screen.queryByText("Welcome 2")).toBeNull();
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "glad" } });
    expect(screen.getByText("Welcome 2")).toBeTruthy();
    expect(screen.queryByText("Acme welcome")).toBeNull();
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "" } });

    chooseOption(screen.getByLabelText("Organization"), "Acme");
    expect(screen.getByText("Acme welcome")).toBeTruthy();
    expect(screen.queryByText("Welcome 2")).toBeNull();
    expect(headCount(view)).toBe("1");
    // The chip says which organization; the row does not say it again.
    expect(within(rowOf("Acme welcome")).queryByText("Acme")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Remove Acme" }));

    chooseOption(screen.getByLabelText("State"), "Draft");
    expect(screen.getByText("Autumn draft")).toBeTruthy();
    expect(screen.queryByText("Welcome 2")).toBeNull();
    chooseOption(screen.getByLabelText("State"), "Published");
    expect(screen.getByText("Welcome 2")).toBeTruthy();
    expect(screen.getByText("Acme welcome")).toBeTruthy();
    expect(screen.queryByText("Autumn draft")).toBeNull();
    expect(headCount(view)).toBe("2");

    // A question that empties the list says so, and stays to be taken back.
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "nothing like it" } });
    expect(await screen.findByText("No matching templates")).toBeTruthy();
    expect(screen.queryByText("No templates yet")).toBeNull();
    expect(screen.getByRole("button", { name: "Remove Published" })).toBeTruthy();
  });

  it("marks a new arrival, not an update", async () => {
    const welcome = templateRow({ id: "t1", name: "Welcome 2" });
    const conn = fakeConnection({ templates: [welcome] });
    mount(conn);
    await screen.findByText("Welcome 2");

    // A copy edit is news, and it is not new.
    await act(async () => {
      conn.subscriptions.emit(TEMPLATE, { ...welcome, subject: "A better subject", createdAt: "2026-08-02T00:00:00Z" });
    });
    expect(itemOf("Welcome 2").getAttribute("data-arrival")).toBe("updated");
    expect(within(itemOf("Welcome 2")).queryByText("new")).toBeNull();

    // A template published while somebody watches arrives with the new tick.
    await act(async () => {
      conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t2", name: "Welcome 3" }), "NODE_CREATED");
    });
    expect(itemOf("Welcome 3").getAttribute("data-arrival")).toBe("added");
    expect(within(itemOf("Welcome 3")).getByText("new")).toBeTruthy();
  });

  it("says what a list with nothing in it is for, and a failed read is not an empty list", async () => {
    const empty = mount(fakeConnection());
    expect(await screen.findByText("No templates yet")).toBeTruthy();
    expect(screen.getByText("Templates are written in the editor and appear here when they're published.")).toBeTruthy();
    expect(headCount(empty.view)).toBe("0");
    // Nothing to ask a question of.
    expect(screen.queryByRole("button", { name: "Refine templates" })).toBeNull();
    empty.view.unmount();

    const denied = fakeConnection();
    denied.query.templates.mockRejectedValue(new Error("templates: permission denied"));
    const second = mount(denied);
    expect(await screen.findByText("This cluster did not return your templates.")).toBeTruthy();
    expect(screen.queryByText("No templates yet")).toBeNull();
    expect(headCount(second.view)).toBeNull();
  });

  it("points at the setting when archived templates are why the list is empty", async () => {
    mount(fakeConnection({ templates: [templateRow({ id: "t1", name: "Old copy", status: "archived" })] }));
    expect(await screen.findByText("No templates in use")).toBeTruthy();
    expect(screen.getByText("Archived templates are hidden. Turn on Include archived in Settings to see them.")).toBeTruthy();
    expect(screen.queryByText("No templates yet")).toBeNull();
  });

  it("says in place that a template asked for isn't available", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
    mount(conn, {}, { id: "ask-1", payload: { templateId: "t-gone" } });
    // Asked for by Ask, and the cluster answered without it: the list, and the sentence.
    expect(await screen.findByText("That template isn't available.")).toBeTruthy();
    expect(screen.getByText("Welcome 2")).toBeTruthy();
    // Opening another one takes the sentence with it.
    fireEvent.click(screen.getByText("Welcome 2"));
    expect(await screen.findByRole("heading", { level: 3, name: "Welcome 2" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to Templates" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Templates" })).toBeTruthy();
    expect(screen.queryByText("That template isn't available.")).toBeNull();
  });

  it("does not call a template asked for missing before the cluster has answered", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const answer = conn.query.templates.getMockImplementation()!;
    conn.query.templates.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    mount(conn, {}, { id: "ask-1", payload: { templateId: "t1" } });
    await waitFor(() => expect(conn.query.templates).toHaveBeenCalled());
    expect(screen.queryByText("That template isn't available.")).toBeNull();
    await act(async () => { release(); });
    // It arrived: its page opens.
    expect(await screen.findByRole("heading", { level: 3, name: "Welcome 2" })).toBeTruthy();
  });
});
