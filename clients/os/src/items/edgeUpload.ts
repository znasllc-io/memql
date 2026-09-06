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
// RESUME IS RE-DROP. localStorage maps (name, size, lastModified) -- plus the
// target artifact, when the upload is a new VERSION of one -- to the open
// session; on a re-drop the provider reads the staged inventory and uploads
// only the missing chunks. A remembered session the server no longer answers
// for falls back to a fresh one -- resume is an optimization, never a
// precondition.
//
// A NEW VERSION IS THE SAME TRANSFER WITH ONE MORE FIELD (epic memql#4806).
// `targetArtifactId` rides the same form and the same init body, so chunking,
// resume, per-chunk retry, progress and verbatim refusals all apply to a
// version exactly as they apply to a first upload -- which is what the one
// upload path is FOR. Nothing below branches on it except the two places that
// have to: the field itself, and the resume key.

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
    // WRAPPED, NOT `fetch` BARE (memql#4842 QA find): a bare reference is
    // UNBOUND, and calling it as `this.fetchImpl(...)` hands the provider as
    // the receiver -- Chromium throws "Illegal invocation" before a single
    // byte moves. Every browser upload died on this line with nothing in any
    // log, which is half of how "uploads never work" stayed a mystery (the
    // other half was the 401 the engine answered once a request DID move).
    // jsdom's fetch tolerates any receiver, so the suite could never see it.
    private readonly fetchImpl: typeof fetch = (input, init) => fetch(input, init),
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
    // A new version of an existing artifact (epic memql#4806): the cluster
    // keeps the artifact's id, filing and labels, and freezes what was there.
    if (opts?.targetArtifactId) body.append("targetArtifactId", opts.targetArtifactId);
    const response = await this.fetchImpl(this.path, {
      method: "POST",
      body,
      credentials: "same-origin",
      headers: await this.headers(),
      signal,
    });
    if (!response.ok) throw new Error(await refusalFrom(response));
    const landed = readUploadResponse(await response.json());
    if (!landed.artifactId) throw new Error("Upload landed but named no artifact.");
    report({ sentBytes: file.size, totalBytes: file.size });
    return {
      artifactId: landed.artifactId,
      title: file.name,
      fileKind: "file",
      source: "uploaded",
      ...(landed.fileId === "" ? {} : { fileId: landed.fileId }),
      ...(landed.versionNumber === undefined ? {} : { versionNumber: landed.versionNumber }),
    };
  }

  // ---- the chunked session ----

  private sessionUrl(uploadId = ""): string {
    return `${this.path}/uploads${uploadId ? `/${encodeURIComponent(uploadId)}` : ""}`;
  }

  /** The remembered session's staged inventory, or null when it is gone --
   *  which is a fresh start, never a failure. */
  private async recallSession(
    file: File,
    opts: UploadOptions | undefined,
    signal: AbortSignal,
  ): Promise<SessionFacts | null> {
    const remembered = this.resume.recall(file, opts?.targetArtifactId);
    if (remembered === null) return null;
    try {
      const response = await this.fetchImpl(this.sessionUrl(remembered), {
        method: "GET",
        credentials: "same-origin",
        headers: await this.headers(),
        signal,
      });
      if (!response.ok) {
        this.resume.forget(file, opts?.targetArtifactId);
        return null;
      }
      // The inventory route's shape (memql#4782): status open|completed|
      // abandoned, staged: [{n, size}] sorted by n. Anything but an OPEN
      // session is a fresh start -- a completed one already landed its file,
      // and re-dropping means the person wants it uploaded again.
      const payload = (await response.json()) as {
        status?: unknown;
        chunkSize?: unknown;
        staged?: unknown;
      };
      if (payload.status !== "open") {
        this.resume.forget(file, opts?.targetArtifactId);
        return null;
      }
      const chunkSize = typeof payload.chunkSize === "number" && payload.chunkSize > 0 ? payload.chunkSize : CHUNK_BYTES;
      const staged = Array.isArray(payload.staged)
        ? payload.staged
            .map((entry) => (entry && typeof entry === "object" ? (entry as { n?: unknown }).n : null))
            .filter((n): n is number => typeof n === "number")
        : [];
      return { uploadId: remembered, chunkSize, stagedChunks: staged };
    } catch (err) {
      if (signal.aborted) throw err;
      this.resume.forget(file, opts?.targetArtifactId);
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
        // Sent at INIT rather than at complete: the cluster gates the target
        // here, before a single chunk moves, so a target that is not the
        // caller's is refused before anybody streams gigabytes at it.
        ...(opts?.targetArtifactId ? { targetArtifactId: opts.targetArtifactId } : {}),
      }),
    });
    if (!response.ok) throw new Error(await refusalFrom(response));
    const payload = (await response.json()) as { uploadId?: unknown; chunkSize?: unknown };
    const uploadId = typeof payload.uploadId === "string" ? payload.uploadId : "";
    if (uploadId === "") throw new Error("The cluster opened no upload session.");
    const chunkSize = typeof payload.chunkSize === "number" && payload.chunkSize > 0 ? payload.chunkSize : CHUNK_BYTES;
    this.resume.remember(file, uploadId, opts?.targetArtifactId);
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
    const session = (await this.recallSession(file, opts, signal)) ?? (await this.openSession(file, opts, signal));
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
    const landed = readUploadResponse(await response.json());
    if (!landed.artifactId) throw new Error("Upload landed but named no artifact.");
    this.resume.forget(file, opts?.targetArtifactId);
    return {
      artifactId: landed.artifactId,
      title: file.name,
      fileKind: "file",
      source: "uploaded",
      ...(landed.fileId === "" ? {} : { fileId: landed.fileId }),
      ...(landed.versionNumber === undefined ? {} : { versionNumber: landed.versionNumber }),
    };
  }
}

/**
 * The 201 body, read defensively: the ids are required and the version is
 * optional. A cluster that predates versions answers without the field, and
 * "not stated" must not become version zero -- a surface reading zero would
 * announce "Version 0 uploaded".
 */
function readUploadResponse(raw: unknown): {
  artifactId: string;
  fileId: string;
  versionNumber?: number;
} {
  const payload = (raw ?? {}) as {
    artifactId?: unknown;
    fileId?: unknown;
    versionNumber?: unknown;
  };
  const artifactId = typeof payload.artifactId === "string" ? payload.artifactId : "";
  const fileId = typeof payload.fileId === "string" ? payload.fileId : "";
  const versionNumber =
    typeof payload.versionNumber === "number" && payload.versionNumber >= 1
      ? payload.versionNumber
      : undefined;
  return versionNumber === undefined
    ? { artifactId, fileId }
    : { artifactId, fileId, versionNumber };
}
