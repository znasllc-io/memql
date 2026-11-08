import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));
import { artifactRow, click, fakeConnection, fileRow, folderRow, renderFiles } from "./harness";
import { readArchiveMembers, planArchive } from "../../src/apps/files/actions/archive";
import { foldFolderTree } from "../../src/apps/files/fold";
import { folderFromRow } from "../../src/apps/files/rows";
import { disposeSessionCollections } from "../../src/live/sessionCollections";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

afterEach(() => {
  if (h.connection) disposeSessionCollections(h.connection as Connection);
  h.connection = null;
});

function directory(size = 25) {
  return Array.from({ length: size }, (_, n) => artifactRow({
    id: `artifact-${n}`, title: n === 0 ? "Ancient needle.pdf" : `Document ${n}.pdf`,
    folderId: "folder", sourceConceptRef: `file-${n}`,
    createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T12:00:00Z`,
  }));
}

describe("Files bounded directory", () => {
  it("reads ten records without backing bodies and retains twenty after reopening", async () => {
    const connection = fakeConnection({ artifacts: directory(), files: Array.from({ length: 25 }, (_, n) => fileRow({ id: `file-${n}` })), folders: [folderRow({ id: "folder", name: "Documents" })] });
    h.connection = connection;
    const first = await renderFiles();
    expect(screen.getAllByRole("button", { name: /Document \d+\.pdf/ })).toHaveLength(10);
    expect(screen.getByText("25 files")).toBeTruthy();
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(1);
    expect(connection.callsNamed("libraryFilesByIds")).toHaveLength(0);
    expect(connection.callsNamed("libraryFileById")).toHaveLength(0);
    for (const name of ["libraryArtifactsByLens", "libraryFilesForOwner", "compositions"]) expect(connection.callsNamed(name)).toHaveLength(0);
    await click(screen.getByRole("button", { name: "Load more" }));
    expect(screen.getAllByRole("button", { name: /Document \d+\.pdf/ })).toHaveLength(20);
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(2);
    first.unmount();
    await renderFiles();
    expect(screen.getAllByRole("button", { name: /Document \d+\.pdf/ })).toHaveLength(20);
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(2);
    expect(connection.subscriptions.activeCount("v1:library:artifact", false)).toBe(1);
    // Facet invalidation has a compact ID-only stream, with no duplicated bodies.
    expect(connection.subscriptions.activeCount("v1:library:artifact", true)).toBe(1);
  });

  it("searches records beyond loaded pages and restores cached pages when cleared", async () => {
    const connection = fakeConnection({ artifacts: directory() });
    h.connection = connection;
    await renderFiles();
    await click(screen.getByRole("button", { name: "Load more" }));
    await click(screen.getByRole("button", { name: "Refine files" }));
    const search = screen.getByRole("textbox", { name: "Search" });
    fireEvent.change(search, { target: { value: "ANCIENT" } });
    await screen.findByRole("button", { name: /Ancient needle/ });
    expect(screen.queryByRole("button", { name: /Document 24/ })).toBeNull();
    expect(connection.callsNamed("libraryArtifactsBrowse").some(call => call.includes('search: "ANCIENT"'))).toBe(true);
    fireEvent.change(search, { target: { value: "" } });
    await waitFor(() => expect(screen.getAllByRole("button", { name: /Document \d+\.pdf/ })).toHaveLength(20));
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(3);
  });

  it.each(["settings", "logs"])("does not read directory or backing catalogs in %s", async section => {
    const connection = fakeConnection();
    h.connection = connection;
    await renderFiles({ section });
    await act(async () => {});
    for (const name of ["libraryArtifactsBrowse", "libraryBrowseFacets", "libraryFolders", "libraryFilesForOwner", "compositionOutputIndex", "libraryWatchedFolders"]) {
      expect(connection.callsNamed(name)).toHaveLength(0);
    }
  });

  it("plans folder operations from all scoped pages, including an archived file beyond the first page", async () => {
    const artifacts = Array.from({ length: 65 }, (_, n) => artifactRow({ id: `artifact-${n}`, folderId: n === 64 ? "child" : "folder", archived: n === 64 }));
    const connection = fakeConnection({ artifacts });
    const tree = foldFolderTree([folderRow({ id: "folder" }), folderRow({ id: "child", parentFolderId: "folder" })].map(folderFromRow));
    const members = await readArchiveMembers(connection.query, tree, "folder");
    const plan = planArchive(tree, members, "folder");
    expect(members).toHaveLength(65);
    expect(connection.callsNamed("libraryArtifactsInFolders")).toHaveLength(2);
    expect(plan.artifactIds).toHaveLength(64);
    expect(plan.archiveFolderIds).toEqual(["child", "folder"]);
    expect(plan.deleteFolderIds).toEqual([]);
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(0);
  });
});
