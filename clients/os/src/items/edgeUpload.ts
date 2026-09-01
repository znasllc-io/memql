// The real upload provider (design D3, epic #4721): every Library upload in
// the OS goes through this class, and the pin test holds it to that -- the
// desk's drops, the Files browse, drop-onto-window and drop-onto-folder all
// ride one path.
//
// ===========================================================================
// ONE LINE DECIDES THE SHAPE OF THE TRANSFER
// ===========================================================================
// At or under 32 MiB: the existing one-shot multipart POST, unchanged wire
// (it gains the folder + provenance form fields). Over it: a chunked
// resumable session -- init, sequential 16 MiB chunk PUTs with per-chunk
// retry and backoff, complete. Staged blocks live with the blob in Azure, so
// any bff serves any chunk and an abandoned session garbage-collects on
// Azure's ~7-day clock; the client's only cleanup is its own resume ledger,
// purged on the same clock.
//
// RESUME IS RE-DROP. localStorage maps (name, size, lastModified) to the
// open session; on a re-drop the provider reads the staged inventory and
// uploads only the missing chunks. A remembered session the server no longer
// answers for falls back to a fresh one -- resume is an optimization, never
// a precondition.

import type { UploadHandle, UploadOptions, UploadProgress, UploadProvider, UploadResult } from "./upload";
import { LocalResumeStore, type ResumeStore } from "./uploadResume";

export const ONE_SHOT_LIMIT_BYTES = 32 * 1024 * 1024;
/** A constant, not a knob (design C3). */
export const CHUNK_BYTES = 16 * 1024 * 1024;
const CHUNK_ATTEMPTS = 3;

export function artifactsUploadPath(baseUrl: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return mount + "_memql/artifacts";
}

/**
 * The refusal a reader sees: the server's own sentence, verbatim. An HTML
 * body is the SPA fallback answering, not the bff, and is named as such
 * rather than rendered as markup.
 */
async function refusalFrom(response: Response): Promise<string> {
  let body = "";
  try {
    body = (await response.text()).trim();
  } catch {
    body = "";
  }
  if (body.startsWith("<")) {
    return `The upload was answered by the site rather than the cluster (${response.status}).`;
  }
  if (body === "") return `The cluster refused the upload (${response.status}).`;
  return body;
}

interface SessionFacts {
  uploadId: string;
  chunkSize: number;
  stagedChunks: number[];
}

export class EdgeUploadProvider implements UploadProvider {
  private readonly resume: ResumeStore;
  private readonly backoffMs: number;

  constructor(
    private readonly bearer: () => Promise<string | null>,
    private readonly path: string = artifactsUploadPath(import.meta.env.BASE_URL),
    private readonly fetchImpl: typeof fetch = fetch,
    opts: { resume?: ResumeStore; backoffMs?: number } = {},
  ) {
    this.resume = opts.resume ?? new LocalResumeStore();
    this.backoffMs = opts.backoffMs ?? 600;
  }

  upload(file: File, opts?: UploadOptions): UploadHandle {
    const abort = new AbortController();
    const listeners = new Set<(progress: UploadProgress) => void>();
    const report = (progress: UploadProgress) => {
      for (const listener of listeners) listener(progress);
    };
    const done =
      file.size <= ONE_SHOT_LIMIT_BYTES
        ? this.oneShot(file, opts, abort.signal, report)
        : this.chunked(file, opts, abort.signal, report);
    return {
      done,
      abort: () => abort.abort(),
      onProgress: (listener) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
    };
  }

  private async headers(): Promise<Record<string, string>> {
    const token = await this.bearer();
    return token ? { Authorization: `Bearer ${token}` } : {};
  }

  private async oneShot(
    file: File,
    opts: UploadOptions | undefined,
    signal: AbortSignal,
    report: (progress: UploadProgress) => void,
  ): Promise<UploadResult> {
    const body = new FormData();
    body.append("file", file);
    body.append("name", file.name);
    // The destination folder rides the same form (design B2): the upload
    // route is the writer that knows it, and promotion forwards it.
    if (opts?.folderId) body.append("folderId", opts.folderId);
    const response = await this.fetchImpl(this.path, {
      method: "POST",
      body,
      credentials: "same-origin",
      headers: await this.headers(),
      signal,
    });
    if (!response.ok) throw new Error(await refusalFrom(response));
    const payload = (await response.json()) as { artifactId?: unknown };
    const artifactId = typeof payload.artifactId === "string" ? payload.artifactId : "";
    if (!artifactId) throw new Error("Upload landed but named no artifact.");
    report({ sentBytes: file.size, totalBytes: file.size });
    return { artifactId, title: file.name, fileKind: "file", source: "uploaded" };
  }

  // ---- the chunked session ----

  private sessionUrl(uploadId = ""): string {
    return `${this.path}/uploads${uploadId ? `/${encodeURIComponent(uploadId)}` : ""}`;
  }

  /** The remembered session's staged inventory, or null when it is gone --
   *  which is a fresh start, never a failure. */
  private async recallSession(file: File, signal: AbortSignal): Promise<SessionFacts | null> {
    const remembered = this.resume.recall(file);
    if (remembered === null) return null;
    try {
      const response = await this.fetchImpl(this.sessionUrl(remembered), {
        method: "GET",
        credentials: "same-origin",
        headers: await this.headers(),
        signal,
      });
      if (!response.ok) {
        this.resume.forget(file);
        return null;
      }
      const payload = (await response.json()) as {
        uploadId?: unknown;
        chunkSize?: unknown;
        stagedChunks?: unknown;
      };
      const chunkSize = typeof payload.chunkSize === "number" && payload.chunkSize > 0 ? payload.chunkSize : CHUNK_BYTES;
      const staged = Array.isArray(payload.stagedChunks)
        ? payload.stagedChunks.filter((n): n is number => typeof n === "number")
        : [];
      return { uploadId: remembered, chunkSize, stagedChunks: staged };
    } catch (err) {
      if (signal.aborted) throw err;
      this.resume.forget(file);
      return null;
    }
  }

  private async openSession(
    file: File,
    opts: UploadOptions | undefined,
    signal: AbortSignal,
  ): Promise<SessionFacts> {
    const response = await this.fetchImpl(this.sessionUrl(), {
      method: "POST",
      credentials: "same-origin",
      headers: { ...(await this.headers()), "Content-Type": "application/json" },
      signal,
      body: JSON.stringify({
        name: file.name,
        size: file.size,
        mimeType: file.type,
        ...(opts?.folderId ? { folderId: opts.folderId } : {}),
      }),
    });
    if (!response.ok) throw new Error(await refusalFrom(response));
    const payload = (await response.json()) as { uploadId?: unknown; chunkSize?: unknown };
    const uploadId = typeof payload.uploadId === "string" ? payload.uploadId : "";
    if (uploadId === "") throw new Error("The cluster opened no upload session.");
    const chunkSize = typeof payload.chunkSize === "number" && payload.chunkSize > 0 ? payload.chunkSize : CHUNK_BYTES;
    this.resume.remember(file, uploadId);
    return { uploadId, chunkSize, stagedChunks: [] };
  }

  private async putChunk(
    session: SessionFacts,
    n: number,
    body: Blob,
    signal: AbortSignal,
  ): Promise<void> {
    let lastRefusal = "";
    for (let attempt = 0; attempt < CHUNK_ATTEMPTS; attempt += 1) {
      if (attempt > 0 && this.backoffMs > 0) {
        // Exponential, and abortable: a person who hit abort must not wait
        // out a backoff first.
        await new Promise<void>((resolve, reject) => {
          const t = setTimeout(resolve, this.backoffMs * 2 ** (attempt - 1));
          signal.addEventListener("abort", () => {
            clearTimeout(t);
            reject(signal.reason instanceof Error ? signal.reason : new Error("upload aborted"));
          });
        });
      }
      const response = await this.fetchImpl(`${this.sessionUrl(session.uploadId)}/chunks/${n}`, {
        method: "PUT",
        credentials: "same-origin",
        headers: { ...(await this.headers()), "Content-Type": "application/octet-stream" },
        body,
        signal,
      });
      if (response.ok) return;
      lastRefusal = await refusalFrom(response);
    }
    throw new Error(lastRefusal || `chunk ${n} did not land`);
  }

  private async chunked(
    file: File,
    opts: UploadOptions | undefined,
    signal: AbortSignal,
    report: (progress: UploadProgress) => void,
  ): Promise<UploadResult> {
    const session = (await this.recallSession(file, signal)) ?? (await this.openSession(file, opts, signal));
    const totalChunks = Math.ceil(file.size / session.chunkSize);
    const staged = new Set(session.stagedChunks.filter((n) => n >= 1 && n <= totalChunks));

    let sentBytes = 0;
    for (const n of staged) {
      sentBytes += n === totalChunks ? file.size - (n - 1) * session.chunkSize : session.chunkSize;
    }
    const base: Pick<UploadProgress, "resumedChunks" | "totalChunks"> = {
      ...(staged.size > 0 ? { resumedChunks: staged.size } : {}),
      totalChunks,
    };
    report({ sentBytes, totalBytes: file.size, ...base });

    // SEQUENTIAL on purpose (design C3): parallel staging is a later
    // throughput optimization; resume does not need it and ordering keeps the
    // failure story simple -- everything before the failed chunk landed.
    for (let n = 1; n <= totalChunks; n += 1) {
      if (staged.has(n)) continue;
      const start = (n - 1) * session.chunkSize;
      const body = file.slice(start, Math.min(start + session.chunkSize, file.size));
      await this.putChunk(session, n, body, signal);
      sentBytes += body.size;
      report({ sentBytes, totalBytes: file.size, ...base });
    }

    const response = await this.fetchImpl(`${this.sessionUrl(session.uploadId)}/complete`, {
      method: "POST",
      credentials: "same-origin",
      headers: await this.headers(),
      signal,
    });
    if (!response.ok) throw new Error(await refusalFrom(response));
    const payload = (await response.json()) as { artifactId?: unknown };
    const artifactId = typeof payload.artifactId === "string" ? payload.artifactId : "";
    if (!artifactId) throw new Error("Upload landed but named no artifact.");
    this.resume.forget(file);
    return { artifactId, title: file.name, fileKind: "file", source: "uploaded" };
  }
}
