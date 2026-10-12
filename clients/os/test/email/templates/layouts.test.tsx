import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { readTheBar } from "../../readTheBar";
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
const { BUILT_IN_LAYOUTS, fakeConnection, layoutRow, rowsResult, templateRow, withSession } = await import("../harness");

// LAYOUTS (memql#6033, Ruling 24): what a template can be made from. The four
// built-ins ship with the renderer and every organization starts from them; an
// organization's own are saved from a finished email in the editor. Listed
// beside the templates under one Head; read, previewed, archived and restored
// here, and never saved from the OS.

type Conn = ReturnType<typeof fakeConnection>;

const ORGANIZATIONS = [
  { id: "self", name: "Operator organization", status: "active" },
  { id: "acme", name: "Acme", status: "active" },
];
const BUILT_IN_NAMES = ["Confirmation", "Letter", "Article", "Spotlight"];

function mount(connection: Conn, settings: Record<string, unknown> = {}) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  if (Object.keys(settings).length > 0) store.save({ ...store.load(), ...settings });
  return render(
    withSession(<EmailApp sectionId="templates" navigate={vi.fn()} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />),
  );
}

async function openLayouts() {
  fireEvent.click(within(await screen.findByRole("navigation", { name: "Templates and layouts" })).getByRole("button", { name: "Layouts" }));
}

const list = () => screen.getByRole("list", { name: "Layouts" });
const names = () => within(list()).getAllByRole("listitem").map((item) => item.querySelector(".os-row-name")?.textContent ?? "");
const rowOf = (name: string) => within(list()).getByText(name).closest(".os-record-row") as HTMLElement;
const headCount = (view: ReturnType<typeof render>) => view.container.querySelector(".os-head-meta")?.textContent ?? null;
const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));
const barActs = () =>
  within(bar())
    .queryAllByRole("button")
    .map((button) => [button.textContent, button.classList.contains("os-actbar-text") ? "text" : (button.getAttribute("data-tone") ?? "quiet")]);
const frame = () => screen.queryByTitle("Email preview") as HTMLIFrameElement | null;
/** The buttons a person could press that do nothing: disabled, and not because they are working. */
const deadButtons = () =>
  screen.queryAllByRole("button").filter((button) => (button as HTMLButtonElement).disabled && button.getAttribute("aria-busy") !== "true");

afterEach(() => {
  vi.useRealTimers();
});

describe("the layouts list", () => {
  it("lists the built-ins once, then each organization's layouts", async () => {
    const conn = fakeConnection({
      accounts: ORGANIZATIONS,
      layouts: {
        self: [layoutRow({ layoutId: "l-self", name: "Operator letter", description: "Our own letter." })],
        acme: [layoutRow({ layoutId: "l-acme", name: "Acme welcome", description: "The Acme welcome." })],
      },
    });
    const view = mount(conn);
    await openLayouts();

    // One reading per organization, archived layouts included; the list narrows.
    await waitFor(() => expect(names()).toEqual([...BUILT_IN_NAMES, "Operator letter", "Acme welcome"]));
    expect(conn.query.emailLayouts).toHaveBeenCalledWith({ accountId: "self", includeArchived: true }, expect.anything());
    expect(conn.query.emailLayouts).toHaveBeenCalledWith({ accountId: "acme", includeArchived: true }, expect.anything());
    await waitFor(() => expect(headCount(view)).toBe("6"));

    // A built-in says so and carries no state; an organization's names it and is Active.
    expect(within(rowOf("Article")).getByText("Built in")).toBeTruthy();
    expect(rowOf("Article").querySelector(".os-record-status")).toBeNull();
    expect(within(rowOf("Article")).getByText("A banner, a tinted article body with a highlighted statement, and a signature.")).toBeTruthy();
    expect(within(rowOf("Acme welcome")).getByText("Acme")).toBeTruthy();
    expect(within(rowOf("Acme welcome")).getByText("The Acme welcome.")).toBeTruthy();
    expect(rowOf("Acme welcome").querySelector(".os-record-status")?.textContent).toBe("Active");
    // An opaque layout id is never shown.
    expect(screen.queryByText(/builtin:|org:|l-acme/)).toBeNull();
    // Nothing to add: layouts are saved from the editor.
    expect(screen.queryByRole("button", { name: /^(Add|Create|New)/ })).toBeNull();

    // Refine: by name and description, and by organization.
    fireEvent.click(screen.getByRole("button", { name: "Refine layouts" }));
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "the acme" } });
    expect(names()).toEqual(["Acme welcome"]);
    fireEvent.change(screen.getByLabelText("Search"), { target: { value: "" } });
    chooseOption(screen.getByLabelText("Organization"), "Acme");
    // The built-ins are every organization's to start from.
    expect(names()).toEqual([...BUILT_IN_NAMES, "Acme welcome"]);
  });

  it("says once that layouts are saved from the editor when an organization has none", async () => {
    const conn = fakeConnection({ accounts: ORGANIZATIONS });
    mount(conn);
    await openLayouts();
    await waitFor(() => expect(names()).toEqual(BUILT_IN_NAMES));
    expect(screen.getAllByText("Layouts you save from a finished email in the editor appear here.")).toHaveLength(1);
  });

  it("points at the setting when archived layouts are why none of an organization's own is listed", async () => {
    mount(fakeConnection({ layouts: { self: [layoutRow({ layoutId: "l1", name: "Old letter", status: "archived" })] } }));
    await openLayouts();
    await waitFor(() => expect(names()).toEqual(BUILT_IN_NAMES));
    expect(screen.getByText("Archived layouts are hidden. Turn on Include archived in Settings to see them.")).toBeTruthy();
    expect(screen.queryByText("Layouts you save from a finished email in the editor appear here.")).toBeNull();
  });

  it("lists archived layouts only with Include archived", async () => {
    const layouts = { self: [layoutRow({ layoutId: "l1", name: "Kept letter" }), layoutRow({ layoutId: "l2", name: "Old letter", status: "archived" })] };
    const first = mount(fakeConnection({ layouts }));
    await openLayouts();
    await waitFor(() => expect(names()).toEqual([...BUILT_IN_NAMES, "Kept letter"]));
    first.unmount();

    mount(fakeConnection({ layouts }), { includeArchived: true });
    await openLayouts();
    await waitFor(() => expect(names()).toEqual([...BUILT_IN_NAMES, "Kept letter", "Old letter"]));
    expect(rowOf("Old letter").querySelector(".os-record-status")?.textContent).toBe("Archived");
    expect(rowOf("Old letter").getAttribute("data-dim")).toBe("true");
  });

  it("draws the list's shape while it is read, and a refused reading is never an empty list", async () => {
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const conn = fakeConnection({ accounts: ORGANIZATIONS, layouts: { acme: new Error("emailLayouts: layout read permission is required in this organization") } });
    const answer = conn.query.emailLayouts.getMockImplementation()!;
    conn.query.emailLayouts.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    const view = mount(conn);
    await openLayouts();
    expect(view.container.querySelector(".os-record-skeleton")).not.toBeNull();
    expect(headCount(view)).toBeNull();
    await act(async () => { release(); });

    // The organization that answered is listed; the one that refused says so, in the engine's words.
    await waitFor(() => expect(names()).toEqual(BUILT_IN_NAMES));
    expect(screen.getByText("This cluster did not return the layouts for Acme.")).toBeTruthy();
    expect(screen.getByText("layout read permission is required in this organization")).toBeTruthy();
    // What cannot be read is not counted, and not called empty.
    expect(headCount(view)).toBeNull();
    expect(screen.queryByText("Layouts you save from a finished email in the editor appear here.")).toBeNull();
  });

  it("keeps the layouts current at the kit's pace", async () => {
    // Only the clock that schedules is fake: nothing here waits on it.
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    const conn = fakeConnection();
    mount(conn);
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(conn.query.emailLayouts).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(14_000); });
    expect(conn.query.emailLayouts).toHaveBeenCalledTimes(1);
    // The kit's default pace: fifteen seconds after the last read.
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    expect(conn.query.emailLayouts).toHaveBeenCalledTimes(2);
  });
});

describe("a layout's page", () => {
  it("a built-in layout's page previews it and offers nothing", async () => {
    const conn = fakeConnection({ accounts: ORGANIZATIONS });
    const view = mount(conn);
    await openLayouts();
    fireEvent.click(await within(await screen.findByRole("list", { name: "Layouts" })).findByText("Article"));
    expect(await screen.findByRole("heading", { level: 3, name: "Article" })).toBeTruthy();
    // Templates > Article, and Back returns to the layouts.
    expect(screen.getByRole("button", { name: "Back to Templates" })).toBeTruthy();

    const facts = screen.getByRole("region", { name: "Layout details" });
    expect([...facts.querySelectorAll("dt")].map((term) => term.textContent)).toEqual(["Organization", "Description"]);
    expect(within(facts).getByText("Built in")).toBeTruthy();
    // Previewed for the first organization in view.
    const article = BUILT_IN_LAYOUTS.find((row) => row.id === "builtin:article")!;
    await waitFor(() => expect(conn.query.emailRender).toHaveBeenCalledWith({ document: article.document, accountId: "self" }, expect.anything()));
    await waitFor(() => expect(frame()).not.toBeNull());
    expect(frame()!.getAttribute("sandbox")).toBe("allow-popups allow-popups-to-escape-sandbox");
    expect(screen.getByRole("navigation", { name: "Preview" })).toBeTruthy();
    // No lifecycle, no bar.
    expect(view.container.querySelector(".os-actbar")).toBeNull();
    expect(conn.query.emailLayoutById).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Back to Templates" }));
    expect(await screen.findByRole("list", { name: "Layouts" })).toBeTruthy();
  });

  it("archives and restores an organization layout with its exact revision", async () => {
    const letter = layoutRow({ layoutId: "l1", name: "Acme letter", revision: "2026-10-03T09:00:00Z" });
    const stored = {
      id: "l1",
      ownerUserId: "v1:identity:user:me",
      accountId: "acme",
      name: "Acme letter",
      description: "Our letter, with the logo and the footer.",
      document: { subject: "From Acme", blocks: [{ id: "logo", type: "image" }] },
      sourceArtifactId: "",
      status: "active",
      createdAt: "2026-10-03T09:00:00.123456Z",
    };
    const conn = fakeConnection({ accounts: ORGANIZATIONS, layouts: { acme: [letter] }, layoutRecords: [stored] });
    mount(conn);
    await openLayouts();
    fireEvent.click(await within(await screen.findByRole("list", { name: "Layouts" })).findByText("Acme letter"));
    expect(await screen.findByRole("heading", { level: 3, name: "Acme letter" })).toBeTruthy();
    expect(barWord()).toBe("Active");
    expect(barActs()).toEqual([["Archive", "text"]]);
    // Absent, never disabled.
    expect(deadButtons()).toEqual([]);

    // Archived layouts stay readable, so the next reading has it archived.
    conn.query.emailLayouts.mockImplementation(async (args: { accountId?: string }) =>
      rowsResult([...BUILT_IN_LAYOUTS, ...(args.accountId === "acme" ? [{ ...letter, status: "archived", revision: "2026-10-04T09:00:00Z" }] : [])]),
    );
    press("Archive");
    await waitFor(() => expect(conn.query.campaignSaveLayout).toHaveBeenCalledTimes(1));
    // The bare layout id, its organization, and the revision read by id just now;
    // the stored name and document go back as they are.
    expect(conn.query.emailLayoutById).toHaveBeenCalledWith({ layoutId: "l1" }, expect.anything());
    expect(conn.query.campaignSaveLayout.mock.calls[0]![0]).toEqual({
      layoutId: "l1",
      accountId: "acme",
      name: "Acme letter",
      document: stored.document,
      expectedRevision: "2026-10-03T09:00:00.123456Z",
      action: "archive",
    });
    // The page stays, with the layout archived and Restore right there.
    await waitFor(() => expect(barWord()).toBe("Archived"));
    expect(barActs()).toEqual([["Restore", "primary"]]);

    // Somebody saved it meanwhile: the engine's sentence, verbatim, and nothing moves.
    conn.query.campaignSaveLayout.mockRejectedValueOnce(new Error("campaignSaveLayout: this layout changed; compare the latest revision before saving"));
    await readTheBar();
    press("Restore");
    await waitFor(() => expect(conn.query.campaignSaveLayout).toHaveBeenCalledTimes(2));
    expect(conn.query.campaignSaveLayout.mock.calls[1]![0]).toMatchObject({ layoutId: "l1", accountId: "acme", action: "restore" });
    expect(await screen.findByText("The layout wasn't restored.")).toBeTruthy();
    expect(screen.getByText("this layout changed; compare the latest revision before saving")).toBeTruthy();
    expect(barActs()).toEqual([["Restore", "primary"]]);
  });

  it("says what a layout was saved from and by whom, without printing an id", async () => {
    const conn = fakeConnection({
      accounts: ORGANIZATIONS,
      templates: [templateRow({ id: "t9", name: "Welcome 2", accountId: "acme", sourceArtifactId: "art-1" })],
      layouts: {
        acme: [
          layoutRow({ layoutId: "l1", name: "From a template", sourceArtifactId: "art-1", revision: "2026-10-03T09:00:00Z" }),
          layoutRow({ layoutId: "l2", name: "From a file", sourceArtifactId: "art-2" }),
          layoutRow({ layoutId: "l3", name: "From nowhere", sourceArtifactId: "art-3" }),
        ],
      },
      layoutRecords: [
        { id: "l1", ownerUserId: "v1:identity:user:dana", accountId: "acme", name: "From a template", description: "", document: { subject: "A" }, sourceArtifactId: "art-1", status: "active", createdAt: "2026-10-03T09:00:00Z" },
        { id: "l2", ownerUserId: "v1:identity:user:me", accountId: "acme", name: "From a file", description: "", document: { subject: "B" }, sourceArtifactId: "art-2", status: "active", createdAt: "2026-10-03T09:00:00Z" },
        { id: "l3", ownerUserId: "v1:identity:user:stranger", accountId: "acme", name: "From nowhere", description: "", document: { subject: "C" }, sourceArtifactId: "art-3", status: "active", createdAt: "2026-10-03T09:00:00Z" },
      ],
      people: [{ id: "dana", displayName: "Dana Lee", primaryEmail: "dana@acme.com" }],
      artifacts: [{ id: "art-2", title: "Autumn letter.email.json" }],
    });
    mount(conn);
    await openLayouts();
    const page = async (name: string) => {
      fireEvent.click(await within(await screen.findByRole("list", { name: "Layouts" })).findByText(name));
      expect(await screen.findByRole("heading", { level: 3, name })).toBeTruthy();
      return screen.getByRole("region", { name: "Layout details" });
    };
    const fact = (facts: HTMLElement, label: string) => {
      const term = [...facts.querySelectorAll("dt")].find((dt) => dt.textContent === label);
      return term?.nextElementSibling?.textContent ?? null;
    };
    const back = async () => {
      fireEvent.click(screen.getByRole("button", { name: "Back to Templates" }));
      await screen.findByRole("list", { name: "Layouts" });
    };

    // Saved from the file a template was published from: the template's name. By a person the roster names.
    let facts = await page("From a template");
    await waitFor(() => expect(fact(facts, "Saved by")).toBe("Dana Lee"));
    expect(fact(facts, "Saved from")).toBe("Welcome 2");
    expect(fact(facts, "Organization")).toBe("Acme");
    expect(fact(facts, "Saved")).toMatch(/Oct 3/);
    await back();

    // No template from that file: the file's own title. Saved by the person looking.
    facts = await page("From a file");
    await waitFor(() => expect(fact(facts, "Saved from")).toBe("Autumn letter.email.json"));
    await waitFor(() => expect(fact(facts, "Saved by")).toBe("You"));
    await back();

    // Neither the file nor the person can be named: both facts are left out, never an id.
    facts = await page("From nowhere");
    await waitFor(() => expect(conn.query.libraryArtifactById).toHaveBeenCalledWith({ artifactId: "art-3" }, expect.anything()));
    await waitFor(() => expect(fact(facts, "Saved")).not.toBeNull());
    expect(fact(facts, "Saved from")).toBeNull();
    expect(fact(facts, "Saved by")).toBeNull();
    expect(document.body.textContent).not.toMatch(/stranger|art-3|l3\b/);
  });

  it("reads a layout whose document the listing left out before previewing it", async () => {
    const conn = fakeConnection({
      layouts: { self: [layoutRow({ layoutId: "big", name: "Big letter", document: undefined, documentOmitted: true })] },
      layoutRecords: [
        { id: "big", ownerUserId: "v1:identity:user:me", accountId: "self", name: "Big letter", description: "", document: { subject: "A big one", blocks: [] }, sourceArtifactId: "", status: "active", createdAt: "2026-10-03T09:00:00Z" },
      ],
    });
    mount(conn);
    await openLayouts();
    fireEvent.click(await within(await screen.findByRole("list", { name: "Layouts" })).findByText("Big letter"));
    await waitFor(() => expect(conn.query.emailLayoutById).toHaveBeenCalledWith({ layoutId: "big" }, expect.anything()));
    await waitFor(() => expect(conn.query.emailRender).toHaveBeenCalledWith({ document: { subject: "A big one", blocks: [] }, accountId: "self" }, expect.anything()));
    // Never a render of the empty document the listing had in its place.
    for (const call of conn.query.emailRender.mock.calls) expect(call[0].document).toEqual({ subject: "A big one", blocks: [] });
  });

  it("says in place that a layout asked for isn't available", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Welcome 2", layoutId: "org:l1" })],
      layouts: { self: [layoutRow({ layoutId: "l1", name: "Our letter" })] },
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Welcome 2"));
    const facts = await screen.findByRole("region", { name: "Template details" });
    fireEvent.click(await within(facts).findByRole("button", { name: "Our letter" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Our letter" })).toBeTruthy();

    // The next reading -- the window coming back into focus asks for one -- answers without it.
    conn.query.emailLayouts.mockImplementation(async () => rowsResult(BUILT_IN_LAYOUTS));
    act(() => { window.dispatchEvent(new Event("focus")); });
    expect(await screen.findByText("That layout isn't available.")).toBeTruthy();
    expect(screen.getByRole("list", { name: "Layouts" })).toBeTruthy();
  });
});
