// The four version collectors (memql#3993).
//
// Each collector answers "what does THIS source say this cluster is running",
// and each returns "" for "nothing to say". None of them may throw: a refresh
// is opportunistic background work, and every source here is a subprocess, an
// RPC or a socket that can fail on a plane.

import test from "node:test";
import assert from "node:assert/strict";

import { createVersionCollector } from "../src/version/collectors.js";
import type { VersionCandidate } from "../src/version/learners.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type { Receipt } from "../src/install/receipt.js";
import type { ScriptOutcome } from "../src/install/runner.js";

const cluster = (over: Partial<ClusterConfig> = {}): ClusterConfig => ({
  name: "local",
  endpoint: "api.memql.localhost:443",
  domain: "memql.localhost",
  ...over,
});

function receiptWith(entries: Array<{ id: string; params: Record<string, string> }>): Receipt {
  return {
    version: 1,
    graph: "install",
    startedAt: "2026-08-16T00:00:00Z",
    updatedAt: "2026-08-16T00:00:00Z",
    entries: entries.map((e) => ({
      stepId: e.id,
      script: "",
      receipt: "",
      preExisting: false,
      params: e.params,
      result: {},
      changed: true,
      recordedAt: "2026-08-16T00:00:00Z",
    })),
  };
}

const outcome = (result: Record<string, unknown>, ok = true): ScriptOutcome =>
  ({
    argv: [],
    exitCode: ok ? 0 : 5,
    signal: null,
    stdout: "",
    stderr: "",
    envelope: { ok, capability: "k3d.status", changed: false, result, error: null },
  }) as ScriptOutcome;

const noReceipt = async (): Promise<Receipt | null> => null;
const noScript = async (): Promise<ScriptOutcome> => outcome({});

type Deps = Parameters<typeof createVersionCollector>[0];

function collector(over: Partial<Deps>): (c: ClusterConfig) => Promise<VersionCandidate[]> {
  return createVersionCollector({
    repoRoot: "/repo",
    readReceipt: noReceipt,
    runCapability: noScript,
    ...over,
  });
}

// --- The receipt ------------------------------------------------------------

test("the receipt contributes the tag the wizard checked out", async () => {
  const collect = collector({
    readReceipt: async () =>
      receiptWith([
        { id: "stackCheckout", params: { tag: "v0.18.0", domain: "memql.localhost" } },
      ]),
  });
  const found = await collect(cluster());
  assert.deepEqual(
    found.filter((c) => c.source === "receipt"),
    [{ source: "receipt", value: "v0.18.0" }],
  );
});

test("a receipt describing a DIFFERENT cluster contributes nothing", async () => {
  // There is one receipt per machine and it names the cluster it installed.
  // Letting it speak for another cluster would record a version that machine
  // never ran -- the same failure `receiptNamesAnotherCluster` was added for.
  const collect = collector({
    readReceipt: async () =>
      receiptWith([{ id: "stackCheckout", params: { tag: "v0.18.0", domain: "other.example.com" } }]),
  });
  const found = await collect(cluster({ domain: "memql.localhost" }));
  assert.deepEqual(found.filter((c) => c.source === "receipt"), []);
});

test("a receipt with no recorded domain still speaks, because that is a gap not a contradiction", async () => {
  const collect = collector({
    readReceipt: async () => receiptWith([{ id: "stackCheckout", params: { tag: "v0.18.0" } }]),
  });
  const found = await collect(cluster());
  assert.equal(found.find((c) => c.source === "receipt")?.value, "v0.18.0");
});

test("no receipt on this machine contributes nothing", async () => {
  const found = await collector({})(cluster());
  assert.deepEqual(found.filter((c) => c.source === "receipt"), []);
});

test("a receipt that cannot be read contributes nothing rather than failing the refresh", async () => {
  const collect = collector({
    readReceipt: async () => {
      throw new Error("EACCES");
    },
  });
  const found = await collect(cluster());
  assert.deepEqual(found.filter((c) => c.source === "receipt"), []);
});

// --- ArgoCD -----------------------------------------------------------------

test("argocd contributes the targetRevision it is reconciling", async () => {
  const collect = collector({
    runCapability: async () => outcome({ argocdTargetRevision: "v0.18.0" }),
  });
  const found = await collect(cluster({ local: true }));
  assert.deepEqual(
    found.filter((c) => c.source === "argocd"),
    [{ source: "argocd", value: "v0.18.0" }],
  );
});

test("a BRANCH targetRevision is still reported, because refusing it is not this layer's job", async () => {
  // Tracking a branch is legitimate -- status.sh says so -- and the value is
  // real. It is the WRITE decision that refuses to let it overwrite a recorded
  // release, which is the only place that judgement belongs.
  const collect = collector({
    runCapability: async () => outcome({ argocdTargetRevision: "main" }),
  });
  const found = await collect(cluster({ local: true }));
  assert.equal(found.find((c) => c.source === "argocd")?.value, "main");
});

test("argocd is only asked about a LOCAL cluster", async () => {
  // `k3d.status` inspects a k3d cluster on THIS machine. Running it while
  // pointed at a remote cluster would report the local one's revision as if it
  // were the remote's -- a confidently wrong answer.
  let ran = 0;
  const collect = collector({
    runCapability: async () => {
      ran++;
      return outcome({ argocdTargetRevision: "v0.18.0" });
    },
  });
  await collect(cluster({ local: undefined }));
  assert.equal(ran, 0, "a non-local cluster must not run the k3d status script");
});

test("a failed status script contributes nothing", async () => {
  const collect = collector({
    runCapability: async () => outcome({ argocdTargetRevision: "v0.18.0" }, false),
  });
  const found = await collect(cluster({ local: true }));
  assert.deepEqual(found.filter((c) => c.source === "argocd"), []);
});

test("a status script that throws contributes nothing", async () => {
  const collect = collector({
    runCapability: async () => {
      throw new Error("no bash");
    },
  });
  const found = await collect(cluster({ local: true }));
  assert.deepEqual(found.filter((c) => c.source === "argocd"), []);
});

test("a non-string targetRevision is ignored", async () => {
  const collect = collector({
    runCapability: async () => outcome({ argocdTargetRevision: 42 }),
  });
  const found = await collect(cluster({ local: true }));
  assert.deepEqual(found.filter((c) => c.source === "argocd"), []);
});

// --- Deploy control ---------------------------------------------------------

test("deploy control contributes its version", async () => {
  const collect = collector({
    deployStatus: async () => ({ version: "v0.18.0", engineVersion: "" }),
  });
  const found = await collect(cluster());
  assert.deepEqual(
    found.filter((c) => c.source === "deployControl"),
    [{ source: "deployControl", value: "v0.18.0" }],
  );
});

test("deploy control falls back to engineVersion when version is empty", async () => {
  // `DeploymentStatus.version` is parsed from the overlay's promotion comment
  // and is documented as empty where no such provenance exists. The engine
  // version from the resolved lockfile is the answer there.
  const collect = collector({
    deployStatus: async () => ({ version: "", engineVersion: "v0.17.1" }),
  });
  assert.equal(
    (await collect(cluster())).find((c) => c.source === "deployControl")?.value,
    "v0.17.1",
  );
});

test("a deploy-control call that fails contributes nothing", async () => {
  // It is owner/admin gated, so a PERMISSION_DENIED here is an ordinary
  // outcome for a reader, not a problem to surface.
  const collect = collector({
    deployStatus: async () => {
      throw new Error("PERMISSION_DENIED");
    },
  });
  assert.deepEqual((await collect(cluster())).filter((c) => c.source === "deployControl"), []);
});

test("deploy control is not consulted when no port is wired", async () => {
  assert.deepEqual((await collector({})(cluster())).filter((c) => c.source === "deployControl"), []);
});

// --- The live connection ----------------------------------------------------

test("a live connection contributes what memqlVersion reports", async () => {
  const collect = collector({ liveVersion: async () => "v0.19.0" });
  assert.deepEqual(
    (await collect(cluster())).filter((c) => c.source === "live"),
    [{ source: "live", value: "v0.19.0" }],
  );
});

test("a live connection reporting the build stamp still contributes it", async () => {
  // Reporting it is right; the comparison module calls it notComparable and
  // the write decision refuses to let it overwrite a real release. Filtering
  // it out HERE would hide the fact that the cluster answered at all.
  const collect = collector({ liveVersion: async () => "0.15.0-1737072000" });
  assert.equal(
    (await collect(cluster())).find((c) => c.source === "live")?.value,
    "0.15.0-1737072000",
  );
});

test("a dead connection contributes nothing", async () => {
  const collect = collector({
    liveVersion: async () => {
      throw new Error("stream closed");
    },
  });
  assert.deepEqual((await collect(cluster())).filter((c) => c.source === "live"), []);
});

// --- All four together ------------------------------------------------------

test("every source that answers is collected, and one failure does not silence the rest", async () => {
  const collect = collector({
    readReceipt: async () => receiptWith([{ id: "stackCheckout", params: { tag: "v0.18.0" } }]),
    runCapability: async () => {
      throw new Error("no bash");
    },
    deployStatus: async () => ({ version: "v0.17.1", engineVersion: "" }),
    liveVersion: async () => "0.15.0-1737072000",
  });
  const found = await collect(cluster({ local: true }));
  assert.deepEqual(
    found.map((c) => c.source).sort(),
    ["deployControl", "live", "receipt"],
  );
});

test("a cluster nothing can answer for yields no candidates and no throw", async () => {
  assert.deepEqual(await collector({})(cluster()), []);
});
