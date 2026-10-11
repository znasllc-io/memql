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
const { compositionRow, fakeConnection, memoryStore, rowsResult, templateRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const COMPOSITION = "v1:compose:composition";
const REQUEST_ID = /^[A-Za-z0-9_-]{8,111}$/;

beforeEach(() => {
  h.connection = null;
  h.os = null;
  h.openApp.mockReset();
});

interface Mounted {
  navigate: ReturnType<typeof vi.fn>;
  consumeIntent: ReturnType<typeof vi.fn>;
  /** Deliver a new standing instruction, as the shell does to a window that is already open. */
  deliver: (intent: { id: string; payload: Record<string, unknown> }) => void;
}

function mount(conn: Conn, opts: { sectionId?: string; intent?: { id: string; payload: Record<string, unknown> } } = {}): Mounted {
  h.connection = conn;
  const navigate = vi.fn();
  const consumeIntent = vi.fn();
  const store = memoryStore();
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
const kindOfFile = () => screen.getByLabelText("Kind of file").textContent ?? "";

// ---------------------------------------------------------------------------
// The starting setup: another app opens the composer already pointed somewhere
// ---------------------------------------------------------------------------

describe("a starting setup opens the composer pre-filled", () => {
  it("takes the intent and moves the window to the composer", async () => {
    const conn = fakeConnection();
    const { navigate, consumeIntent } = mount(conn, { sectionId: "templates", intent: setupIntent("i1", { format: "csv" }) });
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
    expect(navigate).toHaveBeenCalledWith("composer");
  });

  it("starts the composer on the kind of file the sender chose, not the one it would have started on", async () => {
    // The app starts on Markdown, so Markdown proves nothing: a sender that asks
    // for anything ELSE is the only way to see the setup was applied.
    mount(fakeConnection(), { intent: setupIntent("i1", { format: "csv" }) });
    await waitFor(() => expect(kindOfFile()).toContain("Spreadsheet (CSV)"));
  });

  it("starts on the person's own default when the sender chose no format", async () => {
    mount(fakeConnection(), { intent: setupIntent("i1", { folderId: "folder-1" }) });
    await screen.findByLabelText("Kind of file");
    expect(kindOfFile()).toContain("Markdown");
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
// The folder: carried only when the person can read it
// ---------------------------------------------------------------------------

describe("the folder a setup names", () => {
  const folder = (over: Record<string, unknown> = {}) => ({ id: "folder-1", name: "Reports", archived: false, ownerUserId: "v1:identity:user:me", ...over }) as unknown as Row;
  const create = async (conn: Conn, folderId: string) => {
    const { consumeIntent } = mount(conn, { intent: setupIntent("i1", { format: "markdown", folderId }) });
    await screen.findByLabelText("Kind of file");
    // However the folder fares, the setup itself is taken and the form pre-filled.
    await waitFor(() => expect(consumeIntent).toHaveBeenCalledWith("i1"));
    expect(kindOfFile()).toContain("Markdown");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
  };
  const reply = { compositionId: "c1" } as unknown as Row;

  it("files into a folder the person can read", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder()] });
    await create(conn, "folder-1");
    expect(sent(conn)["folderId"]).toBe("folder-1");
    expect(conn.query.libraryFolderById).toHaveBeenCalledWith({ folderId: "folder-1" });
  });

  it("drops a folder that is not there, so the server is never asked to file somewhere unknown", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder()] });
    await create(conn, "missing");
    expect(sent(conn)).not.toHaveProperty("folderId");
  });

  it("drops a folder that has been archived", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder({ archived: true })] });
    await create(conn, "folder-1");
    expect(sent(conn)).not.toHaveProperty("folderId");
  });

  it("drops a folder it could not check, rather than filing into one nobody confirmed", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder()] });
    conn.query.libraryFolderById.mockRejectedValueOnce(new Error("denied"));
    await create(conn, "folder-1");
    expect(sent(conn)).not.toHaveProperty("folderId");
  });

  it("makes no folder read when the setup names no folder", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder()] });
    mount(conn, { intent: setupIntent("i1", { format: "markdown" }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(conn.query.libraryFolderById).not.toHaveBeenCalled();
    expect(sent(conn)).not.toHaveProperty("folderId");
  });

  it("sends nothing until the folder is checked, so a quick press never files at the top by accident", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder()] });
    let answer!: () => void;
    conn.query.libraryFolderById.mockImplementationOnce(
      () => new Promise((resolve) => { answer = () => resolve(rowsResult([folder()])); }),
    );
    mount(conn, { intent: setupIntent("i1", { folderId: "folder-1" }) });
    await screen.findByLabelText("Kind of file");
    await materializeNotes();
    await waitFor(() => expect(conn.query.libraryFolderById).toHaveBeenCalledOnce());
    expect(conn.query.composeMaterialize).not.toHaveBeenCalled();
    answer();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    expect(sent(conn)["folderId"]).toBe("folder-1");
  });

  it("is the first file's folder only: a second composition in the window files where the person files", async () => {
    const conn = fakeConnection({ materializeReply: reply, folders: [folder()], compositions: [] });
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
    conn.query.composeMaterialize.mockRejectedValueOnce(new Error("refused"));
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
    conn.query.composeMaterialize.mockRejectedValueOnce(new Error("refused"));
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
// A described Markdown file: the OS hands the description on once the file is ready
// ---------------------------------------------------------------------------

describe("a Markdown file asked for by describing it", () => {
  const ARTIFACT = { id: "artifact-1", sourceConceptRef: "v1:library:file:f-new", title: "Notes.md" } as unknown as Row;
  const ready = (over: Record<string, unknown> = {}) =>
    compositionRow({ id: "c-new", name: "Notes", status: "ready", format: "markdown", outputFileId: "f-new", sources: [], statement: "A one page brief", ...over });
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
    expect(screen.queryByText(/Not attached/)).toBeNull();
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
    conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(new Error("revision budget exhausted"));
    mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");

    expect(await screen.findByText("The file was created, but the description was not sent for writing.")).toBeTruthy();
    // The reason is the cluster's own words, in the data voice.
    expect(screen.getByText(/revision budget exhausted/)).toBeTruthy();
    // And the file is still there to open.
    expect(screen.getByRole("button", { name: "Open Notes" })).toBeTruthy();
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

    expect(await screen.findByText("Not attached: spec.pdf. Only Markdown and images can be attached.")).toBeTruthy();
    expect(conn.query.libraryAddDocumentComment.mock.calls[0]?.[0]).toMatchObject({
      attachments: [{ artifactId: "a-notes", version: 2, revision: "file:2" }],
    });
    await waitFor(() => expect(conn.query.libraryRequestDocumentRevision).toHaveBeenCalledOnce());
    expect(screen.queryByText(/description was not sent/)).toBeNull();
  });

  it("shows what happened to this file's description only on this file", async () => {
    const conn = fakeConnection(seed({ compositions: [ready({ id: "c-other", name: "Another file", outputFileId: "f-other" })] }));
    conn.query.libraryRequestDocumentRevision.mockRejectedValueOnce(new Error("revision budget exhausted"));
    const { deliver } = mount(conn);
    await materializeNotes();
    await waitFor(() => expect(conn.query.composeMaterialize).toHaveBeenCalledOnce());
    conn.subscriptions.emit(COMPOSITION, ready(), "NODE_UPDATED");
    await screen.findByText("The file was created, but the description was not sent for writing.");
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
  });
});
