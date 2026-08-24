// The Library's two BYTE-BEARING addresses, and the only place in the portal
// that speaks HTTP to the engine.
//
// Everything else the Artifacts page does rides the one WebSocket the SDK
// multiplexes (src/cluster/ClusterProvider.tsx). These two do not, and the
// reason is recorded in CLAUDE.md's endpoint-protocol exception table and in
// component/server/artifact_handler.go: an upload is multipart/form-data --
// an arbitrary file, not a fixed protobuf schema -- and the export is the
// same bytes coming back. memql#4341 declared them; this module is the
// browser half.
//
//   POST <root>                       multipart, field `file`, optional
//                                     `name` and `labels` -> 201
//                                     {artifactId, fileId}
//   GET  <root>/{artifactId}/content  the bytes, or a note / generated
//                                     output / memory rendered as a download
//
// ===========================================================================
// WHY THE ROOT CARRIES THE `/_memql` MARKER AND NOT A BARE `/artifacts`
// ===========================================================================
//
// The bff mounts these routes at its own root (`server.ArtifactPaths()` ->
// `/artifacts`, `/artifacts/`) and the front door routes them on
// `api.<domain>`. The portal is NOT served from there: it is site #1 on
// `portal.<domain>`, served by component/edge, and the edge's SPA fallback
// answers any path it cannot resolve to a file with index.html. So a bare
// same-origin `POST /artifacts` from this bundle is not a 404 -- it is a 200
// carrying the portal's own HTML, which is the silent-success shape this tree
// keeps closing.
//
// `/_memql/*` is the edge's reserved same-origin API marker (memql#3712): the
// prefix no site's bundle may claim, reverse-proxied to the bff. The bridge
// maps `/_memql/ws` -> `/memql/ws`; the Library's byte routes live at the
// bff's root, so component/edge/proxy.go strips the marker outright for them
// (`/_memql/artifacts/x/content` -> `/artifacts/x/content`). Going through
// the marker rather than the bare path is what keeps a customer site that
// happens to have its own `/artifacts` page from having it swallowed by the
// proxy.
//
// The upload's bearer is an `Authorization` header; the EXPORT is a plain
// anchor (`ButtonLink`), which cannot carry one -- it relies on the
// `memql_auth` cookie the bff's TokenCookieMiddleware reads, which is exactly
// what going through the site's own origin buys (component/edge/proxy.go's
// header states the SameSite=Lax reasoning). Same-origin is not a
// convenience here; it is the mechanism.

// The mount prefix is derived, never hardcoded, for the same reason
// src/cluster/endpoint.ts derives the bridge path: Vite's `base` is decided
// once and everything else composes from it.
export function artifactsApiRootFor(baseUrl: string): string {
  const prefix = baseUrl.replace(/\/portal\/?$/, "").replace(/\/+$/, "");
  return prefix + "/_memql/artifacts";
}

export const ARTIFACTS_API_ROOT = artifactsApiRootFor(import.meta.env.BASE_URL);

// artifactContentUrl is the export address for ANY artifact -- a file's
// stored bytes, and equally a note / generated output / memory, which the
// handler renders as markdown or plain text with a derived filename (design
// D9: "one route exports the whole Library"). There is no kind to branch on
// here, and a page that grew one would be wrong.
export function artifactContentUrl(artifactId: string): string {
  return `${ARTIFACTS_API_ROOT}/${encodeURIComponent(artifactId)}/content`;
}

// The multipart field names the handler reads (libraryFormFileKey /
// libraryFormLabelsKey in artifact_handler.go). Labels are comma-separated on
// the wire and land on the promoted index row through the same builtin the
// label editor calls.
//
// The route also accepts an optional `name` overriding the part's own
// filename. This page does not send one: the picker already carries the name
// the person chose, and a second field to retype it would be a way to
// disagree with it. The field is the CI-upload case's, not the browser's.
const FORM_FILE = "file";
const FORM_LABELS = "labels";

// Why the failures are an ENUM rather than a message. Three of them are
// remedies a person can act on and one is a deployment fault, and a page that
// only had a string would have to pattern-match prose to tell them apart:
//
//   too_large      the file is over MEMQL_LIBRARY_MAX_UPLOAD_BYTES (256 MB by
//                  default). The handler answers 413 twice over -- a whole-body
//                  MaxBytesReader and a per-file check -- so this is the one
//                  size answer, not a guess.
//   unauthenticated the bearer resolved to no user. The upload stamps
//                  ownerUserId from actor.userId, so there is nowhere to put
//                  the bytes.
//   rejected       the request was malformed (no file part).
//   network        the request never completed.
//   not_routed     a 2xx that is not JSON -- i.e. the origin serving this
//                  bundle answered the upload itself instead of proxying it.
//                  See the header: this is the failure that would otherwise
//                  look like success.
//   server         everything else, with whatever the server said.
//
// THERE IS NO `unsupported_type`, DELIBERATELY. The route accepts ANY MIME
// (design 3.4) -- an unknown type is stored opaquely with `status: ready` and
// no chunks -- so there is no 415 to handle and a page must not imply one by
// filtering the picker.
export type ArtifactUploadFailure =
  | "too_large"
  | "unauthenticated"
  | "rejected"
  | "network"
  | "not_routed"
  | "server";

export class ArtifactUploadError extends Error {
  readonly kind: ArtifactUploadFailure;
  readonly status: number;

  constructor(kind: ArtifactUploadFailure, status: number, message: string) {
    super(message);
    this.name = "ArtifactUploadError";
    this.kind = kind;
    this.status = status;
  }
}

export interface ArtifactUploadResult {
  artifactId: string;
  fileId: string;
}

// The slice of XMLHttpRequest this module uses, named so a test can supply a
// double. XHR rather than fetch for ONE reason: fetch reports no upload
// progress in any browser, and "progress" is a requirement here -- a 256 MB
// cap means an upload people will watch.
export interface UploadProgressEvent {
  lengthComputable: boolean;
  loaded: number;
  total: number;
}

export interface UploadRequest {
  upload: { onprogress: ((event: UploadProgressEvent) => void) | null };
  open(method: string, url: string): void;
  setRequestHeader(name: string, value: string): void;
  send(body: FormData): void;
  readonly status: number;
  readonly responseText: string;
  getResponseHeader(name: string): string | null;
  onload: (() => void) | null;
  onerror: (() => void) | null;
}

export interface UploadArtifactParams {
  file: File;
  // Applied to the PROMOTED INDEX ROW, not to the file: labels are an
  // artifact-index concern (dsl/library/concepts.memql), and the handler
  // routes them through the same libraryAddArtifactLabel capability the label
  // editor on the detail page uses.
  labels?: readonly string[];
  // The connection's current credential, read at call time through the
  // PortalAuthSource seam. Null is a refusal, not a silent anonymous upload.
  bearer: string | null;
  onProgress?: (fraction: number) => void;
}

// No injection seam for the request object, deliberately: a test replaces
// globalThis.XMLHttpRequest (vi.stubGlobal), which exercises this line too. An
// `open?:` parameter would leave the one statement that actually runs in
// production untested, which is the wrong half to skip.
function newRequest(): UploadRequest {
  return new XMLHttpRequest() as unknown as UploadRequest;
}

export function uploadArtifact(params: UploadArtifactParams): Promise<ArtifactUploadResult> {
  const bearer = (params.bearer ?? "").trim();
  if (bearer === "") {
    return Promise.reject(
      new ArtifactUploadError(
        "unauthenticated",
        0,
        "No credential to upload with. The Library stamps the file's owner from the " +
          "signed-in user, so an upload with no session has nowhere to land.",
      ),
    );
  }

  const form = new FormData();
  form.append(FORM_FILE, params.file, params.file.name);
  const labels = (params.labels ?? []).map((one) => one.trim()).filter((one) => one !== "");
  if (labels.length > 0) form.append(FORM_LABELS, labels.join(","));

  const request = newRequest();

  return new Promise<ArtifactUploadResult>((resolve, reject) => {
    request.upload.onprogress = (event) => {
      if (!params.onProgress) return;
      // An indeterminate length reports 0 rather than a made-up number: a bar
      // that claims a percentage it does not know is worse than one that
      // stays at the start.
      params.onProgress(event.lengthComputable && event.total > 0 ? event.loaded / event.total : 0);
    };
    request.onerror = () => {
      reject(
        new ArtifactUploadError(
          "network",
          0,
          "The upload did not reach the cluster. Check the connection and try again.",
        ),
      );
    };
    request.onload = () => {
      try {
        resolve(readUploadResponse(request));
      } catch (err) {
        reject(err);
      }
    };

    request.open("POST", ARTIFACTS_API_ROOT);
    // Authorization only. Content-Type is DELIBERATELY not set: the browser
    // stamps the multipart boundary, and setting it by hand produces a body
    // the server cannot parse -- the same note sdk/ts's uploadAttachment
    // carries.
    request.setRequestHeader("Authorization", `Bearer ${bearer}`);
    request.send(form);
  });
}

function readUploadResponse(request: UploadRequest): ArtifactUploadResult {
  const status = request.status;
  const body = request.responseText ?? "";

  if (status === 413) {
    throw new ArtifactUploadError(
      "too_large",
      status,
      "That file is over this cluster's upload limit (MEMQL_LIBRARY_MAX_UPLOAD_BYTES, " +
        "256 MB unless an operator changed it).",
    );
  }
  if (status === 401 || status === 403) {
    throw new ArtifactUploadError(
      "unauthenticated",
      status,
      "The cluster refused the credential this upload carried. Sign in again and retry.",
    );
  }
  if (status === 400) {
    throw new ArtifactUploadError("rejected", status, firstLine(body) || "The cluster rejected the upload.");
  }
  if (status === 0) {
    throw new ArtifactUploadError(
      "network",
      status,
      "The upload did not reach the cluster. Check the connection and try again.",
    );
  }
  if (status < 200 || status >= 300) {
    throw new ArtifactUploadError("server", status, firstLine(body) || `The cluster answered ${status}.`);
  }

  // A 2xx that is not JSON means the ORIGIN serving this page answered the
  // upload instead of proxying it to the bff -- the SPA fallback returning
  // index.html. Named rather than parsed-and-shrugged: without this the page
  // would show a generic "unexpected response" for a deployment fault whose
  // remedy is a routing rule, not a retry.
  const contentType = (request.getResponseHeader("Content-Type") ?? "").toLowerCase();
  if (!contentType.includes("json")) {
    throw new ArtifactUploadError(
      "not_routed",
      status,
      `The origin serving this page answered the upload itself instead of passing it to the ` +
        `cluster (${status}, ${contentType || "no content type"}). ${ARTIFACTS_API_ROOT} is not ` +
        `routed to the API here.`,
    );
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    throw new ArtifactUploadError(
      "not_routed",
      status,
      `The upload answered ${status} with a body that is not JSON. ${ARTIFACTS_API_ROOT} is not ` +
        `routed to the API here.`,
    );
  }

  const doc = (parsed ?? {}) as Record<string, unknown>;
  const artifactId = typeof doc["artifactId"] === "string" ? doc["artifactId"].trim() : "";
  const fileId = typeof doc["fileId"] === "string" ? doc["fileId"].trim() : "";
  if (artifactId === "") {
    throw new ArtifactUploadError(
      "server",
      status,
      "The upload succeeded but the reply named no artifact, so there is nothing to open.",
    );
  }
  return { artifactId, fileId };
}

function firstLine(body: string): string {
  return body.split("\n")[0]?.trim().slice(0, 300) ?? "";
}
