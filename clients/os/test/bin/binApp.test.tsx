import { screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { ARTIFACT_CONCEPT } from "../../src/apps/files/concepts";
import { artifactRow, click, emit, fakeConnection, fileRow, folderRow } from "../files/harness";
import { renderBin } from "./harness";

// The Bin, end to end through the real LiveCollection and the real generated
// builders (the harness fakes at `executeNamed`, which is what makes that
// true).

beforeEach(() => {
  h.connection = null;
});

describe("what the Bin holds", () => {
  it("lists archived items and archived FOLDERS together", async () => {
    h.connection = fakeConnection({
      archived: [artifactRow({ id: "a-1", title: "cut-03.mov", archived: true })],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderBin();
    expect(screen.getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Old campaign/ })).toBeTruthy();
  });

  it("resolves 'was filed in' against the ARCHIVED folders", async () => {
    // libraryFolders carries `archived != true`, so a folder that went to the
    // Bin with its contents is invisible to every other surface. Reading the
    // Files tree here would render "Library (top level)" -- a different, and
    // wrong, answer.
    h.connection = fakeConnection({
      archived: [artifactRow({ id: "a-1", title: "cut-03.mov", archived: true, folderId: "fo-1" })],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    await renderBin();
    expect(screen.getAllByTitle("Was filed in Old campaign").length).toBeGreaterThan(0);
  });

  it("states the invariant on the page, not only in settings", async () => {
    h.connection = fakeConnection({ archived: [] });
    await renderBin();
    expect(screen.getByText(/Nothing here has been deleted/)).toBeTruthy();
  });

  it("says what an empty Bin means rather than only that it is empty", async () => {
    h.connection = fakeConnection({ archived: [], archivedFolders: [] });
    await renderBin();
    expect(screen.getByText(/keeps its bytes and its history, and waits for you/)).toBeTruthy();
  });
});

describe("the Bin is live", () => {
  it("takes in a row that was archived somewhere else, with the arrival cue", async () => {
    const connection = fakeConnection({ archived: [] });
    h.connection = connection;
    await renderBin();

    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-9", title: "landed.mov", archived: true }),
      "NODE_CREATED",
    );
    const row = screen.getByRole("button", { name: /landed\.mov/ });
    expect(row.closest("li")?.getAttribute("data-arrival")).toBe("added");
  });
});

describe("restoring", () => {
  it("runs BOTH writes for a file, because the archive touched both rows", async () => {
    const connection = fakeConnection({
      archived: [
        artifactRow({
          id: "a-1",
          title: "cut-03.mov",
          archived: true,
          kind: "file",
          sourceConceptRef: "v1:library:file:f-1",
        }),
      ],
      files: [fileRow({ id: "f-1", archived: true })],
    });
    h.connection = connection;
    await renderBin();

    await click(screen.getByRole("button", { name: /cut-03\.mov/ }));
    await click(screen.getByRole("button", { name: "Restore" }));

    expect(connection.callsNamed("restoreArtifact")).toHaveLength(1);
    expect(connection.callsNamed("restoreLibraryFile")).toHaveLength(1);
    // Through the REAL generated builder, which is the thing that turns
    // arguments into the MemQL text the engine parses.
    expect(connection.callsNamed("restoreArtifact")[0]).toContain('artifactId: "a-1"');
    expect(connection.callsNamed("restoreLibraryFile")[0]).toContain('fileId: "f-1"');
  });

  it("renders a refusal in surface and leaves the item where it was", async () => {
    const connection = fakeConnection({
      archived: [artifactRow({ id: "a-1", title: "cut-03.mov", archived: true })],
      refuse: { restoreArtifact: "that row is not yours" },
    });
    h.connection = connection;
    await renderBin();

    await click(screen.getByRole("button", { name: /cut-03\.mov/ }));
    await click(screen.getByRole("button", { name: "Restore" }));

    expect(screen.getByText("The restore was refused.")).toBeTruthy();
    // The server's own sentence, verbatim -- a paraphrase would drop the one
    // fact that helps.
    expect(screen.getByText(/that row is not yours/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /cut-03\.mov/ })).toBeTruthy();
  });

  it("runs only the folder mutation for a folder, and says it comes back empty", async () => {
    const connection = fakeConnection({
      archived: [],
      archivedFolders: [folderRow({ id: "fo-1", name: "Old campaign", archived: true })],
    });
    h.connection = connection;
    await renderBin();

    await click(screen.getByRole("button", { name: /Old campaign/ }));
    expect(screen.getByText(/Anything that was inside it stays here/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "Restore" }));

    expect(connection.callsNamed("restoreLibraryFolder")).toHaveLength(1);
    expect(connection.callsNamed("restoreArtifact")).toHaveLength(0);
  });
});

describe("the detail panel", () => {
  it("leads with where the file came from, and names the machine and the path", async () => {
    h.connection = fakeConnection({
      archived: [
        artifactRow({
          id: "a-1",
          title: "cut-03.mov",
          archived: true,
          kind: "file",
          sourceConceptRef: "v1:library:file:f-1",
          producedByWorkerId: "wrk-1",
          producedByWorkerName: "MacBook-Pro",
        }),
      ],
      files: [
        fileRow({
          id: "f-1",
          archived: true,
          uploadedFromWorkerId: "wrk-1",
          uploadedFromWorkerName: "MacBook-Pro",
          uploadedFromPath: "/Users/a/Clients/acme/cut-03.mov",
          linkState: "origin_gone",
        }),
      ],
    });
    await renderBin();
    await click(screen.getByRole("button", { name: /cut-03\.mov/ }));

    // ONE statement of provenance, at the highest fidelity available: where a
    // machine is named, the block replaces the sentence rather than repeating
    // it. The row in the list still carries the sentence.
    const panel = screen.getByLabelText("Where this came from");
    expect(within(panel).getByText("MacBook-Pro")).toBeTruthy();
    expect(within(panel).getByText("/Users/a/Clients/acme/cut-03.mov")).toBeTruthy();
    // The state says what happened at the ORIGIN, and says the copy is fine.
    expect(within(panel).getByText(/No longer at that path on that machine/)).toBeTruthy();
    expect(
      within(screen.getByLabelText("Archived item details")).queryByText("Uploaded from MacBook-Pro"),
    ).toBeNull();
  });

  it("offers no machine block for something no machine ever touched", async () => {
    h.connection = fakeConnection({
      archived: [artifactRow({ id: "a-1", title: "notes.txt", archived: true })],
      files: [fileRow({ id: "a-1" })],
    });
    await renderBin();
    await click(screen.getByRole("button", { name: /notes\.txt/ }));
    expect(screen.queryByLabelText("Where this came from")).toBeNull();
  });
});

describe("the settings section", () => {
  it("accounts for the retention control it does not have", async () => {
    // An absent control with nothing said about it reads as something nobody
    // got round to building. Retention is explicitly out of scope, so the
    // section says so rather than staying silent.
    h.connection = fakeConnection({});
    await renderBin({ section: "settings" });
    expect(screen.getByText(/There is no automatic cleanup, no expiry and no size limit/)).toBeTruthy();
  });

  it("offers the SAME confirm setting the Files app carries, not a copy", async () => {
    h.connection = fakeConnection({});
    await renderBin({ section: "settings", settings: { confirmBeforeArchive: false } });
    const check = screen.getByRole("checkbox", { name: /Ask before archiving/ });
    expect((check as HTMLInputElement).checked).toBe(false);
    expect(screen.getByText(/changing it here changes it there/)).toBeTruthy();
  });
});
