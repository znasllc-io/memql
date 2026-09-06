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
    onmessage: null,
  };
  const event: Record<string, unknown> = {
    data: { type: "memql-download-open", name: "report.pdf", size: 10 },
    ports: [port],
    origin: over.origin,
  };
  if (over.sourceUrl !== null) event.source = { url: over.sourceUrl ?? `${ORIGIN}/index.html` };
  return { event, replies };
}

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
