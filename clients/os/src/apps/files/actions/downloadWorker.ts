import { artifactContentPath } from "./download";

// The streaming download path (design D13): a service worker turns a fetch
// this page makes into a navigation response the browser writes to disk AS
// THE BYTES ARRIVE, so memory stays flat at any size.
//
// ===========================================================================
// WHY THE PAGE FETCHES AND THE WORKER ONLY SERVES
// ===========================================================================
// Authorization is bearer-only (memql#4341 D1): no signed URLs, no cookies.
// A worker that fetched the content route itself would need the bearer, and
// a bearer handed to a service worker outlives the page that held it. So the
// PAGE fetches -- same code path, same verifier -- and streams body chunks
// over a MessageChannel; the worker's only job is to answer one synthesized
// in-scope URL (`/__memql-dl/<id>`) with a Content-Disposition response fed
// from that channel. The worker never sees a credential.
//
// The worker file itself is `public/download-sw.js` -- plain JS, copied to
// the bundle root, registered lazily the first time a download wants it.

export const DOWNLOAD_SW_PATH = "download-sw.js";
export const DOWNLOAD_SW_SCOPE = "__memql-dl/";

interface PortLike {
  postMessage(message: { type: string; chunk?: Uint8Array }, transfer?: Transferable[]): void;
}

/**
 * Pump a response body to the worker's port: every chunk in order, then
 * `done` -- or `abort` on a mid-stream failure, so the worker terminates the
 * navigation response and the browser reports a failed download rather than
 * quietly keeping a truncated file that looks complete.
 */
export async function pumpToPort(body: ReadableStream<Uint8Array>, port: PortLike): Promise<void> {
  const reader = body.getReader();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (value) port.postMessage({ type: "chunk", chunk: value }, [value.buffer]);
    }
    port.postMessage({ type: "done" });
  } catch (err) {
    port.postMessage({ type: "abort" });
    throw err;
  }
}

/** Resolve the active registration, registering on first use. Null when this
 *  browser has no service workers (private modes, older embeds). */
export async function downloadWorkerRegistration(): Promise<ServiceWorkerRegistration | null> {
  if (!("serviceWorker" in navigator)) return null;
  try {
    const base = import.meta.env.BASE_URL.endsWith("/")
      ? import.meta.env.BASE_URL
      : import.meta.env.BASE_URL + "/";
    const registration = await navigator.serviceWorker.register(base + DOWNLOAD_SW_PATH, {
      scope: base + DOWNLOAD_SW_SCOPE,
    });
    // The OS page is outside the download scope, so serviceWorker.ready
    // would wait forever for a worker controlling this page. Wait for this
    // registration's worker instead, then fall back if it cannot activate.
    const worker = registration.installing ?? registration.waiting ?? registration.active;
    if (!worker) return null;
    if (worker.state !== "activated") {
      const activated = await new Promise<boolean>((resolve) => {
        const finish = (ready: boolean) => {
          clearTimeout(timer);
          worker.removeEventListener("statechange", changed);
          resolve(ready);
        };
        const changed = () => {
          if (worker.state === "activated") finish(true);
          else if (worker.state === "redundant") finish(false);
        };
        const timer = setTimeout(() => finish(false), 4_000);
        worker.addEventListener("statechange", changed);
        changed();
      });
      if (!activated) return null;
    }
    return registration.active ? registration : null;
  } catch {
    return null;
  }
}

/**
 * Stream one artifact to disk through the worker. Resolves when the whole
 * body has been handed over; throws the server's refusal for a non-2xx.
 */
export async function runWorkerDownload(input: {
  artifactId: string;
  fileName: string;
  sizeBytes: number;
  /** An earlier version of this file; omitted means the current one. */
  version?: number;
  bearer: () => Promise<string | null>;
  registration: ServiceWorkerRegistration;
  fetchImpl?: typeof fetch;
  baseUrl?: string;
}): Promise<void> {
  const fetchImpl = input.fetchImpl ?? fetch;
  const base = input.baseUrl ?? import.meta.env.BASE_URL;
  const worker = input.registration.active ?? input.registration.waiting;
  if (!worker) throw new Error("The download worker is not running yet. Try again.");

  const token = await input.bearer();
  const response = await fetchImpl(artifactContentPath(base, input.artifactId, input.version), {
    method: "GET",
    credentials: "same-origin",
    ...(token ? { headers: { Authorization: `Bearer ${token}` } } : {}),
  });
  if (!response.ok || !response.body) {
    const body = await response.text().catch(() => "");
    const line = body.trim().startsWith("<") || body.trim() === "" ? `The cluster refused the download (${response.status}).` : body.trim();
    throw new Error(line);
  }

  // Ask the worker to open a download URL fed by our channel; it answers with
  // the in-scope URL to navigate. An unanswered ask times out rather than
  // hanging the click.
  const channel = new MessageChannel();
  const url = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("The download worker did not answer.")), 4_000);
    channel.port1.onmessage = (event: MessageEvent) => {
      const data = event.data as { type?: string; url?: string };
      if (data?.type === "ready" && typeof data.url === "string") {
        clearTimeout(timer);
        resolve(data.url);
      }
    };
    worker.postMessage(
      { type: "memql-download-open", name: input.fileName, size: input.sizeBytes },
      [channel.port2],
    );
  });

  // A hidden iframe starts the navigation download without leaving the OS.
  const frame = document.createElement("iframe");
  frame.hidden = true;
  frame.src = url;
  document.body.appendChild(frame);
  try {
    await pumpToPort(response.body, channel.port1);
  } finally {
    // A large transfer may take minutes. Keep its navigation alive until the
    // pump settles, then allow the browser to consume the remaining chunks.
    setTimeout(() => frame.remove(), 60_000);
  }
}
