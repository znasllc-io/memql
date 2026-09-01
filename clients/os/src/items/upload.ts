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
export interface UploadHandle<T = UploadResult> {
  done: Promise<T>;
  abort: () => void;
}

export interface UploadOptions {
  /** Library folder the file lands in. Omitted = the root (design D4/B2). */
  folderId?: string;
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

  upload(file: File, _opts?: UploadOptions): UploadHandle {
    let cancelled = false;
    const done = new Promise<UploadResult>((resolve, reject) => {
      setTimeout(() => {
        if (cancelled) {
          reject(new Error("upload aborted"));
          return;
        }
        resolve({
          artifactId: `local-${file.name}`,
          title: file.name,
          fileKind: "file",
          source: "uploaded",
        });
      }, this.delayMs);
    });
    return { done, abort: () => void (cancelled = true) };
  }
}
