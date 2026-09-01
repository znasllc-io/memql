import type { UploadHandle } from "../../items/upload";

// The Training app's upload transport: a file into the caller's own space,
// where the analyzer picks it up.
//
// ===========================================================================
// THE SAME TRANSPORT SHAPE AS THE DESK'S, A DIFFERENT ROUTE
// ===========================================================================
// `EdgeUploadProvider` (items/edgeUpload.ts) posts to `/_memql/artifacts` and
// gets a Library artifact back. This posts to
// `/_memql/spaces/{spaceId}/attachments` and gets an attachment back, and
// everything else about it is the same: the same `UploadHandle` contract the
// per-item progress / in-surface error / retry UI is written against, the same
// same-origin marker, the same bearer-from-a-capability rule.
//
// It is a SECOND PROVIDER rather than a parameter on the first because the two
// return different things. Folding them together would mean one provider whose
// result type depends on a constructor argument, which every caller then has
// to narrow -- an abstraction bought with a cast at both call sites.
//
// ===========================================================================
// WHY THE MARKER, AND WHY THE MARKER HAD TO LEARN THIS ROUTE
// ===========================================================================
// The OS is served by `component/edge` at `os.<domain>`. A bare same-origin
// POST to `/spaces/{id}/attachments` resolves to no file in the bundle and
// takes the SPA fallback, so it is answered with index.html and a 200: a
// silent success for an upload that stored nothing. `/_memql/*` is the one
// prefix a bundle may not claim, and `upstreamPath` (component/edge/proxy.go)
// strips it for the bff's own roots -- which is where `/spaces` was added
// (memql#4738), beside `/artifacts`.

/** What the attachment route answers with. The analysis Plan is NOT in this
 *  reply -- it is stamped server-side and arrives on the plan feed. */
export interface AttachmentUploadResult {
  attachmentId: string;
  fileName: string;
}

export interface AttachmentUploadProvider {
  upload(spaceId: string, file: File): UploadHandle<AttachmentUploadResult>;
}

/** The edge's same-origin API marker, resolved against the bundle's base. */
export function attachmentsPath(baseUrl: string, spaceId: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return `${mount}_memql/spaces/${encodeURIComponent(spaceId)}/attachments`;
}

/**
 * The refusal a reader sees when the server said something.
 *
 * THE SERVER'S OWN SENTENCE, VERBATIM. `attachment_handler.go` answers with
 * plain text and names the actual problem -- "unsupported file type: x",
 * "file too large: max N bytes", "space not found" -- and a friendlier
 * paraphrase would drop the one fact that helps. The status code is the
 * fallback for a body that is empty or is not text.
 */
async function refusalFrom(response: Response): Promise<string> {
  let body = "";
  try {
    body = (await response.text()).trim();
  } catch {
    body = "";
  }
  // An HTML body is the SPA fallback answering, not the bff: rendering a page
  // of markup as an error message would be worse than saying nothing, and this
  // is the exact failure the `/_memql` marker exists to prevent -- so it is
  // named rather than hidden.
  if (body.startsWith("<")) {
    return `The upload was answered by the site rather than the cluster (${response.status}).`;
  }
  if (body === "") return `The cluster refused the upload (${response.status}).`;
  return body;
}

/** The `id` off an attachment reply, in either of the two shapes the handler
 *  itself accepts. "" for anything else -- see the call site. */
async function attachmentIdFrom(response: Response): Promise<string> {
  try {
    const payload: unknown = await response.json();
    const first = Array.isArray(payload) ? payload[0] : payload;
    if (first !== null && typeof first === "object") {
      const id = (first as { id?: unknown }).id;
      if (typeof id === "string") return id.trim();
    }
  } catch {
    // A body that is not JSON. The upload still landed.
  }
  return "";
}

export class EdgeAttachmentUploadProvider implements AttachmentUploadProvider {
  constructor(
    private readonly bearer: () => Promise<string | null>,
    private readonly baseUrl: string = import.meta.env.BASE_URL,
    private readonly fetchImpl: typeof fetch = fetch,
  ) {}

  upload(spaceId: string, file: File): UploadHandle<AttachmentUploadResult> {
    const abort = new AbortController();
    const done = (async (): Promise<AttachmentUploadResult> => {
      const token = await this.bearer();
      const body = new FormData();
      // `file` is `attachmentFormFileKey` (component/server/attachment_handler.go).
      // The handler answers "file field is required" for anything else, which
      // is a 400 nobody could act on from this side.
      body.append("file", file);
      const response = await this.fetchImpl(attachmentsPath(this.baseUrl, spaceId), {
        method: "POST",
        body,
        credentials: "same-origin",
        ...(token ? { headers: { Authorization: `Bearer ${token}` } } : {}),
        signal: abort.signal,
      });
      if (!response.ok) throw new Error(await refusalFrom(response));

      // The 201 carries the attachment node JSON, which the handler itself
      // reads as EITHER an object or a one-element array -- the shaped
      // mutation response differs by which surface wrote it, and
      // `extractAttachmentId` tries both. So does this, for the same reason:
      // a reply in the other shape is a successful upload, not a broken one.
      //
      // BEST-EFFORT THROUGHOUT. The analysis is driven by the Plan the handler
      // stamped, which arrives on the plan feed; nothing on this surface needs
      // this id, so a reply shaped differently than either expectation must
      // not turn a stored file into a failed upload. The name is this side's
      // own, which is what the surface renders.
      return { attachmentId: await attachmentIdFrom(response), fileName: file.name };
    })();
    return { done, abort: () => abort.abort() };
  }
}
