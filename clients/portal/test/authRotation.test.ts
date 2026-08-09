// Bearer auto-rotation, verified ACROSS AN EXPIRY (memql#3315).
//
// This is the acceptance test for the requirement that a long-lived portal
// session is never torn down and redialled just because its token aged out.
// It wires the REAL pieces end to end -- the portal's identity auth source,
// the real `toConnectionAuth` adapter, and the real sdk/ts `Connection` with
// its rotation timer -- against a fake socket and a fake identity service.
// Stubbing the SDK here would test nothing: the rotation timer, the
// exp-decoding, and the in-place `rotateAuth` round-trip are precisely the
// machinery under examination.
//
// What it pins:
//   1. The credential is NEVER in the dial URL. It rides the WebSocket
//      subprotocol channel (#2511).
//   2. When the bearer nears expiry the SDK calls onTokenExpired, which
//      reaches the portal's auth source, which calls POST /auth/refresh.
//   3. The refreshed bearer is installed on the LIVE stream via rotateAuth --
//      the same socket instance, no redial, no lost subscriptions.
//   4. No credential appears in any fetch URL either.

import { describe, expect, it, vi } from "vitest";
import { Connection } from "@znasllc-io/memql-sdk-core/client";

import { createIdentityAuthSource } from "../src/auth/identityAuthSource";
import { toConnectionAuth } from "../src/cluster/auth";
import type { PortalRuntimeConfig } from "../src/cluster/config";
import { FakeWebSocket, jwtWithExp, replyTo } from "./fakeWebSocket";

const CONFIG: PortalRuntimeConfig = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "https://identity.example.com",
  oauthClientId: "portal",
  authEnabled: true,
};

// The SDK's factory signature requires a global WebSocket to exist for its
// readyState constants even when an explicit factory is supplied.
(globalThis as unknown as { WebSocket: typeof FakeWebSocket }).WebSocket = FakeWebSocket;

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

describe("bearer auto-rotation across an expiry", () => {
  it(
    "refreshes and rotates in place -- one socket, no redial",
    { timeout: 15_000 },
    async () => {
      const nowSec = Date.now() / 1000;
      // A 400ms TTL: the SDK rotates at 70% of remaining TTL, so ~280ms.
      const initialBearer = jwtWithExp(nowSec + 0.4, { sub: "u-1" });
      // The rotated token deliberately carries NO exp, so the SDK arms no
      // further timer -- the test asserts one rotation and then stops cleanly
      // instead of racing a second one during teardown.
      const rotatedBearer = jwtWithoutExp();

      const fetchCalls: string[] = [];
      const fetchImpl = vi.fn(async (url: string, init?: RequestInit) => {
        fetchCalls.push(url);
        expect(init?.method).toBe("POST");
        // The refresh cookie is HttpOnly and cross-origin; without this the
        // browser would not attach it and the refresh would 401 forever.
        expect(init?.credentials).toBe("include");
        return jsonResponse({
          access_token: rotatedBearer,
          token_type: "Bearer",
          expires_in: 900,
          // identity returns this; the portal must ignore it (see
          // identityClient.ts). Included here precisely so a future change
          // that starts reading it has to come past this test.
          refresh_token: "REFRESH-TOKEN-THAT-MUST-NEVER-BE-STORED",
        });
      });

      const source = createIdentityAuthSource({
        config: () => CONFIG,
        fetchImpl,
      });
      // Seed the token the authorization-code exchange would have produced.
      source.adopt(initialBearer);

      const auth = await toConnectionAuth(source);
      expect(auth?.bearer).toBe(initialBearer);

      const sockets: FakeWebSocket[] = [];
      let conn: Connection | undefined;
      try {
        const dialing = Connection.dial({
          endpoint: "/memql/ws",
          ...(auth ? { auth } : {}),
          clientId: "memql-portal",
          webSocketFactory: (url, protocols) => {
            const socket = new FakeWebSocket(url, protocols);
            sockets.push(socket);
            return socket as unknown as WebSocket;
          },
        });

        const socket = await waitForSocket(sockets);

        // (1) The credential is not in the URL -- it is in the subprotocols.
        expect(socket.url).not.toContain(initialBearer);
        expect(socket.url).not.toContain("bearer_token");
        expect(socket.url).not.toContain("token=");
        expect(socket.protocols).toEqual(["bearer", initialBearer]);

        const afterHello = await replyTo(
          socket,
          (f) => f.clientHello !== undefined,
          (id) => ({ correlateTo: id, serverHello: { nodeId: "bff-1", version: "test" } }),
        );
        conn = await dialing;

        // (2)+(3) The timer fires, the hook refreshes, and the SDK installs
        // the new bearer on the LIVE stream.
        await replyTo(
          socket,
          (f) => f.rotateAuth !== undefined,
          (id) => ({ correlateTo: id, rotateAuthResult: { ok: true } }),
          afterHello,
        );

        const rotate = socket
          .frames()
          .find((f) => f.rotateAuth !== undefined) as
          | { rotateAuth: { accessToken: string } }
          | undefined;
        expect(rotate?.rotateAuth.accessToken).toBe(rotatedBearer);
        expect(rotate?.rotateAuth.accessToken).not.toBe(initialBearer);

        expect(fetchImpl).toHaveBeenCalled();
        expect(fetchCalls[0]).toBe("https://identity.example.com/auth/refresh");

        // (3) THE POINT: still the same socket. A redial here would mean every
        // open subscription in the portal silently restarted every time a
        // fifteen-minute token aged out.
        expect(sockets).toHaveLength(1);
        expect(socket.readyState).toBe(FakeWebSocket.OPEN);
      } finally {
        conn?.close();
      }
    },
  );

  it("never puts a credential in a fetch URL", async () => {
    const seen: string[] = [];
    const fetchImpl = vi.fn(async (url: string, init?: RequestInit) => {
      seen.push(url);
      // The credential belongs in the body, which is not recorded by proxies,
      // ingress access logs or browser history the way a request line is.
      expect(String(init?.body ?? "")).not.toContain("?");
      return jsonResponse({ access_token: "AT-2", expires_in: 900 });
    });

    const source = createIdentityAuthSource({ config: () => CONFIG, fetchImpl });
    await source.refresh();

    for (const url of seen) {
      const parsed = new URL(url);
      expect([...parsed.searchParams.keys()]).toHaveLength(0);
      expect(url).not.toContain("AT-2");
    }
  });

  it("reports a transient failure as 'not now', not as a sign-out", async () => {
    const onSessionEnded = vi.fn();
    const fetchImpl = vi.fn(async () => {
      throw new TypeError("Load failed");
    });
    const source = createIdentityAuthSource({
      config: () => CONFIG,
      fetchImpl,
      onSessionEnded,
    });
    source.adopt("AT-1");

    expect(await source.refresh()).toBeNull();
    // Signing an operator out because their wifi blinked is its own bug: the
    // SDK retries the hook within the remaining TTL, and the held token may
    // still be perfectly valid.
    expect(onSessionEnded).not.toHaveBeenCalled();
    expect(source.hasToken()).toBe(true);
  });

  it("reports a 401 as a real sign-out and drops the token", async () => {
    const onSessionEnded = vi.fn();
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ error: "invalid_grant", message: "refresh token is no longer valid" }, 401),
    );
    const source = createIdentityAuthSource({
      config: () => CONFIG,
      fetchImpl,
      onSessionEnded,
    });
    source.adopt("AT-1");

    expect(await source.refresh()).toBeNull();
    expect(onSessionEnded).toHaveBeenCalledTimes(1);
    expect(source.hasToken()).toBe(false);
  });

  it("collapses concurrent refreshes into one round-trip", async () => {
    // identity ROTATES the refresh token on every call, so two in-flight
    // refreshes race to present a token the other has just superseded. There
    // is a 30s grace window server-side, which is exactly the kind of
    // "usually fine" that fails under load.
    let calls = 0;
    const fetchImpl = vi.fn(async () => {
      calls++;
      await new Promise((r) => setTimeout(r, 5));
      return jsonResponse({ access_token: `AT-${calls}`, expires_in: 900 });
    });
    const source = createIdentityAuthSource({ config: () => CONFIG, fetchImpl });

    const [a, b, c] = await Promise.all([source.refresh(), source.refresh(), source.bearer()]);
    expect(calls).toBe(1);
    expect(a).toBe("AT-1");
    expect(b).toBe("AT-1");
    expect(c).toBe("AT-1");
  });
});

// jwtWithoutExp is an unsigned JWT with no `exp` claim, which the SDK reads as
// "nothing to schedule against" and therefore leaves un-rotated.
function jwtWithoutExp(): string {
  const seg = (o: unknown) =>
    btoa(JSON.stringify(o)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  return `${seg({ alg: "none" })}.${seg({ sub: "u-1" })}.`;
}

async function waitForSocket(sockets: FakeWebSocket[]): Promise<FakeWebSocket> {
  for (let i = 0; i < 1000 && sockets.length === 0; i++) {
    await new Promise((r) => setTimeout(r, 1));
  }
  const socket = sockets[0];
  if (!socket) throw new Error("the SDK never constructed a WebSocket");
  return socket;
}
