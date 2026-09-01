import { describe, expect, it, vi } from "vitest";

import { pumpToPort } from "../../src/apps/files/actions/downloadWorker";

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

function fakePort() {
  const sent: Sent[] = [];
  return {
    sent,
    postMessage: vi.fn((msg: Sent) => void sent.push(msg)),
  };
}

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
