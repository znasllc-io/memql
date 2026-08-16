// Learning what release a cluster is running, and deciding whether to record it.
//
// Four sources can answer, and they disagree structurally rather than
// occasionally, because none of them was built for this question:
//
//   receipt        what the install wizard checked out (`recordedStackTag`).
//                  A genuine release tag, but a record of ONE MOMENT -- the
//                  cluster may have been moved since by other means.
//   argocd         the `argocdTargetRevision` that `scripts/k3d/status.sh`
//                  publishes in its capability envelope. Legitimately a BRANCH
//                  NAME: that script says so in as many words, and memql#3817
//                  was precisely that fact outliving the branch.
//   deployControl  `GetDeploymentStatus.version` / `engine_version`, from the
//                  resolved lockfile. Absent for a local cluster.
//   live           `memqlVersion()` over a connection. The most DIRECT
//                  evidence and ranked LAST anyway, because today it returns
//                  the `0.15.0-<epoch>` stamp `Dockerfile:178-180` writes over
//                  the VERSION file. memql#3998 is what makes it honest; this
//                  module needs no change when it does, because the quality
//                  rule below already lets a real release through.
//
// So the rule cannot be "the newest observation wins". It is:
//
//   1. a value that NAMES A RELEASE beats one that does not,
//   2. then the more trustworthy source wins,
//   3. and a value that does not name a release NEVER overwrites one that does.
//
// Rule 3 is the one this module exists for. A k3d cluster whose ArgoCD tracks
// `main` reports the word "main" on every single refresh. Letting that land
// would destroy the `v0.18.0` the install receipt established -- and with it
// the only thing on disk that can answer "is this cluster behind?", which is
// the whole point of recording a version at all.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { Latest, type LatestToken } from "../async/latest.js";
import type { ClusterConfig } from "../clusters/model.js";
import { isReleaseShaped } from "./compare.js";

export type VersionSource = "receipt" | "argocd" | "deployControl" | "live";

export interface VersionCandidate {
  readonly source: VersionSource;
  /** What the source said. Empty means it had nothing to say. */
  readonly value: string;
}

// Most trustworthy first. See the header for why `live` is last despite being
// the most direct evidence.
const SOURCE_RANK: readonly VersionSource[] = ["receipt", "argocd", "deployControl", "live"];

function rankOf(source: VersionSource): number {
  const i = SOURCE_RANK.indexOf(source);
  return i < 0 ? SOURCE_RANK.length : i;
}

/**
 * The best of several simultaneous answers, or undefined when none said
 * anything.
 *
 * Blank values are dropped before anything else. Every collector reports "I
 * learned nothing" as an empty string, and an empty value reaching
 * `upsertCluster` would DELETE the record rather than leave it alone -- the
 * one write here that is actively destructive.
 */
export function chooseCandidate(
  candidates: readonly VersionCandidate[],
): VersionCandidate | undefined {
  const usable = candidates
    .map((c) => ({ source: c.source, value: c.value.trim() }))
    .filter((c) => c.value !== "");
  if (usable.length === 0) return undefined;

  // Quality first, then trust. A branch name from the most trusted source is
  // still not a release, and a real tag from a lesser one is the better answer.
  return usable.sort((a, b) => {
    const quality = Number(isReleaseShaped(b.value)) - Number(isReleaseShaped(a.value));
    return quality !== 0 ? quality : rankOf(a.source) - rankOf(b.source);
  })[0];
}

export interface VersionDecision {
  readonly write: boolean;
  readonly value?: string;
  readonly source?: VersionSource;
  /** Why. Carried so a refusal is explainable rather than a silent no-op. */
  readonly reason: string;
}

/**
 * Whether what we learned should be written over what is recorded.
 *
 * Note what is NOT a reason to refuse: an older release. A rollback is a real
 * thing that happens, and declining to record it would leave the file
 * asserting a release the cluster is no longer running. Only a loss of RECORD
 * QUALITY is refused -- a value that does not name a release, landing on one
 * that does.
 */
export function decideVersionWrite(
  recorded: string | undefined,
  candidates: readonly VersionCandidate[],
): VersionDecision {
  const chosen = chooseCandidate(candidates);
  if (chosen === undefined) {
    return { write: false, reason: "no source reported a version" };
  }
  if (chosen.value === (recorded ?? "").trim()) {
    // Not an optimisation. Every write is a read-modify-write of a file the
    // memQL Cockpit also writes, so a no-op write is a real chance to lose a
    // concurrent edit in exchange for nothing.
    return { write: false, reason: `already recorded as ${chosen.value}` };
  }
  if (isReleaseShaped(recorded) && !isReleaseShaped(chosen.value)) {
    return {
      write: false,
      reason: `${chosen.source} reported "${chosen.value}", which does not name a release; keeping the recorded ${recorded}`,
    };
  }
  return {
    write: true,
    value: chosen.value,
    source: chosen.source,
    reason: `${chosen.source} reported ${chosen.value}`,
  };
}

export interface RefresherDeps {
  /** Ask every source. Must resolve; a source that fails contributes nothing. */
  collect: (cluster: ClusterConfig) => Promise<VersionCandidate[]>;
  /** Write through `upsertCluster`. */
  write: (name: string, version: string) => Promise<void>;
}

/**
 * Refreshes a cluster's recorded version, opportunistically.
 *
 * PER-CLUSTER supersession. The guard is keyed by cluster name rather than
 * held once for the refresher, because the tree refreshes every row at the
 * same time: a single shared guard would mean starting staging's refresh
 * silently cancelled local's, which is the common case rather than an edge one.
 *
 * NOTHING HERE THROWS. A refresh is background work triggered by a surface
 * being looked at -- every source is a subprocess, an RPC or a socket, and any
 * of them can fail on a plane. A failure is a version we did not learn, never
 * an error in front of an operator.
 */
export class ClusterVersionRefresher {
  private readonly guards = new Map<string, Latest<"clusterVersion">>();

  constructor(private readonly deps: RefresherDeps) {}

  private guardFor(name: string): Latest<"clusterVersion"> {
    let guard = this.guards.get(name);
    if (guard === undefined) {
      guard = new Latest<"clusterVersion">();
      this.guards.set(name, guard);
    }
    return guard;
  }

  async refresh(cluster: ClusterConfig): Promise<VersionDecision> {
    const guard = this.guardFor(cluster.name);
    const token: LatestToken<"clusterVersion"> = guard.begin();

    let candidates: VersionCandidate[];
    try {
      candidates = await this.deps.collect(cluster);
    } catch (err) {
      return {
        write: false,
        reason: `could not read any version source: ${err instanceof Error ? err.message : String(err)}`,
      };
    }

    // The world may have moved while the sources were being asked -- the
    // operator edited the cluster, or a newer refresh started. Writing now
    // would land a stale answer on top of a current one.
    if (!guard.isCurrent(token)) {
      return { write: false, reason: "superseded by a newer refresh" };
    }

    const decision = decideVersionWrite(cluster.version, candidates);
    if (!decision.write || decision.value === undefined) return decision;

    try {
      await this.deps.write(cluster.name, decision.value);
    } catch {
      // Best-effort: clusters.yaml may be read-only or on a volume that went
      // away. The decision stands and the next refresh will try again.
    }
    return decision;
  }
}
