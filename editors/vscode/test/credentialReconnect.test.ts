// A refresh-driven reconnect must still re-inject the session-define.
//
// memql#3385's third acceptance item, and the reason the credential work and
// the run loop have to be tested TOGETHER at least once.
//
// A session-defined construct lives on the WebSocket (src/run/session.ts). A
// token expiry ends that stream, so a reconnect that silently re-used the
// previous injection record would run the DEPLOYED construct and return a
// perfectly good wrong answer -- the exact invisible failure the run loop was
// built to prevent, arriving through the credential rather than through the
// stream. The issue says so in as many words.
//
// The re-injection rule is held by SessionRegistry's epoch, which the ADAPTER
// bumps from ConnectionManager.onDidChangeState (src/extension.ts). Neither
// half's own tests can prove the wire between them, so this file rebuilds that
// wire exactly as extension.ts does and drives a real credential refresh
// through it.

import test from "node:test";
import assert from "node:assert/strict";

import type { Connection, Row } from "@znasllc-io/memql-sdk-core/client";
import type {
  SessionDefineBundleResult,
  ValidateBundleResult,
} from "@znasllc-io/memql-sdk-core/authoring";

import type { ClusterConfig } from "../src/clusters/model.js";
import { ConnectionManager } from "../src/connection/manager.js";
import { CredentialResolver, type HttpResponseLike } from "../src/connection/credentials.js";
import { assembleBundle } from "../src/run/bundle.js";
import { RunOrchestrator, type RunEngine, type ToolContent } from "../src/run/orchestrator.js";

const NOW_MS = 1_800_000_000_000;

function b64url(value: unknown): string {
  return Buffer.from(JSON.stringify(value)).toString("base64url");
}

function jwtExpiringIn(seconds: number): string {
  return `${b64url({ alg: "RS256" })}.${b64url({
    sub: "u",
    exp: Math.floor(NOW_MS / 1000) + seconds,
  })}.sig`;
}

function fakeConn(nodeId: string): Connection & { terminate(): void } {
  let resolveDone!: () => void;
  const done = new Promise<void>((resolve) => {
    resolveDone = resolve;
  });
  return {
    nodeId,
    query: {},
    subscriptions: {},
    close(): void {
      resolveDone();
    },
    done(): Promise<void> {
      return done;
    },
    terminate(): void {
      resolveDone();
    },
  } as unknown as Connection & { terminate(): void };
}

class StubEngine implements RunEngine {
  readonly ops: string[] = [];
  async validateBundle(): Promise<ValidateBundleResult> {
    this.ops.push("validate");
    return { ok: true, diagnostics: [] };
  }
  async sessionDefineBundle(): Promise<SessionDefineBundleResult> {
    this.ops.push("define");
    return { ok: true, defined: [], diagnostics: [], error: "" };
  }
  async executeNamed(): Promise<{ rows: Row[]; raw: unknown }> {
    this.ops.push("executeNamed");
    return { rows: [], raw: {} };
  }
  async callTool(): Promise<{ content: ToolContent[]; isError: boolean }> {
    this.ops.push("callTool");
    return { content: [], isError: false };
  }
}

test("a credential refresh reconnect re-injects the session-define before the re-run", async () => {
  const engine = new StubEngine();
  const bearers: string[] = [];

  const cluster: ClusterConfig = {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    domain: "local.znas.io",
    local: true,
    token: jwtExpiringIn(600),
    refreshToken: "RT-1",
  };

  const credentials = new CredentialResolver({
    now: () => NOW_MS,
    fetch: async (): Promise<HttpResponseLike> => ({
      ok: true,
      status: 200,
      text: async () =>
        JSON.stringify({ access_token: "REFRESHED", refresh_token: "ROTATED", expires_in: 900 }),
    }),
    persist: async (_name, update) => {
      cluster.token = update.token;
    },
  });

  const conns = new ConnectionManager((opts) => {
    bearers.push(opts.auth?.bearer ?? "");
    return Promise.resolve(fakeConn(`bff-${bearers.length}`));
  }, credentials);

  const orchestrator = new RunOrchestrator({
    cluster: () => ({ name: "local", label: "local", local: true }),
    engine: () => (conns.state.status === "connected" ? engine : undefined),
    assemble: (t) => assembleBundle(t.uri, "query q { }\n", {
      resolveImport: () => undefined,
      read: () => undefined,
      imports: () => [],
    }),
    confirmWrite: async () => true,
    publishDiagnostics: () => {},
  });
  // The wire extension.ts builds: every connection-state change ends the stream
  // that held any session-define.
  conns.onDidChangeState(() => orchestrator.noteStreamReset());

  const target = { uri: "/ws/q.memql", kind: "query" as const, name: "q", args: [] };

  await conns.connect(cluster);
  assert.equal(conns.state.status, "connected");
  await orchestrator.run(target, {});
  assert.deepEqual(engine.ops, ["validate", "define", "executeNamed"]);

  // The access token runs out mid-session. The operator reconnects (or any
  // surface does); the resolver exchanges the refresh token silently.
  cluster.token = jwtExpiringIn(-30);
  await conns.connect(cluster);

  assert.equal(conns.state.status, "connected", "the refresh must produce a live connection");
  assert.deepEqual(bearers, [jwtExpiringIn(600), "REFRESHED"]);

  // The buffer has NOT changed. Without the epoch bump this second run skips
  // the define and silently invokes the deployed construct.
  await orchestrator.run(target, {});
  assert.deepEqual(
    engine.ops,
    ["validate", "define", "executeNamed", "validate", "define", "executeNamed"],
    "a refresh-triggered reconnect must re-inject before honouring the re-run",
  );
});

test("a session whose token expires survives past the 15-minute access-token lifetime", async () => {
  // memql#3385's first acceptance item, stated as behaviour: three consecutive
  // connects across a token lifetime, none of them requiring an edit to
  // clusters.yaml. Each expiry is answered by the stored refresh token, and
  // each rotation is written back so the next one has something to present.
  let issued = 0;
  const rotations: string[] = [];
  const cluster: ClusterConfig = {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    domain: "local.znas.io",
    token: jwtExpiringIn(-1),
    refreshToken: "RT-0",
  };
  const secrets = new Map<string, string>();

  const credentials = new CredentialResolver({
    now: () => NOW_MS,
    secrets: {
      get: async (k) => secrets.get(k),
      store: async (k, v) => {
        secrets.set(k, v);
        // The refresh token only. The same store also carries the access
        // token's expiry and the index that makes an un-enumerable
        // SecretStorage sweepable (memql#3404); what this case is about is the
        // ROTATION, so it watches the one key that rotates.
        if (k === "memql.cluster.refreshToken:local") rotations.push(v);
      },
      delete: async (k) => {
        secrets.delete(k);
      },
    },
    fetch: async (_url, init): Promise<HttpResponseLike> => {
      issued += 1;
      const body = JSON.parse(init.body) as { refresh_token: string };
      assert.equal(body.refresh_token, secrets.get("memql.cluster.refreshToken:local") ?? "RT-0");
      return {
        ok: true,
        status: 200,
        text: async () =>
          JSON.stringify({
            access_token: jwtExpiringIn(-1), // already stale again by the next connect
            refresh_token: `RT-${issued}`,
            expires_in: 900,
          }),
      };
    },
    persist: async (_name, update) => {
      cluster.token = update.token;
      if (update.clearStoredRefreshToken) cluster.refreshToken = "";
    },
  });

  const conns = new ConnectionManager(() => Promise.resolve(fakeConn("bff-0")), credentials);

  for (let i = 0; i < 3; i++) {
    await conns.connect(cluster);
    assert.equal(conns.state.status, "connected", `connect ${i + 1} must succeed`);
  }

  assert.equal(issued, 3, "each expiry must be answered by a refresh exchange");
  assert.deepEqual(rotations, ["RT-1", "RT-2", "RT-3"]);
  assert.equal(cluster.refreshToken, "", "the plaintext refresh token must not linger in the file");
});
