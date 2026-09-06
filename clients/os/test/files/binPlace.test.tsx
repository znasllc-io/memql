import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real LiveCollection
// retain/seed path runs against the harness's executeNamed fake. Per-file,
// for the reason rail.test.tsx records: `vi.hoisted` runs before imports.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";
import { renderBin } from "../bin/harness";

// The Bin place (epic memql#4981, #4982): the rail's third place, renamed
// from Archive and now reaching the FILES in the Bin as well as its folders.
//
// Two things are being pinned here, and the second is the one that made the
// epic worth doing. The first is the shape: a folder, its files one step in,
// and everything loose behind one group. The second is AGREEMENT -- the Bin
// place and the Bin app are two windows onto one population, and they read
// it through two different queries (`libraryArtifactsByLens` folded
// client-side here, `libraryArchivedArtifacts` there). Nothing but a test
// that renders both keeps them saying the same thing.

beforeEach(() => {
  h.connection = null;
});

/** The Bin's group in the rail, which is where every assertion below is
 *  scoped. Indexing out of the whole rail would silently pick up a Library
 *  folder of the same name -- the trap places.test.tsx already records. */
function binGroup(): HTMLElement {
  const rail = screen.getByRole("navigation", { name: "Places and folders" });
  const group = rail.querySelector("#os-files-place-bin");
  if (!group) throw new Error("the Bin place has no group in the rail");
  return group as HTMLElement;
}

describe("the Bin place", () => {
  it("is named for the destination the row menu already names", async () => {
    h.connection = fakeConnection();
    await renderFiles();

    const rail = screen.getByRole("navigation", { name: "Places and folders" });
    // One destination, one word. The row menu has always said "Move to Bin"
    // and the dock fixture has always been the Bin; the rail said Archive,
    // so a person who archived a file went looking for a place that was not
    // there under a name that was.
    expect(within(rail).getByRole("button", { name: /^Bin/ })).toBeTruthy();
    expect(within(rail).queryByRole("button", { name: /^Archive/ })).toBeNull();
  });

  it("shows an archived folder, and its files one step inside it", async () => {
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-1", title: "cut-03.mov", archived: true, folderId: "fo-1" }),
        artifactRow({ id: "a-live", title: "live.bin" }),
      ],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Bin/ }));
    const folder = within(binGroup()).getByRole("button", { name: /^Old campaign/ });
    // The folder counts what is in the Bin under it, not what it once held.
    expect(within(folder).getByText("1")).toBeTruthy();
    // Shut, its file is not in the rail at all -- not hidden, absent.
    expect(within(binGroup()).queryByRole("button", { name: /cut-03\.mov/ })).toBeNull();

    await click(screen.getByRole("button", { name: "Expand Old campaign" }));
    expect(within(binGroup()).getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
    // Nothing live leaked in with it.
    expect(within(binGroup()).queryByRole("button", { name: /live\.bin/ })).toBeNull();
  });

  it("stops claiming the Bin is empty when it holds files and no folders", async () => {
    // THE DEFECT THIS EPIC EXISTS FOR. The place's emptiness was measured on
    // the archived FOLDERS alone, so a Bin holding forty files and no folders
    // expanded to "Nothing archived." -- while the list beside it was showing
    // those forty rows at that moment.
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-1", title: "cut-03.mov", archived: true }),
        artifactRow({ id: "a-2", title: "brief.pdf", archived: true }),
      ],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Bin/ }));
    expect(within(binGroup()).queryByText("Nothing archived.")).toBeNull();
    expect(within(binGroup()).queryByText("The Bin is empty.")).toBeNull();
    expect(within(binGroup()).getByText("Not in a folder")).toBeTruthy();
    expect(within(binGroup()).getByText("2")).toBeTruthy();
  });

  it("says the Bin is empty where nobody else is saying it", async () => {
    // The empty line follows the rule the COUNT follows: it answers only
    // where nothing else is. Expanded from somewhere else it is the whole
    // answer; standing in the Bin, the list is already saying it -- and two
    // copies of one sentence 200px apart is DESIGN.md rule 7.
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-live", title: "live.bin" })] });
    await renderFiles();

    // Looking at the Library, peeking into the Bin.
    await click(screen.getByRole("button", { name: "Expand Bin" }));
    expect(within(binGroup()).getByText("The Bin is empty.")).toBeTruthy();

    // ...and standing in it, the list carries the sentence with the part
    // that matters, so the rail stands down.
    await click(screen.getByRole("button", { name: /^Bin/ }));
    expect(within(binGroup()).queryByText("The Bin is empty.")).toBeNull();
    expect(
      screen.getByText("The Bin is empty. Archiving from the Library keeps files here, not deleted."),
    ).toBeTruthy();
  });

  it("holds loose files behind one group rather than repeating the list in the rail", async () => {
    // The folders-not-files rule still applies to what the LIST is already
    // showing: standing in the Bin, its rows are the loose files, and a copy
    // of them in a 184px column beside it is the same rows twice, narrower.
    // A shut group answers "how much is in here" in one number instead.
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov", archived: true })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Bin/ }));
    // In the list, once.
    expect(screen.getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
    expect(within(binGroup()).queryByRole("button", { name: /cut-03\.mov/ })).toBeNull();

    // ...and in the rail only when asked for.
    await click(screen.getByRole("button", { name: "Expand Not in a folder" }));
    expect(within(binGroup()).getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
  });

  it("shows a file when one is chosen: the list scopes to it and the inspector opens", async () => {
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-1", title: "cut-03.mov", archived: true, folderId: "fo-1" }),
        artifactRow({ id: "a-2", title: "brief.pdf", archived: true }),
      ],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Bin/ }));
    await click(screen.getByRole("button", { name: "Expand Old campaign" }));
    await click(within(binGroup()).getByRole("button", { name: /cut-03\.mov/ }));

    // Naming a thing and leaving the person to find it would be the rail
    // pointing at something it refuses to reach: the scope follows.
    expect(screen.getByRole("heading", { name: "Old campaign" })).toBeTruthy();
    const inspector = screen.getByRole("complementary", { name: "File details" });
    expect(within(inspector).getByText("cut-03.mov")).toBeTruthy();
    // The sibling that is not in that folder is not in the scoped list.
    expect(screen.queryByText(/brief\.pdf/)).toBeNull();
  });

  it("counts files and folders together, and says which is which", async () => {
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-1", title: "cut-03.mov", archived: true }),
        artifactRow({ id: "a-2", title: "brief.pdf", archived: true }),
      ],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderFiles();

    // Shut and elsewhere, the number is everything a person could take back
    // -- the Bin app's own item count, files and folders alike.
    const bin = screen.getByRole("button", { name: /^Bin/ });
    expect(within(bin).getByText("3")).toBeTruthy();
    // "3" over two files and a folder is true and unhelpful, so the title
    // spells it out.
    expect(within(bin).getByTitle("2 files and 1 folder in the Bin")).toBeTruthy();
  });

  it("keeps every disclosure that is opened, not just the last one", async () => {
    // FOUND IN A SCREENSHOT, NOT IN A TEST. Two chevrons flipped against one
    // rendered value both read the SAME `openBinFolders`, so a value-taking
    // setter applies the second over the first and one of them silently does
    // nothing. Every other case in this file clicks once per render, which is
    // why 240 of them were green over it.
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-1", title: "cut-03.mov", archived: true, folderId: "fo-1" }),
        artifactRow({ id: "a-2", title: "loose.png", archived: true }),
      ],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderFiles();
    await click(screen.getByRole("button", { name: /^Bin/ }));

    // Both handles taken from ONE render, which is what a browser hands a
    // person clicking quickly -- and what the harness that caught this did.
    const folder = screen.getByRole("button", { name: "Expand Old campaign" });
    const loose = screen.getByRole("button", { name: "Expand Not in a folder" });
    await act(async () => {
      fireEvent.click(folder);
      fireEvent.click(loose);
    });

    expect(within(binGroup()).getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
    expect(within(binGroup()).getByRole("button", { name: /loose\.png/ })).toBeTruthy();
  });

  it("keeps a folder whose files were all restored, and gives it no chevron", async () => {
    // It is still in the Bin and still restorable. A twisty that opens onto
    // one grey line is how people learn to stop trusting twisties.
    h.connection = fakeConnection({
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Bin/ }));
    expect(within(binGroup()).getByRole("button", { name: /^Old campaign/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Expand Old campaign" })).toBeNull();
  });
});

describe("the Bin place and the Bin app", () => {
  it("hold the same things, read through two different queries", async () => {
    // The two surfaces seed from different reads by design: Files folds the
    // whole artifact lens client-side (its facets must answer from a set that
    // does not depend on when you looked), the Bin asks for the archived rows
    // directly. Same fixtures into both, so a projection that drops something
    // on one side shows up as a disagreement rather than as nothing.
    const archivedFiles = [
      artifactRow({ id: "a-1", title: "cut-03.mov", archived: true, folderId: "fo-1" }),
      artifactRow({ id: "a-2", title: "brief.pdf", archived: true }),
    ];
    const archivedFolders = [folderRow({ id: "fo-1", name: "Old campaign", archived: true })];
    // A records-lens row that is also archived. Neither surface may show it
    // (design D2), and it is here because "both agree" is only worth
    // asserting over a population where they could have disagreed.
    const note = artifactRow({ id: "a-note", lens: "record", kind: "note", title: "standup", archived: true });
    const seed = {
      artifacts: [...archivedFiles, note, artifactRow({ id: "a-live", title: "live.bin" })],
      archived: [...archivedFiles, note],
      archivedFolders,
    };

    h.connection = fakeConnection(seed);
    await renderFiles();
    await click(screen.getByRole("button", { name: /^Bin/ }));
    await click(screen.getByRole("button", { name: "Expand Old campaign" }));
    await click(screen.getByRole("button", { name: "Expand Not in a folder" }));
    const inFiles = binGroup();
    for (const name of [/^cut-03\.mov/, /^brief\.pdf/, /^Old campaign/]) {
      expect(within(inFiles).getByRole("button", { name })).toBeTruthy();
    }
    expect(within(inFiles).queryByText(/standup/)).toBeNull();
    expect(within(inFiles).queryByText(/live\.bin/)).toBeNull();
    cleanup();

    h.connection = fakeConnection(seed);
    await renderBin();
    for (const name of [/^cut-03\.mov/, /^brief\.pdf/, /^Old campaign/]) {
      expect(screen.getByRole("button", { name })).toBeTruthy();
    }
    expect(screen.queryByText(/standup/)).toBeNull();
    expect(screen.queryByText(/live\.bin/)).toBeNull();
  });
});
