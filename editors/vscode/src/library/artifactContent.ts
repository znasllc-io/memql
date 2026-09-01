// Fetching an artifact's bytes over the bff's HTTP edge (memql#4748).
//
// WHY HTTP AT ALL, IN A GRPC-FIRST EXTENSION. `GET /artifacts/{id}/content` is
// one of the documented HTTP exceptions: it is byte transport, it streams, and
// it honours a `Range` -- which is what HTTP is for and what a protobuf message
// on the multiplexed stream is not. Everything ABOUT the artifact still comes
// over the stream; only the bytes come this way.
//
// EVERY REFUSAL IS A 404, AND THAT IS THE SERVER'S POSTURE, NOT A BUG. The
// reads behind that route run under the caller's own actor against owner-gated
// concepts, so "it is not there" and "it is not yours" come back identically --
// deliberately. This module therefore does NOT translate a 404 into "the
// artifact does not exist": the handoff has already read the row over the
// stream under the same actor, so by the time a fetch happens the row is known
// to exist and to be readable, and a 404 means the third thing -- the cluster
// has no downloadable body for it (a `document`-kind artifact, whose backing
// concept the export route does not serve).
//
// A PARTIAL FILE IS WORSE THAN NO FILE. The save path streams to disk, so a
// failure mid-transfer leaves a file that looks finished; it is removed before
// the failure is reported, because the person is about to open whatever is at
// that path.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go);
// artifactDocuments.ts is the adapter that picks the path and shows the dialog.
//
// Refs: #4748

import { createWriteStream } from "node:fs";
import { unlink, writeFile } from "node:fs/promises";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";

/**
 * The slice of `Response` this module uses.
 *
 * A NARROW SEAM RATHER THAN `Response`, for the reason credentials.ts declares
 * `HttpResponseLike`: a test drives this with a plain object, and the whole
 * `fetch` response type would oblige it to build one. It is wider than that
 * one because a download needs the headers and the body stream, which a token
 * exchange does not.
 */
export interface ArtifactResponseLike {
  ok: boolean;
  status: number;
  headers: { get(name: string): string | null };
  text(): Promise<string>;
  arrayBuffer(): Promise<ArrayBuffer>;
  /** A web ReadableStream on a real response; null when there is no body. */
  body: unknown;
}

export type ArtifactFetch = (
  url: string,
  init: { method: string; headers: Record<string, string> },
) => Promise<ArtifactResponseLike>;

/** The real network. */
export const defaultArtifactFetch: ArtifactFetch = (url, init) =>
  fetch(url, init) as unknown as Promise<ArtifactResponseLike>;

/**
 * Why a fetch did not produce content.
 *
 * FOUR REASONS, BECAUSE THEY CALL FOR FOUR DIFFERENT SENTENCES. `noContent` is
 * the cluster saying there is nothing to export; `unauthorized` is a credential
 * the cluster no longer accepts, which is a sign-in problem rather than an
 * artifact problem; `tooLarge` is a decision this editor made, and it carries
 * the number it made it on; `failed` is everything else, and its `detail` is
 * the raw text -- which goes to the channel through the redactor and never into
 * a document.
 */
export type ArtifactContentFailure =
  | { reason: "noContent"; detail: string }
  | { reason: "unauthorized"; detail: string }
  | { reason: "tooLarge"; bytes: number; detail: string }
  | { reason: "failed"; detail: string };

export type ArtifactTextResult = { ok: true; text: string } | ({ ok: false } & ArtifactContentFailure);
/**
 * `bytes` is what the response DECLARED, and is absent when it declared
 * nothing. Not measured on the way past: the stream goes straight to the disk,
 * and counting it would mean putting a tee in the one path whose whole point is
 * that nothing touches the bytes. Absent is "the server did not say", never 0.
 */
export type ArtifactSaveResult = { ok: true; bytes?: number } | ({ ok: false } & ArtifactContentFailure);

/** The declared body length, or undefined when the response does not state one. */
export function declaredLength(response: ArtifactResponseLike): number | undefined {
  const raw = response.headers.get("content-length");
  if (raw === null) return undefined;
  const parsed = Number(raw.trim());
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined;
}

/**
 * Reads an artifact's content as text, refusing anything past `limitBytes`.
 *
 * THE CAP IS CHECKED TWICE, and both are needed. `Content-Length` is the cheap
 * check and the one that matters, because it refuses before the body is read at
 * all; it is not sufficient, because a response may not declare one. The second
 * check measures what actually arrived -- too late to save the memory, in time
 * to keep it out of an editor buffer.
 */
export async function fetchArtifactText(
  fetchImpl: ArtifactFetch,
  params: { url: string; bearer: string; limitBytes: number },
): Promise<ArtifactTextResult> {
  let response: ArtifactResponseLike;
  try {
    response = await fetchImpl(params.url, requestInit(params.bearer));
  } catch (err) {
    return { ok: false, reason: "failed", detail: errorDetail(err) };
  }
  const refusal = classify(response);
  if (refusal !== undefined) return { ok: false, ...refusal };

  const declared = declaredLength(response);
  if (declared !== undefined && declared > params.limitBytes) {
    return { ok: false, reason: "tooLarge", bytes: declared, detail: `content-length ${declared}` };
  }
  let text: string;
  try {
    text = await response.text();
  } catch (err) {
    return { ok: false, reason: "failed", detail: errorDetail(err) };
  }
  const actual = Buffer.byteLength(text, "utf8");
  if (actual > params.limitBytes) {
    return { ok: false, reason: "tooLarge", bytes: actual, detail: `read ${actual} bytes` };
  }
  return { ok: true, text };
}

/**
 * Streams an artifact's content to a path on disk.
 *
 * NO SIZE CAP HERE, and that is the point of the path: the cap exists because
 * an editor buffer is a poor container for a large file, and a file on disk is
 * the answer to that rather than another place to apply it.
 */
export async function saveArtifactToPath(
  fetchImpl: ArtifactFetch,
  params: { url: string; bearer: string; destPath: string },
): Promise<ArtifactSaveResult> {
  let response: ArtifactResponseLike;
  try {
    response = await fetchImpl(params.url, requestInit(params.bearer));
  } catch (err) {
    return { ok: false, reason: "failed", detail: errorDetail(err) };
  }
  const refusal = classify(response);
  if (refusal !== undefined) return { ok: false, ...refusal };

  const declared = declaredLength(response);
  try {
    const body = response.body;
    if (isWebReadable(body)) {
      // Constant memory: the response never becomes a buffer on the way to the
      // disk, which is the whole reason this path exists rather than reusing
      // the text one.
      await pipeline(Readable.fromWeb(body as Parameters<typeof Readable.fromWeb>[0]), createWriteStream(params.destPath));
    } else {
      // No stream to pipe (a stubbed response, or a transport that buffers).
      // Correct, just not constant-memory -- the same fallback the server's own
      // download path keeps for a downloader that cannot stream.
      await writeFile(params.destPath, Buffer.from(await response.arrayBuffer()));
    }
  } catch (err) {
    // The file is removed BEFORE the failure is reported: the caller is about
    // to tell someone where their artifact is, and a truncated file at that
    // path is indistinguishable from a whole one until they open it.
    await unlink(params.destPath).catch(() => undefined);
    return { ok: false, reason: "failed", detail: errorDetail(err) };
  }
  return declared === undefined ? { ok: true } : { ok: true, bytes: declared };
}

function requestInit(bearer: string): { method: string; headers: Record<string, string> } {
  return {
    method: "GET",
    headers: {
      authorization: `Bearer ${bearer}`,
      // `*/*` rather than a negotiated list: this route serves whatever the
      // artifact is, the caller has already decided what to do with it from the
      // row's own metadata, and a narrower Accept could only ever refuse
      // something the caller asked for by id.
      accept: "*/*",
    },
  };
}

/** The refusal a response carries, or undefined when it carries content. */
function classify(response: ArtifactResponseLike): ArtifactContentFailure | undefined {
  if (response.ok) return undefined;
  if (response.status === 404) {
    return { reason: "noContent", detail: "the cluster answered 404" };
  }
  if (response.status === 401 || response.status === 403) {
    return { reason: "unauthorized", detail: `the cluster answered ${response.status}` };
  }
  return { reason: "failed", detail: `the cluster answered ${response.status}` };
}

/**
 * Whether a response body is something `Readable.fromWeb` can take.
 *
 * DUCK-TYPED, because the seam admits a stub: `instanceof ReadableStream` would
 * be false for a perfectly good stream from another realm, and undefined in a
 * Node build where the global is absent.
 */
function isWebReadable(body: unknown): boolean {
  return body !== null && typeof body === "object" && typeof (body as { getReader?: unknown }).getReader === "function";
}

function errorDetail(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
