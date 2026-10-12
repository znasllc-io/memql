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
const { lifecycleContent, templateActs, templateEditorURL } = await import("../../../src/apps/email/templates/templateActs");
const { templateFromRow, templateRecordFromRow } = await import("../../../src/apps/email/rows");
const { saveEditorPreference } = await import("../../../src/items/editorPreference");
const { VSCODE_NO_ANSWER_MESSAGE, browserHandoffPorts } = await import("../../../src/items/vscode");
const { fakeConnection, rowsResult, templateRow, withSession } = await import("../harness");

// ONE TEMPLATE'S PAGE (memql#6033, Task 11): what it is, the email as it is
// sent -- rendered by the engine's one renderer, isolated in a sandboxed frame
// -- and ONE bar carrying everything that changes it. Writing it is the
// editor's: the page opens the editor, and archives or restores.

type Conn = ReturnType<typeof fakeConnection>;

const TEMPLATE = "v1:campaigns:template";
const DOCUMENT = { subject: "Glad you're here", preheader: "Three things to try", layout: "builtin:article", theme: { page: "#eef0f3" }, blocks: [{ id: "intro", type: "text", text: "Hello {{displayName}}" }] };
const RENDERED = {
  subject: "Glad you're here",
  html: '<html><body style="background-color:#eef0f3"><table width="600" style="color:#2b2b2b"><tr><td>Hello Dana</td></tr></table></body></html>',
  text: "Hello Dana\n\nThree things to try",
  issues: [],
};

function mount(connection: Conn, settings: Record<string, unknown> = {}) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const store = new LocalEmailSettingsStore({ getItem: (k) => bag.get(k) ?? null, setItem: (k, v) => void bag.set(k, v) });
  if (Object.keys(settings).length > 0) store.save({ ...store.load(), ...settings });
  return render(
    withSession(<EmailApp sectionId="templates" navigate={vi.fn()} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />),
  );
}

/** Open a template's page from its row. */
async function openPage(name: string) {
  fireEvent.click(await screen.findByText(name));
  await screen.findByRole("heading", { level: 3, name });
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barWord = () => bar().querySelector(".os-actbar-word")?.textContent ?? "";
const press = (name: string) => fireEvent.click(within(bar()).getByRole("button", { name }));
/** Each act on the bar, by its label and its dress: text, or the button's tone. */
const barActs = () =>
  within(bar())
    .queryAllByRole("button")
    .map((button) => [button.textContent, button.classList.contains("os-actbar-text") ? "text" : (button.getAttribute("data-tone") ?? "quiet")]);
/** The buttons a person could press that do nothing: disabled, and not because they are working. */
const deadButtons = () =>
  screen.queryAllByRole("button").filter((button) => (button as HTMLButtonElement).disabled && button.getAttribute("aria-busy") !== "true");
const frame = () => screen.queryByTitle("Email preview") as HTMLIFrameElement | null;

afterEach(() => {
  localStorage.clear();
  vi.useRealTimers();
});

describe("a template's page", () => {
  it("previews through emailRender in a sandboxed frame, with a plain-text tab", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Welcome 2", subject: "Glad you're here", preheader: "Three things to try", layoutId: "builtin:article" })],
      templateContent: { t1: { document: DOCUMENT } },
      render: RENDERED,
    });
    mount(conn);
    await openPage("Welcome 2");

    // The template is read whole, by id: a list row carries no document.
    await waitFor(() => expect(conn.query.templateById).toHaveBeenCalledWith({ templateId: "t1" }, expect.anything()));
    // The engine's one renderer, the document as it is stored, for its organization.
    await waitFor(() => expect(conn.query.emailRender).toHaveBeenCalledWith({ document: DOCUMENT, accountId: "self" }, expect.anything()));

    // The facts, then the preview directly under them.
    const facts = screen.getByRole("region", { name: "Template details" });
    const terms = [...facts.querySelectorAll("dt")].map((term) => term.textContent);
    expect(terms).toEqual(["Organization", "Layout", "Subject", "Preheader", "Changed"]);
    expect(within(facts).getByText("Operator organization")).toBeTruthy();
    await waitFor(() => expect(within(facts).getByRole("button", { name: "Article" })).toBeTruthy());
    expect(within(facts).getByText("Glad you're here")).toBeTruthy();
    expect(within(facts).getByText("Three things to try")).toBeTruthy();
    const tabs = screen.getByRole("navigation", { name: "Preview" });
    expect(facts.nextElementSibling).toBe(tabs);
    expect(within(tabs).getAllByRole("button").map((tab) => tab.textContent)).toEqual(["Formatted", "Plain text"]);
    expect(within(tabs).getByRole("button", { name: "Formatted" }).getAttribute("aria-current")).toBe("page");

    // Formatted: the email's own HTML, isolated -- no scripts, no same origin.
    await waitFor(() => expect(frame()).not.toBeNull());
    expect(frame()!.getAttribute("sandbox")).toBe("allow-popups allow-popups-to-escape-sandbox");
    const srcdoc = frame()!.getAttribute("srcdoc") ?? "";
    expect(srcdoc).toContain("Content-Security-Policy");
    // Its own look, kept: inline styles and its own background.
    expect(srcdoc).toContain("color:#2b2b2b");
    expect(srcdoc).toContain("background-color:#eef0f3");
    // The frame's look comes from the stylesheet, not an inline style.
    expect(frame()!.hasAttribute("style")).toBe(false);

    // Plain text: the engine's text part, as it is sent.
    fireEvent.click(within(tabs).getByRole("button", { name: "Plain text" }));
    expect(frame()).toBeNull();
    expect(document.querySelector("pre")?.textContent).toBe("Hello Dana\n\nThree things to try");
  });

  it("draws the frame's shape while the email is being rendered", async () => {
    let release!: () => void;
    const gate = new Promise<void>((done) => { release = done; });
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })], templateContent: { t1: { document: DOCUMENT } }, render: RENDERED });
    const answer = conn.query.emailRender.getMockImplementation()!;
    conn.query.emailRender.mockImplementation(async (args, opts) => { await gate; return answer(args, opts); });
    const view = mount(conn);
    await openPage("Welcome 2");
    await waitFor(() => expect(conn.query.emailRender).toHaveBeenCalled());
    // Loading is the shape of what is coming, in its place; never a caption.
    expect(view.container.querySelector(".os-content-skeleton")).not.toBeNull();
    expect(frame()).toBeNull();
    // What it says while it waits is for a screen reader alone.
    for (const said of screen.queryAllByText(/loading/i)) expect(said.classList.contains("os-sr-only")).toBe(true);
    await act(async () => { release(); });
    await waitFor(() => expect(frame()).not.toBeNull());
    expect(view.container.querySelector(".os-content-skeleton")).toBeNull();
  });

  it("previews a template saved before documents existed from its own HTML", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Old note", subject: "News" })],
      templateContent: { t1: { document: null, textBody: "Hello", htmlBody: "<p>Hello</p>" } },
    });
    mount(conn);
    await openPage("Old note");
    await waitFor(() =>
      expect(conn.query.emailRender).toHaveBeenCalledWith({ document: { subject: "News", textBody: "Hello", htmlBody: "<p>Hello</p>" }, accountId: "self" }, expect.anything()),
    );
    expect(await screen.findByText("Custom")).toBeTruthy();
  });

  it("lists render issues as the engine wrote them", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Welcome 2", status: "draft" })],
      templateContent: { t1: { document: DOCUMENT } },
      render: {
        ...RENDERED,
        issues: [
          { code: "image_missing", block: "banner", message: "The banner block has no picture." },
          { code: "unsubscribe_missing", message: "A marketing email needs an unsubscribe link: add {{unsubscribeUrl}}." },
        ],
      },
    });
    mount(conn);
    await openPage("Welcome 2");
    const notice = (await screen.findByText("Fix these before publishing:")).closest(".os-notice") as HTMLElement;
    expect(notice.getAttribute("data-tone")).toBe("warn");
    expect(within(notice).getAllByRole("listitem").map((item) => item.textContent)).toEqual([
      "The banner block has no picture.",
      "A marketing email needs an unsubscribe link: add {{unsubscribeUrl}}.",
    ]);
  });

  it("says so in place when the preview could not be made", async () => {
    const conn = fakeConnection({
      templates: [templateRow({ id: "t1", name: "Welcome 2" })],
      templateContent: { t1: { document: DOCUMENT } },
      render: new Error("emailRender: Previews are busy right now. Try again in a moment."),
    });
    mount(conn);
    await openPage("Welcome 2");
    expect(await screen.findByText("This cluster did not return the preview.")).toBeTruthy();
    // The engine's sentence, without the call's name in front of it.
    expect(screen.getByText("Previews are busy right now. Try again in a moment.")).toBeTruthy();
    expect(frame()).toBeNull();
  });

  it("keeps the last email on screen when a new revision cannot be rendered, and says so", async () => {
    const welcome = templateRow({ id: "t1", name: "Welcome 2" });
    const conn = fakeConnection({ templates: [welcome], templateContent: { t1: { document: DOCUMENT } }, render: RENDERED });
    mount(conn);
    await openPage("Welcome 2");
    await waitFor(() => expect(frame()).not.toBeNull());

    // Saved again somewhere else: the page reads it again, and this time the renderer is busy.
    const saved = { ...welcome, createdAt: "2026-08-02T00:00:00Z" };
    conn.query.templateById.mockResolvedValue(rowsResult([{ ...saved, textBody: "", htmlBody: "", document: { ...DOCUMENT, subject: "Saved again" } }]));
    conn.query.emailRender.mockRejectedValue(new Error("emailRender: Previews are busy right now. Try again in a moment."));
    act(() => conn.subscriptions.emit(TEMPLATE, saved));
    await waitFor(() => expect(conn.query.emailRender).toHaveBeenLastCalledWith({ document: { ...DOCUMENT, subject: "Saved again" }, accountId: "self" }, expect.anything()));
    expect(await screen.findByText("This cluster did not return the preview.")).toBeTruthy();
    // The email it had stays, beneath the sentence that says the new one is not here.
    expect(frame()).not.toBeNull();
  });

  it.each(["draft", "ready", "archived"] as const)("offers the acts a %s template allows", async (status) => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2", status })], templateContent: { t1: { document: DOCUMENT } } });
    mount(conn, { includeArchived: true });
    await openPage("Welcome 2");
    const expected = {
      draft: { word: "Draft", acts: [["Archive", "text"], ["Open in editor", "primary"]] },
      ready: { word: "Published", acts: [["Archive", "text"], ["Open in editor", "primary"]] },
      archived: { word: "Archived", acts: [["Restore", "primary"]] },
    }[status];
    expect(barWord()).toBe(expected.word);
    expect(barActs()).toEqual(expected.acts);
    // Absent, never disabled.
    expect(deadButtons()).toEqual([]);
  });

  it("archives a published template after asking, and restores", async () => {
    const welcome = templateRow({ id: "t1", name: "Welcome 2", createdAt: "2026-10-01T08:00:00Z" });
    const conn = fakeConnection({ templates: [welcome], templateContent: { t1: { document: DOCUMENT } } });
    mount(conn);
    await openPage("Welcome 2");

    press("Archive");
    // A published template is asked about first: campaigns and automations use it.
    expect(within(bar()).getByText("Archive “Welcome 2”? Campaigns and automations that use it can't send it until it's restored.")).toBeTruthy();
    expect(barActs()).toEqual([["Keep", "text"], ["Archive", "danger"]]);
    // Keep is the way out, and nothing was written.
    await readTheBar();
    press("Keep");
    expect(barActs()).toEqual([["Archive", "text"], ["Open in editor", "primary"]]);
    expect(conn.query.campaignSaveTemplate).not.toHaveBeenCalled();

    await readTheBar();
    press("Archive");
    await readTheBar();
    const readsBefore = conn.query.templateById.mock.calls.length;
    press("Archive");
    await waitFor(() => expect(conn.query.campaignSaveTemplate).toHaveBeenCalledTimes(1));
    // The revision of the row on screen, and no content: the engine never reads
    // content for an archive or a restore, so nothing is read to send it either.
    expect(conn.query.campaignSaveTemplate.mock.calls[0]![0]).toEqual({
      templateId: "t1",
      name: "Welcome 2",
      accountId: "self",
      expectedRevision: "2026-10-01T08:00:00Z",
      content: {},
      action: "archive",
    });
    expect(conn.query.templateById.mock.calls.length).toBe(readsBefore);
    // The act stays busy until the change arrives.
    await waitFor(() => expect(barWord()).toBe("Archiving"));
    act(() => conn.subscriptions.emit(TEMPLATE, { ...welcome, status: "archived", createdAt: "2026-10-02T08:00:00Z" }));
    // The page stays with its template, archived, and Restore is right there.
    await waitFor(() => expect(barWord()).toBe("Archived"));
    expect(barActs()).toEqual([["Restore", "primary"]]);

    await readTheBar();
    press("Restore");
    await waitFor(() => expect(conn.query.campaignSaveTemplate).toHaveBeenCalledTimes(2));
    expect(conn.query.campaignSaveTemplate.mock.calls[1]![0]).toEqual({
      templateId: "t1",
      name: "Welcome 2",
      accountId: "self",
      expectedRevision: "2026-10-02T08:00:00Z",
      content: {},
      action: "restore",
    });
  });

  // The list rows hold no body, so nothing here has one to send back: archiving
  // and restoring name the revision and the action, and the content is empty. A
  // restore that saved the row's (absent) body would have written an empty email.
  it("restores an archived template by its revision alone, sending no content", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "August copy", status: "archived" })] });
    mount(conn, { includeArchived: true });
    await openPage("August copy");
    const readsBefore = conn.query.templateById.mock.calls.length;
    press("Restore");
    await waitFor(() => expect(conn.query.campaignSaveTemplate).toHaveBeenCalledTimes(1));
    expect(conn.query.campaignSaveTemplate.mock.calls[0]![0]).toEqual({
      templateId: "t1",
      name: "August copy",
      accountId: "self",
      expectedRevision: "2026-08-01T00:00:00Z",
      content: {},
      action: "restore",
    });
    // Nothing is read for it: a read that failed would say the restore failed when nothing was written.
    expect(conn.query.templateById.mock.calls.length).toBe(readsBefore);
  });

  it("archives with no content either", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "August copy", status: "draft" })], templateContent: { t1: new Error("templateById: unavailable") } });
    mount(conn);
    await openPage("August copy");
    press("Archive");
    await waitFor(() => expect(conn.query.campaignSaveTemplate).toHaveBeenCalledTimes(1));
    const sent = conn.query.campaignSaveTemplate.mock.calls[0]![0] as Record<string, unknown>;
    expect(sent.content).toEqual({});
    expect(sent.action).toBe("archive");
    // A template whose whole could not be read is still archived: the call never needed it.
    expect(screen.queryByText("The template wasn't archived.")).toBeNull();
    expect(screen.queryByText(/Couldn't confirm/)).toBeNull();
  });

  it("archives a draft without asking, and shows a refusal in the engine's words", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2", status: "draft" })], templateContent: { t1: { document: DOCUMENT } } });
    conn.query.campaignSaveTemplate.mockRejectedValueOnce(new Error("campaignSaveTemplate: this template changed; compare the latest revision before saving or publishing"));
    mount(conn);
    await openPage("Welcome 2");
    press("Archive");
    await waitFor(() => expect(conn.query.campaignSaveTemplate).toHaveBeenCalledTimes(1));
    expect(conn.query.campaignSaveTemplate.mock.calls[0]![0]).toMatchObject({ action: "archive" });
    expect(await screen.findByText("The template wasn't archived.")).toBeTruthy();
    expect(screen.getByText("this template changed; compare the latest revision before saving or publishing")).toBeTruthy();
    // Nothing moved, so the acts are back.
    expect(barActs()).toEqual([["Archive", "text"], ["Open in editor", "primary"]]);
  });

  it("opens the source file when the template has one", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    try {
      const conn = fakeConnection({
        templates: [templateRow({ id: "t1", name: "Welcome 2", sourceArtifactId: "art-9" }), templateRow({ id: "t2", name: "August copy" })],
      });
      mount(conn);
      await openPage("Welcome 2");
      press("Open in editor");
      expect(open).toHaveBeenCalledWith(expect.stringContaining("https://vscode.memql.example.com/editor/?resource="), "_blank", "noopener,noreferrer");
      expect(new URL(String(open.mock.calls[0]![0])).searchParams.get("resource")).toBe("memql-file://memql.example.com/artifacts/art-9/Welcome%202.email.json");

      fireEvent.click(screen.getByRole("button", { name: "Back to Templates" }));
      await openPage("August copy");
      press("Open in editor");
      // No file of its own: the template itself, as a file.
      expect(new URL(String(open.mock.calls[1]![0])).searchParams.get("resource")).toBe("memql-file://memql.example.com/templates/t2/August%20copy.email.json");
      // The OS never edits: no embedded editor, no write.
      expect(document.querySelector("textarea")).toBeNull();
      expect(conn.query.campaignSaveTemplate).not.toHaveBeenCalled();
    } finally {
      open.mockRestore();
    }
  });

  it("shows the editor's silence where it happened", async () => {
    saveEditorPreference("vscode");
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2" })] });
    mount(conn);
    await openPage("Welcome 2");
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    const navigate = vi.spyOn(browserHandoffPorts, "navigate").mockImplementation(() => {});
    try {
      press("Open in editor");
      expect(navigate.mock.calls[0]![0]).toMatch(/^vscode:\/\/znasllc\.memql-productivity-tools\/open\?resource=memql-file%3A%2F%2Fmemql\.example\.com%2Ftemplates%2Ft1%2FWelcome%25202\.email\.json/);
      expect(screen.queryByText(VSCODE_NO_ANSWER_MESSAGE)).toBeNull();
      act(() => { vi.advanceTimersByTime(3_000); });
    } finally {
      navigate.mockRestore();
      vi.useRealTimers();
    }
    // Under the Head, before anything else on the page.
    const notice = screen.getByText(VSCODE_NO_ANSWER_MESSAGE).closest(".os-notice") as HTMLElement;
    expect(notice.previousElementSibling?.classList.contains("os-head")).toBe(true);
  });

  it("opens the layout it was made from", async () => {
    const conn = fakeConnection({ templates: [templateRow({ id: "t1", name: "Welcome 2", layoutId: "builtin:article" })] });
    mount(conn);
    await openPage("Welcome 2");
    const facts = screen.getByRole("region", { name: "Template details" });
    fireEvent.click(await within(facts).findByRole("button", { name: "Article" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Article" })).toBeTruthy();
    // Back is the Layouts view.
    fireEvent.click(screen.getByRole("button", { name: "Back to Templates" }));
    expect(await screen.findByRole("heading", { level: 3, name: "Templates" })).toBeTruthy();
    expect(within(screen.getByRole("navigation", { name: "Templates and layouts" })).getByRole("button", { name: "Layouts" }).getAttribute("aria-current")).toBe("page");
  });
});

describe("what a template's bar offers, as a table", () => {
  const dress = (specs: ReturnType<typeof templateActs>) => specs.map((spec) => [spec.label, spec.tone ?? (spec.text ? "text" : "quiet")]);
  const template = (status: string) => templateFromRow(templateRow({ id: "t1", name: "Welcome 2", status }));

  it("asks before archiving a published template, and only a published one", () => {
    expect(dress(templateActs(template("ready"), ""))).toEqual([["Archive", "text"], ["Open in editor", "primary"]]);
    expect(dress(templateActs(template("ready"), "archive"))).toEqual([["Keep", "text"], ["Archive", "danger"]]);
    // The Archive that asks and the Archive that does are two acts.
    expect(templateActs(template("ready"), "")[0]?.id).not.toBe(templateActs(template("draft"), "")[0]?.id);
    // A stale question never brings an archived template more than Restore.
    expect(dress(templateActs(template("archived"), "archive"))).toEqual([["Restore", "primary"]]);
    expect(dress(templateActs(template("draft"), "archive"))).toEqual([["Archive", "text"], ["Open in editor", "primary"]]);
  });

  it("opens the file a template was published from, else the template", () => {
    const fromFile = templateFromRow(templateRow({ id: "t1", name: "Welcome 2", sourceArtifactId: "art/9" }));
    const resource = (url: string) => new URL(url).searchParams.get("resource");
    expect(resource(templateEditorURL("memql.example.com", fromFile))).toBe("memql-file://memql.example.com/artifacts/art%2F9/Welcome%202.email.json");
    const alone = templateFromRow(templateRow({ id: "t2", name: "August copy" }));
    expect(resource(templateEditorURL("memql.example.com", alone))).toBe("memql-file://memql.example.com/templates/t2/August%20copy.email.json");
    // However long the name, the file keeps the extension that picks the email editor.
    const long = templateFromRow(templateRow({ id: "t3", name: "x".repeat(400), sourceArtifactId: "art-3" }));
    expect(resource(templateEditorURL("memql.example.com", long))).toMatch(/\.email\.json$/);
  });

  it("hands back the stored document, else the custom email it was saved as", () => {
    const record = (over: Record<string, unknown>) => templateRecordFromRow(rowsResult([{ ...templateRow({ id: "t1", subject: "News" }), textBody: "Hi", htmlBody: "<p>Hi</p>", document: null, ...over }]).rows()[0]!);
    expect(lifecycleContent(record({ document: DOCUMENT }))).toEqual(DOCUMENT);
    expect(lifecycleContent(record({}))).toEqual({ subject: "News", textBody: "Hi", htmlBody: "<p>Hi</p>" });
  });
});
