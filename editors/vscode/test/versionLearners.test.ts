// Version-learner tests (memql#3993).
//
// Four sources can say what release a cluster is running, and they disagree --
// not occasionally, but structurally, because none of them was built to answer
// this question:
//
//   receipt       what the wizard checked out. A real release tag, but a record
//                 of one moment; the cluster may have moved since.
//   argocd        the targetRevision being reconciled. Legitimately a BRANCH
//                 NAME -- `scripts/k3d/status.sh` says so in as many words, and
//                 memql#3817 was exactly that fact outliving the branch.
//   deployControl the resolved lockfile's version. Absent for a local cluster.
//   live          `memqlVersion()`. Returns the Dockerfile's build stamp today,
//                 which is why it is ranked LAST despite being the most direct
//                 evidence -- memql#3998 is what will make it honest.
//
// So the rule cannot be "newest observation wins". It is: prefer a value that
// names a release, then prefer the more trustworthy source, and NEVER let a
// value that does not name a release overwrite one that does. Without that last
// clause, a k3d cluster whose ArgoCD tracks `main` would have its recorded
// v0.18.0 replaced by the word "main" on the next refresh -- losing the only
// thing on disk that could answer the operator's question.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ClusterVersionRefresher,
  chooseCandidate,
  decideVersionWrite,
  type VersionCandidate,
} from "../src/version/learners.js";
import type { ClusterConfig } from "../src/clusters/model.js";

const cluster = (over: Partial<ClusterConfig> = {}): ClusterConfig => ({
  name: "local",
  endpoint: "api.memql.localhost:443",
  ...over,
});

// --- Choosing between simultaneous candidates -------------------------------

test("a release-shaped candidate beats a non-release-shaped one from a better source", () => {
  // Quality outranks trust. The receipt is the most trusted source, but a
  // branch name from it is still not a release, and the deploy record's real
  // tag is the better answer.
  const chosen = chooseCandidate([
    { source: "receipt", value: "main" },
    { source: "deployControl", value: "v0.18.0" },
  ]);
  assert.deepEqual(chosen, { source: "deployControl", value: "v0.18.0" });
});

test("among release-shaped candidates the more trustworthy source wins", () => {
  const chosen = chooseCandidate([
    { source: "live", value: "v0.17.0" },
    { source: "receipt", value: "v0.18.0" },
    { source: "argocd", value: "v0.17.1" },
  ]);
  assert.deepEqual(chosen, { source: "receipt", value: "v0.18.0" });
});

test("the source ranking is receipt, argocd, deployControl, live", () => {
  // live is LAST despite being the most direct evidence, because today it
  // reports the build stamp. memql#3998 makes it honest; the quality rule
  // already lets it through once it is.
  const all: VersionCandidate[] = [
    { source: "live", value: "v0.1.0" },
    { source: "deployControl", value: "v0.2.0" },
    { source: "argocd", value: "v0.3.0" },
    { source: "receipt", value: "v0.4.0" },
  ];
  assert.equal(chooseCandidate(all)?.source, "receipt");
  assert.equal(chooseCandidate(all.filter((c) => c.source !== "receipt"))?.source, "argocd");
  assert.equal(
    chooseCandidate(all.filter((c) => c.source !== "receipt" && c.source !== "argocd"))?.source,
    "deployControl",
  );
});

test("among only non-release-shaped candidates the ranking still applies", () => {
  const chosen = chooseCandidate([
    { source: "live", value: "0.15.0-1737072000" },
    { source: "argocd", value: "main" },
  ]);
  assert.deepEqual(chosen, { source: "argocd", value: "main" });
});

test("blank and whitespace-only candidates are not candidates", () => {
  // Every collector reports "I learned nothing" as an empty string, and an
  // empty value written through upsertCluster would DELETE the record.
  assert.equal(chooseCandidate([{ source: "receipt", value: "" }]), undefined);
  assert.equal(chooseCandidate([{ source: "receipt", value: "   " }]), undefined);
  assert.deepEqual(chooseCandidate([{ source: "receipt", value: "" }, { source: "live", value: "v1.0.0" }]), {
    source: "live",
    value: "v1.0.0",
  });
});

test("no candidates at all is undefined, not an error", () => {
  assert.equal(chooseCandidate([]), undefined);
});

// --- The write decision -----------------------------------------------------

test("a first release-shaped value is written", () => {
  const d = decideVersionWrite(undefined, [{ source: "receipt", value: "v0.18.0" }]);
  assert.equal(d.write, true);
  assert.equal(d.value, "v0.18.0");
  assert.equal(d.source, "receipt");
});

test("a first NON-release-shaped value is still written when nothing is recorded", () => {
  // A branch name is a poor record, but it is strictly better than no record:
  // the comparison module reports it as notComparable, which is honest, and
  // the surfaces render "unknown" rather than pretending.
  const d = decideVersionWrite(undefined, [{ source: "argocd", value: "main" }]);
  assert.equal(d.write, true);
  assert.equal(d.value, "main");
});

test("an unchanged value is not written", () => {
  // Not an optimisation. Every write is a read-modify-write of a file the
  // cockpit also writes, so a no-op write is a real chance to lose a
  // concurrent edit for nothing.
  const d = decideVersionWrite("v0.18.0", [{ source: "receipt", value: "v0.18.0" }]);
  assert.equal(d.write, false);
});

test("a differing release-shaped value replaces a release-shaped record", () => {
  // The upgrade case: the cluster genuinely moved.
  const d = decideVersionWrite("v0.17.0", [{ source: "receipt", value: "v0.18.0" }]);
  assert.equal(d.write, true);
  assert.equal(d.value, "v0.18.0");
});

test("an OLDER release-shaped value still replaces a newer record", () => {
  // A downgrade of the VERSION is not a downgrade of record QUALITY. Clusters
  // do get rolled back, and refusing to record it would leave the file
  // asserting a release that is no longer running.
  const d = decideVersionWrite("v0.18.0", [{ source: "receipt", value: "v0.17.0" }]);
  assert.equal(d.write, true);
  assert.equal(d.value, "v0.17.0");
});

test("a NON-release-shaped value never overwrites a release-shaped record", () => {
  // THE RULE THIS MODULE EXISTS FOR. A k3d cluster whose ArgoCD tracks `main`
  // reports "main" on every refresh. Letting that land would destroy the
  // v0.18.0 the receipt established, and with it the only thing on disk that
  // can answer "is this cluster behind".
  for (const value of ["main", "feat/x", "0.15.0-1737072000", "a256ab11", "v1"]) {
    const d = decideVersionWrite("v0.18.0", [{ source: "live", value }]);
    assert.equal(d.write, false, `${value} must not overwrite a recorded release`);
  }
});

test("a non-release-shaped value MAY replace another non-release-shaped record", () => {
  // Neither is a release, so no quality is lost, and the newer observation is
  // the better description of the cluster right now.
  const d = decideVersionWrite("main", [{ source: "argocd", value: "feat/upgrade" }]);
  assert.equal(d.write, true);
  assert.equal(d.value, "feat/upgrade");
});

test("no candidates leaves the record alone", () => {
  // The ordinary offline case. Undefined must NOT reach upsertCluster as "",
  // which would delete a version a better source established.
  const d = decideVersionWrite("v0.18.0", []);
  assert.equal(d.write, false);
  assert.equal(d.value, undefined);
});

test("every decision carries a reason", () => {
  // The reasons are what makes a refusal explainable in a log rather than a
  // silent no-op that looks like the feature not working.
  assert.match(decideVersionWrite("v0.18.0", []).reason, /\S/);
  assert.match(decideVersionWrite("v0.18.0", [{ source: "live", value: "main" }]).reason, /\S/);
  assert.match(decideVersionWrite("v0.18.0", [{ source: "live", value: "v0.18.0" }]).reason, /\S/);
  assert.match(decideVersionWrite(undefined, [{ source: "live", value: "v0.18.0" }]).reason, /\S/);
});

// --- The refresher: collection, supersession, and the write -----------------

function refresher(
  collected: Record<string, string>,
  writes: Array<{ name: string; version: string }>,
  opts: { collect?: () => Promise<VersionCandidate[]> } = {},
): ClusterVersionRefresher {
  return new ClusterVersionRefresher({
    collect:
      opts.collect ??
      (async () =>
        Object.entries(collected).map(([source, value]) => ({
          source: source as VersionCandidate["source"],
          value,
        }))),
    write: async (name, version) => {
      writes.push({ name, version });
    },
  });
}

test("a learned version is written through the writer", async () => {
  const writes: Array<{ name: string; version: string }> = [];
  const r = refresher({ receipt: "v0.18.0" }, writes);
  await r.refresh(cluster());
  assert.deepEqual(writes, [{ name: "local", version: "v0.18.0" }]);
});

test("a refused decision writes nothing at all", async () => {
  const writes: Array<{ name: string; version: string }> = [];
  const r = refresher({ argocd: "main" }, writes);
  await r.refresh(cluster({ version: "v0.18.0" }));
  assert.deepEqual(writes, []);
});

test("an unchanged value writes nothing", async () => {
  const writes: Array<{ name: string; version: string }> = [];
  const r = refresher({ receipt: "v0.18.0" }, writes);
  await r.refresh(cluster({ version: "v0.18.0" }));
  assert.deepEqual(writes, []);
});

test("a superseded refresh does not write, even when it lands LAST", async () => {
  // The guard `src/async/latest.ts` exists for. Two refreshes overlap, and the
  // STALE one finishes second -- the ordering that makes supersession actually
  // matter, because last-write-wins would otherwise leave the older answer on
  // disk. The newer refresh's value must survive.
  const writes: Array<{ name: string; version: string }> = [];
  const releases: Array<(c: VersionCandidate[]) => void> = [];
  const r = refresher({}, writes, {
    collect: () =>
      new Promise<VersionCandidate[]>((resolve) => {
        releases.push(resolve);
      }),
  });

  const stale = r.refresh(cluster());
  const current = r.refresh(cluster());
  assert.equal(releases.length, 2);
  // The current refresh answers first, the superseded one second.
  releases[1]?.([{ source: "receipt", value: "v0.18.0" }]);
  releases[0]?.([{ source: "receipt", value: "v0.17.0" }]);
  await Promise.all([stale, current]);

  assert.deepEqual(
    writes,
    [{ name: "local", version: "v0.18.0" }],
    "the superseded refresh must not write, even arriving last",
  );
});

test("refreshes for DIFFERENT clusters do not supersede each other", async () => {
  // The guard is per cluster. A global one would mean refreshing staging
  // silently cancelled the refresh of local, which is the common case: the
  // tree refreshes every row at once.
  const writes: Array<{ name: string; version: string }> = [];
  const r = refresher({ receipt: "v0.18.0" }, writes);
  await Promise.all([r.refresh(cluster({ name: "local" })), r.refresh(cluster({ name: "staging" }))]);
  assert.deepEqual(
    writes.map((w) => w.name).sort(),
    ["local", "staging"],
  );
});

test("a collector that throws does not take the refresh down", async () => {
  // Every source is a subprocess, an RPC or a socket. A refresh is
  // opportunistic background work and must never surface as an error.
  const writes: Array<{ name: string; version: string }> = [];
  const r = refresher({}, writes, {
    collect: async () => {
      throw new Error("git is not installed");
    },
  });
  const decision = await r.refresh(cluster({ version: "v0.18.0" }));
  assert.equal(decision.write, false);
  assert.deepEqual(writes, []);
});

test("a write that fails does not throw at the caller", async () => {
  // clusters.yaml may be read-only, or on a disconnected volume. The refresh
  // is best-effort.
  const r = new ClusterVersionRefresher({
    collect: async () => [{ source: "receipt", value: "v0.18.0" }],
    write: async () => {
      throw new Error("EACCES");
    },
  });
  const decision = await r.refresh(cluster());
  assert.equal(decision.write, true, "the decision stands even though the write failed");
});

test("the refresher never writes an empty version", async () => {
  // The one write that would be actively destructive: "" is a DELETE through
  // upsertCluster's documented string semantics.
  const writes: Array<{ name: string; version: string }> = [];
  const r = refresher({ receipt: "", argocd: "  " }, writes);
  await r.refresh(cluster({ version: "v0.18.0" }));
  assert.deepEqual(writes, []);
});
