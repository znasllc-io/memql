// A WebSocket stand-in for tests that drive the REAL sdk/ts Connection.
//
// Modelled on sdk/ts/test/connection-autorotate.test.ts's fake so the two
// behave the same way; kept here (rather than imported) because the SDK does
// not publish its test helpers.
//
// It records the URL it was constructed with and the subprotocols it was
// handed, which is what lets the portal's tests assert the property that
// matters most: the credential rides the subprotocol channel and never the URL.

export class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;

  readonly url: string;
  readonly protocols: string[] | undefined;
  readyState: number = FakeWebSocket.CONNECTING;
  binaryType: "blob" | "arraybuffer" = "blob";
  readonly outbound: string[] = [];

  private listeners: Record<string, Set<(ev: unknown) => void>> = {};

  constructor(url: string, protocols?: string[]) {
    this.url = url;
    this.protocols = protocols;
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

  // frames returns the outbound frames parsed, for assertions.
  frames(): Array<Record<string, unknown>> {
    return this.outbound.map((raw) => JSON.parse(raw) as Record<string, unknown>);
  }

  private dispatch(type: string, ev: unknown): void {
    for (const fn of this.listeners[type] ?? []) fn(ev);
  }
}

// replyTo waits for an outbound frame matching `predicate` and answers it,
// correlating on the frame's messageId. Returns the index just past the frame
// it answered so a caller can scan forward for the next one.
export async function replyTo(
  socket: FakeWebSocket,
  predicate: (frame: Record<string, unknown>) => boolean,
  build: (messageId: string) => unknown,
  sinceIndex = 0,
  timeoutMs = 4000,
): Promise<number> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    for (let i = sinceIndex; i < socket.outbound.length; i++) {
      const frame = JSON.parse(socket.outbound[i]!) as Record<string, unknown>;
      if (predicate(frame)) {
        socket.pushServer(build(frame.messageId as string));
        return i + 1;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("replyTo: no matching client frame arrived before the deadline");
}

// jwtWithExp builds an unsigned JWT carrying only `exp`. The SDK decodes exp
// WITHOUT verifying the signature (it only needs the deadline to schedule
// rotation), so an unsigned token is sufficient and keeps the test free of key
// material.
export function jwtWithExp(expSeconds: number, extra: Record<string, unknown> = {}): string {
  return `${b64urlJson({ alg: "none" })}.${b64urlJson({ exp: expSeconds, ...extra })}.`;
}

function b64urlJson(obj: unknown): string {
  return btoa(JSON.stringify(obj)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
