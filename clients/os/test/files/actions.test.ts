import { describe, expect, it, vi } from "vitest";

import {
  BUFFER_LIMIT_BYTES,
  OVER_LIMIT_SENTENCE,
  planDownload,
  runBufferedDownload,
} from "../../src/apps/files/actions/download";
import {
  planArchive,
  runArchiveWalk,
  subtreeHoldsArtifact,
} from "../../src/apps/files/actions/archive";
import { foldFolderTree } from "../../src/apps/files/fold";
import { artifactFromRow, type ArtifactRow, type FolderRow } from "../../src/apps/files/rows";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The row actions (design D13 client half + B5): decisions as pure functions,
// executors at the fetch/mutation boundary with everything injectable.

/** A live folder row. `deleted` is spelled out rather than defaulted away
 *  because the whole point of these cases is which of the two dispositions a
 *  folder ends up taking. */
function folder(id: string, name: string, parentFolderId = ""): FolderRow {
  return { id, name, parentFolderId, archived: false, deleted: false };
}

function content(over: Partial<Row> & { id: string }): ArtifactRow {
  return artifactFromRow({
    lens: "artifact",
    kind: "file",
    source: "uploaded",
    title: over.id,
    labels: [],
    archived: false,
    createdAt: "2026-08-20T10:00:00Z",
    ...over,
  } as Row);
}

describe("planDownload", () => {
  it("streams through the service worker when one is available, at any size", () => {
    expect(planDownload({ workerAvailable: true, sizeBytes: 8 * 1024 ** 3 })).toEqual({
      path: "worker",
    });
  });

  it("buffers below the limit when no worker is available", () => {
    expect(planDownload({ workerAvailable: false, sizeBytes: 100 })).toEqual({ path: "buffered" });
    expect(planDownload({ workerAvailable: false, sizeBytes: BUFFER_LIMIT_BYTES })).toEqual({
      path: "buffered",
    });
  });

  it("refuses above the limit with the sentence naming the limit and the alternatives", () => {
    const plan = planDownload({ workerAvailable: false, sizeBytes: BUFFER_LIMIT_BYTES + 1 });
    expect(plan.path).toBe("refused");
    expect(OVER_LIMIT_SENTENCE).toContain("512 MiB");
    expect(OVER_LIMIT_SENTENCE).toContain("VS Code");
    expect(OVER_LIMIT_SENTENCE).toContain("cockpit");
  });

  it("buffers an unknown size rather than refusing what it cannot measure", () => {
    expect(planDownload({ workerAvailable: false, sizeBytes: 0 })).toEqual({ path: "buffered" });
  });
});

describe("runBufferedDownload", () => {
  const ports = () => {
    const revoked: string[] = [];
    const saved: Array<{ url: string; name: string }> = [];
    return {
      revoked,
      saved,
      createObjectUrl: vi.fn(() => "blob:one"),
      revokeObjectUrl: (url: string) => void revoked.push(url),
      save: (url: string, name: string) => void saved.push({ url, name }),
    };
  };

  it("fetches with the bearer, saves under the file's name, and revokes the object URL", async () => {
    const p = ports();
    const fetchImpl = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer tok-1");
      return new Response(new Blob(["bytes"]), { status: 200 });
    });
    await runBufferedDownload({
      artifactId: "a-1",
      fileName: "report.pdf",
      bearer: async () => "tok-1",
      fetchImpl: fetchImpl as typeof fetch,
      ...p,
    });
    expect(fetchImpl).toHaveBeenCalledOnce();
    expect(String(fetchImpl.mock.calls[0]?.[0])).toContain("/artifacts/a-1/content");
    expect(p.saved).toEqual([{ url: "blob:one", name: "report.pdf" }]);
    expect(p.revoked).toEqual(["blob:one"]);
  });

  it("throws the refusal for a non-2xx so the surface can render it with retry", async () => {
    const p = ports();
    const fetchImpl = vi.fn(async () => new Response("no such artifact", { status: 404 }));
    await expect(
      runBufferedDownload({
        artifactId: "a-1",
        fileName: "report.pdf",
        bearer: async () => "tok-1",
        fetchImpl: fetchImpl as typeof fetch,
        ...p,
      }),
    ).rejects.toThrow("no such artifact");
    expect(p.saved).toEqual([]);
    expect(p.revoked).toEqual([]);
  });
});

describe("planArchive", () => {
  const tree = foldFolderTree([
    folder("f-top", "Top"),
    folder("f-mid", "Mid", "f-top"),
  ]);
  const rows = [
    content({ id: "a-1", folderId: "f-top" }),
    content({ id: "a-2", folderId: "f-mid" }),
    content({ id: "a-3", folderId: "f-mid", archived: true }),
    content({ id: "a-out" }),
  ];

  it("names the live count the confirm shows and lists contents before folders, leaves inward", () => {
    const plan = planArchive(tree, rows, "f-top");
    // The archived row is not re-archived and not counted -- the confirm
    // recomputes from live rows, which is what makes re-running idempotent.
    expect(plan.itemCount).toBe(2);
    expect(plan.artifactIds).toEqual(["a-2", "a-1"]);
    expect(plan.folderIds).toEqual(["f-mid", "f-top"]);
    // Both hold files, so both archive and nothing is deleted.
    expect(plan.archiveFolderIds).toEqual(["f-mid", "f-top"]);
    expect(plan.deleteFolderIds).toEqual([]);
  });
});

// The empty-tree rule: archiving exists so a person can get something back,
// and a folder with no file anywhere beneath it has nothing to offer them.
// Those folders are DELETED, and the partition is what says so.
describe("planArchive dispositions", () => {
  it("deletes an empty leaf folder rather than archiving it", () => {
    const tree = foldFolderTree([folder("f-empty", "Empty")]);
    const plan = planArchive(tree, [content({ id: "a-out" })], "f-empty");
    expect(plan.itemCount).toBe(0);
    expect(plan.archiveFolderIds).toEqual([]);
    expect(plan.deleteFolderIds).toEqual(["f-empty"]);
    expect(subtreeHoldsArtifact(tree, [content({ id: "a-out" })], "f-empty")).toBe(false);
  });

  it("deletes a whole tree of empty folders and archives none of it", () => {
    const tree = foldFolderTree([
      folder("f-a", "A"),
      folder("f-b", "B", "f-a"),
      folder("f-c", "C", "f-b"),
    ]);
    const plan = planArchive(tree, [], "f-a");
    expect(plan.artifactIds).toEqual([]);
    expect(plan.archiveFolderIds).toEqual([]);
    // Children first, the root last -- the delete half keeps the walk order.
    expect(plan.deleteFolderIds).toEqual(["f-c", "f-b", "f-a"]);
    expect(subtreeHoldsArtifact(tree, [], "f-a")).toBe(false);
  });

  it("archives a folder holding one file, and deletes its empty siblings", () => {
    const tree = foldFolderTree([
      folder("f-top", "Top"),
      folder("f-has", "Has", "f-top"),
      folder("f-none", "None", "f-top"),
    ]);
    const rows = [content({ id: "a-1", folderId: "f-has" })];
    const plan = planArchive(tree, rows, "f-top");
    expect(plan.artifactIds).toEqual(["a-1"]);
    // The root archives too: one file anywhere beneath it is enough.
    expect(plan.archiveFolderIds).toEqual(["f-has", "f-top"]);
    expect(plan.deleteFolderIds).toEqual(["f-none"]);
    expect(subtreeHoldsArtifact(tree, rows, "f-has")).toBe(true);
    expect(subtreeHoldsArtifact(tree, rows, "f-none")).toBe(false);
    expect(subtreeHoldsArtifact(tree, rows, "f-top")).toBe(true);
  });

  it("archives a folder whose only file is ALREADY archived", () => {
    // An archived file is still a file somebody can restore, so the folder
    // above it is not empty -- and this is also what keeps a walk interrupted
    // between its artifact and folder phases consistent on the re-run.
    const tree = foldFolderTree([
      folder("f-top", "Top"),
      folder("f-deep", "Deep", "f-top"),
    ]);
    const rows = [content({ id: "a-1", folderId: "f-deep", archived: true })];
    const plan = planArchive(tree, rows, "f-top");
    // Nothing live to archive, and still no delete: the folders stay reachable.
    expect(plan.itemCount).toBe(0);
    expect(plan.artifactIds).toEqual([]);
    expect(plan.archiveFolderIds).toEqual(["f-deep", "f-top"]);
    expect(plan.deleteFolderIds).toEqual([]);
    expect(subtreeHoldsArtifact(tree, rows, "f-top")).toBe(true);
  });
});

describe("runArchiveWalk", () => {
  it("archives artifacts then folders and reports progress", async () => {
    const calls: string[] = [];
    const onProgress = vi.fn();
    await runArchiveWalk(
      {
        artifactIds: ["a-2", "a-1"],
        folderIds: ["f-mid", "f-top"],
        archiveFolderIds: ["f-mid", "f-top"],
        deleteFolderIds: [],
        itemCount: 2,
      },
      {
        archiveArtifact: async (id) => void calls.push(`artifact:${id}`),
        archiveFolder: async (id) => void calls.push(`folder:${id}`),
        deleteFolder: async (id) => void calls.push(`delete:${id}`),
        onProgress,
      },
    );
    expect(calls).toEqual(["artifact:a-2", "artifact:a-1", "folder:f-mid", "folder:f-top"]);
    expect(onProgress).toHaveBeenLastCalledWith(4, 4);
  });

  it("sends each folder to its own port, children first, in one interleaved pass", async () => {
    // f-empty is deleted while its parent archives, and it goes FIRST: the
    // two dispositions share one ordered pass precisely so children-first
    // survives the split.
    const calls: string[] = [];
    await runArchiveWalk(
      {
        artifactIds: ["a-1"],
        folderIds: ["f-empty", "f-has", "f-top"],
        archiveFolderIds: ["f-has", "f-top"],
        deleteFolderIds: ["f-empty"],
        itemCount: 1,
      },
      {
        archiveArtifact: async (id) => void calls.push(`artifact:${id}`),
        archiveFolder: async (id) => void calls.push(`folder:${id}`),
        deleteFolder: async (id) => void calls.push(`delete:${id}`),
        onProgress: () => {},
      },
    );
    expect(calls).toEqual(["artifact:a-1", "delete:f-empty", "folder:f-has", "folder:f-top"]);
  });

  it("stops at the first refusal, leaving the remainder for an idempotent re-run", async () => {
    const calls: string[] = [];
    await expect(
      runArchiveWalk(
        {
          artifactIds: ["a-2", "a-1"],
          folderIds: ["f-top"],
          archiveFolderIds: ["f-top"],
          deleteFolderIds: [],
          itemCount: 2,
        },
        {
          archiveArtifact: async (id) => {
            if (id === "a-1") throw new Error("the cluster refused");
            calls.push(`artifact:${id}`);
          },
          archiveFolder: async (id) => void calls.push(`folder:${id}`),
          deleteFolder: async (id) => void calls.push(`delete:${id}`),
          onProgress: () => {},
        },
      ),
    ).rejects.toThrow("the cluster refused");
    // a-2 landed; a-1 and the folder wait for the re-run, which recomputes
    // from live rows and archives only the remainder.
    expect(calls).toEqual(["artifact:a-2"]);
  });

  it("re-running after a partial run does only the remainder, deletes included", async () => {
    // The interruption lands mid folder phase: f-empty was deleted, f-has and
    // f-top were not reached. The engine's folder reads exclude both archived
    // and deleted rows, so the re-run's tree simply does not carry f-empty --
    // which is the whole of the idempotency, and why the plan holds no state.
    const folders = [
      folder("f-top", "Top"),
      folder("f-has", "Has", "f-top"),
      folder("f-empty", "Empty", "f-top"),
    ];
    const rows = [content({ id: "a-1", folderId: "f-has" })];

    const first: string[] = [];
    await expect(
      runArchiveWalk(planArchive(foldFolderTree(folders), rows, "f-top"), {
        archiveArtifact: async (id) => void first.push(`artifact:${id}`),
        archiveFolder: async (id) => {
          throw new Error(`interrupted before folder:${id}`);
        },
        deleteFolder: async (id) => void first.push(`delete:${id}`),
        onProgress: () => {},
      }),
    ).rejects.toThrow("interrupted before folder:f-has");
    expect(first).toEqual(["artifact:a-1", "delete:f-empty"]);

    // What the cluster now returns: f-empty gone from every folder read, and
    // a-1 archived rather than absent.
    const second: string[] = [];
    const remainder = planArchive(
      foldFolderTree(folders.filter((f) => f.id !== "f-empty")),
      [content({ id: "a-1", folderId: "f-has", archived: true })],
      "f-top",
    );
    expect(remainder.artifactIds).toEqual([]);
    expect(remainder.deleteFolderIds).toEqual([]);
    await runArchiveWalk(remainder, {
      archiveArtifact: async (id) => void second.push(`artifact:${id}`),
      archiveFolder: async (id) => void second.push(`folder:${id}`),
      deleteFolder: async (id) => void second.push(`delete:${id}`),
      onProgress: () => {},
    });
    expect(second).toEqual(["folder:f-has", "folder:f-top"]);
  });
});
