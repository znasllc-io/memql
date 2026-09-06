import { act, fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real LiveCollection
// retain/seed path runs against the harness's executeNamed fake -- the same
// shape browse.test.tsx uses, and it has to be per-file: `vi.hoisted` runs
// before imports, so a shared one exported from the harness would be read
// after the mock factory needed it.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { chooseOption } from "../selectControl";
import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";

// The rail's disclosures, the one Add control, and the row's menu.
//
// Three surfaces that were reported together by the owner and that share one
// cause: the Files browse had grown a rail that rendered every place's
// folders at once, an action wedged between two of those places, and a
// right-click that offered a single verb. Each is a small change; what they
// have in common is that the app was making the person work out where things
// were rather than saying so.

beforeEach(() => {
  h.connection = fakeConnection();
});

describe("the rail's places", () => {
  it("starts shut, so the three destinations are what you see first", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" }), folderRow({ id: "f-b", name: "Video" })],
      artifacts: [artifactRow({ id: "a-1", title: "one.pdf", folderId: "f-a" })],
    });
    await renderFiles();

    const rail = screen.getByRole("navigation", { name: "Places and folders" });
    // The places themselves are reachable...
    expect(within(rail).getByRole("button", { name: /^Library/ })).toBeTruthy();
    expect(within(rail).getByRole("button", { name: /^Desktop/ })).toBeTruthy();
    expect(within(rail).getByRole("button", { name: /^Bin/ })).toBeTruthy();
    // ...and their folders are not, because nothing has been opened. The
    // group carries `hidden`, so it is out of the accessibility tree rather
    // than merely invisible.
    expect(within(rail).queryByRole("button", { name: /Contracts/ })).toBeNull();
    expect(within(rail).queryByRole("button", { name: /Video/ })).toBeNull();
  });

  it("opens on the chevron WITHOUT changing what the list is showing", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [
        artifactRow({ id: "a-root", title: "root.txt" }),
        artifactRow({ id: "a-filed", title: "filed.pdf", folderId: "f-a" }),
      ],
    });
    await renderFiles();

    // Looking at the Library root: the root file is listed.
    expect(screen.getByRole("button", { name: /root\.txt/ })).toBeTruthy();

    await click(screen.getByRole("button", { name: "Expand Library" }));
    expect(screen.getByRole("button", { name: /Contracts/ })).toBeTruthy();
    // THE POINT OF THE SEPARATE CONTROL: looking inside the rail did not
    // move the person out of the place they were reading.
    expect(screen.getByRole("button", { name: /root\.txt/ })).toBeTruthy();
    expect(screen.queryByText(/filed\.pdf/)).toBeNull();

    // ...and it shuts again, naming what a click will do either way.
    await click(screen.getByRole("button", { name: "Collapse Library" }));
    expect(screen.queryByRole("button", { name: /Contracts/ })).toBeNull();
  });

  it("opens the place when the place itself is chosen", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [artifactRow({ id: "a-1", title: "one.pdf", folderId: "f-a" })],
    });
    await renderFiles();

    // Clicking "Bin" is the gesture for "show me what is in the Bin";
    // answering it with a shut disclosure would make the person click the
    // same row twice to mean one thing.
    await click(screen.getByRole("button", { name: /^Bin/ }));
    expect(screen.getByRole("button", { name: "Collapse Bin" })).toBeTruthy();
  });

  it("opens every place a person opens, not just the last one", async () => {
    // THE SAME DEFECT THE BIN'S OWN DISCLOSURES HAD, one level up, and it
    // predates them: two chevrons flipped against one rendered `expanded`
    // both read the same value, so the second applied over the first. Found
    // in a screenshot of three places being opened at once -- every other
    // case in this file clicks one per render, which is why the suite was
    // green over it.
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [artifactRow({ id: "a-old", title: "old.zip", archived: true })],
    });
    await renderFiles();

    // Both handles taken from ONE render, which is what a browser hands a
    // person clicking quickly.
    const library = screen.getByRole("button", { name: "Expand Library" });
    const bin = screen.getByRole("button", { name: "Expand Bin" });
    await act(async () => {
      fireEvent.click(library);
      fireEvent.click(bin);
    });

    expect(screen.getByRole("button", { name: "Collapse Library" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Collapse Bin" })).toBeTruthy();
  });

  it("counts a shut place you are NOT in, and leaves the Head to count the one you are", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [
        artifactRow({ id: "a-root", title: "root.txt" }),
        artifactRow({ id: "a-1", title: "one.pdf", folderId: "f-a" }),
        artifactRow({ id: "a-2", title: "two.pdf", folderId: "f-a" }),
        artifactRow({ id: "a-old", title: "old.zip", archived: true }),
      ],
    });
    await renderFiles();

    // The Bin is shut and elsewhere, so its number says what is waiting there.
    const bin = screen.getByRole("button", { name: /^Bin/ });
    expect(within(bin).getByText("1")).toBeTruthy();

    // Library is where the person IS, and the Head already names and counts
    // that scope. "Library 2" in the Head beside "Library 3" in the rail is
    // two numbers under one word (DESIGN.md rule 7), and it was the FIRST
    // thing anybody saw.
    const library = screen.getByRole("button", { name: /^Library/ });
    expect(within(library).queryByText("3")).toBeNull();

    // A FOLDER counts what is directly in it, wherever you are standing.
    await click(screen.getByRole("button", { name: "Expand Library" }));
    const folder = screen.getByRole("button", { name: /Contracts/ });
    expect(within(folder).getByText("2")).toBeTruthy();
  });
});

describe("the Add control", () => {
  it("is the one way to put something here, and names where 'here' is", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
    });
    await renderFiles();

    // The rail action is gone -- it read as a folder you could open, sitting
    // between the Library tree and the Desktop place.
    expect(screen.queryByRole("button", { name: /^New folder$/ })).toBeNull();

    await click(screen.getByRole("button", { name: /Add/ }));
    expect(screen.getByRole("menuitem", { name: /Upload files/ })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /New folder/ })).toBeTruthy();
    // Both actions land in the folder being looked at, which is invisible
    // from the button, so the menu says it.
    expect(screen.getByText("Into Library")).toBeTruthy();
  });

  it("creates the folder inside the folder being looked at", async () => {
    const connection = fakeConnection({ folders: [folderRow({ id: "f-a", name: "Contracts" })] });
    h.connection = connection;
    await renderFiles();

    await click(screen.getByRole("button", { name: "Expand Library" }));
    await click(screen.getByRole("button", { name: /Contracts/ }));
    await click(screen.getByRole("button", { name: /Add/ }));
    expect(screen.getByText("Into Contracts")).toBeTruthy();
    await click(screen.getByRole("menuitem", { name: /New folder/ }));

    const calls = connection.callsNamed("createLibraryFolder");
    expect(calls).toHaveLength(1);
    expect(calls[0]).toContain('parentFolderId: "f-a"');
  });
});

describe("the row's right-click menu", () => {
  it("carries the actions the panel carries, not just one", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })] });
    await renderFiles();

    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    const menu = screen.getByRole("menu", { name: "File" });
    for (const name of [
      "Open in VS Code",
      "Send to desktop",
      "Download",
      "Upload new version",
      "Move to folder",
      "Ask about this file",
      "Move to Bin",
    ]) {
      expect(within(menu).getByRole("menuitem", { name })).toBeTruthy();
    }
  });

  it("offers an archived row what an archived row can do", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "old.zip", archived: true })],
    });
    await renderFiles();
    await click(screen.getByRole("button", { name: /^Bin/ }));

    fireEvent.contextMenu(screen.getByRole("button", { name: /old\.zip/ }));
    const menu = screen.getByRole("menu", { name: "File" });
    expect(within(menu).getByRole("menuitem", { name: "Restore" })).toBeTruthy();
    // Archiving something already archived is not an action, and offering it
    // would be the menu describing a state it can see is not the case.
    expect(within(menu).queryByRole("menuitem", { name: "Move to Bin" })).toBeNull();
  });

  it("re-files a row from the menu, which is where re-filing lives now", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    h.connection = connection;
    await renderFiles();

    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(screen.getByRole("menuitem", { name: "Move to folder" }));
    // Driven the way a person drives it: the picker is the kit's own listbox
    // now (#4862), so a `change` event fired at the element would reach
    // nothing and leave the assertion below standing over no interaction.
    chooseOption(screen.getByLabelText("Move to folder"), "Contracts");

    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "f-a")',
    ]);
  });
});
