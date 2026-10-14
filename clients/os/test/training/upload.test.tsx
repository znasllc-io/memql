import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { TrainingApp } from "../../src/apps/training/TrainingApp";
import { LocalTrainingSettingsStore } from "../../src/apps/training/settings";
import { FILE_CONCEPT } from "../../src/apps/training/concepts";
import type { UploadHandle, UploadProvider, UploadResult } from "../../src/items/upload";
import {
  FAILED_FILE,
  READING_FILE,
  READING_RUN,
  READY_FILE,
  READY_RUN,
  TRAINED_FILE,
  UNREADABLE_FILE,
  UNREADABLE_RUN,
  click,
  chunkRow,
  domainLiteRow,
  domainRow,
  emit,
  fakeConnection,
  fileRow,
  runRow,
  settle,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// The Teach section, through the real LiveCollection.
//
// What this file is FOR: the surface is a worklist, so every test below is
// about a row saying where its file is and offering exactly the act that is
// legal from there.

function memStore() {
  const data = new Map<string, string>();
  return new LocalTrainingSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

/** An upload provider the test drives by hand. */
function fakeUploads() {
  const calls: File[] = [];
  let settleNext: ((result: UploadResult) => void) | null = null;
  let failNext: ((err: Error) => void) | null = null;
  let report: ((p: { sentBytes: number; totalBytes: number }) => void) | null = null;
  const provider: UploadProvider = {
    upload(file): UploadHandle {
      calls.push(file);
      const done = new Promise<UploadResult>((resolve, reject) => {
        settleNext = resolve;
        failNext = reject;
      });
      return {
        done,
        abort: () => failNext?.(new Error("aborted")),
        onProgress: (listener) => {
          report = listener;
          return () => void (report = null);
        },
      };
    },
  };
  return {
    provider,
    calls,
    land: (fileId = "file-new") =>
      settleNext?.({
        artifactId: "art-1",
        fileId,
        title: "notes.pdf",
        fileKind: "file",
        source: "uploaded",
      }),
    fail: (message: string) => failNext?.(new Error(message)),
    progress: (sentBytes: number, totalBytes: number) => report?.({ sentBytes, totalBytes }),
  };
}

function mount(
  connection: FakeConnection,
  opts: { section?: string; uploads?: UploadProvider; navigate?: () => void; role?: string } = {},
) {
  h.connection = connection;
  return render(
    withSession(
      <TrainingApp
        sectionId={opts.section ?? "upload"}
        navigate={opts.navigate ?? vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
        {...(opts.uploads ? { uploads: opts.uploads } : {})}
      />,
      { userId: "u-me", role: opts.role },
    ),
  );
}

function rowFor(name: string): HTMLElement {
  const row = screen.getByText(name).closest(".os-livelist-row");
  if (!(row instanceof HTMLElement)) throw new Error(`no row for ${name}`);
  return row;
}

beforeEach(() => {
  h.connection = null;
});

describe("the file list", () => {
  it("reads libraryFilesForOwner and workRunsForOwner, and renders the caller's files", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    mount(connection);
    await settle();

    expect(connection.calls).toContain("query libraryFilesForOwner()");
    expect(connection.calls).toContain("query workRunsForOwner()");
    expect(screen.getByText("handbook.docx")).toBeTruthy();
  });

  // NAMES NO SPACE AND NO PLAN, which is what the re-key was for.
  it("never reads a space or a plan", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    mount(connection);
    await settle();

    for (const call of connection.calls) {
      expect(call).not.toContain("userActiveSpace");
      expect(call).not.toContain("plansForUser");
      expect(call).not.toContain("attachment");
    }
  });

  it("drops a run of another template rather than showing it as a file", async () => {
    const stray = runRow({ id: "r-other", automationName: "somethingElse", input: { fileId: "file-1" } });
    const connection = fakeConnection({ files: [READY_FILE], runs: [stray] });
    mount(connection);
    await settle();

    // The file still renders -- the run is what was filtered, and its
    // outcome is what goes missing, so no passage count appears.
    expect(screen.getByText("handbook.docx")).toBeTruthy();
    expect(screen.queryByText(/passages/)).toBeNull();
  });

  it("says how many passages a file became, which only the run knows", async () => {
    const connection = fakeConnection({
      files: [READY_FILE],
      runs: [runRow({ id: "r-1", input: { fileId: "file-1" }, outcome: { readable: true, chunks: 12, embedded: 12 } })],
    });
    mount(connection);
    await settle();

    expect(within(rowFor("handbook.docx")).getByText(/12 passages/)).toBeTruthy();
  });

  it("says when only some of a file is searchable", async () => {
    const connection = fakeConnection({
      files: [READY_FILE],
      runs: [runRow({ id: "r-1", input: { fileId: "file-1" }, outcome: { readable: true, chunks: 12, embedded: 9 } })],
    });
    mount(connection);
    await settle();

    expect(within(rowFor("handbook.docx")).getByText(/9 searchable/)).toBeTruthy();
  });

  it("pulses on a status transition and marks a new row", async () => {
    const connection = fakeConnection({ files: [READING_FILE], runs: [READING_RUN] });
    mount(connection);
    await settle();
    expect(within(rowFor("contract.pdf")).getByText("reading")).toBeTruthy();

    await emit(connection, FILE_CONCEPT, fileRow({ id: "file-reading", name: "contract.pdf" }));
    expect(within(rowFor("contract.pdf")).getByText("ready to teach")).toBeTruthy();
  });

  it("puts a failure's own sentence on the row", async () => {
    const connection = fakeConnection({ files: [FAILED_FILE] });
    mount(connection);
    await settle();

    const row = rowFor("broken.pdf");
    expect(within(row).getByText("could not be read")).toBeTruthy();
    expect(within(row).getByText(/image-only/)).toBeTruthy();
  });
});

describe("the act each row offers", () => {
  it("offers Teach a domain on a file that has been read", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    mount(connection);
    await settle();

    expect(within(rowFor("handbook.docx")).getByRole("button", { name: "Teach a domain" })).toBeTruthy();
  });

  it("offers Teach another on a file already teaching one", async () => {
    const connection = fakeConnection({ files: [TRAINED_FILE], domainRows: [domainLiteRow("domain-sales", "unvalidated")] });
    mount(connection);
    await settle();

    const row = rowFor("pricing.csv");
    expect(within(row).getByRole("button", { name: "Teach another" })).toBeTruthy();
    expect(within(row).getByText(/Teaching/)).toBeTruthy();
  });

  // AN ACT THAT IS NOT LEGAL IS ABSENT, NEVER DISABLED (the shell's rule 12).
  // There is no act that would make a photograph teach something, so a
  // disabled Teach beside it would be a control whose only purpose is to be
  // refused.
  it("offers NOTHING on a file with no text in it", async () => {
    const connection = fakeConnection({ files: [UNREADABLE_FILE], runs: [UNREADABLE_RUN] });
    mount(connection);
    await settle();

    const row = rowFor("scan.png");
    expect(within(row).getByText("nothing to read")).toBeTruthy();
    expect(within(row).queryByRole("button", { name: /Teach/ })).toBeNull();
  });

  it("offers nothing while the cluster is still reading a file", async () => {
    const connection = fakeConnection({ files: [READING_FILE], runs: [READING_RUN] });
    mount(connection);
    await settle();

    expect(within(rowFor("contract.pdf")).queryByRole("button", { name: /Teach/ })).toBeNull();
  });
});

describe("teaching a domain", () => {
  async function open(connection: FakeConnection) {
    mount(connection);
    await settle();
    await click(within(rowFor("handbook.docx")).getByRole("button", { name: "Teach a domain" }));
  }

  it("calls libraryTrainFile with the file and the chosen domain", async () => {
    const connection = fakeConnection({
      files: [READY_FILE],
      runs: [READY_RUN],
      domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })],
    });
    await open(connection);

    await click(within(rowFor("handbook.docx")).getByRole("combobox", { name: /Knowledge domain/ }));
    await click(screen.getByRole("option", { name: "Sales" }));
    await click(within(rowFor("handbook.docx")).getByRole("button", { name: "Teach" }));
    await settle();

    const trained = connection.callsNamed("libraryTrainFile");
    expect(trained.length).toBe(1);
    expect(trained[0]).toContain(`fileId: "file-1"`);
    expect(trained[0]).toContain(`domainId: "domain-sales"`);
  });

  // A REFUSAL RENDERS ON THE ROW THAT PRODUCED IT, never as a toast: somebody
  // who looked away would have lost the only account of what happened.
  it("renders the server's own sentence on the row when the engine refuses", async () => {
    const connection = fakeConnection({
      files: [READY_FILE],
      runs: [READY_RUN],
      domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })],
      trainError: "not authorized to write to this knowledge domain",
    });
    await open(connection);

    await click(within(rowFor("handbook.docx")).getByRole("combobox", { name: /Knowledge domain/ }));
    await click(screen.getByRole("option", { name: "Sales" }));
    await click(within(rowFor("handbook.docx")).getByRole("button", { name: "Teach" }));
    await settle();

    expect(within(rowFor("handbook.docx")).getByText(/not authorized/)).toBeTruthy();
  });

  it("will not send without a domain chosen", async () => {
    const connection = fakeConnection({
      files: [READY_FILE],
      runs: [READY_RUN],
      domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })],
    });
    await open(connection);

    const teach = within(rowFor("handbook.docx")).getByRole("button", { name: "Teach" });
    expect(teach.hasAttribute("disabled")).toBe(true);
    expect(connection.callsNamed("libraryTrainFile").length).toBe(0);
  });

  it("lets an owner teach the first domain, trimming the exact key and refusing whitespace", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    await open(connection);
    const input = screen.getByRole("textbox", { name: /Knowledge domain/ });
    const teach = screen.getByRole("button", { name: "Teach" });
    fireEvent.change(input, { target: { value: "   " } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(teach).toHaveProperty("disabled", true);
    expect(connection.callsNamed("libraryTrainFile")).toHaveLength(0);
    fireEvent.change(input, { target: { value: "  software-development  " } });
    fireEvent.keyDown(input, { key: "Enter" });
    await settle();
    expect(connection.callsNamed("libraryTrainFile")).toEqual(['builtin libraryTrainFile(fileId: "file-1", domainId: "software-development")']);
    expect(screen.getByText("Teaching recorded. New passages await review.")).toBeTruthy();
    expect(connection.callsNamed("setChunkValidationStatus")).toHaveLength(0);
    expect(connection.callsNamed("createKnowledgeDomain")).toHaveLength(0);
  });

  it("keeps the known-domain picker and lets an owner return to it after entering a domain", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN], domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })] });
    await open(connection);
    expect(screen.getByRole("combobox", { name: /Knowledge domain/ })).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: /Knowledge domain/ })).toBeNull();
    await click(screen.getByRole("button", { name: "Enter a domain" }));
    fireEvent.change(screen.getByRole("textbox", { name: /Knowledge domain/ }), { target: { value: "unsubmitted" } });
    await click(screen.getByRole("button", { name: "Choose a domain" }));
    expect(screen.getByRole("combobox", { name: /Knowledge domain/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Teach" })).toHaveProperty("disabled", true);
    expect(connection.callsNamed("libraryTrainFile")).toHaveLength(0);
  });

  it.each(["admin", "developer", "writer", ""])("does not offer first-domain entry to role %s", async role => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    mount(connection, { role }); await settle();
    await click(screen.getByRole("button", { name: "Teach a domain" }));
    expect(screen.queryByRole("textbox", { name: /Knowledge domain/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Teach" })).toBeNull();
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Teach a domain" })).toBeTruthy();
    expect(connection.callsNamed("libraryTrainFile")).toHaveLength(0);
  });

  it("preserves the entered domain and displays the server refusal without publishing", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN], trainError: "Embedding service is unavailable." });
    await open(connection);
    fireEvent.change(screen.getByRole("textbox", { name: /Knowledge domain/ }), { target: { value: "software-development" } });
    await click(screen.getByRole("button", { name: "Teach" }));
    expect(screen.getByText("Embedding service is unavailable.")).toBeTruthy();
    expect(screen.getByRole("textbox", { name: /Knowledge domain/ })).toHaveProperty("value", "software-development");
    expect(screen.queryByText("Teaching recorded. New passages await review.")).toBeNull();
    expect(connection.callsNamed("setChunkValidationStatus")).toHaveLength(0);
  });

  it("keeps a non-owner's known-domain choice subject to the existing server authorization", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN], domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })], trainError: "Not authorized to write to this domain." });
    mount(connection, { role: "writer" }); await settle();
    await click(screen.getByRole("button", { name: "Teach a domain" }));
    expect(screen.queryByRole("button", { name: "Enter a domain" })).toBeNull();
    await click(screen.getByRole("combobox", { name: /Knowledge domain/ }));
    await click(screen.getByRole("option", { name: "Sales" }));
    await click(screen.getByRole("button", { name: "Teach" }));
    expect(connection.callsNamed("libraryTrainFile")).toHaveLength(1);
    expect(screen.getByText("Not authorized to write to this domain.")).toBeTruthy();
    expect(connection.callsNamed("setChunkValidationStatus")).toHaveLength(0);
  });

  it("withdraws free entry when the owner role changes before submission", async () => {
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    const view = mount(connection); await settle();
    await click(screen.getByRole("button", { name: "Teach a domain" }));
    fireEvent.change(screen.getByRole("textbox", { name: /Knowledge domain/ }), { target: { value: "software-development" } });
    view.rerender(withSession(<TrainingApp sectionId="upload" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { role: "writer" }));
    expect(screen.queryByRole("textbox", { name: /Knowledge domain/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Teach" })).toBeNull();
    expect(connection.callsNamed("libraryTrainFile")).toHaveLength(0);
  });

  it("records repeated teaching without claiming new pending chunks", async () => {
    const connection = fakeConnection({ files: [TRAINED_FILE], domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })], domainRows: [domainLiteRow("domain-sales", "validated")] });
    mount(connection); await settle();
    await click(screen.getByRole("button", { name: "Teach another" }));
    await click(screen.getByRole("combobox", { name: /Knowledge domain/ }));
    await click(screen.getByRole("option", { name: "Sales" }));
    await click(screen.getByRole("button", { name: "Teach" }));
    expect(connection.callsNamed("libraryTrainFile")).toHaveLength(1);
    expect(screen.getByText("Teaching recorded. New passages await review.")).toBeTruthy();
    expect(connection.callsNamed("documentChunksForDomain")).toHaveLength(0);
    expect(connection.callsNamed("setChunkValidationStatus")).toHaveLength(0);
  });

  it("disables another file's Teach action while a teaching request is in flight", async () => {
    const connection = fakeConnection({ files: [READY_FILE, { ...READY_FILE, id: "file-2", name: "second.md" }], runs: [READY_RUN] });
    let release!: () => void;
    const waiting = new Promise<void>(resolve => { release = resolve; });
    const calls = vi.mocked(connection.query.executeNamed);
    const execute = calls.getMockImplementation()!;
    calls.mockImplementation(async (name, call, opts) => {
      if (call.startsWith("builtin libraryTrainFile(")) await waiting;
      return execute(name, call, opts);
    });
    mount(connection); await settle();
    await click(within(rowFor("handbook.docx")).getByRole("button", { name: "Teach a domain" }));
    await click(within(rowFor("second.md")).getByRole("button", { name: "Teach a domain" }));
    const first = within(rowFor("handbook.docx"));
    const second = within(rowFor("second.md"));
    fireEvent.change(first.getByRole("textbox"), { target: { value: "software-development" } });
    fireEvent.change(second.getByRole("textbox"), { target: { value: "software-development" } });
    await click(first.getByRole("button", { name: "Teach" }));
    expect(second.getByRole("button", { name: "Teach" })).toHaveProperty("disabled", true);
    fireEvent.keyDown(second.getByRole("textbox"), { key: "Enter" });
    expect(calls.mock.calls.filter(([, call]) => call.startsWith("builtin libraryTrainFile("))).toHaveLength(1);
    await act(async () => { release(); }); await settle();
    expect(second.getByRole("button", { name: "Teach" })).toHaveProperty("disabled", false);
  });

  it("refreshes the first pending chunks and offers Review without making a decision", async () => {
    const seed: FakeSeed = { files: [READY_FILE], runs: [READY_RUN], domainRows: [] };
    const connection = fakeConnection(seed);
    const navigate = vi.fn();
    const view = mount(connection, { navigate }); await settle();
    await click(screen.getByRole("button", { name: "Teach a domain" }));
    fireEvent.change(screen.getByRole("textbox", { name: /Knowledge domain/ }), { target: { value: "software-development" } });
    seed.domainRows = [domainLiteRow("software-development", "unvalidated")];
    seed.chunkPages = { "software-development": [[chunkRow({ id: "lesson", domainId: "software-development", source: "fileUpload", sourceRef: "artifact:lesson", text: "Prove persisted workflow metadata through the real schema.", validationStatus: "unvalidated" })]] };
    const reads = connection.callsNamed("allDocumentChunkDomains").length;
    await click(screen.getByRole("button", { name: "Teach" }));
    expect(connection.callsNamed("allDocumentChunkDomains").length).toBeGreaterThan(reads);
    await click(screen.getByRole("button", { name: "Review" }));
    expect(navigate).toHaveBeenCalledWith("review");
    view.rerender(withSession(<TrainingApp sectionId="review" navigate={navigate} askContext={vi.fn()} store={memStore()} />));
    await settle();
    expect(screen.getByText("Prove persisted workflow metadata through the real schema.")).toBeTruthy();
    expect(screen.getByText("artifact:lesson")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Approve chunk lesson" })).toBeTruthy();
    expect(connection.callsNamed("setChunkValidationStatus")).toHaveLength(0);
    view.rerender(withSession(<TrainingApp sectionId="upload" navigate={navigate} askContext={vi.fn()} store={memStore()} />));
    await settle();
    await click(screen.getByRole("button", { name: "Teach a domain" }));
    expect(screen.getByRole("combobox", { name: /Knowledge domain/ })).toBeTruthy();
    expect(connection.callsNamed("setChunkValidationStatus")).toHaveLength(0);
  });
});

describe("the dropzone", () => {
  it("uploads through the Library provider, naming no space", async () => {
    const uploads = fakeUploads();
    const connection = fakeConnection();
    mount(connection, { uploads: uploads.provider });
    await settle();

    const input = document.querySelector('input[type="file"]');
    const file = new File(["hello"], "notes.pdf", { type: "application/pdf" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    fireEvent.change(input!);
    await settle();

    expect(uploads.calls.length).toBe(1);
    expect(uploads.calls[0]?.name).toBe("notes.pdf");
  });

  it("shows byte progress, which the space route could never report", async () => {
    const uploads = fakeUploads();
    mount(fakeConnection(), { uploads: uploads.provider });
    await settle();

    const input = document.querySelector('input[type="file"]');
    const file = new File(["0123456789"], "big.pdf", { type: "application/pdf" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    fireEvent.change(input!);
    await settle();

    // Reported against the FILE's own size, which is what the bar measures:
    // the provider's `totalBytes` and the File's size are the same number on
    // every real path, and the File is the one this browser can be sure of.
    await act(async () => uploads.progress(5, file.size));
    expect(screen.getByRole("progressbar", { name: /big.pdf/ })).toBeTruthy();
    expect(screen.getByText("50%")).toBeTruthy();
  });

  it("renders a refusal IN SURFACE with a retry, and the retry re-sends", async () => {
    const uploads = fakeUploads();
    mount(fakeConnection(), { uploads: uploads.provider });
    await settle();

    const input = document.querySelector('input[type="file"]');
    const file = new File(["x"], "huge.zip", { type: "application/zip" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    fireEvent.change(input!);
    await settle();

    await act(async () => uploads.fail("file too large: max 4294967296 bytes"));
    expect(screen.getByText("file too large: max 4294967296 bytes")).toBeTruthy();

    await click(screen.getByRole("button", { name: "Try again" }));
    expect(uploads.calls.length).toBe(2);
  });

  // THE HANDOVER IS THE ROW APPEARING, not the 201. On a 201 the file row is
  // still on its way, so an entry that vanished there would leave a gap.
  it("keeps the upload entry until its file row arrives on the feed", async () => {
    const uploads = fakeUploads();
    const connection = fakeConnection();
    mount(connection, { uploads: uploads.provider });
    await settle();

    const input = document.querySelector('input[type="file"]');
    const file = new File(["x"], "notes.pdf", { type: "application/pdf" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    fireEvent.change(input!);
    await settle();

    await act(async () => uploads.land("file-new"));
    await settle();
    expect(screen.getByText("in your Library")).toBeTruthy();

    await emit(connection, FILE_CONCEPT, fileRow({ id: "file-new", name: "notes.pdf" }), "NODE_CREATED");
    await settle();
    expect(screen.queryByText("in your Library")).toBeNull();
  });

  it("CONSUMES the drop so the desk never also uploads it", async () => {
    const uploads = fakeUploads();
    mount(fakeConnection(), { uploads: uploads.provider });
    await settle();

    const zone = document.querySelector(".os-train-drop");
    let bubbled = false;
    document.addEventListener("drop", () => void (bubbled = true));
    const file = new File(["x"], "notes.pdf", { type: "application/pdf" });
    await act(async () => {
      fireEvent.drop(zone!, { dataTransfer: { files: [file] } });
    });
    expect(bubbled).toBe(false);
    expect(uploads.calls.length).toBe(1);
  });

  it("aborts an upload in flight and drops the entry", async () => {
    const uploads = fakeUploads();
    mount(fakeConnection(), { uploads: uploads.provider });
    await settle();

    const input = document.querySelector('input[type="file"]');
    const file = new File(["x"], "notes.pdf", { type: "application/pdf" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    fireEvent.change(input!);
    await settle();

    await click(screen.getByRole("button", { name: "Cancel" }));
    await settle();
    expect(screen.queryByText("notes.pdf")).toBeNull();
  });
});

describe("auto-open review", () => {
  it("is OFF by default, so a file becoming teachable moves nobody", async () => {
    const navigate = vi.fn();
    const connection = fakeConnection({ files: [READY_FILE], runs: [READY_RUN] });
    mount(connection, { navigate });
    await settle();

    await emit(connection, FILE_CONCEPT, fileRow({ id: "file-1", trainedIntoDomainIds: ["d-1"] }));
    expect(navigate).not.toHaveBeenCalled();
  });

  // The seed that lands after mount must not look like every file in it was
  // just taught -- that would bounce somebody straight to the queue on open.
  it("does NOT fire for a file already trained when the window opened", async () => {
    const navigate = vi.fn();
    const connection = fakeConnection({ files: [TRAINED_FILE] });
    mount(connection, { navigate });
    await settle();

    expect(navigate).not.toHaveBeenCalledWith("review");
  });
});


describe("the training record list", () => {
  it("waits for the file read before counting and keeps teaching controls out of the collapsible summary", async () => {
    const view = mount(fakeConnection({ files: [READY_FILE], runs: [READY_RUN], domainCatalog: [domainRow({ id: "domain-sales", name: "Sales" })] }));
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
    await settle();
    expect(view.container.querySelector(".os-head-meta")?.textContent).toBe("1");
    const teach = screen.getByRole("button", { name: "Teach a domain" });
    expect(teach.closest(".os-record-actions")).not.toBeNull();
    expect(teach.closest(".os-record-summary")).toBeNull();
    await click(teach);
    const domain = screen.getByRole("combobox", { name: /Knowledge domain/ });
    expect(domain.closest(".os-record-summary")).toBeNull();
  });

  it("does not describe an unreadable file collection as zero files", async () => {
    const view = mount(fakeConnection({ filesError: "files denied" }));
    await settle();
    expect(screen.getByText(/files denied/)).toBeTruthy();
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
  });
});
