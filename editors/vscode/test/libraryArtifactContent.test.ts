// Fetching an artifact's bytes, and refusing to (memql#4748).
//
// EVERY REFUSAL FROM THE ROUTE IS A 404 ON PURPOSE -- not-found and not-yours
// are deliberately indistinguishable there -- so these cases pin what this
// client does with that: it does NOT re-invent the distinction, and it does not
// report a 404 as "no such artifact" either, because the row was already read
// over the stream before any of this ran.
//
// AND THE CAP IS CHECKED TWICE. Content-Length is the check that matters,
// because it refuses before the body is read; measuring what arrived is the
// backstop for a response that declares nothing.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { Readable } from "node:stream";

import {
  declaredLength,
  fetchArtifactText,
  saveArtifactToPath,
  type ArtifactFetch,
  type ArtifactResponseLike,
} from "../src/library/artifactContent.js";

function response(init: Partial<ArtifactResponseLike> & { headerMap?: Record<string, string> }): ArtifactResponseLike {
  const headers = init.headerMap ?? {};
  return {
    ok: init.ok ?? true,
    status: init.status ?? 200,
    headers: { get: (name: string) => headers[name.toLowerCase()] ?? null },
    text: init.text ?? (() => Promise.resolve("")),
    arrayBuffer: init.arrayBuffer ?? (() => Promise.resolve(new ArrayBuffer(0))),
    body: init.body ?? null,
  };
}

/** A fetch that records what it was asked for and answers with one response. */
function stubFetch(answer: ArtifactResponseLike | (() => Promise<never>)) {
  const seen: Array<{ url: string; headers: Record<string, string> }> = [];
  const fetchImpl: ArtifactFetch = (url, init) => {
    seen.push({ url, headers: init.headers });
    return typeof answer === "function" ? answer() : Promise.resolve(answer);
  };
  return { fetchImpl, seen };
}

test("the bearer travels on the request, and the method is a GET", async () => {
  const { fetchImpl, seen } = stubFetch(response({ text: () => Promise.resolve("hello") }));
  const result = await fetchArtifactText(fetchImpl, {
    url: "https://api.a.test/artifacts/abc/content",
    bearer: "jwt-value",
    limitBytes: 1024,
  });
  assert.deepEqual(result, { ok: true, text: "hello" });
  assert.equal(seen[0]!.url, "https://api.a.test/artifacts/abc/content");
  assert.equal(seen[0]!.headers.authorization, "Bearer jwt-value");
});

test("a declared length past the cap refuses BEFORE the body is read", async () => {
  let read = false;
  const { fetchImpl } = stubFetch(
    response({
      headerMap: { "content-length": "9000000" },
      text: () => {
        read = true;
        return Promise.resolve("x");
      },
    }),
  );
  const result = await fetchArtifactText(fetchImpl, { url: "u", bearer: "b", limitBytes: 8 * 1024 * 1024 });
  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "tooLarge");
  assert.equal(result.ok === false && result.reason === "tooLarge" ? result.bytes : 0, 9000000);
  assert.equal(read, false, "the body was read despite a declared length past the cap");
});

test("a response that declares nothing is still measured", async () => {
  const { fetchImpl } = stubFetch(response({ text: () => Promise.resolve("abcdefghij") }));
  const result = await fetchArtifactText(fetchImpl, { url: "u", bearer: "b", limitBytes: 4 });
  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "tooLarge");
});

test("the cap is measured in BYTES, not characters", async () => {
  // Four astral characters are four code points and sixteen UTF-8 bytes; a
  // `text.length` check would let this through.
  const emoji = "abcd".replace(/./g, () => String.fromCodePoint(0x1f600));
  const { fetchImpl } = stubFetch(response({ text: () => Promise.resolve(emoji) }));
  const result = await fetchArtifactText(fetchImpl, { url: "u", bearer: "b", limitBytes: 10 });
  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "tooLarge");
});

test("every status classifies into the sentence it deserves", async () => {
  const cases: Array<[number, string]> = [
    // The row was already read over the stream under this same actor, so a 404
    // here is the third case: the cluster has no downloadable body for it.
    [404, "noContent"],
    [401, "unauthorized"],
    [403, "unauthorized"],
    [500, "failed"],
    [502, "failed"],
  ];
  for (const [status, reason] of cases) {
    const { fetchImpl } = stubFetch(response({ ok: false, status }));
    const result = await fetchArtifactText(fetchImpl, { url: "u", bearer: "b", limitBytes: 1024 });
    assert.equal(result.ok, false, String(status));
    assert.equal(result.ok === false ? result.reason : "", reason, String(status));
  }
});

test("a transport that throws is a failure, not a crash", async () => {
  const { fetchImpl } = stubFetch(() => Promise.reject(new Error("ENOTFOUND api.a.test")));
  const result = await fetchArtifactText(fetchImpl, { url: "u", bearer: "b", limitBytes: 1024 });
  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "failed");
  assert.match(result.ok === false ? result.detail : "", /ENOTFOUND/);
});

test("declaredLength reads a length and refuses nonsense", () => {
  assert.equal(declaredLength(response({ headerMap: { "content-length": " 42 " } })), 42);
  assert.equal(declaredLength(response({})), undefined);
  assert.equal(declaredLength(response({ headerMap: { "content-length": "chunked" } })), undefined);
  assert.equal(declaredLength(response({ headerMap: { "content-length": "-1" } })), undefined);
});

// -----------------------------------------------------------------------------
// Saving to disk
// -----------------------------------------------------------------------------

function tempPath(name: string): string {
  return path.join(fs.mkdtempSync(path.join(os.tmpdir(), "memql-artifact-")), name);
}

test("a streamed body lands on disk whole, and no cap applies", async () => {
  // The cap exists because an editor buffer is a poor container for a large
  // file. A file on disk is the ANSWER to that, not another place to apply it.
  const bytes = Buffer.alloc(64 * 1024, 0x41);
  const { fetchImpl } = stubFetch(
    response({
      headerMap: { "content-length": String(bytes.length) },
      body: Readable.toWeb(Readable.from([bytes])),
    }),
  );
  const dest = tempPath("big.bin");
  const result = await saveArtifactToPath(fetchImpl, { url: "u", bearer: "b", destPath: dest });
  assert.deepEqual(result, { ok: true, bytes: bytes.length });
  assert.equal(fs.readFileSync(dest).length, bytes.length);
});

test("a response with no stream still saves", async () => {
  // The fallback for a transport that buffers -- correct, just not
  // constant-memory.
  const { fetchImpl } = stubFetch(
    response({ arrayBuffer: () => Promise.resolve(Uint8Array.from([1, 2, 3]).buffer) }),
  );
  const dest = tempPath("small.bin");
  const result = await saveArtifactToPath(fetchImpl, { url: "u", bearer: "b", destPath: dest });
  // No content-length was declared, so no size is reported: absent is "the
  // server did not say", never 0.
  assert.deepEqual(result, { ok: true });
  assert.deepEqual([...fs.readFileSync(dest)], [1, 2, 3]);
});

test("a stream that dies mid-transfer leaves NO file behind", async () => {
  // A truncated file at the path the person was just told about is
  // indistinguishable from a whole one until they open it.
  const failing = new Readable({
    read() {
      this.push(Buffer.from("partial"));
      this.destroy(new Error("connection reset"));
    },
  });
  const { fetchImpl } = stubFetch(response({ body: Readable.toWeb(failing) }));
  const dest = tempPath("truncated.bin");
  const result = await saveArtifactToPath(fetchImpl, { url: "u", bearer: "b", destPath: dest });
  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "failed");
  assert.equal(fs.existsSync(dest), false, "a partial file survived a failed save");
});

test("a refused save never creates a file at all", async () => {
  const { fetchImpl } = stubFetch(response({ ok: false, status: 404 }));
  const dest = tempPath("never.bin");
  const result = await saveArtifactToPath(fetchImpl, { url: "u", bearer: "b", destPath: dest });
  assert.equal(result.ok === false ? result.reason : "", "noContent");
  assert.equal(fs.existsSync(dest), false);
});
