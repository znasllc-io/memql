import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

// The download service worker's message handler (CodeQL alert #1035,
// js/missing-origin-check).
//
// `public/download-sw.js` is copied verbatim into the bundle root, outside the
// module graph and the typechecker, so nothing else in the suite loads it. It
// is plain JS against `self`, which makes it loadable here with a fake one --
// and worth loading, because the guard added for that alert is exactly the
// kind that is easy to write in a way that ALSO refuses the legitimate
// sender. The same-origin case below is the negative control: without it, a
// fix that silently broke every download would pass.

const ORIGIN = "https://os.example.test";
const SCOPE = `${ORIGIN}/__memql-dl/`;

interface Listeners {
  message?: (event: unknown) => void;
  fetch?: (event: unknown) => void;
}

/** Load the worker against a fake `self` and return its registered listeners. */
function loadWorker(): Listeners {
  const src = readFileSync(
    resolve(__dirname, "../../public/download-sw.js"),
    "utf8",
  );
  const listeners: Listeners = {};
  const fakeSelf = {
    addEventListener(type: string, fn: (event: unknown) => void) {
      if (type === "message") listeners.message = fn;
      if (type === "fetch") listeners.fetch = fn;
    },
    skipWaiting() {},
    clients: { claim: () => Promise.resolve() },
    registration: { scope: SCOPE },
    location: { origin: ORIGIN },
  };
  // eslint-disable-next-line @typescript-eslint/no-implied-eval
  new Function("self", src)(fakeSelf);
  if (listeners.message === undefined) throw new Error("the worker registered no message handler");
  return listeners;
}

/** A message with a MessagePort the worker would reply on. */
function open(over: { origin?: string; sourceUrl?: string | null }) {
  const replies: unknown[] = [];
  const port = {
    postMessage(m: unknown) {
      replies.push(m);
    },
    onmessage: null as null | ((event: { data: { type: string; chunk?: Uint8Array } }) => void),
  };
  const event: Record<string, unknown> = {
    data: { type: "memql-download-open", name: "report.pdf", size: 10 },
    ports: [port],
    origin: over.origin,
  };
  if (over.sourceUrl !== null) event.source = { url: over.sourceUrl ?? `${ORIGIN}/index.html` };
  return { event, replies, port };
}

describe("the download worker's stream handoff", () => {
  it.each([
    ["README.md", "text/markdown"], ["report.pdf", "application/pdf"],
    ["image.png", "image/png"], ["package.zip", "application/zip"],
    ["package.tar.gz", "application/gzip"], ["opaque.custom", "application/octet-stream"],
  ])("saves %s with its exact filename, MIME type and binary bytes", async (name, mimeType) => {
    const worker = loadWorker();
    const { event, replies, port } = open({ origin: ORIGIN });
    event.data = { type: "memql-download-open", name, mimeType, size: 5 };
    worker.message!(event);
    const url = (replies[0] as { url: string }).url;
    expect(url).toMatch(new RegExp(`/__memql-dl/[^/]+/${name!.replaceAll(".", "\\.")}$`));
    let response: Response | undefined;
    worker.fetch!({ request: { url }, respondWith: (value: Response) => { response = value; } });
    port.onmessage!({ data: { type: "chunk", chunk: new Uint8Array([0, 255, 13, 10, 128]) } });
    port.onmessage!({ data: { type: "done" } });
    expect(response?.headers.get("Content-Type")).toBe(mimeType);
    expect(response?.headers.get("Content-Disposition")).toBe(`attachment; filename="${name}"; filename*=UTF-8''${name}`);
    expect(new Uint8Array(await response!.arrayBuffer())).toEqual(new Uint8Array([0, 255, 13, 10, 128]));
  });

  it.each(["before", "after"])("serves all bytes when the body finishes %s navigation", async (when) => {
    const worker = loadWorker();
    const { event, replies, port } = open({ origin: ORIGIN });
    worker.message!(event);
    const url = (replies[0] as { url: string }).url;
    const finish = () => {
      port.onmessage!({ data: { type: "chunk", chunk: new TextEncoder().encode("inventory") } });
      port.onmessage!({ data: { type: "done" } });
    };
    if (when === "before") finish();
    let response: Response | undefined;
    worker.fetch!({ request: { url }, respondWith: (value: Response) => { response = value; } });
    if (when === "after") finish();
    expect(response?.headers.get("Content-Disposition")).toContain("report.pdf");
    expect(await response?.text()).toBe("inventory");
  });

  it("lets only one navigation claim a live stream", () => {
    const worker = loadWorker();
    const { event, replies } = open({ origin: ORIGIN });
    worker.message!(event);
    const url = (replies[0] as { url: string }).url;
    const responses: Response[] = [];
    const navigate = () => worker.fetch!({ request: { url }, respondWith: (value: Response) => responses.push(value) });
    navigate();
    navigate();
    expect(responses).toHaveLength(1);
  });
});

describe("the download worker's message handler", () => {
  it("answers a same-origin page -- the control that a fix has not broken downloads", () => {
    const { message } = loadWorker();
    const { event, replies } = open({ origin: ORIGIN });
    message!(event);
    expect(replies).toHaveLength(1);
    expect((replies[0] as { type: string }).type).toBe("ready");
    expect((replies[0] as { url: string }).url).toContain("__memql-dl/");
  });

  it("ignores a message whose origin is not this worker's", () => {
    const { message } = loadWorker();
    const { event, replies } = open({ origin: "https://evil.example.test" });
    message!(event);
    expect(replies).toHaveLength(0);
  });

  it("ignores a message whose SOURCE client is on another origin", () => {
    // The stronger of the two checks: a sender can be wrong about its own
    // origin field far more easily than about the url it is running.
    const { message } = loadWorker();
    const { event, replies } = open({ origin: ORIGIN, sourceUrl: "https://evil.example.test/page" });
    message!(event);
    expect(replies).toHaveLength(0);
  });

  it("ignores a message whose source url will not parse", () => {
    const { message } = loadWorker();
    const { event, replies } = open({ origin: ORIGIN, sourceUrl: "not a url" });
    message!(event);
    expect(replies).toHaveLength(0);
  });

  it("still answers when the origin is empty, which the security model already covers", () => {
    // Deliberate: refusing an empty origin would risk silently breaking
    // downloads in an embedding that does not populate it, and an empty origin
    // cannot name a cross-origin sender the service worker model would have
    // admitted in the first place.
    const { message } = loadWorker();
    const { event, replies } = open({ origin: "" });
    message!(event);
    expect(replies).toHaveLength(1);
  });

  it("ignores anything that is not a download-open message", () => {
    const { message } = loadWorker();
    const { event, replies } = open({ origin: ORIGIN });
    (event.data as { type: string }).type = "something-else";
    message!(event);
    expect(replies).toHaveLength(0);
  });
});
