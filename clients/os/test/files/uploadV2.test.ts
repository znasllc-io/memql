import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { EdgeUploadProvider, CHUNK_BYTES, ONE_SHOT_LIMIT_BYTES } from "../../src/items/edgeUpload";
import { InMemoryResumeStore } from "../../src/items/uploadResume";
import type { UploadProgress } from "../../src/items/upload";

// Upload provider v2 (design D3): one-shot under the line, chunked sessions
// over it, per-chunk retry that never restarts the file, resume that uploads
// only the missing chunks, abort that stops sending.

function fileOf(bytes: number, name = "big.bin"): File {
  // A sparse-ish payload: content does not matter, size does.
  return new File([new Uint8Array(bytes)], name, {
    type: "application/octet-stream",
    lastModified: 1724500000000,
  });
}

interface Call {
  url: string;
  method: string;
  bodyBytes: number;
}

/** A fake wire for the session routes; every response shape in one place. */
function fakeWire(opts: { staged?: number[]; failChunkOnce?: number; refuseInit?: string } = {}) {
  const calls: Call[] = [];
  const failed = new Set<number>();
  const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    // The platform contract the provider relies on: an aborted signal makes
    // fetch reject. A fake that ignored it would make abort look broken in
    // the provider when it is broken in the double.
    if (init?.signal?.aborted) throw new DOMException("The user aborted a request.", "AbortError");
    const url = String(input);
    const method = init?.method ?? "GET";
    let bodyBytes = 0;
    const body = init?.body;
    if (body instanceof Blob) bodyBytes = body.size;
    else if (body instanceof FormData) {
      const f = body.get("file");
      if (f instanceof File) bodyBytes = f.size;
    }
    calls.push({ url, method, bodyBytes });

    if (url.endsWith("/_memql/artifacts") && method === "POST") {
      return new Response(JSON.stringify({ artifactId: "art-one-shot" }), { status: 201 });
    }
    if (url.endsWith("/_memql/artifacts/uploads") && method === "POST") {
      if (opts.refuseInit) return new Response(opts.refuseInit, { status: 413 });
      return new Response(JSON.stringify({ uploadId: "up-1", chunkSize: CHUNK_BYTES }), {
        status: 201,
      });
    }
    if (url.includes("/uploads/up-1/chunks/") && method === "PUT") {
      const n = Number(url.split("/").pop());
      if (opts.failChunkOnce === n && !failed.has(n)) {
        failed.add(n);
        return new Response("staging hiccup", { status: 502 });
      }
      return new Response("", { status: 201 });
    }
    if (url.endsWith("/uploads/up-1") && method === "GET") {
      return new Response(JSON.stringify({ uploadId: "up-1", chunkSize: CHUNK_BYTES, stagedChunks: opts.staged ?? [] }), {
        status: 200,
      });
    }
    if (url.endsWith("/uploads/up-1/complete") && method === "POST") {
      return new Response(JSON.stringify({ artifactId: "art-big", fileId: "file-big" }), {
        status: 201,
      });
    }
    return new Response("unexpected route: " + url, { status: 500 });
  });
  return { calls, fetchImpl };
}

function provider(wire: ReturnType<typeof fakeWire>, resume = new InMemoryResumeStore()) {
  return {
    provider: new EdgeUploadProvider(
      async () => "tok-1",
      "/_memql/artifacts",
      wire.fetchImpl as unknown as typeof fetch,
      { resume, backoffMs: 0 },
    ),
    resume,
  };
}

beforeEach(() => vi.useRealTimers());
afterEach(() => vi.restoreAllMocks());

describe("EdgeUploadProvider v2", () => {
  it("keeps files at or under 32 MiB on the one-shot multipart POST", async () => {
    const wire = fakeWire();
    const { provider: p } = provider(wire);
    const result = await p.upload(fileOf(ONE_SHOT_LIMIT_BYTES, "small.pdf")).done;
    expect(result.artifactId).toBe("art-one-shot");
    expect(wire.calls.map((c) => `${c.method} ${c.url}`)).toEqual(["POST /_memql/artifacts"]);
  });

  it("chunks a larger file: init, sequential 16 MiB PUTs, complete -- with byte progress", async () => {
    const wire = fakeWire();
    const { provider: p } = provider(wire);
    const size = CHUNK_BYTES * 2 + 5;
    const progress: UploadProgress[] = [];
    const handle = p.upload(fileOf(size));
    handle.onProgress?.((snapshot) => progress.push(snapshot));
    const result = await handle.done;
    expect(result.artifactId).toBe("art-big");

    const puts = wire.calls.filter((c) => c.method === "PUT");
    expect(puts.map((c) => c.url.split("/").pop())).toEqual(["1", "2", "3"]);
    expect(puts.map((c) => c.bodyBytes)).toEqual([CHUNK_BYTES, CHUNK_BYTES, 5]);
    expect(wire.calls.at(-1)?.url.endsWith("/complete")).toBe(true);
    expect(progress.at(-1)?.sentBytes).toBe(size);
    expect(progress.at(-1)?.totalBytes).toBe(size);
  });

  it("retries a failed chunk with backoff and never restarts the file", async () => {
    const wire = fakeWire({ failChunkOnce: 2 });
    const { provider: p } = provider(wire);
    await p.upload(fileOf(CHUNK_BYTES * 2 + 5)).done;
    const puts = wire.calls.filter((c) => c.method === "PUT").map((c) => c.url.split("/").pop());
    // Chunk 2 appears twice (the retry); chunk 1 exactly once -- a mid-file
    // failure never re-sends what already landed.
    expect(puts).toEqual(["1", "2", "2", "3"]);
  });

  it("resumes from the staged inventory, uploading only the missing chunks", async () => {
    const resume = new InMemoryResumeStore();
    const wire = fakeWire({ staged: [1, 2] });
    const file = fileOf(CHUNK_BYTES * 3 + 7);
    resume.remember(file, "up-1");
    const { provider: p } = provider(wire, resume);
    const progress: UploadProgress[] = [];
    const handle = p.upload(file);
    handle.onProgress?.((snapshot) => progress.push(snapshot));
    const result = await handle.done;
    expect(result.artifactId).toBe("art-big");

    // No re-init: the session already exists; the inventory was read.
    expect(wire.calls.some((c) => c.method === "POST" && c.url.endsWith("/uploads"))).toBe(false);
    expect(wire.calls.some((c) => c.method === "GET" && c.url.endsWith("/uploads/up-1"))).toBe(true);
    const puts = wire.calls.filter((c) => c.method === "PUT").map((c) => c.url.split("/").pop());
    expect(puts).toEqual(["3", "4"]);
    // The surface can say what resume saved: two of four chunks were already
    // in the cluster.
    expect(progress[0]?.resumedChunks).toBe(2);
    expect(progress[0]?.totalChunks).toBe(4);
    // The record is spent on success.
    expect(resume.recall(file)).toBeNull();
  });

  it("falls back to a fresh session when the remembered one is gone", async () => {
    const resume = new InMemoryResumeStore();
    const wire = fakeWire();
    // Remember an id the wire answers 500 for -- a session GC'd server-side.
    const file = fileOf(ONE_SHOT_LIMIT_BYTES + 1);
    resume.remember(file, "up-gone");
    const { provider: p } = provider(wire, resume);
    const result = await p.upload(file).done;
    expect(result.artifactId).toBe("art-big");
    expect(wire.calls.some((c) => c.method === "POST" && c.url.endsWith("/uploads"))).toBe(true);
  });

  it("renders an init refusal verbatim -- the server's sentence is the message", async () => {
    const wire = fakeWire({ refuseInit: "upload of 5 GiB exceeds the 4 GiB limit" });
    const { provider: p } = provider(wire);
    await expect(p.upload(fileOf(ONE_SHOT_LIMIT_BYTES + 1)).done).rejects.toThrow(
      "upload of 5 GiB exceeds the 4 GiB limit",
    );
  });

  it("abort stops sending", async () => {
    const wire = fakeWire();
    const { provider: p } = provider(wire);
    const handle = p.upload(fileOf(CHUNK_BYTES * 4));
    handle.abort();
    await expect(handle.done).rejects.toThrow();
    // Nothing landed after the abort settled.
    const putsAfter = wire.calls.filter((c) => c.method === "PUT").length;
    expect(putsAfter).toBeLessThanOrEqual(1);
  });

  it("carries the folder and provenance fields on init", async () => {
    const captured: string[] = [];
    const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/uploads") && init?.method === "POST") {
        captured.push(String(init.body));
        return new Response(JSON.stringify({ uploadId: "up-1", chunkSize: CHUNK_BYTES }), {
          status: 201,
        });
      }
      if (url.includes("/chunks/")) return new Response("", { status: 201 });
      if (url.endsWith("/complete"))
        return new Response(JSON.stringify({ artifactId: "a", fileId: "f" }), { status: 201 });
      return new Response("", { status: 500 });
    });
    const p = new EdgeUploadProvider(async () => null, "/_memql/artifacts", fetchImpl as unknown as typeof fetch, {
      resume: new InMemoryResumeStore(),
      backoffMs: 0,
    });
    await p.upload(fileOf(ONE_SHOT_LIMIT_BYTES + 1, "video.mp4"), { folderId: "f-vid" }).done;
    const body = JSON.parse(captured[0] ?? "{}") as Record<string, unknown>;
    expect(body.name).toBe("video.mp4");
    expect(body.size).toBe(ONE_SHOT_LIMIT_BYTES + 1);
    expect(body.folderId).toBe("f-vid");
  });
});
