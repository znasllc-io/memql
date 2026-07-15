// Tests for the space attachment upload helper (memql#2523): the standalone
// uploadAttachment (path, auth header, multipart body, response parsing, and
// post-rotation token use) and the Connection.uploadAttachment integration
// (derived HTTPS origin + rotated-bearer carry).

import test from "node:test";
import assert from "node:assert/strict";

import { uploadAttachment } from "../src/client/attachments.js";
import { Connection } from "../src/client/connection.js";
import type { ServerMessage } from "../src/client/wire.js";

// A recorded fetch call.
interface Recorded {
  url: string;
  method: string;
  authorization: string | null;
  contentType: string | null;
  body: unknown;
}

// makeFetch returns a fetch stub that records the request and replies with the
// given JSON body + status.
function makeFetch(
  status: number,
  body: unknown,
): { fetchImpl: typeof fetch; calls: Recorded[] } {
  const calls: Recorded[] = [];
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    calls.push({
      url: String(input),
      method: init?.method ?? "GET",
      authorization: headers.get("Authorization"),
      contentType: headers.get("Content-Type"),
      body: init?.body,
    });
    return new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  }) as unknown as typeof fetch;
  return { fetchImpl, calls };
}

const OK_BODY = {
  id: "att-1",
  partitionId: "spc-1",
  fileName: "notes.txt",
  mimeType: "text/plain",
  fileSize: 5,
  blobUrl: "local://spaces/spc-1/attachments/x/notes.txt",
  status: "processing",
  uploadedBy: "u1",
  createdAt: "2026-07-14T00:00:00Z",
};

// ---------------------------------------------------------------------
// Standalone helper
// ---------------------------------------------------------------------

test("uploadAttachment -- POSTs multipart to /spaces/{id}/attachments with the bearer", async () => {
  const { fetchImpl, calls } = makeFetch(201, OK_BODY);
  const ref = await uploadAttachment(
    { authToken: () => "tok-1", attachmentBaseUrl: () => "https://app.example.com" },
    {
      partitionId: "spc-1",
      file: new TextEncoder().encode("hello"),
      fileName: "notes.txt",
      contentType: "text/plain",
      fetchImpl,
    },
  );

  assert.equal(calls.length, 1);
  const call = calls[0]!;
  assert.equal(call.method, "POST");
  assert.equal(call.url, "https://app.example.com/spaces/spc-1/attachments");
  assert.equal(call.authorization, "Bearer tok-1");
  // The helper must NOT set Content-Type -- fetch stamps the multipart boundary.
  assert.equal(call.contentType, null);
  // Body is multipart form-data carrying the file under the "file" field.
  assert.ok(call.body instanceof FormData, "body should be FormData");
  const part = (call.body as FormData).get("file");
  assert.ok(part instanceof Blob, "file part should be a Blob");
  assert.equal((part as File).name, "notes.txt");

  // Response parsed into a typed reference.
  assert.equal(ref.id, "att-1");
  assert.equal(ref.partitionId, "spc-1");
  assert.equal(ref.status, "processing");
  assert.equal(ref.fileSize, 5);
  assert.deepEqual(ref.raw, OK_BODY);
});

test("uploadAttachment -- reads the CURRENT token on each call (post-rotation)", async () => {
  const { fetchImpl, calls } = makeFetch(201, OK_BODY);
  let token = "tok-old";
  const source = {
    authToken: () => token,
    attachmentBaseUrl: () => "https://app.example.com",
  };
  const params = {
    partitionId: "spc-1",
    file: new Uint8Array([1, 2, 3]),
    fileName: "a.bin",
    contentType: "application/pdf",
    fetchImpl,
  };

  await uploadAttachment(source, params);
  token = "tok-new"; // simulate an in-place rotation between uploads
  await uploadAttachment(source, params);

  assert.equal(calls[0]!.authorization, "Bearer tok-old");
  assert.equal(calls[1]!.authorization, "Bearer tok-new");
});

test("uploadAttachment -- tolerates a single-element array response shape", async () => {
  const { fetchImpl } = makeFetch(201, [OK_BODY]);
  const ref = await uploadAttachment(
    { authToken: () => "tok-1", attachmentBaseUrl: () => "https://app.example.com" },
    { partitionId: "spc-1", file: new Uint8Array([0]), fileName: "a.txt", contentType: "text/plain", fetchImpl },
  );
  assert.equal(ref.id, "att-1");
});

test("uploadAttachment -- rejects when no bearer is available", async () => {
  const { fetchImpl, calls } = makeFetch(201, OK_BODY);
  await assert.rejects(
    uploadAttachment(
      { authToken: () => undefined, attachmentBaseUrl: () => "https://app.example.com" },
      { partitionId: "spc-1", file: new Uint8Array([0]), fileName: "a.txt", contentType: "text/plain", fetchImpl },
    ),
    /no bearer token available/,
  );
  assert.equal(calls.length, 0, "must not issue a request without a token");
});

test("uploadAttachment -- throws on a non-2xx response, surfacing the status", async () => {
  const { fetchImpl } = makeFetch(413, { error: "file too large" });
  await assert.rejects(
    uploadAttachment(
      { authToken: () => "tok-1", attachmentBaseUrl: () => "https://app.example.com" },
      { partitionId: "spc-1", file: new Uint8Array([0]), fileName: "big.txt", contentType: "text/plain", fetchImpl },
    ),
    /server returned 413/,
  );
});

test("uploadAttachment -- throws when the response carries no id", async () => {
  const { fetchImpl } = makeFetch(201, { partitionId: "spc-1" });
  await assert.rejects(
    uploadAttachment(
      { authToken: () => "tok-1", attachmentBaseUrl: () => "https://app.example.com" },
      { partitionId: "spc-1", file: new Uint8Array([0]), fileName: "a.txt", contentType: "text/plain", fetchImpl },
    ),
    /missing an attachment id/,
  );
});

// ---------------------------------------------------------------------
// Connection integration: derived origin + rotated-bearer carry.
// ---------------------------------------------------------------------

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  readyState = FakeWebSocket.OPEN;
  outbound: string[] = [];
  private listeners: Record<string, ((ev: unknown) => void)[]> = {};

  constructor(public url: string) {}

  addEventListener(type: string, fn: (ev: unknown) => void): void {
    (this.listeners[type] ??= []).push(fn);
  }
  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn);
  }
  send(data: string): void {
    this.outbound.push(data);
  }
  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.dispatch("close", { type: "close", code: 1000, reason: "test" });
  }
  pushServer(payload: unknown): void {
    this.dispatch("message", { type: "message", data: JSON.stringify(payload) });
  }
  private dispatch(type: string, ev: unknown): void {
    for (const fn of this.listeners[type] ?? []) fn(ev);
  }
}

(globalThis as unknown as { WebSocket: typeof FakeWebSocket }).WebSocket = FakeWebSocket;

type Frame = { messageId?: string } & Record<string, unknown>;

async function replyTo(
  socket: FakeWebSocket,
  predicate: (msg: Frame) => boolean,
  build: (messageId: string) => ServerMessage,
): Promise<void> {
  for (let i = 0; i < 3000; i++) {
    for (const raw of socket.outbound) {
      const msg = JSON.parse(raw) as Frame;
      if (predicate(msg)) {
        socket.pushServer(build(msg.messageId!));
        return;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("replyTo: matching client frame never arrived");
}

test("Connection.uploadAttachment -- derives the HTTPS origin from the wss endpoint and carries the current bearer", async () => {
  let socket!: FakeWebSocket;
  const { fetchImpl, calls } = makeFetch(201, OK_BODY);

  const dialP = Connection.dial({
    endpoint: "wss://test.local/memql/ws",
    auth: { bearer: "dial-jwt" },
    webSocketFactory: (url) => {
      socket = new FakeWebSocket(url);
      return socket as unknown as WebSocket;
    },
  });
  await replyTo(
    socket,
    (m) => m.clientHello !== undefined,
    (id) => ({ correlateTo: id, serverHello: { nodeId: "n1", version: "test" } }) as unknown as ServerMessage,
  );
  const conn = await dialP;

  try {
    // Upload before any rotation: current bearer is the dial token.
    const ref = await conn.uploadAttachment({
      partitionId: "spc-9",
      file: new Uint8Array([9, 9]),
      fileName: "f.txt",
      contentType: "text/plain",
      fetchImpl,
    });
    assert.equal(ref.id, "att-1");
    assert.equal(calls[0]!.url, "https://test.local/spaces/spc-9/attachments");
    assert.equal(calls[0]!.authorization, "Bearer dial-jwt");

    // Rotate the bearer in place, then upload again: the helper must send the
    // rotated token, proving the connection carries it (#2523 + #2524).
    const rotateP = conn.rotateAuth("rotated-jwt");
    await replyTo(
      socket,
      (m) => m.rotateAuth !== undefined,
      (id) => ({ correlateTo: id, rotateAuthResult: { ok: true } }) as unknown as ServerMessage,
    );
    assert.equal(await rotateP, true);

    await conn.uploadAttachment({
      partitionId: "spc-9",
      file: new Uint8Array([1]),
      fileName: "g.txt",
      contentType: "text/plain",
      fetchImpl,
    });
    assert.equal(calls[1]!.authorization, "Bearer rotated-jwt");
  } finally {
    conn.close();
  }
});

test("Connection.uploadAttachment -- preserves a deployment base-path prefix from the endpoint", async () => {
  let socket!: FakeWebSocket;
  const { fetchImpl, calls } = makeFetch(201, OK_BODY);

  const dialP = Connection.dial({
    endpoint: "wss://test.local/edge/memql/ws",
    auth: { bearer: "dial-jwt" },
    webSocketFactory: (url) => {
      socket = new FakeWebSocket(url);
      return socket as unknown as WebSocket;
    },
  });
  await replyTo(
    socket,
    (m) => m.clientHello !== undefined,
    (id) => ({ correlateTo: id, serverHello: { nodeId: "n1", version: "test" } }) as unknown as ServerMessage,
  );
  const conn = await dialP;

  try {
    await conn.uploadAttachment({
      partitionId: "spc-1",
      file: new Uint8Array([1]),
      fileName: "f.txt",
      contentType: "text/plain",
      fetchImpl,
    });
    assert.equal(calls[0]!.url, "https://test.local/edge/spaces/spc-1/attachments");
  } finally {
    conn.close();
  }
});
