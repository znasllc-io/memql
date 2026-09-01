// The upload seam (spec B): dropping a host file onto the desk goes
// through this provider. PR A ships the in-memory implementation so the
// whole progress/failure/retry UI is real before any network exists; the
// wire task binds POST /artifacts behind the SAME interface -- the result
// shape below matches what the Library promotion returns for a file.

export interface UploadResult {
  artifactId: string;
  title: string;
  /** Library artifact kind for the icon glyph (file/document/...). */
  fileKind: string;
  /** Library provenance source ("uploaded" on this path). */
  source: string;
  /**
   * Which version landed (epic memql#4806). 1 for a fresh upload, N for a new
   * version of an existing artifact, so a surface can say "Version 3 uploaded"
   * without a second read.
   *
   * OPTIONAL, because two providers here honestly have no answer: the
   * in-memory stand-in never talked to a cluster, and the attachment route
   * does not version. Absent means "not stated", which a reader shows as
   * nothing rather than as version zero.
   */
  versionNumber?: number;
}

/**
 * One upload in flight: a promise for its result and a way to stop it.
 *
 * GENERIC OVER THE RESULT, because the second consumer landed (memql#4738).
 * The desk's file drops go to the Library and get an artifact back; the
 * Training app's dropzone goes to the space attachment route and gets an
 * attachment back. What they SHARE is this shape -- an abortable promise --
 * and it is the shape the whole progress / in-surface failure / retry UI is
 * written against, so the alternative to a type parameter was a second copy of
 * that UI beside a second copy of this interface.
 *
 * The parameter DEFAULTS to `UploadResult` so every existing spelling of
 * `UploadHandle` still means what it did.
 */
/** Byte progress, honest to what actually landed. */
export interface UploadProgress {
  sentBytes: number;
  totalBytes: number;
  /** Chunks the cluster already held when a resume started -- what the
   *  surface says "12 of 40 chunks already in the cluster" with. Absent on a
   *  fresh upload. */
  resumedChunks?: number;
  totalChunks?: number;
}

export interface UploadHandle<T = UploadResult> {
  done: Promise<T>;
  abort: () => void;
  /**
   * Subscribe to byte progress. OPTIONAL: the in-memory provider and the
   * attachment route report nothing, and every existing consumer reads only
   * `done`/`abort`. Returns an unsubscribe.
   */
  onProgress?: (listener: (progress: UploadProgress) => void) => () => void;
}

export interface UploadOptions {
  /** Library folder the file lands in. Omitted = the root (design D4/B2). */
  folderId?: string;
  /**
   * The artifact this upload is a NEW VERSION of (epic memql#4806). Omitted is
   * the ordinary case: a fresh upload. Set, and the artifact keeps its id, its
   * folder and its labels while the previous version is frozen as history.
   *
   * IT RIDES THE PROVIDER LIKE EVERY OTHER OPTION, which is the whole point:
   * the one-path rule (test/files/onePath.test.ts) means a version upload
   * inherits chunking, resume, retry, progress and verbatim refusals without
   * a second route speaker learning any of them.
   */
  targetArtifactId?: string;
}

export interface UploadProvider {
  upload(file: File, opts?: UploadOptions): UploadHandle;
}

/**
 * PR A stand-in: resolves after a tick so the uploading state is visible
 * in tests and the dev server. Deterministic ids come from the file name.
 */
export class InMemoryUploadProvider implements UploadProvider {
  constructor(private readonly delayMs = 30) {}

  upload(file: File, opts?: UploadOptions): UploadHandle {
    let cancelled = false;
    const done = new Promise<UploadResult>((resolve, reject) => {
      setTimeout(() => {
        if (cancelled) {
          reject(new Error("upload aborted"));
          return;
        }
        resolve({
          artifactId: opts?.targetArtifactId ?? `local-${file.name}`,
          title: file.name,
          fileKind: "file",
          source: "uploaded",
        });
      }, this.delayMs);
    });
    return { done, abort: () => void (cancelled = true) };
  }
}
