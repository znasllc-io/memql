// Dialing on a host with NO global WebSocket.
//
// The VS Code extension host is Node 20 (the declared `"vscode": "^1.91.0"`
// floor), which has no global `WebSocket` -- which is exactly why
// editors/vscode/src/connection/manager.ts passes a `webSocketFactory` built
// from the `ws` package. A `webSocketFactory` is only half the story though:
// any BARE `WebSocket.OPEN` inside the SDK still dereferences the global, and
// both operands of `===` are evaluated, so the ReferenceError is thrown before
// readyState is ever compared.
//
// Every OTHER connection test in this directory installs a fake at
// `globalThis.WebSocket`, which is a seam ABOVE this bug and is precisely why
// it survived. This file deliberately does the opposite: it deletes the global
// entirely and drives the REAL `Connection.dial` through an injected socket,
// mirroring how manager.ts dials.
//
// node --test runs each test file in its own process, so deleting the global
// here cannot leak into the sibling suites that install one.

import { test } from "node:test";
import assert from "node:assert/strict";

import { Connection } from "../src/client/connection.js";

// Prove the precondition this whole file rests on, and make the deletion the
// first thing that happens in the process.
delete (globalThis as unknown as Record<string, unknown>)["WebSocket"];

// A message event carrying `data`. Built on the platform Event rather than the
// global MessageEvent so this file depends on nothing but EventTarget/Event.
class DataEvent extends Event {
  constructor(type: string, readonly data: string) {
    super(type);
  }
}

type Frame = { messageId?: string; clientHello?: unknown };

// An EventTarget-based socket standing in for `ws`'s WebSocket, the same way
// manager.ts's `new NodeWebSocket(url, protocols) as unknown as WebSocket`
// does. It carries readyState but deliberately exposes NO static OPEN/CLOSING
// constants -- reading those off the INSTANCE is not what the bug is about;
// the bug is the SDK reaching for the absent GLOBAL.
class InjectedSocket extends EventTarget {
  readyState = 1; // OPEN
  readonly outbound: string[] = [];
  closedWith: number | undefined;

  constructor(readonly url: string, readonly protocols?: string[]) {
    super();
  }

  send(data: string): void {
    this.outbound.push(data);
  }

  close(code?: number): void {
    this.closedWith = code;
    this.readyState = 3; // CLOSED
    this.dispatchEvent(new Event("close"));
  }

  pushServer(payload: unknown): void {
    this.dispatchEvent(new DataEvent("message", JSON.stringify(payload)));
  }
}

// Answers the ClientHello as soon as the dial writes it.
async function completeHello(socket: InjectedSocket): Promise<void> {
  for (let i = 0; i < 3000; i++) {
    for (const raw of socket.outbound) {
      const msg = JSON.parse(raw) as Frame;
      if (msg.clientHello !== undefined) {
        socket.pushServer({
          correlateTo: msg.messageId,
          serverHello: { nodeId: "n1", version: "test" },
        });
        return;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("test: ClientHello was never written");
}

test("precondition: this process has no global WebSocket", () => {
  assert.equal(
    typeof (globalThis as unknown as Record<string, unknown>)["WebSocket"],
    "undefined",
    "the regression this file guards only reproduces without a global WebSocket",
  );
});

test("Connection.dial succeeds with an injected socket and no global WebSocket", async () => {
  let socket: InjectedSocket | undefined;

  const dialing = Connection.dial({
    endpoint: "wss://cockpit.local.znas.io:443/memql/ws",
    auth: { bearer: "mql_pat_test" },
    clientId: "memql-vscode",
    sdkName: "memql-vscode",
    webSocketFactory: (url, protocols) => {
      socket = new InjectedSocket(url, protocols);
      return socket as unknown as WebSocket;
    },
  });

  assert.ok(socket, "the factory must have been called synchronously by dial()");
  await completeHello(socket);
  const conn = await dialing;

  assert.equal(conn.nodeId, "n1");
  assert.equal(conn.serverVersion, "test");
  conn.close();
  assert.equal(socket.closedWith, 1000);
});

// waitForOpen's early-return branch is the FIRST thing dial() awaits, so the
// test above already covers it for an already-open socket. This covers the
// other half: a socket that is still CONNECTING when handed over, so
// waitForOpen takes the listener path and the comparison happens against a
// non-OPEN readyState.
test("Connection.dial waits for a CONNECTING injected socket to open", async () => {
  let socket: InjectedSocket | undefined;

  const dialing = Connection.dial({
    endpoint: "wss://cockpit.local.znas.io:443/memql/ws",
    webSocketFactory: (url, protocols) => {
      socket = new InjectedSocket(url, protocols);
      socket.readyState = 0; // CONNECTING
      return socket as unknown as WebSocket;
    },
  });

  assert.ok(socket, "the factory must have been called synchronously by dial()");
  // Nothing may be written before "open" fires.
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(socket.outbound.length, 0, "dial must not write before the socket opens");

  socket.readyState = 1; // OPEN
  socket.dispatchEvent(new Event("open"));

  await completeHello(socket);
  const conn = await dialing;
  assert.equal(conn.nodeId, "n1");
  conn.close();
});

// The dispatcher's own send path guards on readyState too (rawSend). Prove it
// reports the honest "socket not open" error rather than dying on the absent
// global.
test("a send on a closed socket reports socket-not-open, not a missing global", async () => {
  let socket: InjectedSocket | undefined;

  const dialing = Connection.dial({
    endpoint: "wss://cockpit.local.znas.io:443/memql/ws",
    webSocketFactory: (url, protocols) => {
      socket = new InjectedSocket(url, protocols);
      return socket as unknown as WebSocket;
    },
  });
  assert.ok(socket);
  await completeHello(socket);
  const conn = await dialing;

  socket.readyState = 3; // CLOSED, without tearing the dispatcher down
  assert.throws(
    () => conn.dispatcher.send({ clientHello: {} } as never),
    /socket not open \(readyState=3\)/,
  );
  conn.close();
});
