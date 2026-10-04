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
//     never settles still yields a verdict -- the page opens either way.

import test from "node:test";
import assert from "node:assert/strict";

import type { ReadClustersResult } from "../src/clusters/file.js";
import {
  ClusterPresence,
  DEFAULT_LOCAL_ENDPOINT,
  detectPresence,
  PROBE_TIMEOUT_MS,
  PROBE_TRIES,
  probeEndpointFor,
  verdictFor,
  type PresenceEvidence,
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
  endpoint: "api.memql.localhost:443",
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
  wantEvidence: PresenceEvidence;
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
    wantEvidence: { receipt: false, registry: false, liveCluster: false },
    // The verdict is `absent` however the dial goes, so there is nothing to
    // learn from it -- and the operator with no cluster is the one who least
    // deserves a network round trip in front of their page.
    wantProbed: [],
  },
  {
    name: "a receipt whose entries are empty is not evidence: it installed nothing",
    receipt: installedReceipt([]),
    clusters: [],
    probe: fixedProbe(true),
    want: "absent",
    wantEvidence: { receipt: false, registry: false, liveCluster: false },
    wantProbed: [],
  },
  {
    name: "a non-local registry entry is not evidence: ABSENT MEANS NOT LOCAL",
    receipt: null,
    // No `local` flag at all -- the shape of every cluster registered before
    // that field existed, and of every staging/production cluster since.
    clusters: [{ name: "staging", endpoint: "api.example.com:443" }],
    probe: fixedProbe(true),
    want: "absent",
    wantEvidence: { receipt: false, registry: false, liveCluster: false },
    wantProbed: [],
  },
  {
    name: "RECEIPT ONLY + the endpoint answers is INSTALLED-HEALTHY",
    receipt: installedReceipt(),
    clusters: [],
    probe: fixedProbe(true),
    want: "installed-healthy",
    wantEvidence: { receipt: true, registry: false, liveCluster: false },
    wantProbed: [DEFAULT_LOCAL_ENDPOINT],
  },
  {
    name: "RECEIPT ONLY + the dial fails is INSTALLED-UNREACHABLE",
    receipt: installedReceipt(),
    clusters: [],
    probe: fixedProbe(false),
    want: "installed-unreachable",
    wantEvidence: { receipt: true, registry: false, liveCluster: false },
    // One miss is not a verdict: the probe is tried once more first.
    wantProbed: [DEFAULT_LOCAL_ENDPOINT, DEFAULT_LOCAL_ENDPOINT],
  },
  {
    name: "REGISTRY ONLY + the endpoint answers is INSTALLED-HEALTHY (the `make up` cluster)",
    receipt: null,
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(true),
    want: "installed-healthy",
    wantEvidence: { receipt: false, registry: true, liveCluster: false },
    wantProbed: [LOCAL_ENTRY.endpoint],
  },
  {
    name: "REGISTRY ONLY + the dial fails is INSTALLED-UNREACHABLE",
    receipt: null,
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(false),
    want: "installed-unreachable",
    wantEvidence: { receipt: false, registry: true, liveCluster: false },
    wantProbed: [LOCAL_ENTRY.endpoint, LOCAL_ENTRY.endpoint],
  },
  {
    name: "BOTH sources + the endpoint answers is INSTALLED-HEALTHY",
    receipt: installedReceipt(),
    clusters: [LOCAL_ENTRY],
    probe: fixedProbe(true),
    want: "installed-healthy",
    wantEvidence: { receipt: true, registry: true, liveCluster: false },
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
    wantEvidence: { receipt: true, registry: true, liveCluster: false },
    wantProbed: [LOCAL_ENTRY.endpoint, LOCAL_ENTRY.endpoint],
  },
  {
    name: "a probe that NEVER ANSWERS degrades to INSTALLED-UNREACHABLE",
    receipt: installedReceipt(),
    clusters: [LOCAL_ENTRY],
    probe: HANGING_PROBE,
    want: "installed-unreachable",
    wantEvidence: { receipt: true, registry: true, liveCluster: false },
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

// -----------------------------------------------------------------------------
// THE FOURTH SIGNAL (memql#5118, D8)
// -----------------------------------------------------------------------------
//
// The uninstall refuses to delete a k3d cluster it did not create -- correctly
// -- classes that refusal `preserved`, and `ok` ignores preservations. So the
// wizard deleted the receipt, presence read `absent` because it never asked
// k3d, Install was offered again, and it adopted the same database. The
// operator was told everything had been taken back and then handed their own
// data back under a fresh install. Every step was individually correct.

/** detectPresence with nothing on disk: the path the fourth signal is on. */
function bareMachine(listClusters?: () => Promise<string[]>) {
  return detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: clustersWith([]),
    probe: fixedProbe(true),
    probeTimeoutMs: 25,
    ...(listClusters ? { listClusters } : {}),
  });
}

test("a live cluster with no receipt and no registry row reads PRESENT-UNRECEIPTED", async () => {
  const result = await bareMachine(async () => ["memql"]);
  assert.equal(result.verdict, "present-unreceipted");
  assert.equal(result.evidence.liveCluster, true);
  // Nothing was dialed: there is no endpoint to dial, which is exactly why
  // this signal costs no round trip on top of another.
  assert.equal(result.endpoint, "");
});

test("a cluster by another name is somebody else's project", async () => {
  const result = await bareMachine(async () => ["k3s-default", "someone-elses"]);
  assert.equal(result.verdict, "absent");
  assert.equal(result.evidence.liveCluster, false);
});

test("an empty listing is still ABSENT", async () => {
  const result = await bareMachine(async () => []);
  assert.equal(result.verdict, "absent");
});

test("a listing that THROWS answers the same as an empty one", async () => {
  // k3d may not be installed and Docker may be down, and neither is evidence
  // of a cluster. The direction that cannot destroy anything is the one to
  // fail in -- and here that means keeping Install offered on the machine that
  // genuinely has nothing.
  const result = await bareMachine(async () => {
    throw new Error("k3d: command not found");
  });
  assert.equal(result.verdict, "absent");
  assert.equal(result.evidence.liveCluster, false);
});

test("no listener at all is the same again, so an older caller is unchanged", async () => {
  const result = await bareMachine();
  assert.equal(result.verdict, "absent");
});

test("THE RECEIPT OUTRANKS THE LISTING, and is never asked for a second opinion", async () => {
  // A cluster this installer DID create reads installed-healthy even though
  // k3d would list it too: the receipt is what makes repair, rebuild and
  // uninstall mean anything, and demoting it would take those acts away.
  let asked = 0;
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(installedReceipt()),
    readClusters: clustersWith([]),
    probe: fixedProbe(true),
    probeTimeoutMs: 25,
    listClusters: async () => {
      asked += 1;
      return ["memql"];
    },
  });
  assert.equal(result.verdict, "installed-healthy");
  // NOT ASKED AT ALL. Property 2 of this module is that drawing the landing must
  // not wait on `k3d cluster list`; the one exception is the path that would
  // otherwise offer to install over an existing cluster, and this is not it.
  assert.equal(asked, 0);
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
  assert.deepEqual(result.evidence, { receipt: true, registry: false, liveCluster: false });
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
  assert.deepEqual(result.evidence, { receipt: false, registry: false, liveCluster: false });
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
    probeEndpointFor(undefined, installedReceipt([entry({ params: { domain: "memql.localhost" } })])),
    "api.memql.localhost:443"
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
  assert.equal(verdictFor({ receipt: false, registry: false, liveCluster: false }, true), "absent");
  assert.equal(verdictFor({ receipt: false, registry: false, liveCluster: false }, false), "absent");
  assert.equal(verdictFor({ receipt: true, registry: false, liveCluster: false }, true), "installed-healthy");
  assert.equal(verdictFor({ receipt: false, registry: true, liveCluster: false }, false), "installed-unreachable");
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

// -----------------------------------------------------------------------------
// A receipt that names no artifact is not an install (memql#3544)
// -----------------------------------------------------------------------------

test("a receipt whose steps left NO artifact is not evidence of an install", async () => {
  // THE TRAP THIS OPENS, and it is a dead end rather than a cosmetic slip.
  //
  // An install that dies at its first mutating step still records the read-only
  // steps that ran ahead of it -- `detect`, which only inspects the machine, and
  // `providerKey`, which verifies a credential and creates nothing. Both carry
  // `receipt: ""`, because that field names the ARTIFACT class a step left
  // behind and neither leaves one.
  //
  // Counting entries rather than artifacts read that as "a cluster is installed
  // here". The consequences compound: the Install card is withdrawn (it is
  // offered for `absent` and nothing else), so the operator cannot retry the
  // install that just failed; Repair is offered instead and re-runs the same
  // graph into the same failure; and Uninstall correctly reports there is
  // nothing to remove, because there is not. Three screens, no way forward.
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () =>
      Promise.resolve(
        installedReceipt([
          entry({ stepId: "detect", script: "install.detect", receipt: "", changed: false }),
          entry({
            stepId: "providerKey",
            script: "install.verifyProviderKey",
            receipt: "",
            changed: false,
          }),
        ]),
      ),
    readClusters: NO_CLUSTERS,
    probe: fixedProbe(false),
    probeTimeoutMs: 25,
  });

  assert.equal(result.verdict, "absent", "nothing was left on this machine to protect");
  assert.deepEqual(result.evidence, { receipt: false, registry: false, liveCluster: false });
});

test("an artifact a step FOUND already present is still evidence", async () => {
  // The counterpart, and the reason the test is on the artifact rather than on
  // `changed`. A step that finds docker already installed records
  // `preExisting: true, changed: false` -- it changed nothing, and the artifact
  // is on the machine all the same. Reading that as "absent" would offer to
  // install over a cluster that is already there, which is the one direction
  // this module exists to make impossible.
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () =>
      Promise.resolve(
        installedReceipt([
          entry({ stepId: "clusterUp", receipt: "stack", preExisting: true, changed: false }),
        ]),
      ),
    readClusters: NO_CLUSTERS,
    probe: fixedProbe(false),
    probeTimeoutMs: 25,
  });

  assert.equal(result.verdict, "installed-unreachable");
  assert.deepEqual(result.evidence, { receipt: true, registry: false, liveCluster: false });
});

// -----------------------------------------------------------------------------
// One miss is not a verdict, and a miss says why
// -----------------------------------------------------------------------------
//
// THE FIELD FAILURE: the Deployments view said "not answering" for a front
// door that answered a shell in 42 ms. The first TLS handshake from the
// extension host can be slow, the budget was 1.5 s, and the reason was logged
// nowhere.

test("the probe budget is about four seconds, tried twice", () => {
  assert.equal(PROBE_TIMEOUT_MS, 4_000);
  assert.equal(PROBE_TRIES, 2);
});

test("a first miss is tried again, and a second-try answer is healthy", async () => {
  let calls = 0;
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: clustersWith([LOCAL_ENTRY]),
    probe: async () => {
      calls += 1;
      return calls === 2;
    },
    probeTimeoutMs: 25,
  });
  assert.equal(result.verdict, "installed-healthy");
  assert.equal(calls, 2);
});

test("the reason for a miss is reported, once, after the last try", async () => {
  const failures: Array<[string, string]> = [];
  await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: clustersWith([LOCAL_ENTRY]),
    probe: async () => ({ answered: false, reason: "connect ECONNREFUSED 127.0.0.1:443" }),
    probeTimeoutMs: 25,
    onProbeFailure: (endpoint, reason) => failures.push([endpoint, reason]),
  });
  assert.deepEqual(failures, [[LOCAL_ENTRY.endpoint, "connect ECONNREFUSED 127.0.0.1:443"]]);
});

test("a probe that never answers is reported as a timeout", async () => {
  const failures: string[] = [];
  await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: clustersWith([LOCAL_ENTRY]),
    probe: HANGING_PROBE,
    probeTimeoutMs: 25,
    onProbeFailure: (_endpoint, reason) => failures.push(reason),
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0] ?? "", /no answer within 25 ms/);
});

test("with no receipt, the version is the one clusters.yaml records", async () => {
  // A cluster built with `make up`: no receipt, and a version the registry
  // learned. The Deployments view used to say "unknown" for it.
  const result = await detectPresence({
    clustersPath: CLUSTERS_PATH,
    receiptPath: RECEIPT_PATH,
    readReceiptFile: () => Promise.resolve(null),
    readClusters: clustersWith([{ ...LOCAL_ENTRY, version: "main" }]),
    probe: fixedProbe(true),
    probeTimeoutMs: 25,
  });
  assert.equal(result.version, "main");
});
