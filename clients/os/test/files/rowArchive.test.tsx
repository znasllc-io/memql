import { fireEvent, screen, waitFor } from "@testing-library/react";
import { act } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { artifactRow, click, fakeConnection, renderFiles } from "./harness";
import { betweenTheClicks, readTheBar, useFakeDate } from "../readTheBar";

// THE ROW'S OWN ROUTE INTO THE BIN (memql#4784 AC).
//
// The acceptance criterion is that right-click on a file row and dropping it
// on the Bin BOTH archive, and that no hard-delete path exists in the app.
// This covers the first; the drop decision has its own test in test/bin/.
//
// It is a third entry point onto ONE action, not a second flow: the same
// mutation, the same confirm setting, the same in-surface refusal as the
// inspector's Archive button.

beforeEach(() => {
  h.connection = null;
});

async function rightClickRow(name: RegExp) {
  const row = screen.getByRole("button", { name }).closest(".os-files-line");
  if (row === null) throw new Error("the file row is not inside a drag/menu wrapper");
  await act(async () => {
    fireEvent.contextMenu(row);
  });
}

describe("archiving from a file row", () => {
  it("offers Move to Bin, asks first, and runs archiveArtifact", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov" })],
    });
    h.connection = connection;
    await renderFiles({ settings: { confirmBeforeArchive: true } });

    await rightClickRow(/cut-03\.mov/);
    // The action is named for what it DOES. Nothing here deletes, so nothing
    // here is called Delete.
    await click(screen.getByRole("menuitem", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(0);

    expect(screen.getByText('Move "cut-03.mov" to the Bin?')).toBeTruthy();
    // The action KEEPS ITS NAME through the flow.
    await readTheBar();
    await click(screen.getByRole("button", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(1);
    expect(connection.callsNamed("archiveArtifact")[0]).toContain('artifactId: "a-1"');
  });

  it("skips the confirm when the setting is off -- the SAME setting the inspector reads", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov" })],
    });
    h.connection = connection;
    await renderFiles({ settings: { confirmBeforeArchive: false } });

    await rightClickRow(/cut-03\.mov/);
    await click(screen.getByRole("menuitem", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(1);
  });

  it("renders a refusal in surface and leaves the row where it was", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov" })],
      refuse: { archiveArtifact: "that row is not yours" },
    });
    h.connection = connection;
    await renderFiles({ settings: { confirmBeforeArchive: false } });

    await rightClickRow(/cut-03\.mov/);
    await click(screen.getByRole("menuitem", { name: "Move to Bin" }));

    expect(screen.getByText("The archive was refused.")).toBeTruthy();
    // The server's own sentence, verbatim.
    expect(screen.getByText(/that row is not yours/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
  });

  it("offers Restore on an archived row, in the Bin place", async () => {
    // The row's one verb flips with its state (epic memql#4842, #4846): a
    // live row moves to the Bin, an archived one restores -- the Bin's own
    // vocabulary, kept verbatim so one action has one name everywhere.
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov", archived: true })],
    });
    h.connection = connection;
    await renderFiles();
    fireEvent.click(screen.getByRole("button", { name: /^Bin/ }));

    await rightClickRow(/cut-03\.mov/);
    const entry = screen.getByRole("menuitem", { name: "Restore" });
    expect(entry.hasAttribute("disabled")).toBe(false);
    fireEvent.click(entry);
    await waitFor(() => {
      expect(connection.callsNamed("restoreArtifact")).toHaveLength(1);
      expect(connection.callsNamed("restoreLibraryFile")).toHaveLength(1);
    });
  });
});

// The question opens where the menu item that asked it was (memql#6033), outside
// any bar: the second click of a double-click on "Move to Bin" used to answer it.
describe("archiving from a file row, one double-click later", () => {
  useFakeDate();

  async function ask() {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov" })],
    });
    h.connection = connection;
    await renderFiles({ settings: { confirmBeforeArchive: true } });
    await rightClickRow(/cut-03\.mov/);
    await click(screen.getByRole("menuitem", { name: "Move to Bin" }));
    expect(screen.getByText('Move "cut-03.mov" to the Bin?')).toBeTruthy();
    return connection;
  }

  it("does not move the file on the second click of the click that asked, and does once it is read", async () => {
    const connection = await ask();
    betweenTheClicks();
    await click(screen.getByRole("button", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(0);
    expect(screen.getByText('Move "cut-03.mov" to the Bin?')).toBeTruthy();

    await readTheBar();
    await click(screen.getByRole("button", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(1);
  });

  it("does not cancel on that click either", async () => {
    const connection = await ask();
    betweenTheClicks();
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText('Move "cut-03.mov" to the Bin?')).toBeTruthy();
    await readTheBar();
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText('Move "cut-03.mov" to the Bin?')).toBeNull();
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(0);
  });

  it("holds the buttons again when another file is chosen while one is asked about", async () => {
    // The question is about another file now and the same buttons stand where
    // they stood: a press that lands on them was aimed at the first question.
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "cut-03.mov" }), artifactRow({ id: "a-2", title: "cut-04.mov" })],
    });
    h.connection = connection;
    await renderFiles({ settings: { confirmBeforeArchive: true } });
    await rightClickRow(/cut-03\.mov/);
    await click(screen.getByRole("menuitem", { name: "Move to Bin" }));
    expect(screen.getByText('Move "cut-03.mov" to the Bin?')).toBeTruthy();
    await readTheBar();

    await rightClickRow(/cut-04\.mov/);
    await click(screen.getByRole("menuitem", { name: "Move to Bin" }));
    expect(screen.getByText('Move "cut-04.mov" to the Bin?')).toBeTruthy();
    betweenTheClicks();
    await click(screen.getByRole("button", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(0);

    await readTheBar();
    await click(screen.getByRole("button", { name: "Move to Bin" }));
    expect(connection.callsNamed("archiveArtifact")).toHaveLength(1);
    expect(connection.callsNamed("archiveArtifact")[0]).toContain('artifactId: "a-2"');
  });
});
