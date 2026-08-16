// Taking ownership of a local cluster from the editor (znasllc-io#3905).
//
// WHAT THIS CLOSES. `seedBootstrap` creates the owner account, and that account
// holds NO human credential -- no passkey, no magic-link identity. The install
// mints one enrolment link; an operator who dismissed the notification, whose
// 15-minute link expired, or who installed before that screen offered one had
// no route back to a link from the editor at all.
//
// AND `/setup` IS NOT THAT ROUTE, which is the thing worth writing down. The
// first-run wizard 404s the moment the cluster has a user (`setupSealed`,
// memql#3415) -- and `seedBootstrap` always creates one, so on every
// installer-built cluster it is sealed from first boot. That seal is a security
// property, not an oversight: an anonymous page that grants ownership of an
// already-claimed cluster is exactly what it prevents. The authorized route for
// "the owner exists and needs a first credential" is a single-use enrolment
// token minted by somebody who already holds authority over the cluster.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ENROLMENT_CAPABILITY,
  OWNERSHIP_LINK_TTL,
  OwnershipError,
  identityBaseUrlForCluster,
  mintOwnershipLink,
} from "../src/clusters/takeOwnership.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type { ScriptOutcome } from "../src/install/runner.js";

const LINK = "https://identity.memql.localhost/enroll?code=mql_enr_" + "a".repeat(43);

function local(over: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "memql",
    endpoint: "api.memql.localhost:443",
    domain: "memql.localhost",
    local: true,
    ...over,
  } as ClusterConfig;
}

/** A runner that records what it was asked to run and returns a fixed envelope. */
function runner(result: Record<string, unknown>, exitCode = 0) {
  const calls: { scriptPath: string; params: Record<string, string> }[] = [];
  const run = async (r: {
    scriptPath: string;
    params: Record<string, string>;
  }): Promise<ScriptOutcome> => {
    calls.push({ scriptPath: r.scriptPath, params: r.params });
    return {
      argv: [],
      exitCode,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: exitCode === 0,
        capability: ENROLMENT_CAPABILITY,
        changed: true,
        result,
        error: exitCode === 0 ? null : { code: exitCode, message: "the mint refused" },
      },
    } as ScriptOutcome;
  };
  return { calls, run };
}

test("a fresh link is minted and returned", async () => {
  const { calls, run } = runner({ enrolUrl: LINK, enrolmentState: "minted", ownerClaimed: true });
  const url = await mintOwnershipLink(
    { cluster: local(), ownerEmail: "owner@example.com", repoRoot: "/repo" },
    run,
  );
  assert.equal(url, LINK);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].params["user-email"], "owner@example.com");
  assert.equal(calls[0].params.local, "true");
  assert.equal(calls[0].params.ttl, OWNERSHIP_LINK_TTL);
});

test("the link is pointed at the IDENTITY host, not the api front door", async () => {
  // The endpoint is the api door; enrolment lives on the identity one. Passing
  // the endpoint would mint a link at a host that does not serve /enroll.
  const { calls, run } = runner({ enrolUrl: LINK, enrolmentState: "minted" });
  await mintOwnershipLink(
    { cluster: local(), ownerEmail: "owner@example.com", repoRoot: "/repo" },
    run,
  );
  assert.equal(calls[0].params["base-url"], "https://identity.memql.localhost");
});

test("a cluster with no domain lets the pod answer, rather than guessing", async () => {
  // The script falls back to the pod's own MEMQL_IDENTITY_BASE_URL, which is
  // the right answer when this side knows less than the cluster does.
  assert.equal(identityBaseUrlForCluster(local({ domain: "" })), "");
  const { calls, run } = runner({ enrolUrl: LINK });
  await mintOwnershipLink(
    { cluster: local({ domain: "" }), ownerEmail: "owner@example.com", repoRoot: "/repo" },
    run,
  );
  assert.equal("base-url" in calls[0].params, false);
});

test("a remote cluster is refused, because there is no pod here to mint in", async () => {
  const { calls, run } = runner({ enrolUrl: LINK });
  await assert.rejects(
    () =>
      mintOwnershipLink(
        { cluster: local({ local: false }), ownerEmail: "owner@example.com", repoRoot: "/repo" },
        run,
      ),
    (err: unknown) => err instanceof OwnershipError && err.reason === "notLocal",
  );
  assert.deepEqual(calls, [], "nothing may be executed for a cluster this machine does not host");
});

test("no recorded owner is refused before anything runs", async () => {
  const { calls, run } = runner({ enrolUrl: LINK });
  await assert.rejects(
    () => mintOwnershipLink({ cluster: local(), ownerEmail: "  ", repoRoot: "/repo" }, run),
    (err: unknown) => err instanceof OwnershipError && err.reason === "noOwner",
  );
  assert.deepEqual(calls, []);
});

test("a failed mint is reported with the script's own reason", async () => {
  const { run } = runner({}, 5);
  await assert.rejects(
    () =>
      mintOwnershipLink(
        { cluster: local(), ownerEmail: "owner@example.com", repoRoot: "/repo" },
        run,
      ),
    (err: unknown) =>
      err instanceof OwnershipError && err.reason === "mintFailed" && /refused/.test(err.message),
  );
});

test("an unclaimed cluster is told apart from a mint that produced nothing", async () => {
  // Both come back with an empty link and they ask the operator for completely
  // different things: one needs a first sign-in to create the account, the
  // other is a fault.
  const { run } = runner({ enrolUrl: "", enrolmentState: "awaitingFirstSignIn", ownerClaimed: false });
  await assert.rejects(
    () =>
      mintOwnershipLink(
        { cluster: local(), ownerEmail: "owner@example.com", repoRoot: "/repo" },
        run,
      ),
    (err: unknown) =>
      err instanceof OwnershipError &&
      err.reason === "noLink" &&
      /no owner account yet/.test(err.message),
  );
});

test("the link never leaks through an error path", async () => {
  const { run } = runner({ enrolUrl: LINK }, 5);
  try {
    await mintOwnershipLink(
      { cluster: local(), ownerEmail: "owner@example.com", repoRoot: "/repo" },
      run,
    );
    assert.fail("expected the mint to be reported as failed");
  } catch (err) {
    assert.equal((err as Error).message.includes("mql_enr_"), false);
  }
});
