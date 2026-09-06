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
  domainLiteRow,
  domainRow,
  emit,
  fakeConnection,
  fileRow,
  runRow,
  settle,
  withSession,
  type FakeConnection,
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
  opts: { section?: string; uploads?: UploadProvider; navigate?: () => void } = {},
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
      { userId: "u-me" },
    ),
  );
}

function rowFor(name: string): HTMLElement {
  const row = screen.getByText(name).closest(".os-row");
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
