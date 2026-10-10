import { act, fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE (the browse suite's pattern).
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";

// The rail's three places (epic memql#4842, #4846, renamed by memql#4981):
// Library / Desktop / Bin, and the open intent that lands a window on one.

beforeEach(() => {
  h.connection = null;
});

describe("the Desktop place", () => {
  it("mirrors the desks: loose icons at the root, desk folders as children, and nothing that was never placed", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-desk", name: "Reports" })],
      artifacts: [
        artifactRow({ id: "a-loose", title: "loose.bin" }),
        artifactRow({ id: "a-filed", title: "filed.pdf", folderId: "f-desk" }),
        artifactRow({ id: "a-elsewhere", title: "elsewhere.txt" }),
      ],
    });
    await renderFiles({
      desk: {
        files: [{ artifactId: "a-loose", title: "loose.bin" }],
        folders: [{ folderId: "f-desk", name: "Reports" }],
      },
    });

    await click(screen.getByRole("button", { name: /^Desktop/ }));
    expect(screen.getByRole("button", { name: /loose\.bin/ })).toBeTruthy();
    expect(screen.queryByText(/elsewhere\.txt/)).toBeNull();
    expect(screen.queryByText(/filed\.pdf/)).toBeNull();

    // The desk folder is a rail child of Desktop; scoping into it shows its
    // live contents -- the same rows the Library shows for that folder.
    //
    // SCOPED TO THE DESKTOP GROUP rather than indexed out of the whole rail.
    // The same folder is a real Library folder too, so "the second Reports
    // button" only meant the desk one while every place rendered its children
    // at once; it silently becomes the wrong element -- or none -- the moment
    // a place is shut.
    const desktop = screen.getByRole("list", { name: "Folders" });
    expect(desktop).toBeTruthy();
    await click(within(desktop).getByRole("button", { name: /Open folder Reports/ }));
    expect(screen.getByRole("button", { name: /filed\.pdf/ })).toBeTruthy();
    expect(screen.queryByText(/loose\.bin/)).toBeNull();
  });
});

describe("the open intent", () => {
  it("selects a backing file's artifact and opens its folder", async () => {
    const consumed: string[] = [];
    h.connection = fakeConnection({
      folders: [folderRow({ id: "reports", name: "Reports" })],
      artifacts: [
        artifactRow({ id: "output-index", sourceConceptRef: "output-file", title: "Inventory.csv", folderId: "reports" }),
        artifactRow({ id: "output-file", sourceConceptRef: "unrelated-file", title: "Unrelated.csv" }),
      ],
    });
    await renderFiles({
      intent: { id: "open-output", payload: { fileId: "output-file" } },
      consumeIntent: (id) => consumed.push(id),
    });
    expect(screen.getByRole("heading", { name: "Inventory.csv" })).toBeTruthy();
    const details = screen.getByRole("region", { name: "File details" });
    expect(within(details).getByRole("heading", {name: "Inventory.csv"})).toBeTruthy();
    expect(within(details).queryByText("Unrelated.csv")).toBeNull();
    expect(consumed).toEqual(["open-output"]);
  });

  it("waits for indexing on the retained feed and selects the output once", async () => {
    const consumed: string[] = [];
    const conn = fakeConnection({ artifacts: [] });
    h.connection = conn;
    await renderFiles({
      intent: { id: "pending-output", payload: { fileId: "output-file" } },
      consumeIntent: (id) => consumed.push(id),
    });
    expect(consumed).toEqual([]);
    expect(conn.subscriptions.activeCount("v1:library:artifact")).toBe(1);
    await act(async () => {
      conn.subscriptions.emit("v1:library:artifact", artifactRow({
        id: "output-index", sourceConceptRef: "output-file", title: "Inventory.csv",
      }), "NODE_CREATED");
    });
    expect(within(screen.getByRole("region", { name: "File details" })).getByRole("heading", {name: "Inventory.csv"})).toBeTruthy();
    expect(consumed).toEqual(["pending-output"]);

    // A later event must not re-open a file the person has just closed.
    await click(screen.getByRole("button", { name: "Back to Library" }));
    await act(async () => {
      conn.subscriptions.emit("v1:library:artifact", artifactRow({ id: "another-index", title: "Other.csv" }), "NODE_CREATED");
    });
    expect(screen.queryByRole("region", { name: "File details" })).toBeNull();
    expect(consumed).toEqual(["pending-output"]);
  });

  it("lands the window on the asked-for place and folder, and consumes by id", async () => {
    const consumed: string[] = [];
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-desk", name: "Reports" })],
      artifacts: [artifactRow({ id: "a-filed", title: "filed.pdf", folderId: "f-desk" })],
    });
    await renderFiles({
      desk: { folders: [{ folderId: "f-desk", name: "Reports" }] },
      intent: { id: "i-1", payload: { place: "desktop", folderId: "f-desk" } },
      consumeIntent: (id) => consumed.push(id),
    });
    expect(consumed).toEqual(["i-1"]);
    expect(screen.getByRole("heading", { name: "Reports" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /filed\.pdf/ })).toBeTruthy();
  });

  it("consumes and ignores an intent it does not recognize", async () => {
    const consumed: string[] = [];
    h.connection = fakeConnection({ artifacts: [] });
    await renderFiles({
      intent: { id: "i-x", payload: { place: "nonsense" } },
      consumeIntent: (id) => consumed.push(id),
    });
    expect(consumed).toEqual(["i-x"]);
    expect(screen.getByRole("heading", { name: "Library" })).toBeTruthy();
  });
});

describe("restore out of the Bin place", () => {
  it("re-files a row to the root when its folder is not live (the #4846 rule)", async () => {
    h.connection = fakeConnection({
      archivedFolders: [folderRow({ id: "f-gone", name: "Old drafts", archived: true })],
      artifacts: [
        artifactRow({ id: "a-1", title: "stranded.txt", archived: true, folderId: "f-gone" }),
      ],
    });
    await renderFiles();
    await click(screen.getByRole("button", { name: /^Bin/ }));
    fireEvent.contextMenu(screen.getByRole("button", { name: /stranded\.txt/ }));
    await click(screen.getByRole("menuitem", { name: "Restore" }));
    const conn = h.connection as ReturnType<typeof fakeConnection>;
    expect(conn.callsNamed("restoreArtifact")).toHaveLength(1);
    // The folder is archived, so the row re-files to the Library root --
    // restored into an invisible folder is invisible everywhere but search.
    expect(conn.callsNamed("moveArtifactToFolder")[0]).toContain('folderId: ""');
  });
});

describe("the Head line (DESIGN.md rules 1-3)", () => {
  it("carries the place name, the count, the quiet sort and the Refine affordance -- no standing filter strip", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "one.bin" })],
    });
    await renderFiles();
    expect(screen.getByRole("heading", { name: "Library" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Sorted newest first/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Refine files" })).toBeTruthy();
    // The facet controls do not stand in the surface (rule 2).
    expect(screen.queryByLabelText("Source")).toBeNull();
    expect(screen.queryByRole("toolbar")).toBeNull();
    // An active facet surfaces as a removable chip while collapsed.
    await click(screen.getByRole("button", { name: "Refine files" }));
    fireEvent.click(screen.getByRole("radio", { name: "Documents" }));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.getByRole("button", { name: "Remove Documents" })).toBeTruthy();
  });
});
