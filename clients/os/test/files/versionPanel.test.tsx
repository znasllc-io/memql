import { act, fireEvent, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// The connection seam, mocked at the MODULE so the real LiveCollection
// retain/seed path runs against the harness's executeNamed fake -- and so the
// version reads go through the REAL generated builders (harness.tsx's header
// says why that matters).
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  OsConnectionProvider: ({ children }: { children: React.ReactNode }) => children,
  useOsConnection: () => h.connection,
}));

import { ARTIFACT_CONCEPT } from "../../src/apps/files/concepts";
import { artifactRow, click, emit, fakeConnection, fileRow, renderFiles, versionRow } from "./harness";
import type { UploadHandle, UploadOptions, UploadProvider, UploadResult } from "../../src/items/upload";

// The version history panel and the upload-new-version action (epic
// memql#4806, #4808).

beforeEach(() => {
  h.connection = null;
});

/** A provider that records what it was asked to do and answers on demand. */
function recordingProvider(): UploadProvider & {
  calls: Array<{ name: string; opts: UploadOptions | undefined }>;
  settle: (result: UploadResult) => void;
  fail: (message: string) => void;
} {
  const calls: Array<{ name: string; opts: UploadOptions | undefined }> = [];
  let resolveDone: ((r: UploadResult) => void) | null = null;
  let rejectDone: ((e: Error) => void) | null = null;
  return {
    calls,
    upload(file: File, opts?: UploadOptions): UploadHandle {
      calls.push({ name: file.name, opts });
      return {
        done: new Promise<UploadResult>((resolve, reject) => {
          resolveDone = resolve;
          rejectDone = reject;
        }),
        abort: () => {},
      };
    },
    settle: (result) => resolveDone?.(result),
    fail: (message) => rejectDone?.(new Error(message)),
  };
}

async function openInspector(title: string) {
  await click(screen.getByRole("button", { name: new RegExp(title) }));
  return screen.getByRole("complementary", { name: "File details" });
}

function arrivalOf(name: string): string | null {
  const row = screen.getByRole("button", { name: new RegExp(name) });
  return row.closest("li")?.getAttribute("data-arrival") ?? null;
}

describe("the version history panel", () => {
  it("shows every version newest-first, with the current one named and each version's own story", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [
        fileRow({
          id: "f-1",
          name: "q3.pdf",
          size: 4096,
          versionNumber: 3,
          sha256: "a".repeat(64),
        }),
      ],
      versions: [
        versionRow({ id: "f-1-v2", fileId: "f-1", versionNumber: 2, size: 2048 }),
        versionRow({
          id: "f-1-v1",
          fileId: "f-1",
          versionNumber: 1,
          size: 1024,
          name: "q3-draft.pdf",
          uploadedFromWorkerId: "wrk-1",
          uploadedFromWorkerName: "MacBook-Pro",
        }),
      ],
    });
    await renderFiles();
    const inspector = await openInspector("q3\\.pdf");
    const history = within(inspector).getByRole("region", { name: "Version history" });

    const numbers = within(history)
      .getAllByText(/^v\d+$/)
      .map((el) => el.textContent);
    expect(numbers).toEqual(["v3", "v2", "v1"]);
    expect(within(history).getByText("current")).toBeTruthy();

    // EACH VERSION TELLS ITS OWN STORY. The one pushed from a machine says so;
    // the two dropped from a browser say "Uploaded here" -- provenance is per
    // version and never inherited.
    expect(within(history).getByText("Uploaded from MacBook-Pro")).toBeTruthy();
    expect(within(history).getAllByText("Uploaded here")).toHaveLength(2);

    // A version that arrived under a different NAME is news; the two that did
    // not are not repeated.
    expect(within(history).getByText("as q3-draft.pdf")).toBeTruthy();

    // Sizes are the version's own, and an unmeasured hash is a dash rather
    // than an error or an empty cell.
    expect(within(history).getByText("4.0 KiB")).toBeTruthy();
    expect(within(history).getByText("1.0 KiB")).toBeTruthy();
    expect(within(history).getAllByText("--").length).toBeGreaterThan(0);
  });

  it("invites the action on a file with one version instead of listing one row", async () => {
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "only.txt", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", name: "only.txt", versionNumber: 1 })],
      versions: [],
    });
    await renderFiles();
    const inspector = await openInspector("only\\.txt");
    const history = within(inspector).getByRole("region", { name: "Version history" });
    expect(within(history).getByText(/One version so far/)).toBeTruthy();
    expect(within(history).getByText(/same row, same folder, same labels/)).toBeTruthy();
  });

  // THE PANEL SAYS WHEN IT LOOKED. These rows carry no broadcast routing rule,
  // so this is a read rather than a feed -- and a surface that implied one
  // would be the lie worth avoiding.
  it("captions when it read, and offers to read again", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", versionNumber: 2 })],
      versions: [versionRow({ id: "f-1-v1", fileId: "f-1", versionNumber: 1 })],
    });
    h.connection = connection;
    await renderFiles();
    const inspector = await openInspector("q3\\.pdf");
    const history = within(inspector).getByRole("region", { name: "Version history" });
    expect(within(history).getByText(/^Read /)).toBeTruthy();

    const before = connection.callsNamed("libraryFileVersionsForFile").length;
    expect(before).toBeGreaterThan(0);
    await click(within(history).getByRole("button", { name: "Read the version history again" }));
    expect(connection.callsNamed("libraryFileVersionsForFile").length).toBe(before + 1);
  });

  it("reads through the real generated builder", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", versionNumber: 2 })],
      versions: [versionRow({ id: "f-1-v1", fileId: "f-1", versionNumber: 1 })],
    });
    h.connection = connection;
    await renderFiles();
    await openInspector("q3\\.pdf");
    expect(connection.callsNamed("libraryFileVersionsForFile")).toContain(
      'query libraryFileVersionsForFile(fileId: "f-1")',
    );
  });

  // A note has no upload versions. Offering an empty history for one would
  // answer a question nobody asked.
  it("shows no panel for a kind that has no upload versions", async () => {
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({
          id: "a-1",
          title: "notes.md",
          kind: "document",
          sourceConceptRef: "v1:knowledge:document:d-1",
        }),
      ],
    });
    await renderFiles();
    const inspector = await openInspector("notes\\.md");
    expect(within(inspector).queryByRole("region", { name: "Version history" })).toBeNull();
    expect(within(inspector).queryByRole("button", { name: /Upload new version/ })).toBeNull();
  });
});

describe("the upload-new-version action", () => {
  it("sends the target through the ONE provider and says which version landed", async () => {
    const provider = recordingProvider();
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", versionNumber: 1 })],
      versions: [],
    });
    h.connection = connection;
    await renderFiles({ uploads: provider });
    const inspector = await openInspector("q3\\.pdf");

    const picker = within(inspector).getByLabelText("Choose a file to upload as the new version");
    const file = new File(["new bytes"], "q3-final.pdf", { type: "application/pdf" });
    await act(async () => {
      fireEvent.change(picker, { target: { files: [file] } });
    });

    // THE PROVIDER IS THE ONLY ROUTE SPEAKER (test/files/onePath.test.ts), so
    // the target is an OPTION on it rather than a second upload path -- which
    // is what gives a new version chunking, resume, retry and verbatim
    // refusals with nothing here knowing about any of them.
    expect(provider.calls).toEqual([
      { name: "q3-final.pdf", opts: { targetArtifactId: "a-1" } },
    ]);

    const readsBefore = connection.callsNamed("libraryFileVersionsForFile").length;
    await act(async () => {
      provider.settle({ artifactId: "a-1", title: "q3-final.pdf", fileKind: "file", source: "uploaded", versionNumber: 2 });
    });
    expect(within(inspector).getByText("Version 2 landed.")).toBeTruthy();
    // The history is read again, because these rows do not arrive on a feed.
    expect(connection.callsNamed("libraryFileVersionsForFile").length).toBe(readsBefore + 1);
  });

  it("renders the cluster's refusal verbatim, in surface, and offers the same bytes again", async () => {
    const provider = recordingProvider();
    h.connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", versionNumber: 1 })],
    });
    await renderFiles({ uploads: provider });
    const inspector = await openInspector("q3\\.pdf");

    const picker = within(inspector).getByLabelText("Choose a file to upload as the new version");
    const file = new File(["x"], "q3-final.pdf", { type: "application/pdf" });
    await act(async () => {
      fireEvent.change(picker, { target: { files: [file] } });
    });
    const refusal =
      "storage quota exceeded: this upload would take your Library to 101 bytes, over the quota of 100 bytes";
    await act(async () => {
      provider.fail(refusal);
    });

    expect(within(inspector).getByText(refusal)).toBeTruthy();
    // The surface says what is TRUE NOW, not only what failed.
    expect(within(inspector).getByText("This file still holds the version it had.")).toBeTruthy();

    // A refusal that made the person re-pick the file they just picked would
    // cost them the thing they were doing.
    await click(within(inspector).getByRole("button", { name: "Try again" }));
    expect(provider.calls).toHaveLength(2);
    expect(provider.calls[1]).toEqual({ name: "q3-final.pdf", opts: { targetArtifactId: "a-1" } });
  });

  // An archived file's action set is deliberately smaller: the artifact is
  // thrown away, and offering to grow it would be offering to un-throw it
  // sideways.
  it("is absent on an archived file", async () => {
    h.connection = fakeConnection({
      artifacts: [
        artifactRow({
          id: "a-1",
          title: "gone.pdf",
          archived: true,
          sourceConceptRef: "v1:library:file:f-1",
        }),
      ],
      files: [fileRow({ id: "f-1", versionNumber: 2 })],
      versions: [versionRow({ id: "f-1-v1", fileId: "f-1", versionNumber: 1 })],
    });
    await renderFiles();
    fireEvent.click(screen.getByRole("button", { name: /^Bin/ }));
    const inspector = await openInspector("gone\\.pdf");
    expect(within(inspector).queryByRole("button", { name: /Upload new version/ })).toBeNull();
    // The HISTORY is still there: an archived file keeps its bytes and its
    // provenance, and the panel is where a person goes to get an earlier copy.
    expect(within(inspector).getByRole("region", { name: "Version history" })).toBeTruthy();
  });
});

describe("the list when a version lands", () => {
  // #4808's acceptance criterion, stated directly: ONE row, pulsed ONCE.
  //
  // What the cluster emits after a supersede is not a create -- it is the SAME
  // artifact row re-stamped from the new head, which is the whole point of the
  // head-on-the-file-row design. A second row here would mean the artifact
  // index had re-pointed, which is the failure the mechanism exists to avoid.
  it("shows one row, pulsed, and no second row", async () => {
    const connection = fakeConnection({
      artifacts: [artifactRow({ id: "a-1", title: "q3.pdf", sourceConceptRef: "v1:library:file:f-1" })],
      files: [fileRow({ id: "f-1", versionNumber: 1 })],
    });
    h.connection = connection;
    await renderFiles();
    expect(screen.getAllByRole("button", { name: /q3/ })).toHaveLength(1);
    expect(arrivalOf("q3\\.pdf")).toBeNull();

    await emit(
      connection,
      ARTIFACT_CONCEPT,
      artifactRow({ id: "a-1", title: "q3-final.pdf", sourceConceptRef: "v1:library:file:f-1" }),
    );

    expect(screen.getAllByRole("button", { name: /q3/ })).toHaveLength(1);
    expect(arrivalOf("q3-final\\.pdf")).toBe("updated");
  });

  // THE STROBE RULE STILL HOLDS, and this is where the fingerprint's one blind
  // spot is written down rather than left to be discovered.
  //
  // `artifactFingerprint` names what a PERSON would call a change -- title,
  // filing, archive, labels, validation, summary, format -- and deliberately
  // not `updatedAt`, which the analysis pass re-stamps on every touch. A new
  // version normally moves at least one of those (a new filename, or the
  // summary the analysis pass writes for the new bytes), so it pulses.
  //
  // It does NOT pulse for a version that changes none of them: the same
  // filename, an opaque type with no summary to rewrite. That is a real gap
  // and it is left open on purpose -- closing it means carrying a version
  // number on the INDEX, duplicating a fact the file row owns, to drive a cue
  // whose only uncovered case already has its confirmation in surface (the
  // panel that took the upload says which version landed and re-reads at
  // once). Naming `updatedAt` instead would strobe the whole list on analysis
  // churn no person can see, which is the failure the fingerprint exists to
  // prevent.
  it("stays silent when a re-stamp changes nothing a person would call a change", async () => {
    const row = { id: "a-1", title: "backup.zip", sourceConceptRef: "v1:library:file:f-1" };
    const connection = fakeConnection({
      artifacts: [artifactRow({ ...row, updatedAt: "t1" })],
      files: [fileRow({ id: "f-1", versionNumber: 1 })],
    });
    h.connection = connection;
    await renderFiles();

    await emit(connection, ARTIFACT_CONCEPT, artifactRow({ ...row, updatedAt: "t2" }));
    expect(arrivalOf("backup\\.zip")).toBeNull();

    // ...and pulses the moment one of them does move.
    await emit(connection, ARTIFACT_CONCEPT, artifactRow({ ...row, title: "backup-2.zip" }));
    expect(arrivalOf("backup-2\\.zip")).toBe("updated");
  });
});
