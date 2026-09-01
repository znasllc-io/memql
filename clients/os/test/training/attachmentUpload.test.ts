import { describe, expect, it, vi } from "vitest";

import {
  EdgeAttachmentUploadProvider,
  attachmentsPath,
} from "../../src/apps/training/attachmentUpload";

// The upload transport: where the bytes go, what rides with them, and what a
// refusal says.
//
// THE PATH IS THE POINT. A bare same-origin POST to `/spaces/{id}/attachments`
// from a bundle the edge serves resolves to no file and takes the SPA
// fallback -- so it is answered with index.html and a 200, a silent success
// for an upload that stored nothing. `/_memql/*` is the one prefix a bundle
// may not claim, and `upstreamPath` (component/edge/proxy.go) strips it for
// the bff's own roots.

function fileOf(name: string, type = "text/plain"): File {
  return new File(["hello"], name, { type });
}

describe("attachmentsPath", () => {
  it("composes the marker path under the bundle's base", () => {
    expect(attachmentsPath("/", "space-1")).toBe("/_memql/spaces/space-1/attachments");
    expect(attachmentsPath("/os", "space-1")).toBe("/os/_memql/spaces/space-1/attachments");
    expect(attachmentsPath("/os/", "space-1")).toBe("/os/_memql/spaces/space-1/attachments");
  });

  it("ENCODES the space id", () => {
    // A canonical space id carries colons (`v1:cognition:space:abc`). They are
    // legal in a path segment, but a slash is not -- and a segment carrying
    // one would silently address a different route rather than fail.
    expect(attachmentsPath("/", "v1:cognition:space:abc")).toBe(
      "/_memql/spaces/v1%3Acognition%3Aspace%3Aabc/attachments",
    );
    expect(attachmentsPath("/", "a/b")).toBe("/_memql/spaces/a%2Fb/attachments");
  });
});

describe("EdgeAttachmentUploadProvider", () => {
  it("posts multipart to the marker path with the bearer", async () => {
    // The mock DECLARES `fetch`'s OWN parameter types, which is what makes
    // reading them back a narrowing rather than a cast. Two ways to get this
    // wrong, and `vitest run` notices neither -- it transpiles without
    // typechecking, so only `tsc -b` (which covers `test/`) sees them: a
    // `vi.fn(async () => ...)` types `mock.calls[0]` as `[] | undefined`, and
    // narrowing the input to `string` makes the mock unassignable to
    // `typeof fetch`, whose first parameter is `RequestInfo | URL`.
    const fetchImpl = vi.fn(
      async (_url: RequestInfo | URL, _init?: RequestInit) =>
        new Response(JSON.stringify({ id: "att-1" }), { status: 201 }),
    );
    const provider = new EdgeAttachmentUploadProvider(async () => "tok-123", "/", fetchImpl);

    const result = await provider.upload("space-1", fileOf("notes.pdf")).done;

    expect(result).toEqual({ attachmentId: "att-1", fileName: "notes.pdf" });
    // `init` is optional on `fetch`, so it is asserted present rather than
    // read through `?.` -- a chain would let an upload that sent NO init at
    // all pass every assertion below by evaluating to undefined.
    const [url, init] = fetchImpl.mock.calls[0]!;
    if (init === undefined) throw new Error("the upload sent no request init");
    expect(url).toBe("/_memql/spaces/space-1/attachments");
    expect(init.method).toBe("POST");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer tok-123");
    // `file` is `attachmentFormFileKey`; the handler answers "file field is
    // required" for anything else.
    expect((init.body as FormData).get("file")).toBeInstanceOf(File);
  });

  it("sends NO Authorization header when there is no bearer", async () => {
    const fetchImpl = vi.fn(
      async (_url: RequestInfo | URL, _init?: RequestInit) =>
        new Response(JSON.stringify({ id: "att-1" }), { status: 201 }),
    );
    const provider = new EdgeAttachmentUploadProvider(async () => null, "/", fetchImpl);
    await provider.upload("space-1", fileOf("notes.pdf")).done;
    const [, init] = fetchImpl.mock.calls[0]!;
    if (init === undefined) throw new Error("the upload sent no request init");
    expect(init.headers).toBeUndefined();
  });

  it("reads the id out of the ARRAY reply shape too", async () => {
    // `extractAttachmentId` accepts either, because the shaped mutation
    // response differs by which surface wrote it. A reply in the other shape
    // is a successful upload, not a broken one.
    const fetchImpl = vi.fn(
      async () => new Response(JSON.stringify([{ id: "att-9" }]), { status: 201 }),
    );
    const provider = new EdgeAttachmentUploadProvider(async () => "t", "/", fetchImpl);
    expect((await provider.upload("space-1", fileOf("a.txt")).done).attachmentId).toBe("att-9");
  });

  it("still SUCCEEDS when the reply names no id", async () => {
    // The analysis is driven by the Plan the handler stamped, not by this
    // value: a reply shaped differently than expected must not turn a stored
    // file into a failed upload.
    const fetchImpl = vi.fn(async () => new Response("not json at all", { status: 201 }));
    const provider = new EdgeAttachmentUploadProvider(async () => "t", "/", fetchImpl);
    const result = await provider.upload("space-1", fileOf("a.txt")).done;
    expect(result).toEqual({ attachmentId: "", fileName: "a.txt" });
  });

  it("surfaces the SERVER'S OWN SENTENCE on a refusal", async () => {
    const fetchImpl = vi.fn(
      async () => new Response("unsupported file type: application/zip", { status: 415 }),
    );
    const provider = new EdgeAttachmentUploadProvider(async () => "t", "/", fetchImpl);
    await expect(provider.upload("space-1", fileOf("a.zip")).done).rejects.toThrow(
      "unsupported file type: application/zip",
    );
  });

  it("falls back to the status when the body is empty", async () => {
    const fetchImpl = vi.fn(async () => new Response("", { status: 500 }));
    const provider = new EdgeAttachmentUploadProvider(async () => "t", "/", fetchImpl);
    await expect(provider.upload("space-1", fileOf("a.txt")).done).rejects.toThrow(
      "The cluster refused the upload (500).",
    );
  });

  it("NAMES the SPA fallback rather than rendering a page of markup", async () => {
    // This is the failure the marker exists to prevent, so when it happens
    // anyway the message has to say what happened -- an HTML body means the
    // site answered, not the cluster.
    const fetchImpl = vi.fn(
      async () => new Response("<!doctype html><html>...", { status: 404 }),
    );
    const provider = new EdgeAttachmentUploadProvider(async () => "t", "/", fetchImpl);
    await expect(provider.upload("space-1", fileOf("a.txt")).done).rejects.toThrow(
      "answered by the site rather than the cluster",
    );
  });

  it("aborts in flight", async () => {
    // The fake honours an ALREADY-ABORTED signal as well as a later abort,
    // which is not pedantry: `upload` awaits `bearer()` before it calls fetch,
    // so an abort in the same tick lands BEFORE the request exists. A fake
    // that only listened would hang here -- and a real `fetch` rejects
    // immediately on a pre-aborted signal.
    const fetchImpl = vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          const stop = () => reject(new Error("The upload was aborted"));
          if (init.signal?.aborted) {
            stop();
            return;
          }
          init.signal?.addEventListener("abort", stop);
        }),
    ) as unknown as typeof fetch;
    const provider = new EdgeAttachmentUploadProvider(async () => "t", "/", fetchImpl);
    const handle = provider.upload("space-1", fileOf("a.txt"));
    const rejected = expect(handle.done).rejects.toThrow(/abort/i);
    handle.abort();
    await rejected;
  });
});
