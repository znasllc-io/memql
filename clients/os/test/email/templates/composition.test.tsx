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
const { templateActs } = await import("../../../src/apps/email/templates/templateActs");
const { templateFromRow } = await import("../../../src/apps/email/rows");
const { fakeConnection, templateRow, withSession } = await import("../harness");

// AN EMAIL MEMQL IS WRITING (Task 23): the template exists at once, marked as
// being written, and the composition ends done or failed. The page shows the
// state in its one bar: Cancel while writing, Retry after a failure.

type Conn = ReturnType<typeof fakeConnection>;

const TEMPLATE = "v1:campaigns:template";
const STARTED = "2026-11-04T10:00:00Z";
// The person signed in to the test session is "me"; the request records who asked.
const ME = { requestedBy: "me" };
const writing = { state: "writing", startedAt: STARTED, error: "", notes: [], request: ME };
const failed = (error = "Cancelled", request: Record<string, unknown> = ME) => ({ state: "failed", startedAt: STARTED, error, notes: [], request });

function mount(connection: Conn) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  return render(withSession(<EmailApp sectionId="templates" navigate={vi.fn()} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />));
}

async function openPage(name: string) {
  fireEvent.click(await screen.findByText(name));
  await screen.findByRole("heading", { level: 3, name });
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const barActs = () => within(bar()).queryAllByRole("button").map((button) => button.textContent);

afterEach(() => {
  vi.useRealTimers();
});

describe("a template MemQL is writing", () => {
  it("the template page offers Cancel while writing", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: writing })] });
    mount(conn);
    await openPage("Autumn letter");

    expect(barWord()).toBe("MemQL is writing this email");
    // Only Cancel: nothing to archive or open while the writer is at work, and nothing disabled.
    expect(barActs()).toEqual(["Cancel"]);
    // How long, in the editor's words: "Just started", "2 min", "1 h 5 min".
    expect(bar().querySelector(".os-actbar-meta")?.textContent).toMatch(/^(Just started|\d+ min|\d+ h|\d+ h \d+ min)$/);

    await readTheBar();
    fireEvent.click(within(bar()).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(conn.query.campaignCancelComposition).toHaveBeenCalledWith({ templateId: "t1" }));

    // The cluster says it failed as Cancelled: the bar follows the row, with Retry.
    await act(async () => {
      conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t1", name: "Autumn letter", status: "draft", createdAt: "2026-11-04T10:00:05Z", composition: failed("Cancelled") }));
    });
    await waitFor(() => expect(barActs()).toContain("Retry"));
    expect(barWord()).toBe("Failed");
    expect(bar().textContent).toContain("Cancelled");
  });

  it("the template page offers Retry after a failure", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: failed("The writer stopped early.") })] });
    mount(conn);
    await openPage("Autumn letter");

    expect(barWord()).toBe("Failed");
    // The server's sentence, as it is.
    expect(within(bar()).getByText("The writer stopped early.")).toBeTruthy();
    // The usual acts, Retry last as the primary; three at most.
    expect(barActs()).toEqual(["Archive", "Open in editor", "Retry"]);
    expect(within(bar()).getByRole("button", { name: "Retry" }).getAttribute("data-tone")).toBe("primary");

    await readTheBar();
    fireEvent.click(within(bar()).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(conn.query.campaignRetryComposition).toHaveBeenCalledWith({ templateId: "t1" }));

    await act(async () => {
      conn.subscriptions.emit(TEMPLATE, templateRow({ id: "t1", name: "Autumn letter", status: "draft", createdAt: "2026-11-04T10:00:09Z", composition: writing }));
    });
    await waitFor(() => expect(barWord()).toBe("MemQL is writing this email"));
    expect(barActs()).toEqual(["Cancel"]);
  });

  it("offers Retry only to the person who asked, whose files a retry reads", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Their letter", status: "draft", composition: failed("Cancelled", { requestedBy: "v1:identity:user:colleague" }) })],
    });
    mount(conn);
    await openPage("Their letter");
    expect(barWord()).toBe("Failed");
    expect(within(bar()).getByText("Cancelled")).toBeTruthy();
    // Absent, not disabled: the server would refuse it.
    expect(barActs()).toEqual(["Archive", "Open in editor"]);
  });

  it("shows an email a person finished by hand as the template it is: no Failed, no Retry", async () => {
    // A person's save or publish ends the failure; the reason and the notes stay as the record.
    const finished = { state: "done", startedAt: STARTED, error: "Cancelled", notes: ["Left out a.pdf: not a picture or text file."], request: ME };
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Autumn letter", status: "ready", composition: finished })] });
    mount(conn);
    const row = (await screen.findByText("Autumn letter")).closest(".os-row") as HTMLElement;
    expect(row.textContent).toContain("Published");
    expect(row.textContent).not.toContain("Failed");
    await openPage("Autumn letter");
    expect(barWord()).toBe("Published");
    expect(barActs()).toEqual(["Archive", "Open in editor"]);
    expect(bar().textContent).not.toContain("Cancelled");
  });

  it("changes by itself when the composition finishes, on the list and on the page", async () => {
    const draft = templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: writing });
    const conn = fakeConnection({ templates: [draft] });
    mount(conn);
    await openPage("Autumn letter");
    expect(barWord()).toBe("MemQL is writing this email");
    await act(async () => {
      conn.subscriptions.emit(TEMPLATE, { ...draft, createdAt: "2026-11-04T10:01:00Z", composition: { state: "done", startedAt: STARTED, error: "", notes: ["The photo was too large, so it was left out."] } });
    });
    await waitFor(() => expect(barWord()).toBe("Draft"));
    expect(barActs()).toEqual(["Archive", "Open in editor"]);
    // The notes appear once, above the preview.
    expect(screen.getAllByText("The photo was too large, so it was left out.")).toHaveLength(1);
  });

  it("refuses nothing silently: a refused Cancel says so and changes nothing", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: writing })] });
    conn.query.campaignCancelComposition.mockRejectedValueOnce(new Error("composition is not writing"));
    mount(conn);
    await openPage("Autumn letter");
    await readTheBar();
    fireEvent.click(within(bar()).getByRole("button", { name: "Cancel" }));
    expect(await screen.findByText("The writing wasn't stopped.")).toBeTruthy();
    expect(barWord()).toBe("MemQL is writing this email");
  });

  it("reads the templates again every two seconds while one is being written, and stops when none is", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    const draft = templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: writing });
    const conn = fakeConnection({ templates: [draft] });
    mount(conn);
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(screen.getByText("Autumn letter")).toBeTruthy();

    const reads = () => conn.query.templatesBrowse.mock.calls.length;
    const before = reads();
    await act(async () => { await vi.advanceTimersByTimeAsync(2_100); });
    expect(reads()).toBeGreaterThan(before);

    // It finishes: the row leaves writing, and the reading stops.
    await act(async () => {
      conn.subscriptions.emit(TEMPLATE, { ...draft, createdAt: "2026-11-04T10:01:00Z", composition: { state: "done", startedAt: STARTED, error: "", notes: [] } });
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    const settled = reads();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(reads()).toBe(settled);
  });

  it("stops reading when the section goes away", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Autumn letter", status: "draft", composition: writing })] });
    const view = mount(conn);
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    view.unmount();
    const settled = conn.query.templatesBrowse.mock.calls.length;
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(conn.query.templatesBrowse.mock.calls.length).toBe(settled);
  });
});

describe("what a template's bar offers while its email is written, as a table", () => {
  const acts = (status: string, composition: unknown, asking: "" | "archive" = "", viewer = "v1:identity:user:me") =>
    templateActs(templateFromRow(templateRow({ id: "t1", status, composition: composition as never })), asking, viewer).map((spec) => spec.label);

  it("offers Cancel only while writing, whatever the status, to every writer", () => {
    expect(acts("draft", writing)).toEqual(["Cancel"]);
    expect(acts("ready", writing)).toEqual(["Cancel"]);
    expect(acts("draft", { ...writing, request: { requestedBy: "colleague" } })).toEqual(["Cancel"]);
  });

  it("offers the usual acts then Retry after a failure, to the person who asked", () => {
    expect(acts("draft", failed())).toEqual(["Archive", "Open in editor", "Retry"]);
    expect(acts("ready", failed())).toEqual(["Archive", "Open in editor", "Retry"]);
    expect(acts("ready", failed(), "archive")).toEqual(["Keep", "Archive"]);
    expect(acts("archived", failed())).toEqual(["Restore"]);
    // Anyone else, or nobody known, has the usual acts and no Retry.
    expect(acts("draft", failed("Cancelled", { requestedBy: "colleague" }))).toEqual(["Archive", "Open in editor"]);
    expect(acts("draft", failed("Cancelled", {}))).toEqual(["Archive", "Open in editor"]);
    expect(acts("draft", failed(), "", "")).toEqual(["Archive", "Open in editor"]);
  });

  it("leaves a done composition and a template without one as they were", () => {
    expect(acts("draft", { state: "done", startedAt: STARTED, error: "", notes: [] })).toEqual(["Archive", "Open in editor"]);
    expect(acts("ready", null)).toEqual(["Archive", "Open in editor"]);
  });
});
