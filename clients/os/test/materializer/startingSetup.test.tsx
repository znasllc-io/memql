import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { Layers } from "lucide-react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Concepts, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { OsAppManifest } from "../../src/system/registry";

const h = vi.hoisted(() => ({ connection: null as unknown, os: null as unknown, openApp: vi.fn() }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));
// The shell, or none: an app asks `useOsIfPresent` and treats null as "nowhere
// to hand off to". A test that wants a shell supplies one by setting `h.os`.
vi.mock("../../src/chrome/state", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/chrome/state")>()),
  useOsIfPresent: () => h.os,
}));

const { MaterializerApp } = await import("../../src/apps/materializer/MaterializerApp");
const { useMaterialize } = await import("../../src/apps/materializer/actions");
const { composeIntent } = await import("../../src/items/materialize");
const { compositionRow, fakeConnection, memoryStore, refused, rowsResult, templateRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const COMPOSITION = "v1:compose:composition";
const REQUEST_ID = /^[A-Za-z0-9_-]{8,111}$/;

beforeEach(() => {
  h.connection = null;
  h.os = null;
  h.openApp.mockReset();
});

// A stand-in for the Files app's manifest, and a shell that holds the apps it is given.
const FILES: OsAppManifest = {
  id: "files",
  name: "Files",
  icon: Layers,
  requires: "app:files",
  sections: [{ id: "browse", name: "Browse" }],
  settingsSection: "settings",
  logsSection: "logs",
  component: () => null,
};
const shell = (apps: OsAppManifest[]) => ({ actions: { openApp: h.openApp }, registry: { apps, widgets: [] } });

interface Mounted {
  navigate: ReturnType<typeof vi.fn>;
  consumeIntent: ReturnType<typeof vi.fn>;
  /** Deliver a new standing instruction, as the shell does to a window that is already open. */
  deliver: (intent: { id: string; payload: Record<string, unknown> }) => void;
}

function mount(
  conn: Conn,
  opts: { sectionId?: string; intent?: { id: string; payload: Record<string, unknown> }; settings?: Record<string, unknown> } = {},
): Mounted {
  h.connection = conn;
  const navigate = vi.fn();
  const consumeIntent = vi.fn();
  const store = memoryStore(opts.settings);
  const tree = (intent?: { id: string; payload: Record<string, unknown> }) =>
    withSession(
      <MaterializerApp
        sectionId={opts.sectionId ?? "composer"}
        navigate={navigate}
        askContext={() => {}}
        intent={intent}
        consumeIntent={consumeIntent}
        store={store}
      />,
    );
  const view = render(tree(opts.intent));
  return { navigate, consumeIntent, deliver: (intent) => view.rerender(tree(intent)) };
}

const setupIntent = (id: string, setup: Parameters<typeof composeIntent>[0]) => ({ id, payload: composeIntent(setup) });

async function chooseOrganization() {
  fireEvent.click(screen.getByLabelText("Organization"));
  fireEvent.click(await screen.findByRole("option", { name: "Client A" }));
}

/** Fill the brief and press Materialize. */
async function materializeNotes(over: { name?: string; statement?: string } = {}) {
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: over.name ?? "Notes" } });
  fireEvent.change(screen.getByLabelText("What do you want made?"), { target: { value: over.statement ?? "A one page brief" } });
  await chooseOrganization();
  fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));
}

const sent = (conn: Conn, index = 0): Record<string, unknown> => conn.query.composeMaterialize.mock.calls[index]?.[0] ?? {};
// A notice's sentence as it is SHOWN. The live region repeats the words for assistive technology, so a plain
// text search finds the announcement too; this finds only the sentence line of the visible notice.
const shown = (text: string) => screen.findByText(text, { selector: ".os-notice-line" });
const kindOfFile = () => screen.getByLabelText("Kind of file").textContent ?? "";

// ---------------------------------------------------------------------------
// The starting setup: another app opens the composer already pointed somewhere
// ---------------------------------------------------------------------------

describe("a starting setup opens the composer pre-filled", () => {
  it("takes the intent exactly once and moves the window to the composer", async () => {
    const conn = fakeConnection();
    const intent = setupIntent("i1", { format: "csv" });
    const { navigate, consumeIntent, deliver } = mount(conn, { sectionId: "templates", intent });
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
    expect(navigate).toHaveBeenCalledWith("composer");
    // The shell can hand the same instruction over again before it has cleared it
    // (a re-render, a second delivery of one id): it is acted on once.
    deliver(intent);
    deliver({ ...intent });
    await act(async () => {});
    expect(consumeIntent).toHaveBeenCalledTimes(1);
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("starts the composer on the kind of file the sender chose, not the one it would have started on", async () => {
    // The app starts on Markdown, so Markdown proves nothing: a sender that asks
    // for anything ELSE is the only way to see the setup was applied.
    mount(fakeConnection(), { intent: setupIntent("i1", { format: "csv" }) });
    await waitFor(() => expect(kindOfFile()).toContain("Spreadsheet (CSV)"));
  });

  it("starts on the person's own default when the sender chose no format", async () => {
    // The default has to be something other than what the app would start on by
    // itself, or a composer that ignored it would pass.
    mount(fakeConnection(), { settings: { defaultFormat: "html" }, intent: setupIntent("i1", { folderId: "folder-1" }) });
    await waitFor(() => expect(kindOfFile()).toContain("Web page"));
  });

  it("picks the sources it was given, and sends exactly those", async () => {
    const conn = fakeConnection({ materializeReply: { compositionId: "c1" } as unknown as Row });
    mount(conn, {
      intent: setupIntent("i1", {
        format: "csv",
        sources: [
          { kind: "library_file", ref: "f1", label: "notes.md" },
          { kind: "library_file", ref: "f1", label: "notes.md" },
          { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001" },
        ],
      }),
    });
    const picked = await screen.findByRole("list", { name: "Picked sources" });
    // A source listed twice is one source, as it is when a person picks it twice.
    expect(within(picked).getAllByRole("listitem")).toHaveLength(2);
    expect(within(picked).getByText("notes.md")).toBeTruthy();
    expect(within(picked).getByText("INV-1001")).toBeTruthy();

    await chooseOrganization();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Inventory" } });
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)).toMatchObject({
      name: "Inventory",
      format: "csv",
      sources: [
        { kind: "library_file", ref: "f1", label: "notes.md" },
        { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001" },
      ],
    });
  });

  it("starts a new composition from a setup that arrives over a half-typed one", async () => {
    const conn = fakeConnection();
    const { deliver } = mount(conn);
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Old name" } });
    fireEvent.change(screen.getByLabelText("What do you want made?"), { target: { value: "Old brief" } });
    deliver(setupIntent("i2", { format: "html" }));
    await waitFor(() => expect(kindOfFile()).toContain("Web page"));
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("What do you want made?") as HTMLInputElement).value).toBe("");
  });

  it("applies a setup once: leaving the composer and coming back is a blank form, as it always was", async () => {
    const conn = fakeConnection();
    h.connection = conn;
    const store = memoryStore();
    const intent = setupIntent("i1", { format: "csv" });
    const tree = (sectionId: string) =>
      withSession(
        <MaterializerApp sectionId={sectionId} navigate={() => {}} askContext={() => {}} intent={intent} consumeIntent={() => {}} store={store} />,
      );
    const view = render(tree("composer"));
    await waitFor(() => expect(kindOfFile()).toContain("Spreadsheet (CSV)"));
    view.rerender(tree("materialized"));
    view.rerender(tree("composer"));
    await screen.findByLabelText("Kind of file");
    expect(kindOfFile()).toContain("Markdown");
  });

  it("shows the composer, not a settled composition, when a setup arrives over one", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c9`, name: "Finished earlier" })] });
    const { deliver } = mount(conn, { intent: { id: "open", payload: { compositionId: `${COMPOSITION}:c9` } } });
    expect(await screen.findByText("Made from")).toBeTruthy();
    deliver(setupIntent("i2", { format: "csv" }));
    await waitFor(() => expect(kindOfFile()).toContain("Spreadsheet (CSV)"));
    expect(screen.queryByText("Made from")).toBeNull();
  });

  it("leaves an intent that is not a setup, and not a composition, standing", async () => {
    const conn = fakeConnection();
    const { consumeIntent } = mount(conn, { intent: { id: "i3", payload: { somethingElse: "x" } } });
    await screen.findByText(/Describe what to make/);
    expect(consumeIntent).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Where the window opens
// ---------------------------------------------------------------------------

// A window opened FOR an instruction was opened where the sender asked. The app's
// own "open me on ..." preference is for a window nobody asked anything of, and a
// fresh window cannot tell the two apart by section: Compose is both the shell's
// first section and the one a sender names.
describe("the section a window opens on", () => {
  it("is the one a setup asked for, not the person's default section", async () => {
    const { navigate, consumeIntent } = mount(fakeConnection(), { settings: { defaultSection: "materialized" }, intent: setupIntent("i1", { format: "csv" }) });
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
    await act(async () => {});
    expect(navigate).not.toHaveBeenCalledWith("materialized");
    expect(navigate).toHaveBeenLastCalledWith("composer");
  });

  it("is the one an open-this-composition instruction asked for", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c9`, name: "From elsewhere" })] });
    const { navigate, consumeIntent } = mount(conn, { settings: { defaultSection: "materialized" }, intent: { id: "open", payload: { compositionId: `${COMPOSITION}:c9` } } });
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("open"));
    await act(async () => {});
    expect(navigate).not.toHaveBeenCalledWith("materialized");
    expect(navigate).toHaveBeenLastCalledWith("composer");
  });

  it("is the person's default section when nothing asked for one", async () => {
    const { navigate } = mount(fakeConnection(), { settings: { defaultSection: "materialized" } });
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("materialized"));
  });

  // Only an instruction the Materializer acts on is a sender saying where the window
  // goes. One addressed to something else is left standing, and the window opens
  // where the person prefers, as one nobody asked anything of does.
  it("is the person's default section when the instruction pending is not one the Materializer acts on", async () => {
    const { navigate, consumeIntent } = mount(fakeConnection(), { settings: { defaultSection: "materialized" }, intent: { id: "i3", payload: { somethingElse: "x" } } });
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("materialized"));
    expect(consumeIntent).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// The folder: the cluster's to refuse, the form's to carry
// ---------------------------------------------------------------------------

describe("the folder a setup names", () => {
  const reply = { compositionId: "c1" } as unknown as Row;
  const REFUSAL = "That folder isn't available. Choose another folder.";
  const returnTo = { appId: "files", sectionId: "browse" };
  const start = async (conn: Conn, folderId: string) => {
    const { consumeIntent } = mount(conn, { intent: setupIntent("i1", { format: "markdown", folderId }) });
    await screen.findByLabelText("Kind of file");
    // However the folder fares, the setup itself is taken and the form pre-filled.
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
    expect(kindOfFile()).toContain("Markdown");
  };

  it("is sent as the setup named it, and the app reads no folder itself", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    await start(conn, "folder-1");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)["folderId"]).toBe("folder-1");
    // Whether a folder can be filed into is the cluster's answer to give. A read
    // made here would turn its refusal into a file quietly at the top.
    expect(conn.query.libraryFolderById).not.toHaveBeenCalled();
  });

  it("sends no folder when the setup named none", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn, { intent: setupIntent("i1", { format: "markdown" }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)).not.toHaveProperty("folderId");
  });

  it("shows the cluster's refusal beside the control, verbatim, and keeps the act", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    conn.query.composeMaterialize.mockRejectedValueOnce(refused("composeMaterialize", REFUSAL));
    await start(conn, "folder-gone");
    await materializeNotes();
    expect(await screen.findByText(REFUSAL)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Materialize" })).toBeTruthy();
    // Nothing was made, and nothing was made somewhere else.
    expect(conn.query.composeMaterialize).toHaveBeenCalledOnce();
  });

  it("asks again with the folder it was refused for and the same request id, because that is the same ask", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    conn.query.composeMaterialize.mockRejectedValueOnce(refused("composeMaterialize", REFUSAL));
    await start(conn, "folder-gone");
    await materializeNotes();
    await screen.findByText(REFUSAL);
    fireEvent.click(screen.getByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    expect(sent(conn, 1)["folderId"]).toBe("folder-gone");
    expect(sent(conn, 1)["requestId"]).toBe(sent(conn, 0)["requestId"]);
  });

  it("is the first file's folder only: a second composition in the window files where the person files", async () => {
    const conn = fakeConnection({ materializeReply: reply, compositions: [] });
    mount(conn, { intent: setupIntent("i1", { folderId: "folder-1" }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    // The first composition arrives, the person starts another.
    conn.subscriptions.emit(COMPOSITION, compositionRow({ id: "c1", status: "ready", format: "markdown" }), "NODE_CREATED");
    fireEvent.click(await screen.findByRole("button", { name: "Compose another" }));
    await materializeNotes({ name: "Second" });
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    expect(sent(conn, 0)["folderId"]).toBe("folder-1");
    expect(sent(conn, 1)).not.toHaveProperty("folderId");
  });

  // The folder and the way back are part of the form the setup filled in, like
  // its format and its sources: they live and die with it. Leaving the composer
  // is abandoning that form, and the next one is the person's own.
  it("belongs to the form it was applied to: leaving the composer and coming back is a form with no folder and no way back", async () => {
    const conn = fakeConnection({ materializeReply: reply, compositions: [] });
    h.connection = conn;
    h.os = shell([FILES]);
    const store = memoryStore();
    const intent = setupIntent("i1", { format: "csv", folderId: "folder-1", returnTo });
    const tree = (sectionId: string) =>
      withSession(
        <MaterializerApp sectionId={sectionId} navigate={() => {}} askContext={() => {}} intent={intent} consumeIntent={() => {}} store={store} />,
      );
    const view = render(tree("composer"));
    await waitFor(() => expect(kindOfFile()).toContain("Spreadsheet (CSV)"));
    view.rerender(tree("materialized"));
    view.rerender(tree("composer"));
    await screen.findByLabelText("Kind of file");

    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)).not.toHaveProperty("folderId");
    conn.subscriptions.emit(COMPOSITION, compositionRow({ id: "c1", name: "Notes", status: "ready", format: "markdown", sources: [] }), "NODE_UPDATED");
    await screen.findByRole("button", { name: "Open Notes" });
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("is the folder of the newest setup, and the reply to an older ask does not take it away", async () => {
    const conn = fakeConnection({ materializeReply: { compositionId: "c2" } as unknown as Row, compositions: [] });
    let answer!: () => void;
    conn.query.composeMaterialize.mockImplementationOnce(
      () => new Promise((resolve) => { answer = () => resolve(rowsResult([{ compositionId: "c1" } as unknown as Row])); }),
    );
    const { deliver } = mount(conn, { intent: setupIntent("i1", { folderId: "folder-a", returnTo }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    // While the first file is being asked for, another app sends a new setup.
    deliver(setupIntent("i2", { folderId: "folder-b" }));
    await waitFor(() => expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe(""));
    answer();
    await act(async () => {});
    await materializeNotes({ name: "Second" });
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    expect(sent(conn, 0)["folderId"]).toBe("folder-a");
    expect(sent(conn, 1)["folderId"]).toBe("folder-b");
  });
});

// ---------------------------------------------------------------------------
// The email template kind
// ---------------------------------------------------------------------------

describe("the email template kind", () => {
  const reply = { compositionId: "c1" } as unknown as Row;

  it("is carried to the Materializer with the format it needs", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn, { intent: setupIntent("i1", { outputKind: "email_template" }) });
    await waitFor(() => expect(kindOfFile()).toContain("Data (JSON)"));
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)).toMatchObject({ format: "json", outputKind: "email_template" });
  });

  it("is dropped when the person chooses a kind of file that cannot be a template", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn, { intent: setupIntent("i1", { outputKind: "email_template" }) });
    await waitFor(() => expect(kindOfFile()).toContain("Data (JSON)"));
    fireEvent.click(screen.getByLabelText("Kind of file"));
    fireEvent.click(await screen.findByRole("option", { name: "Plain text" }));
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)).toMatchObject({ format: "txt" });
    expect(sent(conn)).not.toHaveProperty("outputKind");
  });
});

// ---------------------------------------------------------------------------
// Creating through the SDK helper: one request id per ask
// ---------------------------------------------------------------------------

describe("creating a file", () => {
  const reply = { compositionId: "c1" } as unknown as Row;

  it("carries a request id the Library will accept as the root of its own", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(String(sent(conn)["requestId"])).toMatch(REQUEST_ID);
  });

  it("asks again with the same request id after a refusal, so a lost reply cannot make two files", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    conn.query.composeMaterialize.mockRejectedValueOnce(new Error("the cluster dropped the connection"));
    mount(conn);
    await materializeNotes();
    expect(await screen.findByText(/dropped the connection/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    expect(String(sent(conn, 0)["requestId"])).toMatch(REQUEST_ID);
    expect(sent(conn, 1)["requestId"]).toBe(sent(conn, 0)["requestId"]);
  });

  it("asks with a new request id once the facts have changed: that is a different ask", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    conn.query.composeMaterialize.mockRejectedValueOnce(refused("composeMaterialize", "refused"));
    mount(conn);
    await materializeNotes();
    expect(await screen.findByText(/refused/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("What do you want made?"), { target: { value: "A different brief" } });
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    expect(sent(conn, 1)["requestId"]).not.toBe(sent(conn, 0)["requestId"]);
  });

  it("asks with a new request id for the next file, once this one has a composition", async () => {
    const conn = fakeConnection({ materializeReply: reply, compositions: [] });
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, compositionRow({ id: "c1", status: "ready", format: "markdown" }), "NODE_CREATED");
    fireEvent.click(await screen.findByRole("button", { name: "Compose another" }));
    // The very same facts: only a fresh id tells the cluster this is another file.
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    expect(sent(conn, 1)["requestId"]).not.toBe(sent(conn, 0)["requestId"]);
  });

  it("starts one file when asked twice before the first reply", async () => {
    const conn = fakeConnection();
    let answer!: () => void;
    conn.query.composeMaterialize.mockImplementationOnce(
      () => new Promise((resolve) => { answer = () => resolve(rowsResult([{ compositionId: "c1" } as unknown as Row])); }),
    );
    h.connection = conn;
    const { result } = renderHook(() => useMaterialize());
    const facts = {
      name: "Notes", statement: "A brief", format: "markdown", sources: [], templateId: "",
      folderId: "", accountIds: ["client-a"], deployableKind: "", outputKind: "",
    };
    let first!: Promise<string>;
    let second!: Promise<string>;
    act(() => {
      first = result.current.materialize(facts);
      second = result.current.materialize(facts);
    });
    answer();
    await expect(Promise.all([first, second])).resolves.toEqual(["c1", "c1"]);
    expect(conn.query.composeMaterialize).toHaveBeenCalledOnce();
  });

  it("asks with a new request id after the form is reset, because a reset abandons the ask", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    conn.query.composeMaterialize.mockRejectedValueOnce(refused("composeMaterialize", "refused"));
    h.connection = conn;
    const { result } = renderHook(() => useMaterialize());
    const facts = {
      name: "Notes", statement: "A brief", format: "markdown", sources: [], templateId: "",
      folderId: "", accountIds: ["client-a"], deployableKind: "", outputKind: "",
    };
    await act(async () => { await result.current.materialize(facts); });
    act(() => result.current.reset());
    await act(async () => { await result.current.materialize(facts); });
    expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2);
    expect(sent(conn, 1)["requestId"]).not.toBe(sent(conn, 0)["requestId"]);
  });

  it("does not claim a composition the cluster did not confirm", async () => {
    const conn = fakeConnection({ materializeReply: {} as unknown as Row });
    mount(conn);
    await materializeNotes();
    expect(await screen.findByText(/did not confirm a composition/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The name, checked as it is typed, and a refusal, as a person reads it
// ---------------------------------------------------------------------------
//
// The composer checks a name with the SDK's fileNameProblem: the rule and the
// words VS Code's New Markdown uses, so the two clients say the same thing about
// the same name. And whatever the cluster refuses arrives framed (the call's
// name, the engine's frame); the person reads the sentence alone.

describe("a name and a refusal, as a person reads them", () => {
  const reply = { compositionId: "c1" } as unknown as Row;
  const problem = () => document.querySelector(".os-mz-name-problem")?.textContent ?? "";
  const typeName = (value: string) => fireEvent.change(screen.getByLabelText("Name"), { target: { value } });

  it("checks a name as it is typed, in the editor's words, and sends nothing until it can be used", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn);
    await materializeNotes({ name: "Notes 10/11" });
    expect(problem()).toBe("Enter a file name, without a folder path.");
    expect(conn.query.composeMaterialize).not.toHaveBeenCalled();
    typeName("Notes");
    expect(problem()).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Materialize" }));
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)["name"]).toBe("Notes");
  });

  it("says nothing about a name before one is typed, and asks for one when Materialize is pressed without it", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn);
    fireEvent.change(screen.getByLabelText("What do you want made?"), { target: { value: "A one page brief" } });
    await chooseOrganization();
    expect(problem()).toBe("");
    fireEvent.click(await screen.findByRole("button", { name: "Materialize" }));
    expect(problem()).toBe("Enter a file name.");
    expect(conn.query.composeMaterialize).not.toHaveBeenCalled();
  });

  it("refuses an extension of the chosen kind of file with nothing before it, and only that kind's", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    mount(conn, { intent: setupIntent("i1", { format: "csv" }) });
    await waitFor(() => expect(kindOfFile()).toContain("Spreadsheet (CSV)"));
    typeName(".CSV");
    expect(problem()).toBe("Enter a name before the extension.");
    // ".md" is not a spreadsheet's extension: it is the name.
    typeName(".md");
    expect(problem()).toBe("");
    typeName("x".repeat(151));
    expect(problem()).toBe("Use a name up to 150 characters.");
  });

  it("shows the cluster's refusal of a file as its sentence alone, not the frames it arrived in", async () => {
    const conn = fakeConnection({ materializeReply: reply });
    conn.query.composeMaterialize.mockRejectedValueOnce(refused("composeMaterialize", "Enter a name without slashes."));
    mount(conn);
    await materializeNotes();
    expect(await shown("Enter a name without slashes.")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/composeMaterialize|MemQL engine|Details:/);
  });

  it("shows the cluster's refusal of a stop the same way, a Go package name taken off as well", async () => {
    const conn = fakeConnection({ compositions: [compositionRow({ id: `${COMPOSITION}:c9`, status: "composing", outputFileId: "" })] });
    conn.query.composeCancel.mockRejectedValueOnce(refused("composeCancel", "compose: no composition with that id is readable by you"));
    mount(conn, { intent: { id: "open", payload: { compositionId: `${COMPOSITION}:c9` } } });
    fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
    expect(await shown("no composition with that id is readable by you")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/composeCancel|MemQL engine|Details:|compose:/);
  });
});

// ---------------------------------------------------------------------------
// A described Markdown file: the OS hands the description on once the file is ready
// ---------------------------------------------------------------------------

describe("a Markdown file asked for by describing it", () => {
  const ARTIFACT = { id: "artifact-1", sourceConceptRef: "v1:library:file:f-new", title: "Notes.md" } as unknown as Row;
  // As the cluster writes a described file: filed blank, so no model was called and nothing is embedded.
  const ready = (over: Record<string, unknown> = {}) =>
    compositionRow({
      id: "c-new", name: "Notes", status: "ready", format: "markdown", outputFileId: "f-new", sources: [], statement: "A one page brief",
      modelsUsed: [], provenanceEmbedded: false, provenanceNote: "Created blank. Its history is kept on its composition, not in the file.", ...over,
    });
  const seed = (over: Record<string, unknown> = {}) => ({
    materializeReply: { compositionId: "c-new" } as unknown as Row,
    compositions: [] as Row[],
    outputArtifact: ARTIFACT,
    ...over,
  });
  const requestId = (conn: Conn) => String(sent(conn)["requestId"]);

  it("posts the description and asks for the document once, as the person, when the file is ready", async () => {
    const conn = fakeConnection(seed());
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    // Not before the file exists.
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();

    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());

    expect(conn.query.libraryAddDocumentComment).toHaveBeenCalledOnce();
    expect(conn.query.libraryAddDocumentComment.mock.calls[0]?.[0]).toMatchObject({
      artifactId: "artifact-1",
      body: "A one page brief",
      requestId: `${requestId(conn)}-comment`,
      attachments: [],
    });
    expect(conn.query.libraryRequestDocumentRevision.mock.calls[0]?.[0]).toMatchObject({
      artifactId: "artifact-1",
      commentIds: ["comment-1"],
      requestId: `${requestId(conn)}-revision`,
    });
    // Nothing to say: it went through.
    expect(screen.queryByText(/description was not sent/)).toBeNull();
    expect(screen.queryByText(/wasn't attached/)).toBeNull();
  });

  it("waits for the composition to be ready, not merely to name a file", async () => {
    const conn = fakeConnection(seed());
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready({ status: "rendering" }), "NODE_UPDATED");
    await screen.findByText("Writing the file");
    await act(async () => {});
    expect(conn.query.libraryArtifactForFile).not.toHaveBeenCalled();
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());
  });

  it("hands it over once however often the composition is heard from again", async () => {
    const conn = fakeConnection(seed());
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready({ name: "Notes, renamed" }), "NODE_UPDATED");
    conn.subscriptions.emit(Concepts.LIBRARY_ARTIFACT, ARTIFACT, "NODE_UPDATED");
    await screen.findByText("Notes, renamed");
    await act(async () => {});
    expect(conn.query.libraryAddDocumentComment).toHaveBeenCalledOnce();
    expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce();
  });

  it("waits for the Files entry on the live feed and does not keep asking for it", async () => {
    const conn = fakeConnection(seed({ outputArtifact: undefined }));
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());

    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await waitFor(() => expect(conn.query.libraryArtifactForFile).toHaveBeenCalledWith({ fileId: "f-new" }, expect.anything()));
    const reads = conn.query.libraryArtifactForFile.mock.calls.length;
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
    expect(conn.query.libraryArtifactForFile.mock.calls.length).toBe(reads);

    // The index catches up and says so.
    conn.subscriptions.emit(Concepts.LIBRARY_ARTIFACT, ARTIFACT, "NODE_CREATED");
    await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());
    expect(conn.query.libraryAddDocumentComment.mock.calls[0]?.[0]).toMatchObject({ artifactId: "artifact-1" });
  });

  it("ignores the Files entry of some other file while it waits", async () => {
    const conn = fakeConnection(seed({ outputArtifact: undefined }));
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await waitFor(() => expect(conn.query.libraryArtifactForFile).toHaveBeenCalled());
    conn.subscriptions.emit(Concepts.LIBRARY_ARTIFACT, { id: "artifact-other", sourceConceptRef: "v1:library:file:f-other" } as unknown as Row, "NODE_CREATED");
    await act(async () => {});
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
  });

  it("says so, beside the file, when the description could not be sent", async () => {
    const conn = fakeConnection(seed());
    conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(refused("libraryRequestDocumentRevision", "revision budget exhausted"));
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");

    expect(await shown("The file was created, but the description was not sent for writing.")).toBeTruthy();
    // The reason is the cluster's own words, in the data voice, and only those: the notice's own
    // sentence already says the description was not sent, so the SDK's lead-in saying so again is gone.
    expect(document.querySelector(".os-notice-detail")?.textContent).toBe("revision budget exhausted");
    // And the file is still there to open.
    expect(screen.getByRole("button", { name: "Open Notes" })).toBeTruthy();
  });

  // WHAT THE FILE IS, under what was asked for. A described file is empty until the
  // person approves the Library's draft, so "the composed text is in the file" would
  // be false; the caption follows what this window knows of the hand-off.
  describe("the caption says what the file is", () => {
    const caption = () => document.querySelector(".os-mz-col-brief .os-caption")?.textContent ?? "";
    const NOT_COMPOSED = /composed text/;

    it("says the description was sent, once this window has sent it", async () => {
      const conn = fakeConnection(seed());
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      await waitFor(() => expect(caption()).toBe("Your description was sent to be written. Open the file to review and approve the draft."));
      expect(document.body.textContent).not.toMatch(NOT_COMPOSED);
    });

    it("leaves it to the failure notice when this window could not send it", async () => {
      const conn = fakeConnection(seed());
      conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(refused("libraryRequestDocumentRevision", "revision budget exhausted"));
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      await shown("The file was created, but the description was not sent for writing.");
      expect(caption()).toBe("");
      expect(document.body.textContent).not.toMatch(NOT_COMPOSED);
    });

    it("says only that it was created from a description when it is opened later, from the list", async () => {
      const conn = fakeConnection(seed({ compositions: [ready({ id: "c-elsewhere" })] }));
      mount(conn, { intent: { id: "open", payload: { compositionId: "c-elsewhere" } } });
      await screen.findByRole("button", { name: "Open Notes" });
      expect(caption()).toBe("This file was created from a description. Open it to continue.");
      expect(document.body.textContent).not.toMatch(NOT_COMPOSED);
    });

    it("says the same while this window has not heard back from the Library", async () => {
      const conn = fakeConnection(seed({ outputArtifact: undefined }));
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      // Ready, but its Files entry has not been written, so nothing has been sent yet.
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      await screen.findByRole("button", { name: "Open Notes" });
      expect(caption()).toBe("This file was created from a description. Open it to continue.");
      expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
    });
  });

  it("names each reference that went without being attached, on its own", async () => {
    const conn = fakeConnection(
      seed({
        artifactsByFile: { "f-spec": { id: "a-spec" } as unknown as Row, "f-sheet": { id: "a-sheet" } as unknown as Row },
        filesById: {
          "f-spec": { id: "f-spec", name: "spec.pdf", size: 900, versionNumber: 1 } as unknown as Row,
          "f-sheet": { id: "f-sheet", name: "numbers.csv", size: 300, versionNumber: 1 } as unknown as Row,
        },
      }),
    );
    mount(conn, {
      intent: setupIntent("i1", {
        format: "markdown",
        sources: [
          { kind: "library_file", ref: "f-spec", label: "spec.pdf" },
          { kind: "library_file", ref: "f-sheet", label: "numbers.csv" },
        ],
      }),
    });
    await screen.findByRole("list", { name: "Picked sources" });
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await waitFor(() => expect(document.querySelectorAll(".os-notice-line")).toHaveLength(2));
    const notices = [...document.querySelectorAll(".os-notice")].map((notice) => [
      notice.querySelector(".os-notice-line")?.textContent,
      notice.querySelector(".os-notice-detail")?.textContent,
    ]);
    expect(notices).toEqual([
      ["This reference wasn't attached.", "spec.pdf"],
      ["This reference wasn't attached.", "numbers.csv"],
    ]);
  });

  // What arrives a moment after "Ready" is not something the person did, so it
  // is announced. The announcement is its own element -- visually hidden, polite,
  // and mounted before there is anything to say, because a live region that
  // appears together with its text is not reliably announced -- and NOT a wrapper
  // around the visible notices: Safari and VoiceOver drop the semantics of an
  // element laid out with `display: contents`, which is how this region used to
  // be hidden from the grid.
  describe("what arrives after Ready is announced", () => {
    const announcer = () => document.querySelector(".os-mz-announce");

    it("in a visually hidden live region of its own, mounted before there is anything to say", async () => {
      const conn = fakeConnection(seed());
      conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(refused("libraryRequestDocumentRevision", "revision budget exhausted"));
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      const region = announcer();
      expect(region, "the live region is mounted before any notice").not.toBeNull();
      expect(region?.getAttribute("role")).toBe("status");
      expect(region?.getAttribute("aria-live")).toBe("polite");
      expect(region?.classList.contains("os-sr-only"), "it is visually hidden, not display: none").toBe(true);
      expect(region?.textContent).toBe("");

      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      const notice = await shown("The file was created, but the description was not sent for writing.");
      // The same element took the words: a new one appearing with its text would not be announced.
      expect(announcer()).toBe(region);
      expect(region?.textContent).toContain("The file was created, but the description was not sent for writing.");
      expect(region?.textContent).toContain("Open the file and choose Describe document to send it again.");
      expect(region?.textContent).toContain("revision budget exhausted");
      // Each part is spoken as a sentence. The cluster's reason has no full stop of its own, and
      // without one a screen reader runs it into whatever comes next.
      expect(region?.textContent).toMatch(/send it again\. revision budget exhausted\.$/);
      expect(region?.textContent).not.toMatch(/could not be sent for writing/);
      // The visible notice is a notice, not a live region and not inside one.
      expect(region?.contains(notice)).toBe(false);
      expect(notice.closest("[role=status]")).toBeNull();
    });

    it("without wrapping the visible notices in a status region", async () => {
      const conn = fakeConnection(seed());
      conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(refused("libraryRequestDocumentRevision", "revision budget exhausted"));
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      const line = await shown("The file was created, but the description was not sent for writing.");
      // The old region was a `display: contents` wrapper with role="status"; neither it nor
      // anything like it holds the notices now: they are rows of the composer's own grid.
      expect(document.querySelector(".os-mz-status")).toBeNull();
      const notice = line.closest(".os-notice");
      expect(notice?.closest("[role=status]")).toBeNull();
      expect(notice?.parentElement?.classList.contains("os-mz")).toBe(true);
    });

    it("including the references that could not be attached, which arrive instead of a failure", async () => {
      const conn = fakeConnection(
        seed({
          artifactsByFile: { "f-notes": { id: "a-notes" } as unknown as Row, "f-spec": { id: "a-spec" } as unknown as Row },
          filesById: {
            "f-notes": { id: "f-notes", name: "notes.md", size: 120, versionNumber: 2 } as unknown as Row,
            "f-spec": { id: "f-spec", name: "spec.pdf", size: 900, versionNumber: 1 } as unknown as Row,
          },
        }),
      );
      mount(conn, {
        intent: setupIntent("i1", {
          format: "markdown",
          sources: [
            { kind: "library_file", ref: "f-notes", label: "notes.md" },
            { kind: "library_file", ref: "f-spec", label: "spec.pdf" },
          ],
        }),
      });
      await screen.findByRole("list", { name: "Picked sources" });
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      const region = announcer();
      expect(region?.textContent).toBe("");
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      await shown("This reference wasn't attached.");
      expect(announcer()).toBe(region);
      expect(region?.textContent).toBe("This reference wasn't attached. spec.pdf.");
    });

    it("and goes quiet again when the file is left", async () => {
      const conn = fakeConnection(seed());
      conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(refused("libraryRequestDocumentRevision", "revision budget exhausted"));
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      const region = announcer();
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      await shown("The file was created, but the description was not sent for writing.");
      fireEvent.click(screen.getByRole("button", { name: "Compose another" }));
      await waitFor(() => expect(screen.queryByText(/not sent for writing/)).toBeNull());
      expect(announcer()).toBe(region);
      expect(region?.textContent).toBe("");
    });

    it("saying nothing when there is nothing to say", async () => {
      const conn = fakeConnection(seed());
      mount(conn);
      await materializeNotes();
      await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
      conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
      await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());
      await act(async () => {});
      expect(announcer()?.textContent).toBe("");
    });
  });

  it("names the references that could not be attached, after opening the revision with the ones that could", async () => {
    const conn = fakeConnection(
      seed({
        artifactsByFile: { "f-notes": { id: "a-notes" } as unknown as Row, "f-spec": { id: "a-spec" } as unknown as Row },
        filesById: {
          "f-notes": { id: "f-notes", name: "notes.md", size: 120, versionNumber: 2 } as unknown as Row,
          "f-spec": { id: "f-spec", name: "spec.pdf", size: 900, versionNumber: 1 } as unknown as Row,
        },
      }),
    );
    // The new file itself has no entry in `artifactsByFile`: it falls back to ARTIFACT.
    mount(conn, {
      intent: setupIntent("i1", {
        format: "markdown",
        sources: [
          { kind: "library_file", ref: "f-notes", label: "notes.md" },
          { kind: "library_file", ref: "f-spec", label: "spec.pdf" },
        ],
      }),
    });
    await screen.findByRole("list", { name: "Picked sources" });
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");

    // Each reference left off is named on its own, by the label the person gave it. The SDK does not say
    // why one was left off (its kind, its size, the Library's limits, not being found), so nor does this.
    const notice = (await shown("This reference wasn't attached.")).closest(".os-notice");
    expect(notice?.querySelector(".os-notice-detail")?.textContent).toBe("spec.pdf");
    expect(document.querySelectorAll(".os-notice-line")).toHaveLength(1);
    expect(document.body.textContent).not.toMatch(/Only Markdown and images/);
    expect(conn.query.libraryAddDocumentComment.mock.calls[0]?.[0]).toMatchObject({
      attachments: [{ artifactId: "a-notes", version: 2, revision: "file:2" }],
    });
    await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());
    expect(screen.queryByText(/description was not sent/)).toBeNull();
  });

  it("shows what happened to this file's description only on this file", async () => {
    const conn = fakeConnection(seed({ compositions: [ready({ id: "c-other", name: "Another file", outputFileId: "f-other" })] }));
    conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(refused("libraryRequestDocumentRevision", "revision budget exhausted"));
    const { deliver } = mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await shown("The file was created, but the description was not sent for writing.");
    deliver({ id: "open-other", payload: { compositionId: "c-other" } });
    expect(await screen.findByRole("button", { name: "Open Another file" })).toBeTruthy();
    expect(screen.queryByText(/description was not sent/)).toBeNull();
  });

  it("does not hand on the request for a spreadsheet: only Markdown is written from a description", async () => {
    const conn = fakeConnection(seed());
    mount(conn, { intent: setupIntent("i1", { format: "csv" }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready({ format: "csv" }), "NODE_UPDATED");
    await screen.findByText("Ready");
    await act(async () => {});
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
    expect(conn.query.libraryRequestDocumentRevision).not.toHaveBeenCalled();
  });

  it("does not hand on a Markdown request that names a template: the template decides what is written", async () => {
    const conn = fakeConnection(seed({ templates: [templateRow({ id: "t1", name: "House style", format: "markdown" })] }));
    mount(conn);
    fireEvent.click(await screen.findByLabelText("Template"));
    fireEvent.click(await screen.findByRole("option", { name: "House style" }));
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)).toMatchObject({ templateId: "t1", format: "markdown" });
    conn.subscriptions.emit(COMPOSITION, ready({ templateId: "t1" }), "NODE_UPDATED");
    await screen.findByText("Ready");
    await act(async () => {});
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
    expect(conn.query.libraryRequestDocumentRevision).not.toHaveBeenCalled();
  });

  it("does not hand on a Markdown request that names records to write from: the composer reads them, the revision workflow cannot", async () => {
    const conn = fakeConnection(seed());
    mount(conn, {
      intent: setupIntent("i1", {
        format: "markdown",
        sources: [{ kind: "query", ref: "query openInvoices()", label: "Open invoices" }],
      }),
    });
    await screen.findByRole("list", { name: "Picked sources" });
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    // It is sent as it was asked for: the statement AND the records. The cluster keeps the composer for it.
    expect(sent(conn)).toMatchObject({
      format: "markdown",
      statement: "A one page brief",
      sources: [{ kind: "query", ref: "query openInvoices()", label: "Open invoices" }],
    });
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await screen.findByText("Ready");
    await act(async () => {});
    expect(conn.query.libraryArtifactForFile).not.toHaveBeenCalled();
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
    expect(conn.query.libraryRequestDocumentRevision).not.toHaveBeenCalled();
  });

  it("does not hand on a composition this window did not start", async () => {
    const conn = fakeConnection(seed({ compositions: [ready({ id: "c-elsewhere" })] }));
    mount(conn, { intent: { id: "open", payload: { compositionId: "c-elsewhere" } } });
    await screen.findByRole("button", { name: "Open Notes" });
    await act(async () => {});
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
  });

  it("does not hand on a file that never became one", async () => {
    const conn = fakeConnection(seed());
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready({ status: "failed", outputFileId: "", failureReason: "No budget left" }), "NODE_UPDATED");
    expect((await screen.findAllByText(/No budget left/)).length).toBeGreaterThan(0);
    await act(async () => {});
    expect(conn.query.libraryArtifactForFile).not.toHaveBeenCalled();
    expect(conn.query.libraryAddDocumentComment).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Back to the app that sent the person
// ---------------------------------------------------------------------------

describe("returning to the app that sent the person", () => {
  const returnTo = { appId: "files", sectionId: "browse" };
  const finished = (over: Record<string, unknown> = {}) =>
    compositionRow({ id: "c1", name: "Notes", status: "ready", format: "csv", sources: [], ...over });
  const create = async (conn: Conn, setup: Parameters<typeof composeIntent>[0] = { format: "csv", returnTo }) => {
    mount(conn, { intent: setupIntent("i1", setup) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, finished(), "NODE_UPDATED");
  };
  const seed = () => fakeConnection({ materializeReply: { compositionId: "c1" } as unknown as Row, compositions: [] });

  it("offers Back to the sender beside Open, and goes there", async () => {
    h.os = shell([FILES]);
    await create(seed());
    const back = await screen.findByRole("button", { name: "Back to Files" });
    expect(screen.getByRole("button", { name: "Open Notes" })).toBeTruthy();
    fireEvent.click(back);
    expect(h.openApp).toHaveBeenCalledExactlyOnceWith("files", "browse");
  });

  it("keeps the bar to three acts, with the return in front of the primary one", async () => {
    h.os = shell([FILES]);
    await create(seed());
    await screen.findByRole("button", { name: "Back to Files" });
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getAllByRole("button").map((button) => button.textContent)).toEqual(["Save as recipe", "Back to Files", "Open the file"]);
  });

  it("offers no way back when the setup named none", async () => {
    h.os = shell([FILES]);
    await create(seed(), { format: "csv" });
    await screen.findByRole("button", { name: "Open Notes" });
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("offers no way back to an app the shell does not hold, rather than a control that does nothing", async () => {
    h.os = shell([]);
    await create(seed());
    await screen.findByRole("button", { name: "Open Notes" });
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("offers no way back to an app this person is not admitted to, though the shell holds it", async () => {
    // `openApp` refuses it, silently: the app is in the registry and the
    // person's capabilities do not open it.
    h.os = shell([{ ...FILES, requires: "app:never-granted" }]);
    await create(seed());
    await screen.findByRole("button", { name: "Open Notes" });
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("offers no way back where there is no shell to go back in", async () => {
    h.os = null;
    await create(seed());
    await screen.findByRole("button", { name: "Open Notes" });
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("belongs to the file this window made from that setup, and to no other the person opens afterwards", async () => {
    h.os = shell([FILES]);
    const conn = fakeConnection({
      materializeReply: { compositionId: "c1" } as unknown as Row,
      compositions: [finished({ id: "c-old", name: "Older file" })],
    });
    const { deliver } = mount(conn, { intent: setupIntent("i1", { format: "csv", returnTo }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, finished(), "NODE_UPDATED");
    await screen.findByRole("button", { name: "Back to Files" });
    deliver({ id: "open-old", payload: { compositionId: "c-old" } });
    expect(await screen.findByRole("button", { name: "Open Older file" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("is not carried to a second file made in the same window", async () => {
    h.os = shell([FILES]);
    const conn = seed();
    mount(conn, { intent: setupIntent("i1", { format: "csv", returnTo }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, finished(), "NODE_UPDATED");
    await screen.findByRole("button", { name: "Back to Files" });
    fireEvent.click(screen.getByRole("button", { name: "Compose another" }));
    conn.query.composeMaterialize.mockResolvedValueOnce(rowsResult([{ compositionId: "c2" } as unknown as Row]));
    await materializeNotes({ name: "Second" });
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledTimes(2));
    conn.subscriptions.emit(COMPOSITION, finished({ id: "c2", name: "Second" }), "NODE_UPDATED");
    await screen.findByRole("button", { name: "Open Second" });
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
  });

  it("ends when the person starts another file, because that is no longer the trip that began elsewhere", async () => {
    h.os = shell([FILES]);
    const conn = seed();
    const { deliver } = mount(conn, { intent: setupIntent("i1", { format: "csv", returnTo }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, finished(), "NODE_UPDATED");
    await screen.findByRole("button", { name: "Back to Files" });
    fireEvent.click(screen.getByRole("button", { name: "Compose another" }));
    await screen.findByLabelText("Kind of file");
    // Coming back to that same file later is not coming back from Files.
    deliver({ id: "reopen", payload: { compositionId: "c1" } });
    expect(await screen.findByRole("button", { name: "Open Notes" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Back to/ })).toBeNull();
    // Archive gave way to the way back only while the trip was live; it is
    // offered again now that it is over.
    expect(screen.getByRole("button", { name: "Archive" })).toBeTruthy();
  });
});
