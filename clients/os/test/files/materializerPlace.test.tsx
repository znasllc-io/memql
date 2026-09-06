import { fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import {
  artifactRow,
  click,
  fakeConnection,
  folderRow,
  registryWith,
  renderFiles,
} from "./harness";
import { Sparkles } from "lucide-react";
import { MATERIALIZER_APP } from "../../src/apps/files/materializer";
import type { OsAppManifest } from "../../src/system/registry";

// The Materializer place (epic memql#4981, #4983): the rail's fourth place,
// over the files a composition produced.
//
// THE SEAM IS WHAT THESE PIN. The Materializer app owns
// `v1:compose:composition`; this place is over the FILES those compositions
// produced, which are ordinary Library artifacts. So the cases below are
// mostly about the join and about the boundary -- what Files shows, what it
// refuses to show, and the one act that hands a person over.

beforeEach(() => {
  h.connection = null;
});

/** A composition row, as `compositions` answers it. */
function compositionRow(over: Record<string, unknown> & { id: string }) {
  return {
    ownerUserId: "u-me",
    name: over.name ?? "A composition",
    statement: "",
    status: "ready",
    format: "pdf",
    templateId: "",
    outputFileId: "",
    folderId: "",
    accountIds: [],
    goalId: "",
    runId: "",
    recipeId: "",
    provenanceEmbedded: true,
    provenanceNote: "",
    deployableKind: "",
    failureReason: "",
    archived: false,
    createdAt: "2026-09-05T10:00:00Z",
    ...over,
  };
}

/** An artifact whose backing file is `fileId` -- the join the place runs. */
function outputArtifact(over: { id: string; title: string; fileId: string; folderId?: string; archived?: boolean }) {
  return artifactRow({
    id: over.id,
    title: over.title,
    sourceConceptRef: `v1:library:file:${over.fileId}`,
    ...(over.folderId !== undefined ? { folderId: over.folderId } : {}),
    ...(over.archived !== undefined ? { archived: over.archived } : {}),
  });
}

function railGroup(): HTMLElement {
  const rail = screen.getByRole("navigation", { name: "Places and folders" });
  const group = rail.querySelector("#os-files-place-materializer");
  if (!group) throw new Error("the Materializer place has no group in the rail");
  return group as HTMLElement;
}

describe("the Materializer place", () => {
  it("is a permanent place, empty or not, and says what the Materializer is for", async () => {
    // The three places above it are LOCATIONS rather than results. A fourth
    // that came and went with the data would make the rail's shape depend on
    // what happens to be in it, and there would be nowhere to find out the
    // feature exists.
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "plain.txt" })] });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Materializer/ }));
    expect(
      screen.getByText(/The Materializer composes a file .* out of what is in the memory graph/),
    ).toBeTruthy();
  });

  it("never says nothing was materialized while it is showing files", async () => {
    // THE BIN'S ORIGINAL FALSEHOOD, which this epic exists to remove, is easy
    // to rebuild in a new place: the rail lists FOLDERS, so its emptiness is
    // about folders, and an output filed at the Library root leaves the group
    // empty while the place is full. Three states, three honest answers.
    h.connection = fakeConnection({
      artifacts: [outputArtifact({ id: "a-1", title: "loose.md", fileId: "f-1" })],
      compositions: [compositionRow({ id: "c-1", outputFileId: "f-1" })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Materializer/ }));
    expect(within(railGroup()).queryByText("Nothing has been materialized yet.")).toBeNull();
    expect(within(railGroup()).getByText("None of these are in a folder.")).toBeTruthy();
    // ...and the file is there to prove the line was about folders.
    expect(screen.getByRole("button", { name: /loose\.md/ })).toBeTruthy();
  });

  it("says nothing was materialized only where nobody else is saying it", async () => {
    h.connection = fakeConnection({ artifacts: [artifactRow({ id: "a-1", title: "plain.txt" })] });
    await renderFiles();

    // Peeking in from the Library: the rail's line is the whole answer.
    await click(screen.getByRole("button", { name: "Expand Materializer" }));
    expect(within(railGroup()).getByText("Nothing has been materialized yet.")).toBeTruthy();

    // Standing in it, the list carries the sentence with the part that
    // matters, so the rail stands down (DESIGN.md rule 7).
    await click(screen.getByRole("button", { name: /^Materializer/ }));
    expect(within(railGroup()).queryByText("Nothing has been materialized yet.")).toBeNull();
  });

  it("holds the files a composition produced, and nothing else", async () => {
    h.connection = fakeConnection({
      artifacts: [
        outputArtifact({ id: "a-made", title: "Q3 report.pdf", fileId: "f-1" }),
        artifactRow({ id: "a-plain", title: "uploaded.txt" }),
      ],
      compositions: [compositionRow({ id: "c-1", name: "Q3 report", outputFileId: "f-1" })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Materializer/ }));
    expect(screen.getByRole("button", { name: /Q3 report\.pdf/ })).toBeTruthy();
    expect(screen.queryByText(/uploaded\.txt/)).toBeNull();
  });

  it("lists the FOLDERS its outputs are filed in, not the files", async () => {
    // Unlike the Bin, this place has the Library's shape -- ordinary files in
    // ordinary folders -- so opening it scopes the list to exactly the rows
    // the rail would otherwise be repeating, narrower.
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-rep", name: "Reports" })],
      artifacts: [
        outputArtifact({ id: "a-1", title: "Q3 report.pdf", fileId: "f-1", folderId: "f-rep" }),
        outputArtifact({ id: "a-2", title: "loose.md", fileId: "f-2" }),
      ],
      compositions: [
        compositionRow({ id: "c-1", outputFileId: "f-1" }),
        compositionRow({ id: "c-2", outputFileId: "f-2" }),
      ],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Materializer/ }));
    const folder = within(railGroup()).getByRole("button", { name: /^Reports/ });
    expect(within(folder).getByText("1")).toBeTruthy();
    // The files are the list's, at every level of the rail.
    expect(within(railGroup()).queryByRole("button", { name: /Q3 report\.pdf/ })).toBeNull();
    expect(within(railGroup()).queryByRole("button", { name: /loose\.md/ })).toBeNull();

    // Scoping into the folder narrows to what is in it.
    await click(folder);
    expect(screen.getByRole("button", { name: /Q3 report\.pdf/ })).toBeTruthy();
    expect(screen.queryByText(/loose\.md/)).toBeNull();
  });

  it("counts every live output, including the ones filed nowhere", async () => {
    h.connection = fakeConnection({
      folders: [folderRow({ id: "f-rep", name: "Reports" })],
      artifacts: [
        outputArtifact({ id: "a-1", title: "one.pdf", fileId: "f-1", folderId: "f-rep" }),
        outputArtifact({ id: "a-2", title: "two.md", fileId: "f-2" }),
      ],
      compositions: [
        compositionRow({ id: "c-1", outputFileId: "f-1" }),
        compositionRow({ id: "c-2", outputFileId: "f-2" }),
      ],
    });
    await renderFiles();

    const place = screen.getByRole("button", { name: /^Materializer/ });
    expect(within(place).getByText("2")).toBeTruthy();
    expect(within(place).getByTitle("2 files made in the Materializer")).toBeTruthy();
  });

  it("leaves an ARCHIVED output to the Bin", async () => {
    // One file offering Restore from two places is the ambiguity the Bin
    // rename removed. This place is about what a person has.
    h.connection = fakeConnection({
      artifacts: [
        outputArtifact({ id: "a-1", title: "old-report.pdf", fileId: "f-1", archived: true }),
      ],
      compositions: [compositionRow({ id: "c-1", outputFileId: "f-1" })],
    });
    await renderFiles();

    await click(screen.getByRole("button", { name: /^Materializer/ }));
    expect(screen.queryByText(/old-report\.pdf/)).toBeNull();

    await click(screen.getByRole("button", { name: /^Bin/ }));
    expect(screen.getByRole("button", { name: /old-report\.pdf/ })).toBeTruthy();
  });

  it("shows nothing for a composition that produced no file", async () => {
    // A draft has not made one yet and a `failed` run never will. This place
    // is over FILES, so there is nothing to show -- and a row for one would
    // offer Open, Download and Move on a file nothing wrote. The failure and
    // its reason belong in the Materialized list, which is the record.
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-plain", title: "unrelated.txt" })],
      compositions: [
        compositionRow({ id: "c-draft", name: "Half-written", status: "draft", outputFileId: "" }),
        compositionRow({
          id: "c-failed",
          name: "Would not render",
          status: "failed",
          outputFileId: "",
          failureReason: "the template was archived",
        }),
      ],
    });
    await renderFiles();

    const place = screen.getByRole("button", { name: /^Materializer/ });
    expect(within(place).queryByText("2")).toBeNull();
    await click(place);
    expect(screen.queryByText(/Half-written/)).toBeNull();
    expect(screen.queryByText(/Would not render/)).toBeNull();
  });
});

/**
 * A stand-in for the Materializer's own manifest.
 *
 * BOTH DIRECTIONS ARE EXPRESSED FROM A FIXTURE, deliberately, rather than
 * from whatever the real registry happens to hold: the "act is absent"
 * case would otherwise stop being tested the day the app lands, and the
 * "act is present" case could not be written before it. What the fixture
 * pins is MY side of the seam -- the app id and the section this app opens,
 * which are `materializer.ts`'s constants and nothing else's.
 */
const MATERIALIZER_MANIFEST: OsAppManifest = {
  id: MATERIALIZER_APP,
  name: "Materializer",
  icon: Sparkles,
  sections: [
    { id: "composer", name: "Composer" },
    { id: "materialized", name: "Materialized" },
    { id: "settings", name: "Settings" },
    { id: "logs", name: "Logs", roles: { min: "admin" } },
  ],
  settingsSection: "settings",
  logsSection: "logs",
  component: () => null,
};

describe("the handoff to the Materializer", () => {
  it("offers Open in Materializer on a file a composition made, and on no other", async () => {
    h.connection = fakeConnection({
      artifacts: [
        outputArtifact({ id: "a-made", title: "Q3 report.pdf", fileId: "f-1" }),
        artifactRow({ id: "a-plain", title: "uploaded.txt" }),
      ],
      compositions: [compositionRow({ id: "c-1", outputFileId: "f-1" })],
    });
    await renderFiles({ registry: registryWith(MATERIALIZER_MANIFEST) });

    fireEvent.contextMenu(screen.getByRole("button", { name: /Q3 report\.pdf/ }));
    expect(
      within(screen.getByRole("menu", { name: "File" })).getByRole("menuitem", {
        name: "Open in Materializer",
      }),
    ).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });

    // An ordinary upload was not composed, so the act is ABSENT rather than
    // disabled (DESIGN.md rule 12).
    fireEvent.contextMenu(screen.getByRole("button", { name: /uploaded\.txt/ }));
    expect(
      within(screen.getByRole("menu", { name: "File" })).queryByRole("menuitem", {
        name: "Open in Materializer",
      }),
    ).toBeNull();
  });

  it("says in the inspector that a file was made there, and nothing about the record", async () => {
    h.connection = fakeConnection({
      artifacts: [outputArtifact({ id: "a-made", title: "Q3 report.pdf", fileId: "f-1" })],
      compositions: [
        compositionRow({ id: "c-1", name: "Q3 report", outputFileId: "f-1", format: "pdf" }),
      ],
    });
    await renderFiles({ registry: registryWith(MATERIALIZER_MANIFEST) });

    await click(screen.getByRole("button", { name: /Q3 report\.pdf/ }));
    const inspector = screen.getByRole("complementary", { name: "File details" });
    expect(within(inspector).getByText("Made in")).toBeTruthy();
    expect(within(inspector).getByRole("button", { name: "Open in Materializer" })).toBeTruthy();
    // The record is the Materializer's. Files does not restate what the file
    // was made FROM -- a second reading of one row is a second answer.
    expect(within(inspector).queryByText(/sources/i)).toBeNull();
    expect(within(inspector).queryByText(/template/i)).toBeNull();
  });

  it("offers nothing at all when the Materializer is not installed", async () => {
    // `openApp` no-ops on an app the registry does not hold, so a control
    // that rendered here would silently do nothing -- worse than one that is
    // not there (DESIGN.md rule 12: an act that is not legal is ABSENT).
    // The FILE is still in the place; only the act to leave for another app
    // depends on that app existing.
    h.connection = fakeConnection({
      artifacts: [outputArtifact({ id: "a-made", title: "Q3 report.pdf", fileId: "f-1" })],
      compositions: [compositionRow({ id: "c-1", outputFileId: "f-1" })],
    });
    await renderFiles({ registry: registryWith(null, MATERIALIZER_APP) });

    await click(screen.getByRole("button", { name: /^Materializer/ }));
    expect(screen.getByRole("button", { name: /Q3 report\.pdf/ })).toBeTruthy();

    fireEvent.contextMenu(screen.getByRole("button", { name: /Q3 report\.pdf/ }));
    expect(
      within(screen.getByRole("menu", { name: "File" })).queryByRole("menuitem", {
        name: "Open in Materializer",
      }),
    ).toBeNull();
    fireEvent.keyDown(document, { key: "Escape" });

    await click(screen.getByRole("button", { name: /Q3 report\.pdf/ }));
    const inspector = screen.getByRole("complementary", { name: "File details" });
    expect(within(inspector).queryByRole("button", { name: "Open in Materializer" })).toBeNull();
  });

  it("reaches the REAL Materializer app, not only the fixture", async () => {
    // The two cases above pin MY side of the seam from a fixture, so neither
    // depends on when the sibling epic lands. This one is the integration:
    // the app id in `materializer.ts` has to be the id the shell's own
    // registry holds, or the act renders and opens nothing.
    h.connection = fakeConnection({
      artifacts: [outputArtifact({ id: "a-made", title: "Q3 report.pdf", fileId: "f-1" })],
      compositions: [compositionRow({ id: "c-1", outputFileId: "f-1" })],
    });
    await renderFiles();

    fireEvent.contextMenu(screen.getByRole("button", { name: /Q3 report\.pdf/ }));
    expect(
      within(screen.getByRole("menu", { name: "File" })).getByRole("menuitem", {
        name: "Open in Materializer",
      }),
    ).toBeTruthy();
  });
});
