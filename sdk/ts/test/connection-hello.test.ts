// What ServerHello states lands on the Connection (memql#5362).
//
// The handshake carries three language facts beside the build facts: the
// edition the node speaks, its grammar version, and the first release of the
// VS Code extension that carries that grammar. The extension compares them
// with the language it was built from, so the copy from the wire onto the
// Connection is the whole of the SDK's part -- and a copy that dropped one
// field would leave the editor believing every cluster predates the contract.
//
// The FakeWebSocket + handshake plumbing mirrors connection-auth.test.ts.

import { test } from "node:test";
import assert from "node:assert/strict";

import { Connection } from "../src/client/connection.js";

type Frame = { messageId?: string; clientHello?: unknown };

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

async function answerHello(socket: FakeWebSocket, serverHello: Record<string, unknown>): Promise<void> {
  for (let i = 0; i < 3000; i++) {
    for (const raw of socket.outbound) {
      const msg = JSON.parse(raw) as Frame;
      if (msg.clientHello !== undefined) {
        socket.pushServer({ correlateTo: msg.messageId, serverHello });
        return;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("answerHello: clientHello never arrived");
}

async function dialAgainst(serverHello: Record<string, unknown>): Promise<Connection> {
  let socket!: FakeWebSocket;
  const dialP = Connection.dial({
    endpoint: "wss://test.local/memql/ws",
    webSocketFactory: (url) => {
      socket = new FakeWebSocket(url);
      return socket as unknown as WebSocket;
    },
  });
  await answerHello(socket, serverHello);
  return dialP;
}

test("the handshake's language facts are readable off the connection", async () => {
  const conn = await dialAgainst({
    nodeId: "bff",
    version: "v1",
    engineVersion: "v0.20.0",
    edition: "2026",
    grammarVersion: "2026.09-example-grammar-0123abcd",
    editorRelease: "0.4.0",
  });
  try {
    assert.equal(conn.edition, "2026");
    assert.equal(conn.grammarVersion, "2026.09-example-grammar-0123abcd");
    assert.equal(conn.editorRelease, "0.4.0");
    // The build facts beside them are untouched.
    assert.equal(conn.engineVersion, "v0.20.0");
    assert.equal(conn.serverVersion, "v1");
  } finally {
    conn.close();
  }
});

test("a node that predates the fields states an empty string for each", async () => {
  // "" rather than undefined, the convention engineVersion set: it is how a
  // caller tells "this cluster answered and is older than the contract" apart
  // from "there is no connection to ask".
  const conn = await dialAgainst({ nodeId: "bff", version: "v1", engineVersion: "v0.19.0" });
  try {
    assert.equal(conn.edition, "");
    assert.equal(conn.grammarVersion, "");
    assert.equal(conn.editorRelease, "");
  } finally {
    conn.close();
  }
});
