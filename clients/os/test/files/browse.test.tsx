import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real LiveCollection
// retain/seed path runs against the harness's executeNamed fake. Default
// null; each test sets what it needs.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { ARTIFACT_CONCEPT } from "../../src/apps/files/concepts";
import { artifactRow, click, emit, fakeConnection, folderRow, renderFiles } from "./harness";

// The browse surface (epic #4722): live rows with the arrival cue in both
// directions, the tree, the filters, the inspector's stories and actions.

beforeEach(() => {
  h.connection = null;
});

function arrivalOf(name: string): string | null {
  const row = screen.getByRole("button", { name: new RegExp(name) });
  return row.closest("li")?.getAttribute("data-arrival") ?? null;
}

describe("the live list and its cue", () => {
  it("renders seeded rows and announces a NEW row with the rise-and-tick cue", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    h.connection = connection;
    await renderFiles();
    expect(screen.getByRole("button", { name: /brief\.pdf/ })).toBeTruthy();
    expect(arrivalOf("brief\\.pdf")).toBeNull();

    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-2", title: "landed.mp4", createdAt: "2026-08-23T10:00:00Z" }),
      "NODE_CREATED",
    );
    expect(arrivalOf("landed\\.mp4")).toBe("added");
    expect(screen.getByText("new")).toBeTruthy();
  });

  it("pulses once on an analysis result landing and stays silent on timestamp churn", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf", updatedAt: "t1" })],
    });
    h.connection = connection;
    await renderFiles();

    // Timestamp-only churn: same fingerprint, no pulse -- the strobe rule.
    await emit(connection, ARTIFACT_CONCEPT, artifactRow({ id: "a-1", title: "brief.pdf", updatedAt: "t2" }));
    expect(arrivalOf("brief\\.pdf")).toBeNull();

    // The analysis result lands (summary): that IS news.
    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-1", title: "brief.pdf", summary: "A quarterly brief." }),
    );
    expect(arrivalOf("brief\\.pdf")).toBe("updated");
  });

  it("never renders a records-lens row, seeded or arriving", async () => {
    const connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-1", title: "brief.pdf" }),
        artifactRow({ id: "a-note", lens: "record", kind: "note", title: "standup notes" }),
      ],
    });
    h.connection = connection;
    await renderFiles();
    expect(screen.queryByText(/standup notes/)).toBeNull();
    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-todo", lens: "record", kind: "todo", title: "chore list" }),
      "NODE_CREATED",
    );
    expect(screen.queryByText(/chore list/)).toBeNull();
  });

  it("distinguishes empty from filtered-to-empty", async () => {
    h.connection = fakeConnection({ artifacts: [] });
    await renderFiles();
    expect(screen.getByText(/Nothing in your Library yet/)).toBeTruthy();

    // The search lives behind the Refine affordance now (DESIGN.md rule 2):
    // collapsed over an empty library, one click away when asked for.
    await click(screen.getByRole("button", { name: "Refine files" }));
    const search = screen.getByPlaceholderText("Search") as HTMLInputElement;
    fireEvent.change(search, { target: { value: "nope" } });
    expect(screen.getByText(/Nothing matches/)).toBeTruthy();
  });

  it("keeps archived rows out of the Library and lists them under the Bin place", async () => {
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({ id: "a-live", title: "live.bin" }),
        artifactRow({ id: "a-old", title: "old.zip", archived: true }),
      ],
    });
    await renderFiles();
    expect(screen.queryByText(/old\.zip/)).toBeNull();
    await click(screen.getByRole("button", { name: /^Bin/ }));
    const row = screen.getByRole("button", { name: /old\.zip/ });
    // No "archived" chip inside the Bin place -- every row there is,
    // and a chip stating the place would be furniture (rule 7).
    expect(within(row).queryByText("archived")).toBeNull();
    expect(screen.queryByText(/live\.bin/)).toBeNull();
  });
});

describe("the folder rail", () => {
  it("nests folders, scopes the list on click, and files wait behind their folder", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-vid", name: "Client videos" })],
      artifacts: [
        artifactRow({ id: "a-root", title: "root.txt" }),
        artifactRow({ id: "a-filed", title: "filed.mp4", folderId: "f-vid" }),
      ],
    });
    await renderFiles();
    expect(screen.queryByText(/filed\.mp4/)).toBeNull();
    // The places start shut, so a folder is something you go and open. The
    // disclosure is a separate control from the destination on purpose --
    // see Rail.tsx.
    await click(screen.getByRole("button", { name: "Expand Library" }));
    await click(screen.getByRole("button", { name: /Client videos/ }));
    expect(screen.getByRole("button", { name: /filed\.mp4/ })).toBeTruthy();
    expect(screen.queryByText(/root\.txt/)).toBeNull();
  });

  it("archives a folder through the confirm that names the live count", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-vid", name: "Client videos" })],
      artifacts: [
        artifactRow({ id: "a-1", title: "one.mp4", folderId: "f-vid" }),
        artifactRow({ id: "a-2", title: "two.mp4", folderId: "f-vid" }),
      ],
    });
    h.connection = connection;
    await renderFiles();
    await click(screen.getByRole("button", { name: "Expand Library" }));
    fireEvent.contextMenu(screen.getByRole("button", { name: /Client videos/ }));
    await click(screen.getByRole("menuitem", { name: "Archive" }));
    expect(screen.getByText(/Archive "Client videos" and its 2 items\?/)).toBeTruthy();
    // The rail's place is named Bin now, but the ROW menu's own verb is
    // still Archive, so the
    // confirm's own action is reached inside its notice.
    const confirm = screen
      .getByText(/Archive "Client videos" and its 2 items\?/)
      .closest(".os-notice") as HTMLElement;
    await click(within(confirm).getByRole("button", { name: "Archive" }));
    // Contents first, then the folder -- the children-first walk, as real
    // rendered calls.
    expect(connection.callsNamed("archiveArtifact")).toEqual([
      'mutation archiveArtifact(artifactId: "a-1")',
      'mutation archiveArtifact(artifactId: "a-2")',
    ]);
    expect(connection.callsNamed("archiveLibraryFolder")).toEqual([
      'mutation archiveLibraryFolder(folderId: "f-vid")',
    ]);
  });
});

describe("the inspector", () => {
  // The panel's own re-filing picker is gone -- re-filing is something you do
  // TO a row, so it lives on the row's context menu. test/files/inspector.test.tsx
  // pins its absence; what stays here is the lead the panel is built around.
  it("tells the provenance story", async () => {
    const connection = fakeConnection({
      folders: [folderRow({ id: "f-vid", name: "Client videos" })],
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    h.connection = connection;
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const inspector = screen.getByRole("complementary", { name: "File details" });
    expect(within(inspector).getByText("Uploaded here")).toBeTruthy();
  });

  it("renders an archive refusal verbatim, in surface", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
      refuse: { archiveArtifact: "the cluster refused: archived rows keep counting" },
    });
    h.connection = connection;
    await renderFiles({ settings: { confirmBeforeArchive: false } });
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const inspector = screen.getByRole("complementary", { name: "File details" });
    await click(within(inspector).getByRole("button", { name: /Archive/ }));
    expect(
      within(inspector).getByText("the cluster refused: archived rows keep counting"),
    ).toBeTruthy();
  });

  it("asks before archiving when the setting says so, and a cancel is a no-op", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "brief.pdf" })],
    });
    h.connection = connection;
    await renderFiles();
    await click(screen.getByRole("button", { name: /brief\.pdf/ }));
    const inspector = screen.getByRole("complementary", { name: "File details" });
    await click(within(inspector).getByRole("button", { name: /Archive/ }));
    expect(within(inspector).getByText(/Archive "brief\.pdf"\?/)).toBeTruthy();
    await click(within(inspector).getByRole("button", { name: "Cancel" }));
    expect(connection.callsNamed("archiveArtifact")).toEqual([]);
  });
});
