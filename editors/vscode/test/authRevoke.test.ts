// Sign-out ends the session on the CLUSTER, not only in this editor
// (memql#4625).
//
// The defect these tests exist for is not a crash: sign-out worked, the toast
// said `signed out of "x"`, and the refresh token stayed live on the cluster
// for its full 30 days. So the assertions are about two things that have to be
// true together -- the revocation call is MADE, and the message TELLS THE TRUTH
// when it did not land.

import test from "node:test";
import assert from "node:assert/strict";

import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import {
  LOGOUT_PATH,
  describeRevocation,
  revocationNeedsAttention,
  revokeRefreshToken,
  type RevocationOutcome,
} from "../src/auth/revoke.js";

const ISSUER = "https://identity.memql.localhost";
const REFRESH = "a-refresh-token";

interface Call {
  url: string;
  init: HttpRequestInit;
}

function recordingFetch(
  respond: (url: string) => HttpResponseLike | Promise<HttpResponseLike>,
): { fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>; calls: Call[] } {
  const calls: Call[] = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({ url, init });
      return respond(url);
    },
  };
}

const noContent: HttpResponseLike = { ok: true, status: 204, text: async () => "" };

test("a sign-out POSTs the refresh token to the cluster's logout route", async () => {
  const { fetch, calls } = recordingFetch(() => noContent);

  const outcome = await revokeRefreshToken({ issuer: ISSUER, refreshToken: REFRESH, fetch });

  assert.equal(outcome.kind, "revoked");
  assert.equal(calls.length, 1, "the cluster was never called; the session would stay live for 30 days");
  assert.equal(calls[0].url, `${ISSUER}${LOGOUT_PATH}`);
  assert.equal(calls[0].init.method, "POST");
  // The JSON body form, which extractRefreshToken accepts alongside the cookie
  // and the Authorization header. The cookie is a browser's spelling.
  assert.deepEqual(JSON.parse(String(calls[0].init.body)), { refresh_token: REFRESH });
});

test("a trailing slash on the issuer does not produce a double-slash URL", async () => {
  const { fetch, calls } = recordingFetch(() => noContent);
  await revokeRefreshToken({ issuer: `${ISSUER}/`, refreshToken: REFRESH, fetch });
  assert.equal(calls[0].url, `${ISSUER}${LOGOUT_PATH}`);
});

test("an unreachable cluster is a value, never a throw", async () => {
  // The caller's next step -- clear the local credentials -- is unconditional.
  // An exception here would put that behind a try/catch somebody could later
  // get wrong, and failing to clear locally is strictly worse than the bug.
  const outcome = await revokeRefreshToken({
    issuer: ISSUER,
    refreshToken: REFRESH,
    fetch: async () => {
      throw new Error("connect ECONNREFUSED");
    },
  });
  assert.equal(outcome.kind, "failed");
  assert.match((outcome as { reason: string }).reason, /ECONNREFUSED/);
});

test("a refusal from the cluster is reported, not swallowed", async () => {
  const outcome = await revokeRefreshToken({
    issuer: ISSUER,
    refreshToken: REFRESH,
    fetch: async () => ({ ok: false, status: 500, text: async () => "boom" }),
  });
  assert.equal(outcome.kind, "failed");
  assert.match((outcome as { reason: string }).reason, /500/);
});

test("no issuer means nowhere to POST, and that is its own outcome", async () => {
  const { fetch, calls } = recordingFetch(() => noContent);
  const outcome = await revokeRefreshToken({ issuer: undefined, refreshToken: REFRESH, fetch });
  assert.equal(outcome.kind, "noIssuer");
  assert.equal(calls.length, 0);
});

test("no refresh token held means there is nothing to revoke", async () => {
  const { fetch, calls } = recordingFetch(() => noContent);
  for (const held of [undefined, "", "   "]) {
    const outcome = await revokeRefreshToken({ issuer: ISSUER, refreshToken: held, fetch });
    assert.equal(outcome.kind, "nothingToRevoke");
  }
  assert.equal(calls.length, 0, "a POST with no token would be a pointless round trip");
});

// THE MESSAGE IS THE OTHER HALF OF THE FIX. `signed out of "x"` reads as "the
// session is over" whether or not it is, and a user who believes they revoked
// access takes no further action.
test("the message distinguishes a revoked session from a merely forgotten one", () => {
  const revoked = describeRevocation("prod", { kind: "revoked" });
  assert.match(revoked, /revoked on the cluster/i);
  assert.ok(!revocationNeedsAttention({ kind: "revoked" }));

  for (const outcome of [
    { kind: "noIssuer" } as RevocationOutcome,
    { kind: "failed", reason: "the cluster answered 500" } as RevocationOutcome,
  ]) {
    const text = describeRevocation("prod", outcome);
    assert.match(text, /HERE ONLY/, `"${text}" does not say the cluster session survived`);
    assert.match(text, /stays valid until it expires/, `"${text}" does not say the credential is still live`);
    // And it names where the user can actually finish the job.
    assert.match(text, /Devices page/, `"${text}" does not name the portal's Devices page`);
    assert.ok(
      revocationNeedsAttention(outcome),
      "a session that survived sign-out must be a warning, not a status line",
    );
  }
});
