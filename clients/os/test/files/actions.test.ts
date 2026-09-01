import { describe, expect, it, vi } from "vitest";

import {
  BUFFER_LIMIT_BYTES,
  OVER_LIMIT_SENTENCE,
  planDownload,
  runBufferedDownload,
} from "../../src/apps/files/actions/download";
import { planArchive, runArchiveWalk } from "../../src/apps/files/actions/archive";
import { foldFolderTree } from "../../src/apps/files/fold";
import { artifactFromRow, type ArtifactRow } from "../../src/apps/files/rows";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// The row actions (design D13 client half + B5): decisions as pure functions,
// executors at the fetch/mutation boundary with everything injectable.

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
    { id: "f-top", name: "Top", parentFolderId: "", archived: false },
    { id: "f-mid", name: "Mid", parentFolderId: "f-top", archived: false },
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
        itemCount: 2,
      },
      {
        archiveArtifact: async (id) => void calls.push(`artifact:${id}`),
        archiveFolder: async (id) => void calls.push(`folder:${id}`),
        onProgress,
      },
    );
    expect(calls).toEqual(["artifact:a-2", "artifact:a-1", "folder:f-mid", "folder:f-top"]);
    expect(onProgress).toHaveBeenLastCalledWith(4, 4);
  });

  it("stops at the first refusal, leaving the remainder for an idempotent re-run", async () => {
    const calls: string[] = [];
    await expect(
      runArchiveWalk(
        { artifactIds: ["a-2", "a-1"], folderIds: ["f-top"], itemCount: 2 },
        {
          archiveArtifact: async (id) => {
            if (id === "a-1") throw new Error("the cluster refused");
            calls.push(`artifact:${id}`);
          },
          archiveFolder: async (id) => void calls.push(`folder:${id}`),
          onProgress: () => {},
        },
      ),
    ).rejects.toThrow("the cluster refused");
    // a-2 landed; a-1 and the folder wait for the re-run, which recomputes
    // from live rows and archives only the remainder.
    expect(calls).toEqual(["artifact:a-2"]);
  });
});
