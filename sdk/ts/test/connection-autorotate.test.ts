// Tests for the SDK-owned in-place WS re-auth (#1110): the auto-rotation
// scheduling math + JWT exp decoding (pure helpers) and an end-to-end check
// that Connection actually invokes onTokenExpired + rotateAuth on the timer.

import test from "node:test";
import assert from "node:assert/strict";

import {
  Connection,
  computeRotateDelayMs,
  decodeJwtExp,
} from "../src/client/connection.js";
import type { ServerMessage } from "../src/client/wire.js";

// Loose view of an outbound frame -- the wire ClientMessage is a discriminated
// union; in tests we just probe for a variant key + messageId.
type Frame = { messageId?: string } & Record<string, unknown>;

// base64url-encode a JSON object the way a JWT segment is encoded.
function b64urlJson(obj: unknown): string {
  const json = JSON.stringify(obj);
  const b64 =
    typeof btoa === "function"
      ? btoa(json)
      : (globalThis as { Buffer?: { from(s: string): { toString(e: string): string } } }).Buffer!.from(
          json,
        ).toString("base64");
  return b64.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function jwtWithExp(expSeconds: number): string {
  return `${b64urlJson({ alg: "none" })}.${b64urlJson({ exp: expSeconds })}.`;
}

// ---------------------------------------------------------------------
// computeRotateDelayMs
// ---------------------------------------------------------------------

test("computeRotateDelayMs -- fires at 70% of remaining TTL by default", () => {
  const now = 1_000_000;
  const exp = now / 1000 + 1000; // 1000s of TTL
  assert.equal(computeRotateDelayMs(exp, now), 700_000); // 70%
});

test("computeRotateDelayMs -- honours a custom fraction", () => {
  const now = 0;
  const exp = 100; // 100s TTL
  assert.equal(computeRotateDelayMs(exp, now, 0.5), 50_000);
});

test("computeRotateDelayMs -- already past expiry yields 0 (rotate now)", () => {
  const now = 5_000_000;
  const exp = now / 1000 - 10; // expired 10s ago
  assert.equal(computeRotateDelayMs(exp, now), 0);
});

test("computeRotateDelayMs -- never negative", () => {
  assert.ok(computeRotateDelayMs(0, 999_999) >= 0);
});

// ---------------------------------------------------------------------
// decodeJwtExp
// ---------------------------------------------------------------------

test("decodeJwtExp -- reads a numeric exp", () => {
  assert.equal(decodeJwtExp(jwtWithExp(1_700_000_000)), 1_700_000_000);
});

test("decodeJwtExp -- null for a token with no exp", () => {
  assert.equal(decodeJwtExp(`${b64urlJson({})}.${b64urlJson({ sub: "u1" })}.`), null);
});

test("decodeJwtExp -- null for a non-JWT / single segment", () => {
  assert.equal(decodeJwtExp("not-a-jwt"), null);
  assert.equal(decodeJwtExp(""), null);
});

test("decodeJwtExp -- null for an unparseable payload", () => {
  assert.equal(decodeJwtExp("aaa.%%%not-base64-json%%%.bbb"), null);
});

test("decodeJwtExp -- null when exp is non-numeric", () => {
  assert.equal(decodeJwtExp(`${b64urlJson({})}.${b64urlJson({ exp: "soon" })}.`), null);
});

// ---------------------------------------------------------------------
// End-to-end: the SDK invokes onTokenExpired + rotateAuth on the timer.
// ---------------------------------------------------------------------

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;

  url: string;
  readyState: number = FakeWebSocket.CONNECTING;
  binaryType: "blob" | "arraybuffer" = "blob";
  private listeners: Record<string, Set<(ev: unknown) => void>> = {};
  readonly outbound: string[] = [];

  constructor(url: string) {
    this.url = url;
    queueMicrotask(() => {
      this.readyState = FakeWebSocket.OPEN;
      this.dispatch("open", { type: "open" });
    });
  }
  addEventListener(type: string, fn: (ev: unknown) => void): void {
    (this.listeners[type] ??= new Set()).add(fn);
  }
  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    this.listeners[type]?.delete(fn);
  }
  dispatchEvent(): boolean {
    return true;
  }
  send(data: string): void {
    this.outbound.push(data);
  }
  close(): void {
    if (this.readyState >= FakeWebSocket.CLOSING) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.dispatch("close", { type: "close", code: 1000, reason: "" });
  }
  pushServer(payload: unknown): void {
    this.dispatch("message", { type: "message", data: JSON.stringify(payload) });
  }
  private dispatch(type: string, ev: unknown): void {
    for (const fn of this.listeners[type] ?? []) fn(ev);
  }
}

// Install a global WebSocket shim so the SDK's readyState/OPEN constant
// references resolve on Node runtimes without a built-in global WebSocket
// (mirrors test/realtime.test.ts). The Connection still dials through the
// explicit webSocketFactory below; this just keeps `WebSocket` defined.
(globalThis as unknown as { WebSocket: typeof FakeWebSocket }).WebSocket = FakeWebSocket;

// Reply to whatever request is currently waiting (matched by messageId) with
// the given server payload. Polls the outbound queue until the frame appears.
async function replyTo(
  socket: FakeWebSocket,
  predicate: (msg: Frame) => boolean,
  build: (messageId: string) => ServerMessage,
  sinceIndex = 0,
): Promise<number> {
  // Poll generously (~3s, bounded by the test's 8s timeout) so the auto-rotation
  // timer + async rotateAuth round-trip is reliably observed even under slow CI.
  for (let i = 0; i < 3000; i++) {
    for (let j = sinceIndex; j < socket.outbound.length; j++) {
      const msg = JSON.parse(socket.outbound[j]!) as Frame;
      if (predicate(msg)) {
        socket.pushServer(build(msg.messageId!));
        return j + 1;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("replyTo: matching client frame never arrived");
}

// `timeout` bounds the test so a leaked timer can never hang the CI runner;
// the finally-close clears the post-rotation reschedule timer (which is armed
// at ~70% of the rotated token's TTL) even if an assertion throws first.
test(
  "Connection auto-rotates in place shortly before the bearer expires (#1110)",
  { timeout: 8000 },
  async () => {
    let socket!: FakeWebSocket;
    const nowSec = Math.floor(Date.now() / 1000);
    // exp ~200ms out -> computeRotateDelayMs fires at ~140ms.
    const shortLivedBearer = jwtWithExp(nowSec + 0.2);
    // Rotated token is intentionally exp-less so scheduleAutoRotate arms NO
    // further timer after the rotation -- keeps the test free of any lingering
    // handle regardless of timing.
    const rotatedBearer = `${b64urlJson({ alg: "none" })}.${b64urlJson({ sub: "u1" })}.`;

    let onTokenExpiredCalls = 0;
    let conn: Connection | undefined;
    try {
      const dialP = Connection.dial({
        endpoint: "wss://test.local/memql/ws",
        auth: {
          bearer: shortLivedBearer,
          onTokenExpired: async () => {
            onTokenExpiredCalls++;
            return rotatedBearer;
          },
        },
        webSocketFactory: (url) => {
          socket = new FakeWebSocket(url);
          return socket as unknown as WebSocket;
        },
      });

      // Complete the ClientHello/ServerHello handshake.
      const next = await replyTo(
        socket,
        (m) => m.clientHello !== undefined,
        (id) =>
          ({ correlateTo: id, serverHello: { nodeId: "n1", version: "test" } }) as unknown as ServerMessage,
      );
      conn = await dialP;

      // The auto-rotation timer should fire, call onTokenExpired, and send a
      // rotateAuth frame which we ack with ok:true.
      await replyTo(
        socket,
        (m) => m.rotateAuth !== undefined,
        (id) =>
          ({ correlateTo: id, rotateAuthResult: { ok: true } }) as unknown as ServerMessage,
        next,
      );

      assert.equal(onTokenExpiredCalls, 1, "onTokenExpired should have been invoked once");
      const sentRotate = socket.outbound.some(
        (f) => (JSON.parse(f) as { rotateAuth?: unknown }).rotateAuth !== undefined,
      );
      assert.ok(sentRotate, "SDK should have sent a rotateAuth frame on the timer");
    } finally {
      conn?.close();
    }
  },
);
