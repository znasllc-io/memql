import { describe, expect, it, vi } from "vitest";

import { EdgeUploadProvider } from "../../src/items/edgeUpload";
import { InMemoryResumeStore } from "../../src/items/uploadResume";

// An upload whose Library entry is not visible yet: the server answers the
// file id alone. The client resolves the artifact by file id, with a short
// backoff, and never shows the raw "named no artifact".

const file = () => new File([new Uint8Array(10)], "logo.png", { type: "image/png" });
const answer = (body: object) => vi.fn(async () => new Response(JSON.stringify(body), { status: 201 }));

function provider(body: object, resolve: (fileId: string, signal: AbortSignal) => Promise<string | null>, wait = 5, budget = 200) {
  return new EdgeUploadProvider(async () => null, "/_memql/artifacts", answer(body), {
    resume: new InMemoryResumeStore(),
    resolveArtifact: resolve,
    resolveWaitMs: wait,
    resolveBudgetMs: budget,
  });
}

describe("upload answered with a file id only", () => {
  it("resolves on the second lookup and succeeds with that artifact", async () => {
    const resolve = vi.fn<(f: string, s: AbortSignal) => Promise<string | null>>()
      .mockResolvedValueOnce(null).mockResolvedValueOnce("art-9");
    const result = await provider({ fileId: "file-1" }, resolve).upload(file()).done;
    expect(result.artifactId).toBe("art-9");
    expect(result.fileId).toBe("file-1");
    expect(resolve).toHaveBeenCalledTimes(2);
    expect(resolve.mock.calls[0]![0]).toBe("file-1");
  });

  it("gives a plain sentence when it never resolves", async () => {
    const resolve = vi.fn(async () => null);
    const err = await provider({ fileId: "file-1" }, resolve, 5, 40).upload(file()).done.then(() => null, (e: Error) => e);
    expect(err?.message).toBe("The file was uploaded but is not in your Library yet. Try again in a moment.");
    expect(err?.message).not.toMatch(/named no artifact/);
  });

  it("stops polling when the upload is aborted", async () => {
    const resolve = vi.fn(async () => null);
    const handle = provider({ fileId: "file-1" }, resolve, 20, 5000).upload(file());
    const settled = handle.done.then(() => "ok", () => "failed");
    await new Promise((r) => setTimeout(r, 50));
    handle.abort();
    expect(await settled).toBe("failed");
    const calls = resolve.mock.calls.length;
    await new Promise((r) => setTimeout(r, 80));
    expect(resolve.mock.calls.length).toBe(calls);
  });

  it("leaves the normal answer unchanged and does not look anything up", async () => {
    const resolve = vi.fn(async () => "other");
    const result = await provider({ artifactId: "art-1", fileId: "file-1" }, resolve).upload(file()).done;
    expect(result.artifactId).toBe("art-1");
    expect(resolve).not.toHaveBeenCalled();
  });
});
