import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE (the shape rail.test.tsx uses), so the Files half of this
// file runs the real LiveCollection retain/seed path against the Files harness's fake.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { folderOptions, FolderSelect, type FolderOption } from "../../src/items/FolderSelect";
import { chooseOption, openSelect, selectedLabel } from "../selectControl";
import { artifactRow, click, fakeConnection, folderRow, renderFiles } from "./harness";

// The folder picker, promoted to items/ on its second use: Files' Move to folder drew it first, and the
// Materializer's Make a file draws it for the Folder step.
//
// WHAT IT OWES A PERSON: the Library's top level first, then their folders in the order the Files rail
// shows them (siblings by name, then id), and the folder they were already in kept on screen as
// "Unavailable folder" when it is not one they can file into -- never an id, never blank.
//
// `folderOptions` carries depth in the label, two spaces a level, and how a listbox draws that is the
// kit's business (its Select trims an option's text), so the rendered tests below compare names in
// tree order and the pure ones compare the labels.

interface Folder { id: string; name: string; parentFolderId: string; archived: boolean; deleted: boolean }

const folder = (id: string, name: string, parentFolderId = "", over: Partial<Folder> = {}): Folder =>
  ({ id, name, parentFolderId, archived: false, deleted: false, ...over });

const labels = (options: readonly FolderOption[]) => options.map((option) => option.label);
const ids = (options: readonly FolderOption[]) => options.map((option) => option.id);
const listed = (list: HTMLElement) => within(list).getAllByRole("option").map((option) => (option.textContent ?? "").trim());

describe("folderOptions", () => {
  it("lists a two-level tree depth first, a child indented two spaces under its parent", () => {
    const options = folderOptions([
      folder("notes", "Notes"),
      folder("c26", "2026", "contracts"),
      folder("contracts", "Contracts"),
      folder("c25", "2025", "contracts"),
    ]);

    expect(labels(options)).toEqual(["Contracts", "  2025", "  2026", "Notes"]);
    expect(ids(options)).toEqual(["contracts", "c25", "c26", "notes"]);
  });

  it("indents every level by two more spaces", () => {
    const options = folderOptions([
      folder("a", "Acme"),
      folder("b", "Bids", "a"),
      folder("c", "Q1", "b"),
      folder("d", "Drafts", "c"),
    ]);

    expect(labels(options)).toEqual(["Acme", "  Bids", "    Q1", "      Drafts"]);
  });

  it("orders siblings by name, then by id when two share one", () => {
    const options = folderOptions([folder("b", "Same"), folder("c", "Other"), folder("a", "Same")]);

    expect(ids(options)).toEqual(["c", "a", "b"]);
  });

  it("does not depend on the order the feed delivers folders in", () => {
    const rows = [folder("x", "Reports"), folder("y", "2026", "x"), folder("z", "Archive"), folder("w", "Q3", "y")];

    expect(folderOptions([...rows].reverse())).toEqual(folderOptions(rows));
  });

  it("leaves out archived and deleted folders", () => {
    const options = folderOptions([
      folder("keep", "Kept"),
      folder("old", "Old", "", { archived: true }),
      folder("gone", "Gone", "", { deleted: true }),
      folder("both", "Both", "", { archived: true, deleted: true }),
    ]);

    expect(ids(options)).toEqual(["keep"]);
  });

  it("puts a live folder whose parent is archived at the top, and a deleted parent too", () => {
    const options = folderOptions([
      folder("old", "Old", "", { archived: true }),
      folder("kid", "Kid", "old"),
      folder("gone", "Gone", "", { deleted: true }),
      folder("child", "Child", "gone"),
      folder("grand", "Grand", "child"),
    ]);

    // Neither parent is a place a file can go, so neither child hangs under a name that is not offered.
    expect(labels(options)).toEqual(["Child", "  Grand", "Kid"]);
  });

  it("puts an orphan at the top, its own folders beneath it", () => {
    const options = folderOptions([
      folder("lost", "Lost", "never-arrived"),
      folder("inner", "Inner", "lost"),
      folder("home", "Home"),
    ]);

    expect(labels(options)).toEqual(["Home", "Lost", "  Inner"]);
  });

  it("terminates on a cycle, listing both folders once", () => {
    const options = folderOptions([folder("a", "Alpha", "b"), folder("b", "Beta", "a")]);

    expect(ids(options).sort()).toEqual(["a", "b"]);
    expect(options).toHaveLength(2);
    // The loop is broken at its first member in name order; the other sits beneath it.
    expect(labels(options)).toEqual(["Alpha", "  Beta"]);
  });

  it("terminates on a folder that is its own parent, and on a longer loop", () => {
    expect(ids(folderOptions([folder("s", "Self", "s")]))).toEqual(["s"]);

    const loop = folderOptions([folder("a", "A", "c"), folder("b", "B", "a"), folder("c", "C", "b"), folder("d", "D")]);
    expect(ids(loop).sort()).toEqual(["a", "b", "c", "d"]);
    expect(loop).toHaveLength(4);
  });

  it("offers nothing when there is no folder", () => {
    expect(folderOptions([])).toEqual([]);
  });

  it("ignores a row that names no folder", () => {
    expect(folderOptions([folder("", "Nameless")])).toEqual([]);
  });
});

describe("FolderSelect", () => {
  const OPTIONS: FolderOption[] = [
    { id: "contracts", label: "Contracts" },
    { id: "c26", label: "  2026" },
    { id: "notes", label: "Notes" },
  ];
  const mount = (value: string, over: Partial<Parameters<typeof FolderSelect>[0]> = {}) => {
    const onChange = vi.fn();
    render(<FolderSelect id="folder" label="Folder" value={value} onChange={onChange} options={OPTIONS} {...over} />);
    return { onChange, trigger: screen.getByLabelText("Folder") };
  };

  it("offers the Library's top level first, then the folders in order", () => {
    const { trigger } = mount("");

    expect(listed(openSelect(trigger))).toEqual(["Library (top level)", "Contracts", "2026", "Notes"]);
  });

  it("shows the top level as the answer for the empty value, with nothing unavailable beside it", () => {
    const { trigger } = mount("");

    expect(selectedLabel(trigger)).toBe("Library (top level)");
    expect(listed(openSelect(trigger))).not.toContain("Unavailable folder");
  });

  it("shows a folder's name once it is chosen", () => {
    const { trigger } = mount("notes");

    expect(selectedLabel(trigger)).toBe("Notes");
    expect(listed(openSelect(trigger))).toEqual(["Library (top level)", "Contracts", "2026", "Notes"]);
  });

  it("answers with the folder's id when one is chosen", () => {
    const { onChange, trigger } = mount("");

    chooseOption(trigger, "Notes");
    expect(onChange).toHaveBeenCalledExactlyOnceWith("notes");
  });

  it("answers with the empty id when the top level is chosen", () => {
    const { onChange, trigger } = mount("notes");

    chooseOption(trigger, "Library (top level)");
    expect(onChange).toHaveBeenCalledExactlyOnceWith("");
  });

  it("chooses a nested folder by its own id", () => {
    const { onChange, trigger } = mount("");

    chooseOption(trigger, "2026");
    expect(onChange).toHaveBeenCalledExactlyOnceWith("c26");
  });

  it("keeps a value that is not among the options, as Unavailable folder, and shows it chosen", () => {
    const { trigger } = mount("archived-folder");

    expect(selectedLabel(trigger)).toBe("Unavailable folder");
    const list = openSelect(trigger);
    expect(listed(list)).toEqual(["Library (top level)", "Contracts", "2026", "Notes", "Unavailable folder"]);
    const chosen = within(list).getAllByRole("option").filter((option) => option.getAttribute("aria-selected") === "true");
    expect(chosen.map((option) => option.textContent)).toEqual(["Unavailable folder"]);
    // Never the id: a person is told it is unavailable, not given a string to decode.
    expect(screen.queryByText("archived-folder")).toBeNull();
  });

  it("names the missing folder as the caller says", () => {
    const { trigger } = mount("gone", { missingLabel: "Folder no longer exists" });

    expect(selectedLabel(trigger)).toBe("Folder no longer exists");
    expect(listed(openSelect(trigger))).toContain("Folder no longer exists");
    expect(listed(openSelect(trigger))).not.toContain("Unavailable folder");
  });

  it("lets a person leave an unavailable folder for a real one", () => {
    const { onChange, trigger } = mount("archived-folder");

    chooseOption(trigger, "Contracts");
    expect(onChange).toHaveBeenCalledExactlyOnceWith("contracts");
  });

  it("does not answer when the unavailable folder is chosen again", () => {
    const { onChange, trigger } = mount("archived-folder");

    chooseOption(trigger, "Unavailable folder");
    expect(onChange).not.toHaveBeenCalled();
  });

  it("is named by its label for a person using a screen reader", () => {
    const { trigger } = mount("");

    expect(trigger.getAttribute("role")).toBe("combobox");
    expect(screen.getByRole("combobox", { name: "Folder" })).toBe(trigger);
  });

  it("shows the options it has when the feed has delivered none, rather than nothing", () => {
    const { trigger } = mount("", { options: [] });

    expect(listed(openSelect(trigger))).toEqual(["Library (top level)"]);
  });
});

describe("Files' Move to folder", () => {
  beforeEach(() => {
    h.connection = fakeConnection();
  });

  async function openMove(connection: ReturnType<typeof fakeConnection>) {
    h.connection = connection;
    await renderFiles();
    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(screen.getByRole("menuitem", { name: "Move to folder" }));
    return screen.getByLabelText("Move to folder");
  }

  it("still moves a row to the folder that was chosen", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    const picker = await openMove(connection);

    chooseOption(picker, "Contracts");

    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "f-a")',
    ]);
  });

  it("moves a row into a folder inside another, by the nested folder's own id", async () => {
    const connection = fakeConnection({
      folders: [
        folderRow({ id: "f-a", name: "Contracts" }),
        folderRow({ id: "f-b", name: "2026", parentFolderId: "f-a" }),
      ],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    const picker = await openMove(connection);

    chooseOption(picker, "2026");

    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "f-b")',
    ]);
  });

  it("moves a row back to the top level", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf", folderId: "f-a" })],
    });
    h.connection = connection;
    await renderFiles();
    await click(screen.getByRole("button", { name: /Open folder Contracts/ }));
    fireEvent.contextMenu(screen.getByRole("button", { name: /brief\.pdf/ }));
    await click(screen.getByRole("menuitem", { name: "Move to folder" }));

    chooseOption(screen.getByLabelText("Move to folder"), "Library (top level)");

    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "")',
    ]);
  });

  it("says Unavailable folder, never an id, when the file's own folder is not one it can be filed into", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-a", name: "Contracts" }), folderRow({ id: "f-old", name: "Old", archived: true })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf", folderId: "f-old" })],
    });
    const picker = await openMove(connection);

    expect(selectedLabel(picker)).toBe("Unavailable folder");
    expect(screen.queryByText("f-old")).toBeNull();
    // Another folder is one choice away.
    chooseOption(picker, "Contracts");
    expect(connection.callsNamed("moveArtifactToFolder")).toEqual([
      'mutation moveArtifactToFolder(artifactId: "a-1", folderId: "f-a")',
    ]);
  });

  it("offers the top level first and then the tree in the rail's order", async () => {
    const connection = fakeConnection({
      folders: [
        folderRow({ id: "f-n", name: "Notes" }),
        folderRow({ id: "f-b", name: "Q3", parentFolderId: "f-a" }),
        folderRow({ id: "f-a", name: "Contracts" }),
        folderRow({ id: "f-old", name: "Old", archived: true }),
      ],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    const picker = await openMove(connection);

    expect(listed(openSelect(picker))).toEqual(["Library (top level)", "Contracts", "Q3", "Notes"]);
  });
});
