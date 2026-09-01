import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE (the browse suite's pattern).
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";

// The rail's three places (epic memql#4842, #4846): Library / Desktop /
// Archive, and the open intent that lands a window on one.

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
    const rail = screen.getByRole("navigation", { name: "Places and folders" });
    await click(within(rail).getAllByRole("button", { name: /Reports/ })[1]!);
    expect(screen.getByRole("button", { name: /filed\.pdf/ })).toBeTruthy();
    expect(screen.queryByText(/loose\.bin/)).toBeNull();
  });
});

describe("the open intent", () => {
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
