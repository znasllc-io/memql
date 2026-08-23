// Tests for the SDK-owned in-place WS re-auth (#1110): the auto-rotation
// scheduling math + JWT exp decoding (pure helpers) and an end-to-end check
// that Connection actually invokes onTokenExpired + rotateAuth on the timer.

import test from "node:test";
import assert from "node:assert/strict";

import {
  Connection,
  DEFAULT_ROTATE_FLOOR_MS,
  computeRotateDelayMs,
  decodeJwtLifetime,
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

// A token stamped by the SERVER's clock: both claims come from the same clock,
// which is the whole premise of the skew-proof schedule.
function jwtWithLifetime(iatSeconds: number, ttlSeconds: number): string {
  return `${b64urlJson({ alg: "none" })}.${b64urlJson({
    iat: iatSeconds,
    exp: iatSeconds + ttlSeconds,
  })}.`;
}

// ---------------------------------------------------------------------
// computeRotateDelayMs -- scheduled from the token's OWN lifetime
//
// memql#4326. The old arithmetic was `0.7 * (exp*1000 - Date.now())`: `exp` is
// the identity service's clock and `Date.now()` is the browser's, so a browser
// running ahead saw every fresh token as nearly expired and rotated every few
// seconds forever. `exp - iat` is stamped by ONE clock, so skew cancels.
// ---------------------------------------------------------------------

test("computeRotateDelayMs -- fires at 70% of the token's own lifetime", () => {
  const iat = 1_700_000_000;
  // Receipt and now are the same instant: the full 70% is still ahead.
  const receivedAt = iat * 1000;
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 900 }, receivedAt, receivedAt),
    630_000,
  );
});

test("computeRotateDelayMs -- a browser 14 minutes FAST schedules the same 630s", () => {
  const iat = 1_700_000_000;
  const skewMs = 14 * 60 * 1000;
  const receivedAt = iat * 1000 + skewMs; // the browser's clock at receipt
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 900 }, receivedAt, receivedAt),
    630_000,
  );
});

test("computeRotateDelayMs -- a browser 14 minutes SLOW schedules the same 630s", () => {
  const iat = 1_700_000_000;
  const skewMs = 14 * 60 * 1000;
  const receivedAt = iat * 1000 - skewMs;
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 900 }, receivedAt, receivedAt),
    630_000,
  );
});

test("computeRotateDelayMs -- a 20s token schedules the 30s floor, not 14s", () => {
  const iat = 1_700_000_000;
  const receivedAt = iat * 1000;
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 20 }, receivedAt, receivedAt),
    DEFAULT_ROTATE_FLOOR_MS,
  );
});

test("computeRotateDelayMs -- subtracts the time already elapsed since receipt", () => {
  const iat = 1_700_000_000;
  const receivedAt = iat * 1000;
  // 100s after the token arrived, 530s of the 630s target remain.
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 900 }, receivedAt, receivedAt + 100_000),
    530_000,
  );
});

test("computeRotateDelayMs -- an elapsed-past target still waits the floor, never 0", () => {
  const iat = 1_700_000_000;
  const receivedAt = iat * 1000;
  // A token held well past its rotation point must not spin at network speed.
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 900 }, receivedAt, receivedAt + 10_000_000),
    DEFAULT_ROTATE_FLOOR_MS,
  );
});

test("computeRotateDelayMs -- honours a custom fraction", () => {
  const iat = 1_000_000;
  const receivedAt = iat * 1000;
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 1000 }, receivedAt, receivedAt, 0.5),
    500_000,
  );
});

test("computeRotateDelayMs -- no iat falls back to exp - now, still floored", () => {
  const nowMs = 1_700_000_000_000;
  const exp = nowMs / 1000 + 900;
  assert.equal(computeRotateDelayMs({ iat: null, exp }, nowMs, nowMs), 630_000);

  const nearlyExpired = nowMs / 1000 + 10;
  assert.equal(
    computeRotateDelayMs({ iat: null, exp: nearlyExpired }, nowMs, nowMs),
    DEFAULT_ROTATE_FLOOR_MS,
  );
});

test("computeRotateDelayMs -- an expired token waits the floor rather than spinning", () => {
  const nowMs = 1_700_000_000_000;
  const exp = nowMs / 1000 - 10; // expired 10s ago
  assert.equal(computeRotateDelayMs({ iat: null, exp }, nowMs, nowMs), DEFAULT_ROTATE_FLOOR_MS);
});

test("computeRotateDelayMs -- an explicit floor replaces the default in both directions", () => {
  const iat = 1_700_000_000;
  const receivedAt = iat * 1000;
  // A 20ms lifetime: 70% is 14ms, so the custom 50ms floor is what binds.
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 0.02 }, receivedAt, receivedAt, 0.7, 50),
    50,
  );
  // A 200ms lifetime: 70% is 140ms, above the custom floor, so the fraction
  // wins -- a lowered floor must not become a fixed interval.
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 0.2 }, receivedAt, receivedAt, 0.7, 50),
    140,
  );
  // And the DEFAULT floor would have swallowed both.
  assert.equal(
    computeRotateDelayMs({ iat, exp: iat + 0.2 }, receivedAt, receivedAt),
    DEFAULT_ROTATE_FLOOR_MS,
  );
});

// ---------------------------------------------------------------------
// decodeJwtLifetime
// ---------------------------------------------------------------------

test("decodeJwtLifetime -- reads both iat and exp", () => {
  assert.deepEqual(decodeJwtLifetime(jwtWithLifetime(1_700_000_000, 900)), {
    iat: 1_700_000_000,
    exp: 1_700_000_900,
  });
});

test("decodeJwtLifetime -- exp with no iat reports iat null", () => {
  assert.deepEqual(decodeJwtLifetime(jwtWithExp(1_700_000_000)), {
    iat: null,
    exp: 1_700_000_000,
  });
});

test("decodeJwtLifetime -- null for a token with no exp", () => {
  assert.equal(decodeJwtLifetime(`${b64urlJson({})}.${b64urlJson({ sub: "u1" })}.`), null);
});

test("decodeJwtLifetime -- null for a non-JWT / single segment", () => {
  assert.equal(decodeJwtLifetime("not-a-jwt"), null);
  assert.equal(decodeJwtLifetime(""), null);
});

test("decodeJwtLifetime -- null for an unparseable payload", () => {
  assert.equal(decodeJwtLifetime("aaa.%%%not-base64-json%%%.bbb"), null);
});

test("decodeJwtLifetime -- null when exp is non-numeric", () => {
  assert.equal(decodeJwtLifetime(`${b64urlJson({})}.${b64urlJson({ exp: "soon" })}.`), null);
});

test("decodeJwtLifetime -- a non-numeric iat degrades to the no-iat fallback", () => {
  assert.deepEqual(
    decodeJwtLifetime(`${b64urlJson({})}.${b64urlJson({ iat: "yesterday", exp: 1_700_000_000 })}.`),
    { iat: null, exp: 1_700_000_000 },
  );
});

test("decodeJwtLifetime -- an iat at or after exp degrades to the no-iat fallback", () => {
  // A non-positive lifetime would make `0.7 * (exp - iat)` zero or negative;
  // the floor would still catch it, but the claim is nonsense and the
  // wall-clock fallback is the honest reading.
  assert.deepEqual(
    decodeJwtLifetime(`${b64urlJson({})}.${b64urlJson({ iat: 1_700_000_900, exp: 1_700_000_000 })}.`),
    { iat: null, exp: 1_700_000_000 },
  );
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
//
// `rotateFloorMs` is what keeps this a REAL end-to-end rotation rather than a
// 30-second test: the production floor (memql#4326) refuses to schedule sooner
// than 30s, and a 200ms token under it would rotate long after its own expiry.
// Lowering the floor is the documented opt-out, and this is the case it exists
// for -- a harness driving a deliberately short-lived token.
test(
  "Connection auto-rotates in place shortly before the bearer expires (#1110)",
  { timeout: 8000 },
  async () => {
    let socket!: FakeWebSocket;
    const nowSec = Math.floor(Date.now() / 1000);
    // A 200ms lifetime -> 70% is 140ms, above the 50ms floor this dial sets.
    const shortLivedBearer = jwtWithLifetime(nowSec, 0.2);
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
          rotateFloorMs: 50,
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

// The retry path used to spend a full rotation per attempt at a ONE-SECOND
// floor, so a refresh outage turned into three requests a second. It is the
// second half of memql#4326 and is bounded by the same floor.
test(
  "the retry path never re-fires under the rotation floor (memql#4326)",
  { timeout: 8000 },
  async () => {
    let socket!: FakeWebSocket;
    const nowSec = Math.floor(Date.now() / 1000);
    // 200ms lifetime, 50ms floor: the FIRST rotation fires at 140ms. The hook
    // then fails, and the retry must be scheduled at the 50ms floor rather
    // than at the ~30ms a third of the remaining TTL would give.
    const bearer = jwtWithLifetime(nowSec, 0.2);

    const hookCallsAt: number[] = [];
    let conn: Connection | undefined;
    try {
      const dialP = Connection.dial({
        endpoint: "wss://test.local/memql/ws",
        auth: {
          bearer,
          rotateFloorMs: 50,
          onTokenExpired: async () => {
            hookCallsAt.push(Date.now());
            return null; // never succeeds -> the retry path drives the rest
          },
        },
        webSocketFactory: (url) => {
          socket = new FakeWebSocket(url);
          return socket as unknown as WebSocket;
        },
      });
      await replyTo(
        socket,
        (m) => m.clientHello !== undefined,
        (id) =>
          ({ correlateTo: id, serverHello: { nodeId: "n1", version: "test" } }) as unknown as ServerMessage,
      );
      conn = await dialP;

      // Wait for at least two hook calls (the scheduled rotation + one retry).
      for (let i = 0; i < 3000 && hookCallsAt.length < 2; i++) {
        await new Promise((r) => setTimeout(r, 1));
      }
      assert.ok(
        hookCallsAt.length >= 2,
        `expected the retry to fire; saw ${hookCallsAt.length} hook call(s)`,
      );
      const gap = hookCallsAt[1]! - hookCallsAt[0]!;
      assert.ok(
        gap >= 45,
        `retry fired ${gap}ms after the first attempt, under the 50ms floor this dial set`,
      );
    } finally {
      conn?.close();
    }
  },
);
