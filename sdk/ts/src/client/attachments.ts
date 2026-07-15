// Space attachment upload helper (memql#2523).
//
// The engine exposes `POST /spaces/{partitionId}/attachments` on the same
// front door as the `/memql/ws` bridge: a multipart/form-data upload with a
// single `file` part, authenticated by `Authorization: Bearer <jwt>`. On
// success it returns 201 with the created attachment node JSON (a top-level
// `id`, plus the fields in the server's AttachmentResponse). See
// component/server/attachment_handler.go.
//
// This helper is auth-consistent with a dialed Connection: it reads the
// CURRENT bearer through the injected source at call time, so an upload issued
// after an in-place rotation (#1110) carries the rotated token, not the token
// the connection first dialed with. The HTTP origin is derived from the same
// endpoint the WebSocket dialed, so a single `Connection.dial` configures both
// transports.

// The multipart form field the server reads the file from
// (attachmentFormFileKey in attachment_handler.go).
const ATTACHMENT_FORM_FILE_KEY = "file";

/** The credential + endpoint source an upload draws from. A live Connection
 * implements this so the helper always sends the connection's current token
 * (post-rotation) to the connection's own host. */
export interface AttachmentUploadSource {
  /** The current bearer JWT, or undefined when the source has none. Read at
   * call time so rotation is reflected. */
  authToken(): string | undefined;
  /** Absolute HTTP(S) origin the attachments endpoint lives under, e.g.
   * `https://staging.host`. */
  attachmentBaseUrl(): string;
}

/** The bytes to upload. A Blob carries its own content type; a raw
 * Uint8Array / ArrayBuffer relies on `contentType`. */
export type AttachmentFile = Blob | Uint8Array | ArrayBuffer;

export interface UploadAttachmentParams {
  /** Bare space id (server canonicalizes). Path segment. */
  partitionId: string;
  /** File bytes. */
  file: AttachmentFile;
  /** File name recorded on the attachment node. */
  fileName: string;
  /** MIME type for the file part. Used when `file` is not a typed Blob; the
   * server validates it against its allow-list. Defaults to a Blob's own
   * type, then `application/octet-stream`. */
  contentType?: string;
  /** Cancels the request. */
  signal?: AbortSignal;
  /** fetch override (tests / non-browser runtimes without a global fetch).
   * Defaults to `globalThis.fetch`. */
  fetchImpl?: typeof fetch;
}

/** The created attachment reference. `id` is always present; the remaining
 * fields mirror the server's AttachmentResponse when it returns them, and
 * `raw` carries the untouched server JSON for forward compatibility. */
export interface AttachmentRef {
  id: string;
  partitionId?: string;
  fileName?: string;
  mimeType?: string;
  fileSize?: number;
  blobUrl?: string;
  status?: string;
  uploadedBy?: string;
  createdAt?: string;
  transcription?: string;
  summary?: string;
  raw: unknown;
}

/** uploadAttachment POSTs a file to `/spaces/{partitionId}/attachments` on the
 * source's origin, authenticated with the source's current bearer, and returns
 * the created attachment reference. Throws on a missing token, a transport
 * failure, a non-2xx response, or a response missing an `id`. */
export async function uploadAttachment(
  source: AttachmentUploadSource,
  params: UploadAttachmentParams,
): Promise<AttachmentRef> {
  const partitionId = params.partitionId.trim();
  if (!partitionId) throw new Error("uploadAttachment: partitionId is required");
  const fileName = params.fileName.trim();
  if (!fileName) throw new Error("uploadAttachment: fileName is required");

  const token = source.authToken()?.trim();
  if (!token) {
    throw new Error(
      "uploadAttachment: no bearer token available on the connection (attachments require Authorization: Bearer)",
    );
  }

  const doFetch = params.fetchImpl ?? globalThis.fetch;
  if (typeof doFetch !== "function") {
    throw new Error("uploadAttachment: no fetch available -- pass fetchImpl or run on Node 18+/a browser");
  }

  const blob = toBlob(params.file, params.contentType);
  const form = new FormData();
  form.append(ATTACHMENT_FORM_FILE_KEY, blob, fileName);

  const base = source.attachmentBaseUrl().replace(/\/+$/, "");
  const url = `${base}/spaces/${encodeURIComponent(partitionId)}/attachments`;

  // NOTE: do not set Content-Type; fetch stamps the multipart boundary.
  const res = await doFetch(url, {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
    body: form,
    signal: params.signal,
  });

  if (!res.ok) {
    const detail = await safeReadText(res);
    throw new Error(
      `uploadAttachment: server returned ${res.status}${detail ? ` -- ${detail}` : ""}`,
    );
  }

  const parsed = (await res.json()) as unknown;
  return toAttachmentRef(parsed);
}

function toBlob(file: AttachmentFile, contentType?: string): Blob {
  if (file instanceof Blob) {
    // A typed Blob already carries its content type; only wrap to override.
    if (contentType && file.type !== contentType) {
      return new Blob([file], { type: contentType });
    }
    return file;
  }
  const type = contentType ?? "application/octet-stream";
  const source = file instanceof Uint8Array ? file : new Uint8Array(file);
  // Copy into a fresh ArrayBuffer-backed buffer: a plain ArrayBuffer is an
  // unambiguous BlobPart, sidestepping the Uint8Array<ArrayBufferLike> vs
  // ArrayBuffer generic mismatch (a SharedArrayBuffer-backed view is not a
  // valid BlobPart under the DOM lib types).
  const buffer = new ArrayBuffer(source.byteLength);
  new Uint8Array(buffer).set(source);
  return new Blob([buffer], { type });
}

function toAttachmentRef(body: unknown): AttachmentRef {
  const obj = pickObject(body);
  const id = typeof obj.id === "string" ? obj.id.trim() : "";
  if (!id) {
    throw new Error("uploadAttachment: server response missing an attachment id");
  }
  return {
    id,
    partitionId: str(obj.partitionId),
    fileName: str(obj.fileName),
    mimeType: str(obj.mimeType),
    fileSize: num(obj.fileSize),
    blobUrl: str(obj.blobUrl),
    status: str(obj.status),
    uploadedBy: str(obj.uploadedBy),
    createdAt: str(obj.createdAt),
    transcription: str(obj.transcription),
    summary: str(obj.summary),
    raw: body,
  };
}

// pickObject returns the top-level attachment object. The shaped mutation
// response can be an object or a single-element array; mirror the server's
// extractAttachmentId tolerance.
function pickObject(body: unknown): Record<string, unknown> {
  if (Array.isArray(body)) {
    const first = body[0];
    return first && typeof first === "object" ? (first as Record<string, unknown>) : {};
  }
  return body && typeof body === "object" ? (body as Record<string, unknown>) : {};
}

function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

function num(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
}

async function safeReadText(res: Response): Promise<string> {
  try {
    return (await res.text()).trim().slice(0, 500);
  } catch {
    return "";
  }
}
