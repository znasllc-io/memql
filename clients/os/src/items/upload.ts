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

export interface UploadHandle {
  done: Promise<UploadResult>;
  abort: () => void;
}

export interface UploadProvider {
  upload(file: File): UploadHandle;
}

/**
 * PR A stand-in: resolves after a tick so the uploading state is visible
 * in tests and the dev server. Deterministic ids come from the file name.
 */
export class InMemoryUploadProvider implements UploadProvider {
  constructor(private readonly delayMs = 30) {}

  upload(file: File): UploadHandle {
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
