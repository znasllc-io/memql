// Whether the "+" knows a local cluster is already here (memql#3412).
//
// The table below is the acceptance criterion, not an illustration of it: all
// three verdicts against BOTH evidence sources, each source alone and both
// together, and a probe that never answers. The two rows that matter most are
// the ones the feature exists for:
//
//   - registry-only evidence. A cluster built by hand with `make up` and
//     registered as `local: true` leaves NO install receipt, so a detector
//     reading only the receipt would call it absent and offer to install over
//     the top of it.
//   - the hanging probe. The deadline is the module's own, so a probe that
//     never settles still yields a verdict -- the menu opens either way.

import test from "node:test";
import assert from "node:assert/strict";

import type { ReadClustersResult } from "../src/clusters/file.js";
import {
  ClusterPresence,
  DEFAULT_LOCAL_ENDPOINT,
  addClusterMenu,
  detectPresence,
  probeEndpointFor,
  verdictFor,
  type EndpointProbe,
  type PresenceVerdict,
} from "../src/clusters/presence.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import { emptyReceipt, type Receipt, type ReceiptEntry } from "../src/install/receipt.js";

const CLUSTERS_PATH = "/tmp/does-not-exist/clusters.yaml";
const RECEIPT_PATH = "/tmp/does-not-exist/install-receipt.json";

function entry(over: Partial<ReceiptEntry> = {}): ReceiptEntry {
  return {
    stepId: "clusterUp",
    script: "k3d.up",
    receipt: "stack",
    preExisting: false,
    params: {},
    result: {},
    changed: true,
    recordedAt: "2026-08-09T00:00:00.000Z",
    ...over,
  };
}

/** A receipt describing one executed step -- the "an install happened" shape. */
function installedReceipt(entries: ReceiptEntry[] = [entry()]): Receipt {
  return { ...emptyReceipt("install"), entries };
}

function clustersWith(clusters: ClusterConfig[]): () => Promise<ReadClustersResult> {
  return () => Promise.resolve({ ok: true, file: { clusters, selectedCluster: "" } });
}

const NO_CLUSTERS = clustersWith([]);
const LOCAL_ENTRY: ClusterConfig = {
  name: "local",
  endpoint: "cockpit.local.znas.io:443",
  local: true,
};

/** A probe with a fixed answer that records every endpoint it was asked about. */
interface RecordingProbe {
  (endpoint: string, timeoutMs: number): Promise<boolean>;
  calls: string[];
}

function fixedProbe(answer: boolean): RecordingProbe {
  const calls: string[] = [];
  const probe: RecordingProbe = Object.assign(
    (endpoint: string) => {
      calls.push(endpoint);
      return Promise.resolve(answer);
    },
    { calls }
  );
  return probe;
}

/** A probe that never settles -- the deadline has to be someone else's job. */
const HANGING_PROBE: EndpointProbe = () => new Promise<boolean>(() => undefined);

// ---------------------------------------------------------------------------
// THE TABLE: three verdicts x both evidence sources, plus probe timeout
// ---------------------------------------------------------------------------

interface Row {
  name: string;
  receipt: Receipt | null;
  clusters: ClusterConfig[];
  probe: EndpointProbe;
  want: PresenceVerdict;
  wantEvidence: { receipt: boolean; registry: boolean };
  /** Endpoints the probe should have been asked about. */
  wantProbed: string[];
}

const TABLE: Row[] = [
  {
    name: "no receipt and no local entry is ABSENT, and nothing is dialed",
    receipt: null,
    clusters: [],
    probe: fixedProbe(true),
    want: "absent",
    wantEvidence: { receipt: false, registry: false },
    // The verdict is `absent` however the dial goes, so there is nothing to
    // learn from it -- and the operator with no cluster is the one who least
    // deserves a network round trip in front of their menu.
    wantProbed: [],
  },
  {
    name: "a receipt whose entries are empty is not evidence: it installed nothing",
    receipt: installedReceipt([]),
    clusters: [],
    probe: fixedProbe(true),
    want: "absent",
    wantEvidence: { receipt: false, registry: false },
    wantProbed: [],
  },
  {
    name: "a non-local registry entry is not evidence: ABSENT MEANS NOT LOCAL",
    receipt: null,
    // No `local` flag at all -- the shape of every cluster registered before
    // that field existed, and of every staging/production cluster since.
    clusters: [{ name: "staging", endpoint: "cockpit.staging.example.com:443" }],
    probe: fixedProbe(true),
    want: "absent",
    wantEvidence: { receipt: false, registry: false },
    wantProbed: [],
  },
  {
    name: "RECEIPT ONLY + the endpoint answers is INSTALLED-HEALTHY",
    receipt: installedReceipt(),
    clusters: [],
    probe: fixedProbe(true),
    want: "installed-healthy",
    wantEvidence: { receipt: true, registry: false },
    wantProbed: [DEFAULT_LOCAL_ENDPOINT],
  },
  {
    name: "RECEIPT ONLY + the dial fails is INSTALLED-UNREACHABLE",
    receipt: installedReceipt(),
    clusters: [],
    probe: fixedProbe(false),
    want: "installed-unreachable",
    wantEvidence: { receipt: true, registry: false },
    wantProbed: [DEFAULT_LOCAL_ENDPOINT],
  },
  {
    name: "REGISTRY ONLY + the endpoint answers is INSTALLED-HEALTHY (the `make up` cluster)",
    receipt: null,
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(true),
    want: "installed-healthy",
    wantEvidence: { receipt: false, registry: true },
    wantProbed: [LOCAL_ENTRY.endpoint],
  },
  {
    name: "REGISTRY ONLY + the dial fails is INSTALLED-UNREACHABLE",
    receipt: null,
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(false),
    want: "installed-unreachable",
    wantEvidence: { receipt: false, registry: true },
    wantProbed: [LOCAL_ENTRY.endpoint],
  },
  {
    name: "BOTH sources + the endpoint answers is INSTALLED-HEALTHY",
    receipt: installedReceipt(),
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(true),
    want: "installed-healthy",
    wantEvidence: { receipt: true, registry: true },
    // The registered endpoint wins over the receipt's convention: an operator
    // who wrote one down is naming the front door they actually use.
    wantProbed: [LOCAL_ENTRY.endpoint],
  },
  {
    name: "BOTH sources + the dial fails is INSTALLED-UNREACHABLE",
    receipt: installedReceipt(),
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(false),
    want: "installed-unreachable",
    wantEvidence: { receipt: true, registry: true },
    wantProbed: [LOCAL_ENTRY.endpoint],
  },
  {
    name: "a probe that NEVER ANSWERS degrades to INSTALLED-UNREACHABLE",
    receipt: installedReceipt(),
    clusters: [LOCAL_ENTRY],
    probe: HANGING_PROBE,
    want: "installed-unreachable",
    wantEvidence: { receipt: true, registry: true },
    wantProbed: [LOCAL_ENTRY.endpoint],
  },
];

for (const row of TABLE) {
  test(`presence: ${row.name}`, async () => {
    const probe = row.probe;
    const result = await detectPresence({
      clustersPath: CLUSTERS_PATH,
      receiptPath: RECEIPT_PATH,
      readReceiptFile: () => Promise.resolve(row.receipt),
      readClusters: clustersWith(row.clusters),
      probe,
      // Short enough that the hanging-probe row is a fast test rather than a
      // 1.5-second one, and long enough that a resolved probe always wins.
      probeTimeoutMs: 25,
    });

    assert.equal(result.verdict, row.want);
    assert.deepEqual(result.evidence, row.wantEvidence);
    const calls = (probe as { calls?: string[] }).calls;
    if (calls !== undefined) {
      assert.deepEqual(calls, row.wantProbed);
    }
  });
}

test("INSTALL IS NEVER OFFERED once either source says a cluster is here", () => {
  // The table above is the input side of this; this is the output side, and
  // together they are the acceptance criterion. An install run over an
  // existing cluster rebuilds a k3d stack, a hosts block and a trust-store CA
  // underneath a working one.
  for (const verdict of ["installed-healthy", "installed-unreachable"] as const) {
    const actions = addClusterMenu(verdict).map((c) => c.action);
    assert.ok(!actions.includes("install"), `${verdict} offered an install: ${actions.join(", ")}`);
  }
});

test("the menu matches the table in the issue", () => {
  assert.deepEqual(
    addClusterMenu("absent").map((c) => c.action),
    // Install first: it is the recommended action for a machine with no
    // cluster, and the page renders the first card as the primary one.
    // Guided is its own entry rather than a mode toggle inside the run, because
    // the choice is made before any work starts and changes what the first
    // screen asks for (memql#3471).
    ["install", "installGuided", "connect"]
  );
  assert.deepEqual(
    addClusterMenu("installed-healthy").map((c) => c.action),
    ["connect", "uninstall"]
  );
  assert.deepEqual(
    addClusterMenu("installed-unreachable").map((c) => c.action),
    ["repair", "uninstall", "connect"]
  );
});

test("uninstall is offered exactly when a cluster is here to uninstall", () => {
  // The gap this closes: a local cluster that existed and did not answer could
  // be offered a repair, but there was no way to take it off the machine at
  // all. The substrate has had `cli.js uninstall` since #3357; nothing in the
  // editor could reach it.
  assert.ok(
    !addClusterMenu("absent")
      .map((c) => c.action)
      .includes("uninstall"),
    "nothing is installed, so there is nothing to uninstall",
  );
  for (const verdict of ["installed-healthy", "installed-unreachable"] as const) {
    assert.ok(
      addClusterMenu(verdict)
        .map((c) => c.action)
        .includes("uninstall"),
      `${verdict} must offer an uninstall`,
    );
  }
});

test("repair leads on the verdict that describes a broken cluster", () => {
  // An operator whose cluster is installed and not answering came to the "+"
  // to fix that, not to register a second cluster. Ordering is the only
  // recommendation a card list can make.
  assert.equal(addClusterMenu("installed-unreachable")[0]?.action, "repair");
});

test("every menu item carries a label and a detail", () => {
  for (const verdict of ["absent", "installed-healthy", "installed-unreachable"] as const) {
    for (const choice of addClusterMenu(verdict)) {
      assert.notEqual(choice.label.trim(), "", `${verdict}/${choice.action} has no label`);
      assert.notEqual(choice.detail.trim(), "", `${verdict}/${choice.action} has no detail`);
    }
  }
});

// ---------------------------------------------------------------------------
// evidence edge cases
// ---------------------------------------------------------------------------

test("an UNREADABLE receipt counts as evidence rather than as an empty install", async () => {
  // The direction that cannot destroy anything: a receipt that will not parse
  // cannot rule an install out, and the only action gated on the absence of
  // evidence is the one that installs over whatever is there.
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.reject(new Error("receipt is not JSON")),
    readClusters: NO_CLUSTERS,
    probe: fixedProbe(false),
    probeTimeoutMs: 25,
  });
  assert.equal(result.verdict, "installed-unreachable");
  assert.deepEqual(result.evidence, { receipt: true, registry: false });
});

test("a malformed clusters.yaml yields no registry evidence rather than an error", async () => {
  // The tree already renders that failure as a row of its own; a broken file
  // must not also decide what the "+" offers.
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: () => Promise.resolve({ ok: false, error: "clusters.yaml is malformed" }),
    probe: fixedProbe(true),
    probeTimeoutMs: 25,
  });
  assert.equal(result.verdict, "absent");
  assert.deepEqual(result.evidence, { receipt: false, registry: false });
});

test("a clusters read that THROWS does not reject the detection", async () => {
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: () => Promise.reject(new Error("EACCES")),
    probe: fixedProbe(true),
    probeTimeoutMs: 25,
  });
  assert.equal(result.verdict, "absent");
});

test("a probe that REJECTS is a failed dial, not a failed detection", async () => {
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(installedReceipt()),
    readClusters: NO_CLUSTERS,
    probe: () => Promise.reject(new Error("ECONNREFUSED")),
    probeTimeoutMs: 25,
  });
  assert.equal(result.verdict, "installed-unreachable");
});

test("the local cluster's registry name comes back with the verdict", async () => {
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: clustersWith([LOCAL_ENTRY]),
    probe: fixedProbe(true),
    probeTimeoutMs: 25,
  });
  assert.equal(result.clusterName, "local");
  assert.equal(result.endpoint, LOCAL_ENTRY.endpoint);
});

// ---------------------------------------------------------------------------
// which endpoint gets dialed
// ---------------------------------------------------------------------------

test("the endpoint comes from the registry, then the receipt's domain, then the default", () => {
  assert.equal(probeEndpointFor(LOCAL_ENTRY, installedReceipt()), LOCAL_ENTRY.endpoint);
  assert.equal(
    probeEndpointFor(undefined, installedReceipt([entry({ params: { domain: "local.znas.io" } })])),
    "cockpit.local.znas.io:443"
  );
  assert.equal(probeEndpointFor(undefined, installedReceipt()), DEFAULT_LOCAL_ENDPOINT);
  assert.equal(probeEndpointFor(undefined, null), DEFAULT_LOCAL_ENDPOINT);
  // A registered local cluster with no endpoint names no front door, so the
  // receipt still gets its turn.
  assert.equal(
    probeEndpointFor({ name: "local", endpoint: "  ", local: true }, null),
    DEFAULT_LOCAL_ENDPOINT
  );
});

test("verdictFor is evidence first, reachability second", () => {
  assert.equal(verdictFor({ receipt: false, registry: false }, true), "absent");
  assert.equal(verdictFor({ receipt: false, registry: false }, false), "absent");
  assert.equal(verdictFor({ receipt: true, registry: false }, true), "installed-healthy");
  assert.equal(verdictFor({ receipt: false, registry: true }, false), "installed-unreachable");
});

// ---------------------------------------------------------------------------
// the memo
// ---------------------------------------------------------------------------

function memoFixture(probe: RecordingProbe, now: () => number): ClusterPresence {
  return new ClusterPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(installedReceipt()),
    readClusters: NO_CLUSTERS,
    probe,
    probeTimeoutMs: 25,
    ttlMs: 30_000,
    now,
  });
}

test("the verdict is memoized inside the window and re-probed after it", async () => {
  const probe = fixedProbe(true);
  let clock = 1_000;
  const presence = memoFixture(probe, () => clock);

  assert.equal((await presence.get()).verdict, "installed-healthy");
  assert.equal(probe.calls.length, 1);

  clock += 29_000;
  await presence.get();
  assert.equal(probe.calls.length, 1, "a second look inside the window dialed again");

  clock += 2_000; // now 31s past the first look
  await presence.get();
  assert.equal(probe.calls.length, 2, "the memo outlived its window");
});

test("invalidate() drops the memo, so a completed install is seen immediately", async () => {
  const probe = fixedProbe(true);
  const presence = memoFixture(probe, () => 1_000);

  await presence.get();
  assert.equal(probe.calls.length, 1);
  presence.invalidate();
  await presence.get();
  assert.equal(probe.calls.length, 2);
});

test("two concurrent looks share one dial", async () => {
  // A double click on the "+" is one question, and the probe is the expensive
  // half of answering it.
  const probe = fixedProbe(true);
  const presence = memoFixture(probe, () => 1_000);

  const [a, b] = await Promise.all([presence.get(), presence.get()]);
  assert.equal(a.verdict, "installed-healthy");
  assert.equal(b.verdict, "installed-healthy");
  assert.equal(probe.calls.length, 1);
});

test("a hanging probe does not hang the memo either", async () => {
  const presence = new ClusterPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(installedReceipt()),
    readClusters: NO_CLUSTERS,
    probe: HANGING_PROBE,
    probeTimeoutMs: 25,
  });
  assert.equal((await presence.get()).verdict, "installed-unreachable");
});
