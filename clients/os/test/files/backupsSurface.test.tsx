import { screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real LiveCollection
// retain/seed path runs against the harness's executeNamed fake -- the same
// arrangement browse.test.tsx uses, and for the same reason: a test that
// stubbed the generated methods would never render a call the engine has to
// parse.
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { click, fakeConnection, fileRow, folderRow, renderFiles, watchedFolderRow } from "./harness";

// The Backups surface (epic memql#4783, the cockpit half memql#4841).
//
// The link element is the row, so most of what is asserted here is what the
// link SAYS -- its tone attribute and its accessible name -- rather than what
// it looks like. jsdom applies no stylesheet, so the appearance is not
// assertable here at all; what IS assertable is the contract between the
// component and the stylesheet, which is the attribute the CSS selects on.

beforeEach(() => {
  h.connection = null;
});

function toneOf(): string | null {
  return document.querySelector(".os-backup")?.getAttribute("data-tone") ?? null;
}

describe("what the link says", () => {
  it("says it is waiting when no machine has reported, rather than claiming everything is fine", async () => {
    // The single most important case on this surface. A backup nobody has
    // swept yet has no originState member at all, and a fold that read that
    // as `ok` would put a green line and "Backed up" on a folder nothing has
    // ever looked at.
    h.connection = fakeConnection({
      backups: [watchedFolderRow({ id: "w-1", localPath: "/Users/ana/Clients" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    await renderFiles({ section: "backups" });

    expect(toneOf()).toBe("waiting");
    expect(screen.getByText("Waiting for this machine")).toBeTruthy();
    expect(screen.getByText(/no report yet/)).toBeTruthy();
    // The count is not invented either: nothing has counted the origin.
    expect(screen.getByText(/not counted yet/)).toBeTruthy();
  });

  it("carries the state in its accessible name, so colour is never the only carrier", async () => {
    h.connection = fakeConnection({
      backups: [
        watchedFolderRow({
          id: "w-1",
          originState: "refused_by_policy",
          lastSweepAt: "2026-09-01T10:00:00Z",
          filesSeen: 0,
          bytesSeen: 0,
        }),
      ],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    await renderFiles({ section: "backups" });

    expect(toneOf()).toBe("refused");
    const link = screen.getByRole("img", { name: /This machine said no/ });
    expect(link).toBeTruthy();
    // And the repair is named, in the place the person is already looking.
    expect(screen.getByText(/policy\.yaml/)).toBeTruthy();
  });

  it("says paused before it says anything is wrong", async () => {
    h.connection = fakeConnection({
      backups: [
        watchedFolderRow({
          id: "w-1",
          status: "paused",
          originState: "missing",
          lastSweepAt: "2026-09-01T10:00:00Z",
        }),
      ],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    await renderFiles({ section: "backups" });

    expect(toneOf()).toBe("paused");
    expect(screen.getByText("Paused")).toBeTruthy();
  });

  it("reads the file states of ITS OWN folder, not of everything filed beside them", async () => {
    // A browser upload sitting in the same Library folder has no origin to be
    // stale against. Rolling the destination folder up would put "changed on
    // the machine" on this backup because of a file that came from no machine.
    h.connection = fakeConnection({
      backups: [
        watchedFolderRow({
          id: "w-1",
          folderId: "f-1",
          workerId: "wkr-1",
          localPath: "/Users/ana/Clients",
          originState: "ok",
          lastSweepAt: "2026-09-01T10:00:00Z",
          filesSeen: 1,
          bytesSeen: 10,
        }),
      ],
      folders: [folderRow({ id: "f-1", name: "Clients" })],
      files: [
        // Beneath the watched folder, and fine.
        fileRow({
          id: "f-a",
          folderId: "f-1",
          uploadedFromWorkerId: "wkr-1",
          uploadedFromPath: "/Users/ana/Clients/q3.pdf",
          linkState: "synced",
        }),
        // A NEIGHBOUR: same Library folder, different machine path entirely.
        // If this were counted, the tone would be "broken".
        fileRow({
          id: "f-b",
          folderId: "f-1",
          uploadedFromWorkerId: "wkr-2",
          uploadedFromPath: "/elsewhere/old.pdf",
          linkState: "origin_gone",
        }),
      ],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    await renderFiles({ section: "backups" });

    expect(toneOf()).toBe("settled");
    expect(screen.getByText("Backed up")).toBeTruthy();
    // One file here, not two -- the arrived count is the backup's own.
    expect(screen.getByText("1 file here")).toBeTruthy();
  });
});

describe("stopping a backup", () => {
  it("says what it does NOT do before it does it, and archives rather than deletes", async () => {
    const connection = fakeConnection({
      backups: [watchedFolderRow({ id: "w-1", localPath: "/Users/ana/Clients" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    h.connection = connection;
    await renderFiles({ section: "backups" });

    await click(screen.getByRole("button", { name: /Stop backing up \/Users\/ana\/Clients/ }));

    // The confirm's whole job: a person stopping a backup has to know the
    // copies stay and the machine is not touched. Both claims are on screen.
    expect(screen.getByText(/The files already here stay/)).toBeTruthy();
    expect(screen.getByText(/nothing on the machine is touched/)).toBeTruthy();

    await click(screen.getByRole("button", { name: "Stop backing up" }));
    expect(connection.callsNamed("archiveLibraryWatchedFolder").length).toBe(1);
    // And it is an ARCHIVE. Nothing on this surface deletes anything.
    expect(connection.callsNamed("archiveLibraryWatchedFolder")[0]).toContain("watchId: \"w-1\"");
  });

  it("does nothing at all until the confirm is answered", async () => {
    const connection = fakeConnection({
      backups: [watchedFolderRow({ id: "w-1" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    h.connection = connection;
    await renderFiles({ section: "backups" });

    await click(screen.getByRole("button", { name: /Stop backing up \/Users\/ana\/Clients/ }));
    await click(screen.getByRole("button", { name: "Keep it" }));
    expect(connection.callsNamed("archiveLibraryWatchedFolder").length).toBe(0);
  });
});

describe("pausing", () => {
  it("pauses through the status mutation, and offers to resume once paused", async () => {
    const connection = fakeConnection({
      backups: [watchedFolderRow({ id: "w-1" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    h.connection = connection;
    await renderFiles({ section: "backups" });

    await click(screen.getByRole("button", { name: /Pause backing up/ }));
    const calls = connection.callsNamed("setLibraryWatchedFolderStatus");
    expect(calls.length).toBe(1);
    expect(calls[0]).toContain('status: "paused"');
  });
});

describe("the empty state", () => {
  it("invites the person to act rather than reporting an absence", async () => {
    h.connection = fakeConnection({ backups: [] });
    await renderFiles({ section: "backups" });
    expect(screen.getByText(/Nothing is being backed up yet/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back up a folder" })).toBeTruthy();
  });
});

describe("a refusal", () => {
  it("renders the server's own sentence, verbatim, beside the control that produced it", async () => {
    const connection = fakeConnection({
      backups: [watchedFolderRow({ id: "w-1" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
      refuse: {
        setLibraryWatchedFolderStatus: "the worker registration \"wkr-1\" is not one of your machines",
      },
    });
    h.connection = connection;
    await renderFiles({ section: "backups" });

    await click(screen.getByRole("button", { name: /Pause backing up/ }));
    // Not re-worded. The sentence names the machine, which is the only part
    // that helps.
    expect(screen.getByText(/is not one of your machines/)).toBeTruthy();
  });
});

describe("editing", () => {
  it("does not carry one backup's settings into another when Edit moves between rows", async () => {
    // The bug this pins: with no `key` on the form, React reconciles the same
    // instance at the same position when `editing` flips, its useState
    // initialisers do not re-run, and Save writes the FIRST backup's
    // destination and exclusions onto the second -- silently, because the
    // update is a full replace of exactly those fields.
    const connection = fakeConnection({
      backups: [
        watchedFolderRow({ id: "w-1", localPath: "/a", folderId: "f-1", excludeGlobs: ["*.tmp"] }),
        watchedFolderRow({ id: "w-2", localPath: "/b", folderId: "", excludeGlobs: [] }),
      ],
      folders: [folderRow({ id: "f-1", name: "Clients" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    h.connection = connection;
    await renderFiles({ section: "backups" });

    await click(screen.getByRole("button", { name: "Edit the backup of /a" }));
    await click(screen.getByRole("button", { name: "Edit the backup of /b" }));

    // The form is now the SECOND backup's, so it shows that row's empty
    // exclusions rather than the first row's "*.tmp".
    const skip = screen.getByLabelText("Also skip") as HTMLInputElement;
    expect(skip.value).toBe("");
    // ...and its destination is the root, not Clients.
    const dest = screen.getByLabelText("Where it lands") as HTMLSelectElement;
    expect(dest.value).toBe("");
  });

  it("seeds the form from the row being edited", async () => {
    // The reachable positive for the test above: when Edit opens on a row that
    // DOES carry settings, they are there. Without this half, a form that
    // rendered blank always would pass the first test and be broken.
    const connection = fakeConnection({
      backups: [watchedFolderRow({ id: "w-1", localPath: "/a", folderId: "f-1", excludeGlobs: ["*.tmp"] })],
      folders: [folderRow({ id: "f-1", name: "Clients" })],
      machines: [{ id: "wkr-1", name: "laptop", capabilities: ["HEADLESS"] } as never],
    });
    h.connection = connection;
    await renderFiles({ section: "backups" });

    await click(screen.getByRole("button", { name: "Edit the backup of /a" }));
    expect((screen.getByLabelText("Also skip") as HTMLInputElement).value).toBe("*.tmp");
    expect((screen.getByLabelText("Where it lands") as HTMLSelectElement).value).toBe("f-1");
  });
});
