import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE (the browse suite's pattern).
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";
import { chooseOption, openSelect, selectedLabel } from "../selectControl";

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
    expect(conn.subscriptions.activeCount("v1:library:artifact", false)).toBe(1);
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

describe("the Head line and its hidden search panel (José, Nov 19 2026)", () => {
  it("holds the picked file's icons, one search button and Add, and no sort button", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "one.bin" })],
    });
    await renderFiles();
    expect(screen.getByRole("heading", { name: "Library" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Sort order/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Refine files" })).toBeNull();
    expect(screen.getByRole("button", { name: "Add to this folder" })).toBeTruthy();
    // Hidden until asked: nothing of the panel stands in the surface.
    const toggle = screen.getByRole("button", { name: "Search" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("textbox", { name: "Search" })).toBeNull();
    expect(screen.queryByLabelText("Sort")).toBeNull();

    await click(screen.getByRole("button", { name: /one\.bin/ }));
    const head = screen.getByRole("heading", { name: "Library" }).parentElement!;
    expect(within(head).getByRole("group", { name: "Selected: one.bin" })).toBeTruthy();

    // Open: Fleet's search bar (kind of file + search, no add), then Sort and Added.
    await click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const panel = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    expect(within(panel).getByRole("textbox", { name: "Search" })).toBe(document.activeElement);
    expect(selectedLabel(within(panel).getByLabelText("Filter files"))).toBe("All");
    expect(selectedLabel(within(panel).getByLabelText("Sort"))).toBe("Newest first");
    expect(selectedLabel(within(panel).getByLabelText("Added"))).toBe("Any time");
    expect(within(panel).queryByRole("button", { name: /^Add/ })).toBeNull();
    // Kind first, then Source and Client (no Label group: nothing is labelled).
    const kinds = openSelect(within(panel).getByLabelText("Filter files"));
    expect([...kinds.querySelectorAll(".os-select-group")].map((g) => g.textContent)).toEqual(["Kind", "Source", "Client"]);
    expect(within(kinds).getAllByRole("option").map((o) => o.textContent).slice(0, 10))
      .toEqual(["All", "Folders", "Documents", "Images", "Media", "Sheets", "Archives", "Misc", "Any source", "Uploaded"]);
    expect(within(openSelect(within(panel).getByLabelText("Sort"))).getAllByRole("option").map((o) => o.textContent))
      .toEqual(["Name", "Newest first", "Oldest first", "Size", "Kind"]);
    expect(within(openSelect(within(panel).getByLabelText("Added"))).getAllByRole("option").map((o) => o.textContent))
      .toEqual(["Any time", "Today", "Last 7 days", "Last 30 days"]);

    await click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("textbox", { name: "Search" })).toBeNull();
  });

  it("filters by kind of file, shows only folders for Folders, and closing ends the search but keeps the order", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-1", name: "Reports" })],
      artifacts: [
        artifactRow({ id: "a-photo", title: "Site visit.JPG", createdAt: "2026-08-23T10:00:00Z" }),
        artifactRow({ id: "a-scan", title: "scan", mimeType: "application/pdf", createdAt: "2026-08-22T10:00:00Z" }),
        artifactRow({ id: "a-sheet", title: "Pricing.xlsx", createdAt: "2026-08-21T10:00:00Z" }),
        artifactRow({ id: "a-misc", title: "export.json", createdAt: "2026-08-20T10:00:00Z" }),
      ],
    });
    await renderFiles();
    const toggle = screen.getByRole("button", { name: "Search" });
    await click(toggle);
    const kind = () => screen.getByLabelText("Filter files");
    const tiles = () => [...document.querySelectorAll(".os-files-files .os-files-tile-name")].map((name) => name.textContent);

    chooseOption(kind(), "Images");
    await act(async () => {});
    expect(tiles()).toEqual(["Site visit.JPG"]);
    expect(screen.queryByRole("button", { name: "Open folder Reports" })).toBeNull();
    chooseOption(kind(), "Documents");
    await act(async () => {});
    expect(tiles()).toEqual(["scan"]);
    chooseOption(kind(), "Misc");
    await act(async () => {});
    expect(tiles()).toEqual(["export.json"]);
    chooseOption(kind(), "Folders");
    await act(async () => {});
    expect(tiles()).toEqual([]);
    expect(screen.getByRole("button", { name: "Open folder Reports" })).toBeTruthy();
    expect(screen.queryByText(/Nothing matches/)).toBeNull();

    chooseOption(screen.getByLabelText("Sort"), "Name");
    await act(async () => {});
    await click(toggle);
    // Every file is back, in the order the panel left: by name.
    expect(tiles()).toEqual(["export.json", "Pricing.xlsx", "scan", "Site visit.JPG"]);
    await click(toggle);
    expect(selectedLabel(kind())).toBe("All");
    expect(selectedLabel(screen.getByLabelText("Sort"))).toBe("Name");
  });

  it("puts Label, Source and Client in the kind menu as single choices that stack, and closing clears them (Jesús, Nov 20 2026, p17 C)", { timeout: 20000 }, async () => {
    h.connection = fakeConnection({
      accounts: [{ id: "acme", name: "Acme Consulting", status: "active", createdAt: "2026-08-01T00:00:00Z" }],
      artifacts: [
        artifactRow({ id: "a-1", title: "invoice.pdf", labels: ["invoices"], accountIds: ["acme"], createdAt: "2026-08-23T10:00:00Z" }),
        artifactRow({ id: "a-2", title: "agent.pdf", labels: ["invoices"], source: "agent_generated", accountIds: ["acme"], createdAt: "2026-08-22T10:00:00Z" }),
        artifactRow({ id: "a-3", title: "photo.jpg", labels: ["invoices"], accountIds: ["acme"], createdAt: "2026-08-21T10:00:00Z" }),
        artifactRow({ id: "a-4", title: "loose.pdf", labels: ["misc"], createdAt: "2026-08-20T10:00:00Z" }),
      ],
    });
    await renderFiles();
    const toggle = screen.getByRole("button", { name: "Search" });
    await click(toggle);
    const menu = () => screen.getByRole("combobox", { name: "Filter files" });
    const tiles = () => [...document.querySelectorAll(".os-files-files .os-files-tile-name")].map((name) => name.textContent);
    const checked = () => within(openSelect(menu())).getAllByRole("option").filter((o) => o.getAttribute("aria-selected") === "true").map((o) => o.textContent);

    const list = openSelect(menu());
    expect([...list.querySelectorAll(".os-select-group")].map((g) => g.textContent)).toEqual(["Kind", "Label", "Source", "Client"]);
    expect(within(list).getByRole("option", { name: "Made by an agent" })).toBeTruthy();
    // Only the sources some file has: uploaded and agent-made here, nothing else.
    expect(within(list).queryByRole("option", { name: "Derived" })).toBeNull();
    expect(within(list).queryByRole("option", { name: "Pipeline" })).toBeNull();
    expect(within(list).getByRole("option", { name: "No client" })).toBeTruthy();
    // Every group starts on its "any" answer, each with its own check.
    expect(checked()).toEqual(["All", "Any label", "Any source", "Any client"]);
    fireEvent.keyDown(menu(), { key: "Escape" });

    chooseOption(menu(), "invoices");
    await act(async () => {});
    chooseOption(menu(), "Documents");
    await act(async () => {});
    chooseOption(menu(), "Uploaded");
    await act(async () => {});
    chooseOption(menu(), "Acme Consulting");
    await act(async () => {});
    await waitFor(() => expect(tiles()).toEqual(["invoice.pdf"]));
    expect(checked()).toEqual(["Documents", "invoices", "Uploaded", "Acme Consulting"]);
    fireEvent.keyDown(menu(), { key: "Escape" });
    // The button reads the kind, plus how many of the other three are on.
    expect(selectedLabel(menu())).toBe("Documents +3");

    chooseOption(menu(), "Any source");
    await act(async () => {});
    await waitFor(() => expect(tiles()).toEqual(["invoice.pdf", "agent.pdf"]));
    expect(selectedLabel(menu())).toBe("Documents +2");

    // Closing the panel clears all four (item 8 keeps only the sort).
    await click(toggle);
    await waitFor(() => expect(tiles()).toHaveLength(4));
    await click(toggle);
    expect(selectedLabel(menu())).toBe("All");
    expect(checked()).toEqual(["All", "Any label", "Any source", "Any client"]);
  });

  it("reads every page before sorting by name, so the first tile is the Library's first name", async () => {
    const artifacts = Array.from({ length: 25 }, (_, n) => artifactRow({
      id: `artifact-${n}`, title: n === 3 ? "Aardvark.pdf" : `Document ${n}.pdf`,
      createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T12:00:00Z`,
    }));
    const connection = fakeConnection({ artifacts });
    h.connection = connection;
    await renderFiles();
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(1);
    await click(screen.getByRole("button", { name: "Search" }));
    chooseOption(screen.getByLabelText("Sort"), "Name");
    await act(async () => {});
    await waitFor(() => expect(screen.getAllByRole("button", { name: /\.pdf/ })).toHaveLength(25));
    expect(connection.callsNamed("libraryArtifactsBrowse")).toHaveLength(3);
    expect(screen.getAllByRole("button", { name: /\.pdf/ })[0]!.textContent).toContain("Aardvark.pdf");
  });
});
