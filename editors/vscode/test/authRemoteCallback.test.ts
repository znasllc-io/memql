// Sign-in under Remote-SSH, Codespaces and dev containers (memql#4623).
//
// The defect was not a crash and not an error: the loopback listener bound
// 127.0.0.1 on the SERVER, the browser opened on the user's machine and
// redirected to THEIR loopback, and nothing was listening there. The bind
// succeeded and `openExternal` succeeded, so neither fallback trigger fired --
// and `timeout` had been removed as a trigger in memql#4600 -- so the user
// watched a 600-second spinner and was then told the browser "could not reach
// 127.0.0.1", which was true and useless.
//
// README and two source comments all asserted it "falls back automatically",
// and test/authFlow.test.ts pinned an invariant named "so a remote host
// tunnels". Neither `env.remoteName` nor `env.uiKind` appeared anywhere in
// src/. So these tests exist to make the remote path a thing that is EXERCISED
// rather than a thing that is claimed.

import test from "node:test";
import assert from "node:assert/strict";

import type { ClusterConfig } from "../src/clusters/model.js";
import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import { isAuthFlowError } from "../src/auth/errors.js";
import { runAuthorizationFlow, type AuthFlowDeps } from "../src/auth/flow.js";
import { OAUTH_METADATA_PATH } from "../src/auth/discovery.js";
import {
  awaitUriCallback,
  deliverUriCallback,
  pendingUriCallbackCount,
} from "../src/auth/uriCallback.js";
import { WELL_KNOWN_REDIRECT_URI_VSCODE } from "../src/auth/wellKnownClient.js";

const ISSUER = "https://identity.memql.localhost";

function cluster(): ClusterConfig {
  return { name: "remote", endpoint: "api.memql.localhost:443", domain: "memql.localhost" };
}

function identityFetch(): (url: string, init: HttpRequestInit) => Promise<HttpResponseLike> {
  return async (url) => {
    const body = url.endsWith(OAUTH_METADATA_PATH)
      ? {
          issuer: ISSUER,
          authorization_endpoint: `${ISSUER}/authorize`,
          token_endpoint: `${ISSUER}/oauth/token`,
          grant_types_supported: ["authorization_code", "refresh_token"],
          code_challenge_methods_supported: ["S256"],
        }
      : { access_token: "AT", refresh_token: "RT", token_type: "Bearer", expires_in: 900 };
    return { ok: true, status: 200, text: async () => JSON.stringify(body) };
  };
}

/**
 * A remote host: the "browser" runs elsewhere, and the only way back is the
 * vscode:// URI its client forwards. Modelled by capturing the authorize URL
 * and delivering the callback the way the extension's URI handler does.
 */
function remoteDeps(overrides: Partial<AuthFlowDeps> = {}): AuthFlowDeps & { opened: string[] } {
  const opened: string[] = [];
  return {
    opened,
    fetch: identityFetch(),
    resolveExternalUri: async (u) => u,
    openExternal: async (u) => {
      opened.push(u);
      // The user's VS Code client resolves the vscode:// redirect and hands it
      // to the extension -- across the remote boundary, over the connection
      // that already exists. This is the extension's URI handler doing it.
      const url = new URL(u);
      const state = url.searchParams.get("state") ?? "";
      setImmediate(() => {
        deliverUriCallback(`?code=AUTHCODE&state=${encodeURIComponent(state)}`, "/callback");
      });
      return true;
    },
    isRemote: true,
    // A LOOPBACK LISTENER MUST NOT BE STARTED AT ALL on a remote host. Binding
    // one on the server is the defect; it is not merely useless, it is what
    // made the failure invisible (the bind succeeded, so no fallback fired).
    startListener: () => {
      throw new Error("a loopback listener was started on a remote host");
    },
    now: () => 1_800_000_000_000,
    ...overrides,
  } as AuthFlowDeps & { opened: string[] };
}

test("a remote host signs in through the vscode:// callback, never a loopback port", async () => {
  const deps = remoteDeps();

  const tokens = await runAuthorizationFlow(cluster(), deps);

  assert.equal(tokens.accessToken, "AT");
  assert.equal(deps.opened.length, 1);
  const authorize = new URL(deps.opened[0]);
  assert.equal(
    authorize.searchParams.get("redirect_uri"),
    WELL_KNOWN_REDIRECT_URI_VSCODE,
    "a remote host sent the browser to a loopback port bound on the SERVER",
  );
});

test("a local host still uses loopback", async () => {
  // The remote path must not become the only path: a loopback callback is
  // faster, needs no URI handler, and is what the overwhelming majority of
  // sign-ins use.
  let started = false;
  const deps = remoteDeps({
    isRemote: false,
    startListener: async () => {
      started = true;
      return {
        host: "127.0.0.1",
        port: 54321,
        redirectUri: "http://127.0.0.1:54321/callback",
        waitForCallback: async () => ({ code: "AUTHCODE", state: "" }),
        close: () => {},
      };
    },
  });

  // The flow verifies state, so the fake listener has to echo the real one.
  const opened: string[] = [];
  deps.openExternal = async (u) => {
    opened.push(u);
    return true;
  };
  const state = "";
  void state;

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps),
    (err: unknown) => {
      // A state mismatch is the expected refusal here -- what matters is that a
      // loopback listener was STARTED, which is the branch under test.
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "stateMismatch");
      return true;
    },
  );
  assert.ok(started, "a local host did not start a loopback listener");
  assert.equal(
    new URL(opened[0]).searchParams.get("redirect_uri"),
    "http://127.0.0.1:54321/callback",
  );
});

// -----------------------------------------------------------------------------
// the waiter itself
// -----------------------------------------------------------------------------

test("a callback is routed to the sign-in whose state it carries", async () => {
  // Two sign-ins in flight -- two windows, two clusters -- is ordinary. Routing
  // a callback to the wrong one hands an authorization code to a flow that
  // correctly rejects it as a state mismatch, while the flow it belonged to
  // waits out its whole deadline.
  const first = awaitUriCallback({
    redirectUri: WELL_KNOWN_REDIRECT_URI_VSCODE,
    state: "state-one",
    timeoutMs: 5_000,
  });
  const second = awaitUriCallback({
    redirectUri: WELL_KNOWN_REDIRECT_URI_VSCODE,
    state: "state-two",
    timeoutMs: 5_000,
  });

  assert.ok(deliverUriCallback("?code=CODE-TWO&state=state-two", "/callback"));
  assert.deepEqual(await second.waitForCallback(), {
    code: "CODE-TWO",
    state: "state-two",
    error: "",
    errorDescription: "",
  });

  assert.ok(deliverUriCallback("?code=CODE-ONE&state=state-one", "/callback"));
  assert.equal((await first.waitForCallback()).code, "CODE-ONE");
  assert.equal(pendingUriCallbackCount(), 0, "a settled waiter was left armed");
});

test("a uri this is not waiting on falls through untouched", () => {
  // The SAME handler serves the portal's handoff links. Consuming one of those
  // would report a handoff failure for a uri that was exactly right.
  assert.equal(deliverUriCallback("?space=abc", "/open"), false, "a handoff link was consumed");
  assert.equal(deliverUriCallback("?code=X&state=nobody-waiting", "/callback"), false);
  assert.equal(deliverUriCallback("?code=X", "/callback"), false, "a callback with no state was routed");
});

test("closing a waiter releases it rather than leaking a timer", async () => {
  const waiter = awaitUriCallback({
    redirectUri: WELL_KNOWN_REDIRECT_URI_VSCODE,
    state: "state-closed",
    timeoutMs: 60_000,
  });
  const settled = waiter.waitForCallback();
  waiter.close();
  await assert.rejects(settled, (err: unknown) => {
    assert.ok(isAuthFlowError(err));
    assert.equal(err.kind, "cancelled");
    return true;
  });
  assert.equal(pendingUriCallbackCount(), 0);
});

test("the timeout message names the handoff, not a loopback port", async () => {
  const waiter = awaitUriCallback({
    redirectUri: WELL_KNOWN_REDIRECT_URI_VSCODE,
    state: "state-timeout",
    timeoutMs: 1,
  });
  await assert.rejects(waiter.waitForCallback(), (err: unknown) => {
    assert.ok(isAuthFlowError(err));
    assert.equal(err.kind, "timeout");
    // The old sentence blamed 127.0.0.1 in every case, including this one where
    // no loopback port was ever involved.
    assert.ok(!/127\.0\.0\.1/.test(err.message), `still blames loopback: ${err.message}`);
    assert.match(err.message, /vscode:\/\/znasllc\.memql\/callback/);
    return true;
  });
});
