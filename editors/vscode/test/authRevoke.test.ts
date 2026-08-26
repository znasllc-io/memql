// Sign-out reaches the cluster (znasllc-io/memql#4625).
//
// THE DEFECT THESE PIN. Signing out cleared SecretStorage, blanked the file
// keys and dropped the connection, and told the cluster nothing. The refresh
// token stayed live for its full thirty days while a toast said
// `signed out of "x"` -- a claim about the session that was only ever true of
// this editor. Anyone holding a copy of the file could still mint access
// tokens against an account whose owner believed the session was over.

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  revokeRefreshToken,
  signOutMessage,
  type RevocationOutcome,
  type RevokeFetch,
} from "../src/auth/revoke.js";

type Call = { url: string; method: string; body: string; headers: Record<string, string> };

function recorder(response: { ok: boolean; status: number } | Error): {
  calls: Call[];
  fetch: RevokeFetch;
} {
  const calls: Call[] = [];
  const fetch: RevokeFetch = async (url, init) => {
    calls.push({ url, method: init.method, body: init.body, headers: init.headers });
    if (response instanceof Error) throw response;
    return response;
  };
  return { calls, fetch };
}

// ---------------------------------------------------------------------------
// the call itself
// ---------------------------------------------------------------------------

test("revokeRefreshToken POSTs the token to the issuer's logout endpoint", async () => {
  const { calls, fetch } = recorder({ ok: true, status: 204 });
  const outcome = await revokeRefreshToken("https://identity.example.com", "rt-abc", fetch);

  assert.deepEqual(outcome, { attempted: true, revoked: true });
  assert.equal(calls.length, 1, "the cluster was never told; this is the whole defect");
  assert.equal(calls[0]?.url, "https://identity.example.com/auth/logout");
  assert.equal(calls[0]?.method, "POST");
  assert.deepEqual(
    JSON.parse(calls[0]?.body ?? "{}"),
    { refresh_token: "rt-abc" },
    "the body must carry refresh_token, which is the key extractRefreshToken reads " +
      "(component/identity/http/refresh.go)",
  );
});

test("a trailing slash on the issuer does not produce a double slash", async () => {
  const { calls, fetch } = recorder({ ok: true, status: 204 });
  await revokeRefreshToken("https://identity.example.com/", "rt-abc", fetch);
  assert.equal(calls[0]?.url, "https://identity.example.com/auth/logout");
});

// The handler answers 204, but a proxy that rewrites it to 200 has not failed
// to revoke anything.
test("any 2xx counts as revoked", async () => {
  const { fetch } = recorder({ ok: true, status: 200 });
  assert.deepEqual(await revokeRefreshToken("https://i.example.com", "rt", fetch), {
    attempted: true,
    revoked: true,
  });
});

// ---------------------------------------------------------------------------
// what happens when it cannot be done
// ---------------------------------------------------------------------------

test("an unreachable issuer is a failed attempt naming the host", async () => {
  const { fetch } = recorder(new Error("getaddrinfo ENOTFOUND identity.example.com"));
  const outcome = await revokeRefreshToken("https://identity.example.com", "rt", fetch);
  assert.equal(outcome.attempted, true);
  assert.equal(outcome.attempted && outcome.revoked, false);
  // THE WHOLE SENTENCE, not a substring of it. The person reading this has to
  // decide whether to go and revoke the session by hand, so the reason must
  // name the host AND say what happened to it -- which an equality asserts and
  // a containment check does not. (It also keeps CodeQL from reading a
  // host-shaped substring check as an incomplete URL sanitizer.)
  assert.equal(
    outcome.attempted && !outcome.revoked ? outcome.reason : "",
    "https://identity.example.com could not be reached " +
      "(getaddrinfo ENOTFOUND identity.example.com)",
  );
});

test("a non-2xx is a failed attempt naming the status", async () => {
  const { fetch } = recorder({ ok: false, status: 502 });
  const outcome = await revokeRefreshToken("https://i.example.com", "rt", fetch);
  assert.match(outcome.attempted && !outcome.revoked ? outcome.reason : "", /502/);
});

// Nothing to revoke is NOT a failure. A cluster signed in with a bare token
// has no server-side session to end, and saying "only forgotten locally" there
// would be alarming and false.
test("no token and no issuer are not attempts", async () => {
  const { calls, fetch } = recorder({ ok: true, status: 204 });
  assert.deepEqual(await revokeRefreshToken("https://i.example.com", "   ", fetch), {
    attempted: false,
  });
  assert.deepEqual(await revokeRefreshToken("", "rt", fetch), { attempted: false });
  assert.equal(calls.length, 0);
});

// ---------------------------------------------------------------------------
// what the person is told
// ---------------------------------------------------------------------------

test("a revoked session is described as ended on the cluster", () => {
  const msg = signOutMessage("prod", { attempted: true, revoked: true });
  assert.match(msg, /prod/);
  assert.match(msg, /cluster/i);
});

// The message that was wrong before: "signed out" over a token that is still
// live for thirty days.
test("an unrevoked session says so, and names where to end it", () => {
  const outcome: RevocationOutcome = {
    attempted: true,
    revoked: false,
    reason: "https://identity.example.com could not be reached (offline)",
  };
  const msg = signOutMessage("prod", outcome);
  assert.match(
    msg,
    /this machine/i,
    "the message claims the session ended when only this machine forgot it",
  );
  assert.match(msg, /Devices/, "the person is left with a live credential and no next step");
  assert.ok(
    !/^MemQL: signed out of "prod"\.$/.test(msg),
    "the unrevoked case still reads as a clean sign-out",
  );
});

test("nothing to revoke keeps the ordinary wording", () => {
  const msg = signOutMessage("local", { attempted: false });
  assert.match(msg, /signed out of "local"/);
  assert.ok(
    !/could not/.test(msg),
    "a cluster with no server-side session should not be told revocation failed",
  );
});
