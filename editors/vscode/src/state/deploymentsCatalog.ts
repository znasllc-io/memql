// What the Deployments tree renders: every instance, and the runs beneath it.
//
// The tree itself is a mapping onto VS Code's TreeItem vocabulary and nothing
// else. Everything that is a DECISION -- which instances exist, which runs
// belong to which, what a row says, which icon it carries -- is here, where it
// runs under bare `node --test` with no workbench, no cluster and no network.
// That division is enforced mechanically (cmd/memql-lsp/vscodeimportrule_test.go)
// and it is the only reason the "renders on a fresh machine" acceptance is
// checkable at all: the failure it guards against is a panel that goes blank,
// which no unit test of a VS Code adapter could see.
//
// NOTHING HERE THROWS. Every input is a file that may not exist, a file that
// may not parse, or a cluster that may not answer, and the alternative to a
// row is an empty panel that tells an operator nothing. Each failure has a
// stated direction:
//
//   - clusters.yaml unreadable -> the synthetic error row, exactly as the
//     Clusters tree already does. A rejection reaching VS Code's tree API has
//     nowhere to be shown.
//   - the receipt unreadable   -> version unknown, which renders as the WORD.
//   - presence undeterminable  -> `installed-unreachable`, never `absent`.
//     `absent` is the one verdict that offers an INSTALL, and an install run
//     over a cluster that already exists rebuilds a k3d cluster, a hosts block
//     and a trust-store CA underneath it. Failing toward "something is here"
//     is the direction that cannot destroy anything -- the same call
//     clusters/presence.ts makes for an unreadable receipt.
//   - the run log unreadable   -> no runs, which is what an instance with no
//     runs renders as anyway, and that is not an empty state.
//
// Refs: #3737 #3733

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readClustersFileSafe } from "../clusters/file.js";
import type { PresenceResult, PresenceVerdict } from "../clusters/presence.js";
import { defaultReceiptPath, readReceipt, type Receipt } from "../install/receipt.js";
import {
  LOCAL_INSTANCE_NAME,
  displayVersion,
  localInstance,
  remoteInstance,
  runsFromDeployments,
  sortInstances,
  type Instance,
  type Run,
} from "./deployments.js";
import {
  currentDeploymentId,
  projectDeployments,
  projectNodeSpecs,
} from "./deploymentHistory.js";
import { defaultRunsDir, listRuns } from "./runLog.js";

/** The live connection, as far as this view cares. */
export interface ConnectionFacts {
  clusterName: string;
  connected: boolean;
}

export interface CatalogInputs {
  clustersPath: string;
  receiptPath?: string;
  runsDir?: string;
  /** ClusterPresence.get, bound. */
  presence: () => Promise<PresenceResult>;
  connection?: ConnectionFacts;
  /**
   * Reads the CONNECTED cluster's `v1:cluster:deployment` and
   * `deploymentNodeSpec` rows. Absent when nothing is connected -- and that is
   * the ordinary case for every remote instance except one, because the
   * extension holds at most one connection at a time. Those instances render
   * with their actions and no children, which is not an empty state.
   */
  readDeployments?: () => Promise<{ deployments: Row[]; specs: Row[] }>;
  readClusters?: (file: string) => ReturnType<typeof readClustersFileSafe>;
  readReceiptFile?: (file: string) => Promise<Receipt | null>;
  listRunsIn?: (dir: string) => Promise<Run[]>;
}

export interface Catalog {
  instances: Instance[];
  /** Instance name -> its runs, newest first. */
  runs: Map<string, Run[]>;
  /** Set when clusters.yaml could not be read; rendered as the sole row. */
  error?: string;
}

/**
 * Everything the tree draws, in one pass.
 *
 * One pass rather than a second async call per expanded instance, because the
 * connected cluster's deployment rows answer TWO questions -- the instance's
 * current version and its run list -- and reading them twice would let the two
 * disagree about the same cluster in the same frame.
 */
export async function buildCatalog(inputs: CatalogInputs): Promise<Catalog> {
  const readClusters = inputs.readClusters ?? readClustersFileSafe;
  const registry = await readClusters(inputs.clustersPath).catch((err: unknown) => ({
    ok: false as const,
    error: (err as Error).message,
  }));

  const [presence, receipt, localRunList, remote] = await Promise.all([
    resolvePresence(inputs),
    resolveReceipt(inputs),
    resolveLocalRuns(inputs),
    resolveRemote(inputs),
  ]);

  if (!registry.ok) {
    return { instances: [], runs: new Map(), error: registry.error };
  }

  const registered = registry.file.clusters.find((c) => c.local === true);
  const connection = inputs.connection;
  const instances: Instance[] = [
    localInstance({
      presence,
      receipt,
      ...(registered !== undefined
        ? { registered: { name: registered.name, domain: registered.domain } }
        : {}),
      connected:
        connection?.connected === true &&
        registered !== undefined &&
        connection.clusterName === registered.name,
    }),
  ];

  const runs = new Map<string, Run[]>();
  // EVERY run in the log belongs to the local instance, whatever name the
  // instance currently carries. The log is per-machine and there is one local
  // install per machine (the receipt path, the hosts block and the k3d cluster
  // name are each singular), so filtering by name would silently drop the runs
  // recorded before the operator registered the cluster and gave it a name.
  runs.set(instances[0].name, localRunList);

  for (const cluster of registry.file.clusters) {
    if (cluster.local === true) continue;
    const isConnected = connection?.connected === true && connection.clusterName === cluster.name;
    const instance = remoteInstance({
      name: cluster.name,
      ...(cluster.domain !== undefined ? { domain: cluster.domain } : {}),
      // Reachability is only ever KNOWN for the cluster we hold a connection
      // to. For the rest the honest verdict is the one that says it does not
      // answer, because as far as this editor can tell, it does not.
      reachable: isConnected,
      connected: isConnected,
      ...(isConnected && remote !== undefined
        ? { deployments: remote.records, currentDeploymentId: remote.currentId }
        : {}),
    });
    instances.push(instance);
    if (isConnected && remote !== undefined) {
      runs.set(
        instance.name,
        runsFromDeployments({
          instance: instance.name,
          deployments: remote.records,
          specs: remote.specs,
        }),
      );
    }
  }

  return { instances: sortInstances(instances), runs };
}

async function resolvePresence(inputs: CatalogInputs): Promise<PresenceVerdict> {
  try {
    return (await inputs.presence()).verdict;
  } catch {
    // See the header: never `absent`, because `absent` is what offers an install.
    return "installed-unreachable";
  }
}

async function resolveReceipt(inputs: CatalogInputs): Promise<Receipt | null> {
  const read = inputs.readReceiptFile ?? readReceipt;
  try {
    return await read(inputs.receiptPath ?? defaultReceiptPath());
  } catch {
    return null;
  }
}

async function resolveLocalRuns(inputs: CatalogInputs): Promise<Run[]> {
  const list = inputs.listRunsIn ?? listRuns;
  try {
    return await list(inputs.runsDir ?? defaultRunsDir());
  } catch {
    return [];
  }
}

async function resolveRemote(
  inputs: CatalogInputs,
): Promise<{ records: ReturnType<typeof projectDeployments>; specs: ReturnType<typeof projectNodeSpecs>; currentId: string } | undefined> {
  if (inputs.readDeployments === undefined) return undefined;
  try {
    const raw = await inputs.readDeployments();
    const records = projectDeployments(raw.deployments);
    return {
      records,
      specs: projectNodeSpecs(raw.specs),
      currentId: currentDeploymentId(records),
    };
  } catch {
    // A cluster that stopped answering mid-read still has an instance row; it
    // simply has no history to show and no resolvable version, which renders
    // as the word "unknown".
    return undefined;
  }
}

// ---------------------------------------------------------------------------
// what a row says
// ---------------------------------------------------------------------------

/**
 * The icon vocabulary, named rather than spelled as ThemeIcons.
 *
 * Same split as clusters/status.ts: the WORDING and the CHOICE are testable
 * here, and views/deploymentsTree.ts is left as a mapping onto VS Code's icon
 * names.
 */
export type InstanceRowIcon = "healthy" | "unreachable" | "absent";

export interface InstanceRowStatus {
  icon: InstanceRowIcon;
  /** The right-hand text on the row. */
  description: string;
  tooltip: string;
}

export function instanceRowStatus(instance: Instance): InstanceRowStatus {
  if (instance.presence === "absent") {
    return {
      icon: "absent",
      description: "not installed",
      tooltip:
        instance.kind === "local"
          ? "No local cluster on this machine. Create a deployment to install one."
          : "Not installed.",
    };
  }
  // The version is ALWAYS printed, and printed as the word "unknown" when it
  // could not be resolved. A blank reads as a fact about the instance ("it has
  // no version") when it is a fact about the read.
  const version = displayVersion(instance.version);
  if (instance.presence === "installed-unreachable") {
    return {
      icon: "unreachable",
      description: `not answering - ${version}`,
      tooltip:
        instance.kind === "local"
          ? `A local cluster is installed but is not answering. Version ${version}.`
          : `${instance.name} is not answering. Version ${version}.`,
    };
  }
  return {
    icon: "healthy",
    description: `healthy - ${version}`,
    tooltip: `${instance.name} is healthy. Version ${version}.`,
  };
}

export type RunRowIcon =
  | "running"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "interrupted"
  | "replaced";

export interface RunRowStatus {
  icon: RunRowIcon;
  /** The row's label: the verb, which is what the operator chose. */
  label: string;
  /** version transition, status, and when. */
  description: string;
  tooltip: string;
}

export function runRowStatus(run: Run, nowMs: number): RunRowStatus {
  const parts: string[] = [];
  const transition = versionTransition(run);
  if (transition !== "") parts.push(transition);
  parts.push(run.status);
  const when = relativeTime(run.finishedAt ?? run.startedAt, nowMs);
  if (when !== "") parts.push(when);
  return {
    icon: runRowIcon(run.status),
    label: run.kind,
    description: parts.join("  "),
    tooltip: `${run.kind} ${transition} ${run.status} - started ${run.startedAt}`.replace(
      /\s+/g,
      " ",
    ),
  };
}

/**
 * `v0.16.1 -> v0.17.0`, or just the target when there is nothing to come from.
 *
 * An install has no `fromVersion` -- there was nothing there -- and rendering
 * `unknown -> v0.17.0` for it would invent a predecessor. A run with neither
 * renders no transition at all rather than an arrow between two blanks.
 */
export function versionTransition(run: Run): string {
  const to = (run.toVersion ?? "").trim();
  const from = (run.fromVersion ?? "").trim();
  if (from !== "" && to !== "") return `${from} -> ${to}`;
  return to !== "" ? to : "";
}

function runRowIcon(status: Run["status"]): RunRowIcon {
  switch (status) {
    case "running":
      return "running";
    case "succeeded":
      return "succeeded";
    case "failed":
      return "failed";
    case "cancelled":
      return "cancelled";
    case "interrupted":
      // ITS OWN ICON, not the cancelled one it is nearest to. Both ended
      // without finishing, but an operator scanning this list is asking a
      // different question of each: `cancelled` is a decision they made and
      // need not revisit, while `interrupted` is work that stopped for a
      // reason unrelated to the work -- so it is the row worth re-running,
      // and it has to be findable as such (memql#3886).
      return "interrupted";
    case "superseded":
    case "rolled_back":
      // Both LANDED and were later replaced. Drawing them as failures would
      // blame the run for a subsequent decision; drawing them as plain
      // successes would hide that the cluster is no longer running them.
      return "replaced";
  }
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * How long ago, coarsely.
 *
 * Coarse on purpose: the question a history answers is "when, roughly", and a
 * row that says `2d ago` is read at a glance where a timestamp is not. The
 * exact instant is in the tooltip.
 *
 * An unparseable or empty stamp renders as nothing rather than as `NaN ago`,
 * and a stamp in the FUTURE renders as `just now` rather than as a negative
 * age -- clock skew between a cluster and this machine is ordinary, and a run
 * dated slightly ahead is not a run that happened in the future.
 */
export function relativeTime(iso: string | undefined, nowMs: number): string {
  const at = Date.parse(iso ?? "");
  if (!Number.isFinite(at)) return "";
  const elapsed = nowMs - at;
  if (elapsed < MINUTE) return "just now";
  if (elapsed < HOUR) return `${Math.floor(elapsed / MINUTE)}m ago`;
  if (elapsed < DAY) return `${Math.floor(elapsed / HOUR)}h ago`;
  return `${Math.floor(elapsed / DAY)}d ago`;
}

/** The context value a row carries, which is what scopes its menu entries. */
export function instanceContextValue(instance: Instance): string {
  if (instance.kind === "remote") return "memqlRemoteInstance";
  return instance.presence === "absent" ? "memqlLocalInstanceAbsent" : "memqlLocalInstance";
}

export { LOCAL_INSTANCE_NAME };
