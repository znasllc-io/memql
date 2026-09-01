import { afterEach, describe, expect, it, vi } from "vitest";

import { CHUNK_BYTES, EdgeUploadProvider, ONE_SHOT_LIMIT_BYTES } from "../../src/items/edgeUpload";
import { InMemoryResumeStore } from "../../src/items/uploadResume";
import { artifactContentPath } from "../../src/apps/files/actions/download";

// A NEW VERSION IS THE SAME TRANSFER WITH ONE MORE FIELD (epic memql#4806).
//
// The provider is the only thing in clients/os that speaks the artifact upload
// wire (test/files/onePath.test.ts pins it), so a version has to ride it --
// which is exactly what gives a version chunking, per-chunk retry, resume,
// progress and verbatim refusals without a second route speaker learning any
// of them. These tests assert the field reaches BOTH shapes of that transfer,
// and that the one place the target changes behaviour -- the resume key --
// changes it in the direction that stops a version landing as a fresh upload.

function fileOf(bytes: number, name = "big.bin"): File {
  return new File([new Uint8Array(bytes)], name, {
    type: "application/octet-stream",
    lastModified: 1724500000000,
  });
}

interface Seen {
  url: string;
  method: string;
  target: string;
  json: Record<string, unknown> | null;
}

function wire(opts: { sessionStatus?: string } = {}) {
  const seen: Seen[] = [];
  const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    let target = "";
    let json: Record<string, unknown> | null = null;
    const body = init?.body;
    if (body instanceof FormData) {
      target = String(body.get("targetArtifactId") ?? "");
    } else if (typeof body === "string") {
      json = JSON.parse(body) as Record<string, unknown>;
      target = String(json.targetArtifactId ?? "");
    }
    seen.push({ url, method, target, json });

    if (url.endsWith("/_memql/artifacts") && method === "POST") {
      return new Response(JSON.stringify({ artifactId: "a-1", fileId: "f-1", versionNumber: 4 }), {
        status: 201,
      });
    }
    if (url.endsWith("/_memql/artifacts/uploads") && method === "POST") {
      return new Response(JSON.stringify({ uploadId: "up-2", chunkSize: CHUNK_BYTES }), {
        status: 201,
      });
    }
    if (url.includes("/uploads/") && url.includes("/chunks/") && method === "PUT") {
      return new Response("", { status: 201 });
    }
    if (url.endsWith("/complete") && method === "POST") {
      return new Response(JSON.stringify({ artifactId: "a-1", fileId: "f-1", versionNumber: 2 }), {
        status: 201,
      });
    }
    if (url.includes("/uploads/") && method === "GET") {
      return new Response(
        JSON.stringify({
          uploadId: url.split("/").pop(),
          status: opts.sessionStatus ?? "open",
          size: 0,
          chunkSize: CHUNK_BYTES,
          staged: [],
        }),
        { status: 200 },
      );
    }
    return new Response("unexpected route: " + url, { status: 500 });
  });
  return { seen, fetchImpl };
}

function providerOn(w: ReturnType<typeof wire>, resume = new InMemoryResumeStore()) {
  return new EdgeUploadProvider(
    async () => "tok-1",
    "/_memql/artifacts",
    w.fetchImpl as unknown as typeof fetch,
    { resume, backoffMs: 0 },
  );
}

afterEach(() => vi.restoreAllMocks());

describe("uploading a new version", () => {
  it("carries the target on the one-shot form and reports which version landed", async () => {
    const w = wire();
    const result = await providerOn(w).upload(fileOf(ONE_SHOT_LIMIT_BYTES, "q3.pdf"), {
      targetArtifactId: "a-1",
    }).done;

    expect(result.artifactId).toBe("a-1");
    expect(result.versionNumber).toBe(4);
    expect(w.seen.map((c) => `${c.method} ${c.url}`)).toEqual(["POST /_memql/artifacts"]);
    expect(w.seen[0]?.target).toBe("a-1");
  });

  it("carries the target in the session INIT body, before a chunk moves", async () => {
    const w = wire();
    const result = await providerOn(w).upload(fileOf(CHUNK_BYTES * 2 + 5), {
      targetArtifactId: "a-1",
    }).done;

    expect(result.versionNumber).toBe(2);
    const init = w.seen.find((c) => c.url.endsWith("/uploads") && c.method === "POST");
    // AT INIT, deliberately: the cluster gates the target there, so a target
    // that is not the caller's is refused before anybody streams gigabytes.
    expect(init?.json?.targetArtifactId).toBe("a-1");
    expect(w.seen.filter((c) => c.method === "PUT")).toHaveLength(3);
  });

  it("omits the field entirely for an ordinary upload", async () => {
    const w = wire();
    await providerOn(w).upload(fileOf(1024, "fresh.txt")).done;
    expect(w.seen[0]?.target).toBe("");

    const w2 = wire();
    await providerOn(w2).upload(fileOf(ONE_SHOT_LIMIT_BYTES + 1)).done;
    const init = w2.seen.find((c) => c.url.endsWith("/uploads") && c.method === "POST");
    expect(init?.json).not.toHaveProperty("targetArtifactId");
  });

  // A cluster that predates versions answers without the field, and "not
  // stated" must not become version zero -- a surface reading zero would
  // announce "Version 0 landed".
  it("leaves the version absent when the cluster does not state one", async () => {
    const fetchImpl = vi.fn(
      async () => new Response(JSON.stringify({ artifactId: "a-1" }), { status: 201 }),
    );
    const p = new EdgeUploadProvider(
      async () => "tok-1",
      "/_memql/artifacts",
      fetchImpl as unknown as typeof fetch,
      { resume: new InMemoryResumeStore(), backoffMs: 0 },
    );
    const result = await p.upload(fileOf(10, "a.txt")).done;
    expect(result.artifactId).toBe("a-1");
    expect(result.versionNumber).toBeUndefined();
  });
});

// THE RESUME KEY IS WHERE THE TARGET CHANGES BEHAVIOUR, and the failure it
// prevents is silent: without it, dropping `report.pdf` as a fresh upload and
// then dropping the SAME `report.pdf` as a new version recalls the first
// session -- which carries no target -- and the version lands as a second
// artifact.
describe("the resume ledger separates a version from a fresh upload", () => {
  it("does not resume a fresh session into a version upload", async () => {
    const resume = new InMemoryResumeStore();
    const file = fileOf(CHUNK_BYTES * 2 + 5, "report.pdf");

    const first = wire();
    await providerOn(first, resume).upload(file).done;
    // The fresh upload's session is remembered against no target.
    expect(resume.recall(file)).toBeNull(); // forgotten on success
    resume.remember(file, "up-fresh");

    const second = wire();
    await providerOn(second, resume).upload(file, { targetArtifactId: "a-1" }).done;
    // It opened a NEW session rather than recalling the fresh one: no
    // inventory GET against up-fresh anywhere.
    expect(second.seen.some((c) => c.url.includes("up-fresh"))).toBe(false);
    const init = second.seen.find((c) => c.url.endsWith("/uploads") && c.method === "POST");
    expect(init?.json?.targetArtifactId).toBe("a-1");
  });

  it("does resume a version session on a re-drop at the same target", async () => {
    const resume = new InMemoryResumeStore();
    const file = fileOf(ONE_SHOT_LIMIT_BYTES + 1, "report.pdf");
    resume.remember(file, "up-2", "a-1");

    const w = wire();
    await providerOn(w, resume).upload(file, { targetArtifactId: "a-1" }).done;
    // The remembered session was read back rather than a new one opened.
    expect(w.seen.some((c) => c.url.endsWith("/uploads/up-2") && c.method === "GET")).toBe(true);
    expect(w.seen.some((c) => c.url.endsWith("/uploads") && c.method === "POST")).toBe(false);
  });

  it("keys the two independently, so neither drop can recall the other's session", () => {
    const resume = new InMemoryResumeStore();
    const file = fileOf(10, "report.pdf");
    resume.remember(file, "up-fresh");
    resume.remember(file, "up-version", "a-1");
    expect(resume.recall(file)).toBe("up-fresh");
    expect(resume.recall(file, "a-1")).toBe("up-version");
    resume.forget(file, "a-1");
    expect(resume.recall(file)).toBe("up-fresh");
    expect(resume.recall(file, "a-1")).toBeNull();
  });
});

describe("the content route's version selector", () => {
  it("omits the parameter for the current version and names it for an older one", () => {
    expect(artifactContentPath("/", "a-1")).toBe("/_memql/artifacts/a-1/content");
    expect(artifactContentPath("/", "a-1", 2)).toBe("/_memql/artifacts/a-1/content?version=2");
  });

  // A QUERY PARAMETER, not a path segment: the cluster's front-door path set
  // is generated from its route table, and a new path shape would change it.
  it("keeps the version under the same /content path", () => {
    expect(artifactContentPath("/base/", "a-1", 9)).toBe(
      "/base/_memql/artifacts/a-1/content?version=9",
    );
  });
});
