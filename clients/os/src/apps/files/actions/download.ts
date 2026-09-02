// The download action's client half (design D13). Authorization stays
// bearer-only -- no signed URLs, no cookies, no redirects (memql#4341 D1
// upheld) -- so the bytes always move through a fetch this page makes, and
// the question is only what happens to them next:
//
//   worker    a service worker streams them to disk as they arrive; size
//             does not matter, memory stays flat.
//   buffered  the whole body lands in memory, becomes an object URL, and the
//             browser saves it. Honest up to a limit.
//   refused   past the limit with no worker, the surface renders a sentence
//             naming the limit and the alternatives -- an in-surface answer,
//             never a hung download that dies at 90%.

import { downloadWorkerRegistration, runWorkerDownload } from "./downloadWorker";

export const BUFFER_LIMIT_BYTES = 512 * 1024 * 1024;

export const OVER_LIMIT_SENTENCE =
  "This file is over 512 MiB, and this browser cannot stream it to disk. Open it in VS Code, or pull it with the cockpit on one of your machines.";

export interface DownloadPlan {
  path: "worker" | "buffered" | "refused";
}

export function planDownload(input: { workerAvailable: boolean; sizeBytes: number }): DownloadPlan {
  if (input.workerAvailable) return { path: "worker" };
  // Size 0 is "not measured" (a pre-transfer row, a rendered body), not a
  // refusal: refusing what we could not measure would block small downloads
  // exactly when the metadata is at its weakest.
  if (input.sizeBytes > BUFFER_LIMIT_BYTES) return { path: "refused" };
  return { path: "buffered" };
}

/**
 * The edge's same-origin API marker for the content route.
 *
 * `version` selects an EARLIER version of a file (epic memql#4806). Omitted --
 * every existing caller -- means the current one. A QUERY PARAMETER rather
 * than a path segment because the cluster's front-door path set is generated
 * from its route table, and a new path shape would change it; both spellings
 * live under the one /artifacts/{id}/content rule that already exists.
 */
export function artifactContentPath(baseUrl: string, artifactId: string, version?: number): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  const path = `${mount}_memql/artifacts/${encodeURIComponent(artifactId)}/content`;
  return version === undefined ? path : `${path}?version=${encodeURIComponent(String(version))}`;
}

/**
 * The refusal a reader sees. The server's own sentence, verbatim; an HTML
 * body is the SPA fallback answering, not the bff, and is named as such.
 */
async function refusalFrom(response: Response): Promise<string> {
  let body = "";
  try {
    body = (await response.text()).trim();
  } catch {
    body = "";
  }
  if (body.startsWith("<")) {
    return `The download was answered by the site rather than the cluster (${response.status}).`;
  }
  if (body === "") return `The cluster refused the download (${response.status}).`;
  return body;
}

/**
 * The whole download decision for one artifact, from the row to the bytes.
 *
 * SHARED BECAUSE IT IS ONE ACTION WITH TWO ENTRY POINTS -- the inspector's
 * Download button and the row's right-click menu. The size lookup, the
 * worker-versus-buffered plan and the over-limit refusal are the parts that
 * would drift if each surface kept its own copy, and the way they would drift
 * is silent: a second copy that forgets the size read simply plans as though
 * every file were small, and the refusal that protects a browser from a
 * 512 MiB buffer never fires.
 *
 * THE OVER-LIMIT CASE THROWS rather than returning a status. Both callers
 * already render a caught error verbatim beside the control that produced it,
 * so throwing gives the refusal the same route and the same sentence as every
 * other failure -- one path, and no caller that can forget to check a flag.
 */
export async function downloadArtifact(input: {
  artifactId: string;
  /** The artifact's display name -- the fallback when the backing file row
   *  carries none, and the whole answer for the kinds that have no such row. */
  name: string;
  /** The backing file id, or "" for a kind with no file row behind it. Those
   *  are small rendered bodies (design D13) and skip the read entirely. */
  fileId: string;
  /** Reads the backing file row. Null when there is no connection, which is
   *  not an error here: the download itself is a bearer fetch, so it can still
   *  run -- it just plans against an unknown size. */
  readFile: ((fileId: string) => Promise<{ sizeBytes: number; name: string } | null>) | null;
  bearer: () => Promise<string | null>;
}): Promise<void> {
  // The 512 MiB decision needs the SIZE, which lives on the backing file row
  // -- the index deliberately does not carry it.
  let sizeBytes = 0;
  let fileName = input.name;
  if (input.fileId !== "" && input.readFile) {
    const meta = await input.readFile(input.fileId);
    if (meta) {
      sizeBytes = meta.sizeBytes;
      fileName = meta.name || input.name;
    }
  }
  const registration = await downloadWorkerRegistration();
  const plan = planDownload({ workerAvailable: registration !== null, sizeBytes });
  if (plan.path === "refused") throw new Error(OVER_LIMIT_SENTENCE);
  if (plan.path === "worker" && registration !== null) {
    await runWorkerDownload({
      artifactId: input.artifactId,
      fileName,
      sizeBytes,
      bearer: input.bearer,
      registration,
    });
    return;
  }
  await runBufferedDownload({
    artifactId: input.artifactId,
    fileName,
    bearer: input.bearer,
  });
}

export interface BufferedDownloadPorts {
  artifactId: string;
  fileName: string;
  /** An earlier version of this file; omitted means the current one. */
  version?: number;
  bearer: () => Promise<string | null>;
  fetchImpl?: typeof fetch;
  baseUrl?: string;
  createObjectUrl?: (blob: Blob) => string;
  revokeObjectUrl?: (url: string) => void;
  /** Hands the browser a URL to save under `name`. Injectable so tests never
   *  navigate; the default clicks a temporary anchor. */
  save?: (url: string, name: string) => void;
}

function anchorSave(url: string, name: string): void {
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
}

/**
 * The buffered path: fetch whole, save, revoke. The object URL is revoked on
 * a timeout after the save call rather than synchronously -- a URL revoked
 * before the browser opens it saves nothing, silently.
 */
export async function runBufferedDownload(ports: BufferedDownloadPorts): Promise<void> {
  const fetchImpl = ports.fetchImpl ?? fetch;
  const createUrl = ports.createObjectUrl ?? ((blob: Blob) => URL.createObjectURL(blob));
  const revokeUrl = ports.revokeObjectUrl ?? ((url: string) => URL.revokeObjectURL(url));
  const save = ports.save ?? anchorSave;
  const base = ports.baseUrl ?? import.meta.env.BASE_URL;

  const token = await ports.bearer();
  const response = await fetchImpl(artifactContentPath(base, ports.artifactId, ports.version), {
    method: "GET",
    credentials: "same-origin",
    ...(token ? { headers: { Authorization: `Bearer ${token}` } } : {}),
  });
  if (!response.ok) throw new Error(await refusalFrom(response));
  const blob = await response.blob();
  const url = createUrl(blob);
  try {
    save(url, ports.fileName);
  } finally {
    // In the test ports this runs inline; in the browser the default revoke
    // waits a beat so the click above has opened the URL.
    if (ports.revokeObjectUrl) revokeUrl(url);
    else setTimeout(() => revokeUrl(url), 10_000);
  }
}
