import { scrollNextPage } from "../scrollNextPage";
import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { ArchiveTransfers, DownloadSetup } from "../../src/apps/files/backups/ArchiveTransfers";
import { StorageAllowance, lowStorage } from "../../src/apps/files/backups/StorageAllowance";
import { formatStorageBytes, transferActions, transferLine, validArchive, absoluteBackupPath, type BackupTransfer } from "../../src/apps/files/backups/transfers";
import { uploadLine } from "../../src/apps/files/backups/ArchiveTransfers";
import { FILES_SECTIONS } from "../../src/apps/files/settings";
import { failureReason, trackDeskUpload, useUploadSpeedHistory, useUploadTasks, type UploadTask } from "../../src/apps/files/useUploadTasks";
import { formatBytes } from "../../src/kit";
import { fakeConnection } from "./harness";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { readTheBar, useFakeDate } from "../readTheBar";
const job: BackupTransfer = { id: "backup-001", workerId: "worker-001", path: "/backup.zip", name: "backup.zip", kind: "archive", size: 12, committedBytes: 4, sha256: "a".repeat(64), state: "uploading", desiredState: "running", etag: "etag", failure: "", metadata: {} };
const machines = [machineFromRow({ id: "worker-001", name: "Laptop" } as never)];
beforeEach(() => { h.connection = null; });
describe("resumable backup controls", () => {
  useFakeDate();
  it("keeps a refused pause visible after refreshing progress", async () => {
    const connection = fakeConnection({ transfers: [job as never], refuse: { libraryBackupControl: "Storage is unavailable" } }); h.connection = connection;
    const view = render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} />);
    fireEvent.click(await screen.findByRole("button", { name: "Pause" }));
    expect(await screen.findByText("Storage is unavailable")).toBeTruthy();
    view.rerender(<ArchiveTransfers machines={machines} refresh={1} onDownload={vi.fn()} />);
    await waitFor(() => expect(connection.callsNamed("libraryBackupList").length).toBe(2));
    expect(screen.getByText("Storage is unavailable")).toBeTruthy();
    expect(connection.callsNamed("libraryBackupControl")[0]).toContain('action: "pause"');
  });
  it("offers resume for preserved progress and download only after completion", async () => {
    const download = vi.fn(); h.connection = fakeConnection({ transfers: [{ ...job, state: "paused", desiredState: "paused" } as never, { ...job, id: "backup-002", state: "complete", committedBytes: 12 } as never] });
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={download} />);
    expect(await screen.findByRole("button", { name: "Resume" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Pause" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(download.mock.calls[0]![0].id).toBe("backup-002");
  });
  it("pauses the folder before cancelling one unfinished sync file", async () => {
    // Cancel sits beside Resume once the transfer is stopped (one button per
    // moving row, owner's pick 1A).
    const connection = fakeConnection({ transfers: [{ ...job, kind: "sync_file", state: "paused", desiredState: "paused", metadata: { watchId: "watch-001" } } as never], refuse: { libraryBackupControl: "Try again" } }); h.connection = connection;
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} />);
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" })); await readTheBar();
    fireEvent.click(screen.getAllByRole("button", { name: "Cancel transfer" }).at(-1)!);
    await screen.findByText("Try again");
    const pause = connection.calls.findIndex(c => c.includes("setLibraryWatchedFolderStatus("));
    const cancel = connection.calls.findIndex(c => c.includes("libraryBackupControl("));
    expect(pause).toBeGreaterThan(-1); expect(cancel).toBeGreaterThan(pause);
  });
  it("keeps one download identity across retries and requires an absolute destination", async () => {
    const connection = fakeConnection({ refuse: { libraryBackupCreate: "Connection interrupted" } }); h.connection = connection;
    render(<DownloadSetup source={{ ...job, state: "complete" }} machines={machines} onCancel={vi.fn()} onStarted={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Full destination file path"), { target: { value: "/Downloads/backup.zip" } }); await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Download" })); await screen.findByText("Connection interrupted"); await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() => expect(connection.callsNamed("libraryBackupCreate")).toHaveLength(2));
    expect(connection.callsNamed("libraryBackupCreate")[0]).toBe(connection.callsNamed("libraryBackupCreate")[1]);
  });
  it("shows decimal quota units and reserves editing for the admin surface", async () => {
    h.connection = fakeConnection(); const view = render(<StorageAllowance />);
    expect(await screen.findByText(/10 GB available of 10 GB/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Change allowance" })).toBeNull();
    view.rerender(<StorageAllowance userId="user-001" editable />);
    fireEvent.click(await screen.findByRole("button", { name: "Change allowance" }));
    fireEvent.change(screen.getByLabelText("Storage allowance in GB"), { target: { value: "5001" } });
    expect(screen.queryByRole("button", { name: "Save allowance" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Storage allowance in GB"), { target: { value: "0" } });
    expect(screen.getByRole("button", { name: "Save allowance" })).toBeTruthy();
  });
});
it("Backups shows storage only when it runs low", async () => {
  const GB = 1_000_000_000, base = { used: 0, reserved: 0, clusterUsed: 0, clusterReserved: 0, clusterLimit: 5000 * GB };
  expect(lowStorage({ ...base, available: 10 * GB, limit: 10 * GB, exempt: false })).toBeNull();
  expect(lowStorage({ ...base, available: 0.5 * GB, limit: 10 * GB, exempt: false })).toBe("Storage is almost full: 500 MB left.");
  expect(lowStorage({ ...base, available: 0, limit: 10 * GB, exempt: false })).toBe("Storage is full. New backups can't copy anything.");
  expect(lowStorage({ ...base, available: 0, limit: 0, exempt: true })).toBeNull();
  expect(lowStorage({ ...base, clusterUsed: 4900 * GB, available: 0, limit: 0, exempt: true })).toMatch(/almost full/);
  h.connection = fakeConnection(); const view = render(<StorageAllowance alertsOnly />);
  await new Promise((r) => setTimeout(r, 50));
  expect(view.container.textContent).toBe("");
});
it("limits archive input and exposes only legal state transitions", () => {
  for (const name of ["a.zip", "a.tar", "a.tar.gz", "a.TGZ"]) expect(validArchive(name)).toBe(true);
  for (const name of ["a.pdf", "a.md", "a.zip.exe"]) expect(validArchive(name)).toBe(false);
  expect(absoluteBackupPath("/a/../secret")).toBe(false); expect(absoluteBackupPath("C:\\backup.zip")).toBe(true);
  expect(transferActions({ ...job, state: "bin" })).toEqual(["restore", "purge"]);
  expect(transferActions({ ...job, state: "complete", kind: "sync_file" })).toEqual([]);
  expect(formatStorageBytes(5e12)).toBe("5 TB");
});


it("loads one transfer page at a time, restores it, and searches older archives", async () => {
 const transfers=Array.from({length:25},(_,n)=>({...job,id:`backup-${n}`,name:`archive-${n}.zip`,state:"complete"}));
 const connection=fakeConnection({transfers:transfers as never});h.connection=connection;
 const props={machines,refresh:0,onDownload:vi.fn()};let view=render(<ArchiveTransfers {...props} />);
 await screen.findByText("archive-9.zip");expect(screen.queryByText("archive-10.zip")).toBeNull();
 await scrollNextPage();await screen.findByText("archive-19.zip");
 expect(connection.callsNamed("libraryBackupList")).toHaveLength(2);view.unmount();
 view=render(<ArchiveTransfers {...props} />);expect(screen.getByText("archive-19.zip")).toBeTruthy();expect(connection.callsNamed("libraryBackupList")).toHaveLength(2);
 fireEvent.click(screen.getByRole("button",{name:"Search transfers"}));
 fireEvent.change(screen.getByRole("textbox",{name:"Search transfers"}),{target:{value:"archive-24"}});
 await screen.findByText("archive-24.zip");expect(connection.callsNamed("libraryBackupList").at(-1)).toContain('search: "archive-24"');
 view.unmount();
});

// THE DOWNLOADS TAB (owner's pick 1A): every transfer one row, in the Backups
// row's look, with only the figures the transfer carries.
describe("downloads tab", () => {
  it("sits between Browse and Backups", () => {
    expect(FILES_SECTIONS.map(s => s.name).slice(0, 3)).toEqual(["Browse", "Downloads", "Backups"]);
  });

  it("says each transfer's state in one muted line, with no speed or time left", () => {
    const b = (n: number) => `${n} B`;
    expect(transferLine(job, "Laptop", b)).toEqual({ text: "Backup from Laptop · Uploading · 4 B of 12 B", warn: false, progress: 4 / 12, backup: true });
    expect(transferLine({ ...job, kind: "download", state: "downloading" }, "Laptop", b)).toMatchObject({ text: "Downloading to Laptop · 4 B of 12 B", backup: false });
    expect(transferLine({ ...job, state: "complete", committedBytes: 12 }, "Laptop", b)).toMatchObject({ text: "Backup from Laptop · up to date · 12 B", progress: null });
    expect(transferLine({ ...job, state: "error", failure: "connection lost" }, "Laptop", b)).toMatchObject({ text: "Upload failed · connection lost · progress is kept", warn: true, progress: null });
    expect(transferLine({ ...job, state: "paused", desiredState: "paused" }, "Laptop", b)).toMatchObject({ text: "Backup from Laptop · Paused · 4 B of 12 B", progress: null });
    const task = { id: "up-1", label: "a.bin", kind: "file", sentBytes: 5, totalBytes: 10, doneFiles: 0, totalFiles: 1, state: "sending", error: "", failures: [], retry: null, abort: vi.fn(), dismiss: vi.fn() } as UploadTask;
    // A sending upload is its bar alone: speed and time left are the graph's (look A).
    expect(uploadLine(task, b)).toEqual({ text: "", warn: false, progress: 0.5 });
    expect(uploadLine({ ...task, state: "failed", error: "Over quota" }, b)).toMatchObject({ text: "Upload failed · Over quota", warn: true });
    expect(uploadLine({ ...task, kind: "tree", doneFiles: 2, totalFiles: 3 }, b).text).toBe("2 of 3 files");
  });

  it("measures speed, and look A draws the last minute and a ring with the speed inside, no time left (Jesús, Nov 23)", async () => {
    // The measure: bytes since the last report over the time since it.
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    vi.setSystemTime(1_000);
    let report: (p: { sentBytes: number; totalBytes: number }) => void = () => {};
    const handle = { done: new Promise<never>(() => {}), abort: vi.fn(), onProgress: (fn: typeof report) => { report = fn; } };
    trackDeskUpload("icon-1", new File(["x"], "big.bin"), handle as never, null);
    report({ sentBytes: 0, totalBytes: 4_000_000 });
    vi.setSystemTime(3_000);
    report({ sentBytes: 2_000_000, totalBytes: 4_000_000 });
    const { result } = renderHook(() => useUploadTasks({ upload: vi.fn() } as never, vi.fn()));
    const task = result.current.tasks.find((t) => t.id === "desk:icon-1")!;
    expect(task.bytesPerSecond).toBe(1_000_000);

    // One sample a second while it sends.
    act(() => { vi.advanceTimersByTime(3_000); });
    const history = renderHook(() => useUploadSpeedHistory()).result.current;
    expect(history).toEqual([1_000_000, 1_000_000, 1_000_000]);
    vi.useRealTimers();

    h.connection = fakeConnection({ transfers: [] });
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[task]} />);
    const graph = screen.getByRole("group", { name: "Upload speed" });
    // The ring: progress filled, the speed inside, no time left (Jesús, Nov 23).
    expect(within(graph).getByRole("img", { name: `Uploading, 50%, ${formatBytes(1_000_000)}/s` })).toBeTruthy();
    expect(graph.textContent).toContain(formatBytes(1_000_000).split(" ")[0]);
    expect(graph.textContent).not.toContain("left");
    expect(graph.textContent).not.toContain("peak");
    expect(graph.querySelectorAll("rect")).toHaveLength(3);
  });

  it("shows a check when the last upload lands, then the ring empties to grey and stays, with the graph (Jesús, Nov 23)", () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    const sending = { id: "up-1", label: "a.bin", kind: "file", sentBytes: 5, totalBytes: 10, doneFiles: 0, totalFiles: 1, state: "sending", error: "", failures: [], retry: null, abort: vi.fn(), dismiss: vi.fn(), bytesPerSecond: 2048 } as UploadTask;
    trackDeskUpload("c", new File(["z"], "c.bin"), { done: new Promise<never>(() => {}), abort: vi.fn() } as never, null);
    act(() => { vi.advanceTimersByTime(1_000); });
    h.connection = fakeConnection({ transfers: [] });
    const { rerender } = render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[sending]} />);
    expect(screen.getByRole("img", { name: /^Uploading, 50%/ })).toBeTruthy();
    rerender(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[]} />);
    expect(screen.getByRole("img", { name: "Uploads done" })).toBeTruthy();
    act(() => { vi.advanceTimersByTime(2_000); });
    expect(screen.queryByRole("img", { name: "Uploads done" })).toBeNull();
    // The ring stays, emptied and grey, ready for the next upload, with no green left on it.
    const idle = screen.getByRole("img", { name: "Nothing uploading" });
    expect(idle.querySelector(".os-transfer-ring-fill")!.hasAttribute("data-empty")).toBe(true);
    expect(screen.getByRole("group", { name: "Upload speed" }).querySelectorAll("rect").length).toBeGreaterThan(0);
    vi.useRealTimers();
  });

  it("says why an upload failed in a few plain words, the full sentence going to the logs (Jesús, Nov 23)", () => {
    expect(failureReason("storage limit reached; stored files, the Bin and unfinished transfers all count (334 bytes available, 600 required)")).toBe("Storage is full");
    expect(failureReason("TypeError: Failed to fetch")).toBe("Connection lost");
    expect(failureReason("upload is not complete: 0 bytes are staged of the declared 2344379152 -- read GET /artifacts/uploads/x")).toBe("");
    const task = { state: "failed", error: "" } as unknown as UploadTask;
    expect(uploadLine(task, formatBytes).text).toBe("Upload failed");
    expect(uploadLine({ ...task, error: "Storage is full" }, formatBytes).text).toBe("Upload failed · Storage is full");
    expect(uploadLine({ ...task, state: "done", totalBytes: 50 * 1024 * 1024 } as UploadTask, formatBytes).text).toBe(formatBytes(50 * 1024 * 1024));
  });

  it("keeps a finished upload on the list, newest first, and counts only the new batch on the ring (Jesús, Nov 23)", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    const { result } = renderHook(() => useUploadTasks({ upload: vi.fn() } as never, vi.fn()));
    await act(async () => { trackDeskUpload("old", new File(["x"], "old.bin"), { done: Promise.resolve(), abort: vi.fn() } as never, null); });
    act(() => { vi.advanceTimersByTime(5_000); });
    const old = result.current.tasks.find(task => task.label === "old.bin")!;
    expect(old.state).toBe("done");
    expect(old.settled).toBe(true);
    act(() => { trackDeskUpload("new", new File(["yy"], "new.bin"), { done: new Promise<never>(() => {}), abort: vi.fn() } as never, null); });
    expect(result.current.tasks.map(task => task.label).slice(0, 2)).toEqual(["new.bin", "old.bin"]);
    h.connection = fakeConnection({ transfers: [] });
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={result.current.tasks} />);
    expect(screen.getByText("old.bin")).toBeTruthy();
    expect(screen.getByRole("img", { name: /^Uploading, 0%/ })).toBeTruthy();
    vi.useRealTimers();
  });

  it("keeps the graph and an empty grey ring on screen before anything uploads (Jesús, Nov 23)", () => {
    h.connection = fakeConnection({ transfers: [] });
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[]} />);
    const panel = screen.getByRole("group", { name: "Upload speed" });
    expect(within(panel).getByRole("img", { name: "Nothing uploading" })).toBeTruthy();
    expect(panel.querySelectorAll("rect")).toHaveLength(0);
  });

  it("turns the ring the error colour with an X when an upload fails, even with no graph yet (Jesús, Nov 23)", () => {
    const failed = { id: "up-f", label: "big.bin", kind: "file", sentBytes: 0, totalBytes: 10, doneFiles: 0, totalFiles: 1, state: "failed", error: "storage limit reached", failures: [], retry: vi.fn(), abort: vi.fn(), dismiss: vi.fn() } as unknown as UploadTask;
    h.connection = fakeConnection({ transfers: [] });
    const { rerender } = render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[failed]} />);
    const ring = screen.getByRole("img", { name: "Upload failed" });
    expect(ring.hasAttribute("data-failed")).toBe(true);
    expect(ring.querySelectorAll(".os-transfer-ring-check[data-on]")).toHaveLength(1);
    // Dismissed: no failure left, so the error ring goes.
    rerender(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[]} />);
    expect(screen.queryByRole("img", { name: "Upload failed" })).toBeNull();
  });

  it("keeps the graph frozen once nothing uploads, and the next upload carries it on (Jesús, Nov 23)", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    let report: (p: { sentBytes: number; totalBytes: number }) => void = () => {};
    let land: () => void = () => {};
    const upload = () => ({ done: new Promise<never>((resolve) => { land = () => resolve(undefined as never); }), abort: vi.fn(), onProgress: (fn: typeof report) => { report = fn; } });
    trackDeskUpload("a", new File(["x"], "a.bin"), upload() as never, null);
    vi.setSystemTime(1_000); report({ sentBytes: 0, totalBytes: 10 });
    vi.setSystemTime(2_000); report({ sentBytes: 5, totalBytes: 10 });
    act(() => { vi.advanceTimersByTime(2_000); });
    expect(renderHook(() => useUploadSpeedHistory()).result.current).toHaveLength(2);

    // It lands: nothing sends, the ticker stops, the graph stays as it was.
    await act(async () => { land(); await Promise.resolve(); await Promise.resolve(); });
    act(() => { vi.advanceTimersByTime(5_000); });
    expect(renderHook(() => useUploadSpeedHistory()).result.current).toHaveLength(2);

    h.connection = fakeConnection({ transfers: [] });
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[]} />);
    const graph = screen.getByRole("group", { name: "Upload speed" });
    // Opened after it landed: the frozen graph and an empty grey ring.
    expect(within(graph).getByRole("img", { name: "Nothing uploading" })).toBeTruthy();
    expect(graph.querySelectorAll("rect")).toHaveLength(2);

    // The next upload resumes the same graph rather than starting over.
    trackDeskUpload("b", new File(["y"], "b.bin"), upload() as never, null);
    act(() => { vi.advanceTimersByTime(1_000); });
    expect(renderHook(() => useUploadSpeedHistory()).result.current).toHaveLength(3);
    vi.useRealTimers();
  });

  it("draws one row per transfer with a Backup pill, a bar and one button while moving", async () => {
    h.connection = fakeConnection({ transfers: [job as never, { ...job, id: "backup-002", name: "photos.zip", state: "error", failure: "connection lost" } as never] });
    const retry = vi.fn();
    const failed = { id: "up-1", label: "photo-shoot.zip", kind: "file", sentBytes: 6, totalBytes: 10, doneFiles: 0, totalFiles: 1, state: "failed", error: "connection lost", failures: [], retry, abort: vi.fn(), dismiss: vi.fn() } as UploadTask;
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} uploads={[failed]} />);
    const row = (await screen.findByText("backup.zip")).closest("article")!;
    expect(row.textContent).toContain(`Backup from Laptop · Uploading · ${formatBytes(4)} of ${formatBytes(12)}`);
    expect(row.textContent).toContain("Backup");
    expect(row.querySelector('[role="progressbar"]')?.getAttribute("aria-valuenow")).toBe("33");
    expect([...row.querySelectorAll("button")].map(b => b.textContent)).toEqual(["Pause"]);
    const broken = screen.getByText("photos.zip").closest("article")!;
    expect(broken.querySelector(".os-transfer-failed")?.textContent).toBe("Upload failed · connection lost · progress is kept");
    expect([...broken.querySelectorAll("button")].map(b => b.textContent)).toEqual(["Retry", "Cancel"]);
    const upload = screen.getByText("photo-shoot.zip").closest("article")!;
    expect(upload.querySelector(".os-transfer-failed")?.textContent).toBe("Upload failed · connection lost");
    fireEvent.click(within(upload).getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalled();
  });
});
