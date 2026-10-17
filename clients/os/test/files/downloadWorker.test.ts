import { afterEach, describe, expect, it, vi } from "vitest";

import { downloadWorkerRegistration, pumpToPort, runWorkerDownload } from "../../src/apps/files/actions/downloadWorker";

// The page half of the streaming download (design D13): the page fetches with
// the bearer -- authorization never leaves this side -- and pumps body chunks
// over a MessageChannel to the service worker, which serves them as a
// navigation response the browser writes to disk as they arrive.

function bodyOf(...chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}

interface Sent {
  type: string;
  chunk?: Uint8Array;
}

it("keeps a long download navigation alive until its body finishes", async () => {
  vi.useFakeTimers();
  const messages: string[] = [];
  class Channel {
    port1 = { onmessage: null as ((event: { data: unknown }) => void) | null, postMessage: (message: { type: string }) => messages.push(message.type) };
    port2 = { postMessage: (data: unknown) => this.port1.onmessage?.({ data }) };
  }
  vi.stubGlobal("MessageChannel", Channel);
  let body!: ReadableStreamDefaultController<Uint8Array>;
  const stream = new ReadableStream<Uint8Array>({ start(controller) { body = controller; } });
  const registration = { active: { postMessage: (_data: unknown, ports: Channel["port2"][]) => ports[0]!.postMessage({ type: "ready", url: "/__memql-dl/fixture" }) } } as unknown as ServiceWorkerRegistration;
  try {
    const downloading = runWorkerDownload({ artifactId: "owned-file", fileName: "release.tar", sizeBytes: 3, bearer: async () => "fixture", registration, fetchImpl: vi.fn(async () => new Response(stream)) });
    await vi.advanceTimersByTimeAsync(61_000);
    expect(document.querySelector('iframe[src="/__memql-dl/fixture"]')).not.toBeNull();
    body.enqueue(new TextEncoder().encode("end"));
    body.close();
    await downloading;
    expect(messages).toEqual(["chunk", "done"]);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(document.querySelector('iframe[src="/__memql-dl/fixture"]')).toBeNull();
  } finally {
    document.querySelector('iframe[src="/__memql-dl/fixture"]')?.remove();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  }
});

function fakePort() {
  const sent: Sent[] = [];
  return {
    sent,
    postMessage: vi.fn((msg: Sent) => void sent.push(msg)),
  };
}

describe("the download-only worker registration", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  function install(state: ServiceWorkerState) {
    const worker = Object.assign(new EventTarget(), { state });
    const registration = {
      active: state === "activated" ? worker : null,
      waiting: null,
      installing: state === "activated" ? null : worker,
    };
    vi.stubGlobal("navigator", {
      serviceWorker: {
        register: async () => registration,
        // The OS page is outside /__memql-dl/, so this never settles.
        ready: new Promise(() => {}),
      },
    });
    return { worker, registration };
  }

  it("uses an active scoped worker without waiting for control of the OS page", async () => {
    const { registration } = install("activated");
    await expect(downloadWorkerRegistration()).resolves.toBe(registration);
  }, 500);

  it("waits for its own installation to activate", async () => {
    const { worker, registration } = install("installing");
    let finished = false;
    const result = downloadWorkerRegistration().then((value) => { finished = true; return value; });
    await Promise.resolve();
    expect(finished).toBe(false);
    worker.state = "activated";
    registration.active = worker;
    registration.installing = null;
    worker.dispatchEvent(new Event("statechange"));
    await expect(result).resolves.toBe(registration);
  }, 500);

  it("falls back when installation cannot activate within the bounded wait", async () => {
    vi.useFakeTimers();
    install("installing");
    const result = downloadWorkerRegistration();
    await vi.advanceTimersByTimeAsync(4_000);
    await expect(result).resolves.toBeNull();
  }, 500);

  it("waits for an available update before using a previously active worker", async () => {
    const { worker, registration } = install("installing");
    registration.active = Object.assign(new EventTarget(), { state: "activated" as ServiceWorkerState });
    let finished = false;
    const result = downloadWorkerRegistration().then((value) => { finished = true; return value; });
    await Promise.resolve();
    await Promise.resolve();
    expect(finished).toBe(false);
    worker.state = "activated";
    registration.active = worker;
    registration.installing = null;
    worker.dispatchEvent(new Event("statechange"));
    await expect(result).resolves.toBe(registration);
  }, 500);
});

describe("pumpToPort", () => {
  it("forwards every chunk in order and closes with done", async () => {
    const port = fakePort();
    await pumpToPort(bodyOf("hel", "lo"), port);
    expect(port.sent.map((m) => m.type)).toEqual(["chunk", "chunk", "done"]);
    expect(new TextDecoder().decode(port.sent[0]?.chunk)).toBe("hel");
    expect(new TextDecoder().decode(port.sent[1]?.chunk)).toBe("lo");
  });

  it("reports a mid-stream failure as abort, never as a clean done", async () => {
    const port = fakePort();
    // Error on the SECOND pull: erroring inside start() would discard the
    // queued chunk by spec, which is a different scenario than a stream that
    // fails mid-flight.
    let pulls = 0;
    const failing = new ReadableStream<Uint8Array>({
      pull(controller) {
        pulls += 1;
        if (pulls === 1) controller.enqueue(new TextEncoder().encode("part"));
        else controller.error(new Error("connection lost"));
      },
    });
    await expect(pumpToPort(failing, port)).rejects.toThrow("connection lost");
    expect(port.sent.map((m) => m.type)).toEqual(["chunk", "abort"]);
  });
});
