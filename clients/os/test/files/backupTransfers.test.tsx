import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { ArchiveTransfers, DownloadSetup } from "../../src/apps/files/backups/ArchiveTransfers";
import { StorageAllowance } from "../../src/apps/files/backups/StorageAllowance";
import { formatStorageBytes, transferActions, validArchive, absoluteBackupPath, type BackupTransfer } from "../../src/apps/files/backups/transfers";
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
    const connection = fakeConnection({ transfers: [{ ...job, kind: "sync_file", metadata: { watchId: "watch-001" } } as never], refuse: { libraryBackupControl: "Try again" } }); h.connection = connection;
    render(<ArchiveTransfers machines={machines} refresh={0} onDownload={vi.fn()} />);
    fireEvent.click(await screen.findByRole("button", { name: "Cancel transfer" })); await readTheBar();
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
it("limits archive input and exposes only legal state transitions", () => {
  for (const name of ["a.zip", "a.tar", "a.tar.gz", "a.TGZ"]) expect(validArchive(name)).toBe(true);
  for (const name of ["a.pdf", "a.md", "a.zip.exe"]) expect(validArchive(name)).toBe(false);
  expect(absoluteBackupPath("/a/../secret")).toBe(false); expect(absoluteBackupPath("C:\\backup.zip")).toBe(true);
  expect(transferActions({ ...job, state: "bin" })).toEqual(["restore", "purge"]);
  expect(transferActions({ ...job, state: "complete", kind: "sync_file" })).toEqual([]);
  expect(formatStorageBytes(5e12)).toBe("5 TB");
});
