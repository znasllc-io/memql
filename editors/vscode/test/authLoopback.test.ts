// The one-shot loopback listener.
//
// These tests drive a REAL socket rather than a stub, because every property
// worth asserting here is a property of the socket: which interface it bound,
// that a second path does not end the flow, that the deadline actually fires,
// and that closing releases the port. A fake http server would let all four
// pass while the real one bound 0.0.0.0.

import test from "node:test";
import assert from "node:assert/strict";

import { isAuthFlowError } from "../src/auth/errors.js";
import {
  CALLBACK_PATH,
  DEFAULT_CALLBACK_TIMEOUT_MS,
  LOOPBACK_HOST,
  startLoopbackListener,
  type LoopbackListener,
} from "../src/auth/loopback.js";

interface HttpReply {
  status: number;
  body: string;
}

// get issues a plain GET against the listener, the way a browser following the
// authorization redirect would.
async function get(url: string): Promise<HttpReply> {
  const res = await fetch(url);
  return { status: res.status, body: await res.text() };
}

// pending reports whether a promise is still unsettled, without consuming it.
async function pending(promise: Promise<unknown>): Promise<boolean> {
  const marker = Symbol("pending");
  const winner = await Promise.race([promise.catch(() => marker), delay(25).then(() => marker)]);
  return winner === marker;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function withListener(
  options: Parameters<typeof startLoopbackListener>[0],
  body: (listener: LoopbackListener) => Promise<void>,
): Promise<void> {
  const listener = await startLoopbackListener(options);
  try {
    await body(listener);
  } finally {
    listener.close();
  }
}

test("binds 127.0.0.1, never a wider interface", async () => {
  await withListener({}, async (listener) => {
    // Read back off the socket, not composed from a constant: the redirect URI
    // would say 127.0.0.1 either way.
    assert.equal(listener.host, "127.0.0.1");
    assert.equal(LOOPBACK_HOST, "127.0.0.1");
    assert.notEqual(listener.host, "0.0.0.0");
    assert.ok(listener.port > 0, "port 0 must have been resolved to a real ephemeral port");
    assert.equal(listener.redirectUri, `http://127.0.0.1:${listener.port}/callback`);
  });
});

test("the default deadline is two minutes", () => {
  assert.equal(DEFAULT_CALLBACK_TIMEOUT_MS, 120_000);
});

test("serves the callback once and hands back its query parameters", async () => {
  await withListener({}, async (listener) => {
    const waiting = listener.waitForCallback();
    const reply = await get(`${listener.redirectUri}?code=AUTHCODE&state=STATE123`);

    assert.equal(reply.status, 200);
    assert.match(reply.body, /close this tab/i);

    assert.deepEqual(await waiting, {
      code: "AUTHCODE",
      state: "STATE123",
      error: undefined,
      errorDescription: undefined,
    });
  });
});

test("a request to any other path gets a 404 and does NOT resolve the flow", async () => {
  await withListener({}, async (listener) => {
    const waiting = listener.waitForCallback();
    const base = `http://127.0.0.1:${listener.port}`;

    // A browser sends more than the callback: a favicon fetch, a prefetch, a
    // stray tab. None of them may end the sign-in.
    for (const path of ["/", "/favicon.ico", "/callback/extra", "/Callback"]) {
      const reply = await get(`${base}${path}?code=WRONG&state=WRONG`);
      assert.equal(reply.status, 404, `${path} should 404`);
    }
    assert.ok(await pending(waiting), "the flow resolved on a request that was not the callback");

    // The listener is still live and still willing to serve the real callback.
    await get(`${listener.redirectUri}?code=REAL&state=STATE`);
    assert.equal((await waiting).code, "REAL");
  });
});

test("only the FIRST callback is served -- the listener is one-shot", async () => {
  await withListener({}, async (listener) => {
    const waiting = listener.waitForCallback();
    await get(`${listener.redirectUri}?code=FIRST&state=S`);
    assert.equal((await waiting).code, "FIRST");

    // The port is released, so a replay of the same URL has nothing to hit.
    await assert.rejects(() => get(`${listener.redirectUri}?code=SECOND&state=S`));
  });
});

test("an OAuth error envelope is handed through, not swallowed", async () => {
  await withListener({}, async (listener) => {
    const waiting = listener.waitForCallback();
    await get(
      `${listener.redirectUri}?error=access_denied&error_description=${encodeURIComponent("user said no")}&state=S`,
    );
    const params = await waiting;
    assert.equal(params.error, "access_denied");
    assert.equal(params.errorDescription, "user said no");
    assert.equal(params.code, undefined);
  });
});

test("the deadline fires with a distinguishable timeout error", async () => {
  const listener = await startLoopbackListener({ timeoutMs: 40 });
  await assert.rejects(
    () => listener.waitForCallback(),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "timeout");
      assert.match(err.message, /callback/i);
      return true;
    },
  );
  // The timeout closes the port too -- an abandoned sign-in must not leave a
  // listener up for the rest of the session.
  await assert.rejects(() => get(listener.redirectUri));
});

test("the deadline runs from listener start, not from the first await", async () => {
  const listener = await startLoopbackListener({ timeoutMs: 40 });
  await delay(80);
  await assert.rejects(
    () => listener.waitForCallback(),
    (err: unknown) => isAuthFlowError(err) && err.kind === "timeout",
  );
});

test("close() rejects a pending wait as cancelled rather than leaving it hanging", async () => {
  const listener = await startLoopbackListener({});
  const waiting = listener.waitForCallback();
  listener.close();
  await assert.rejects(
    () => waiting,
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "cancelled");
      return true;
    },
  );
});

test("an abort signal cancels the wait and releases the port", async () => {
  const controller = new AbortController();
  const listener = await startLoopbackListener({ signal: controller.signal });
  const waiting = listener.waitForCallback();
  controller.abort();
  await assert.rejects(
    () => waiting,
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "cancelled");
      return true;
    },
  );
  await assert.rejects(() => get(listener.redirectUri));
});

test("an already-aborted signal cancels immediately", async () => {
  const listener = await startLoopbackListener({ signal: AbortSignal.abort() });
  await assert.rejects(
    () => listener.waitForCallback(),
    (err: unknown) => isAuthFlowError(err) && err.kind === "cancelled",
  );
});

test("a custom path is the only path that resolves", async () => {
  await withListener({ path: "/oauth/done" }, async (listener) => {
    assert.equal(listener.redirectUri, `http://127.0.0.1:${listener.port}/oauth/done`);
    const waiting = listener.waitForCallback();
    assert.equal((await get(`http://127.0.0.1:${listener.port}${CALLBACK_PATH}?code=X`)).status, 404);
    assert.ok(await pending(waiting));
    await get(`${listener.redirectUri}?code=Y&state=S`);
    assert.equal((await waiting).code, "Y");
  });
});
