// Every instance on this machine and reachable from it, and the runs beneath
// each -- from which the Deployments view takes ONE (memql#4426).
//
// The catalog is still whole-machine. What narrowed is the VIEW: it renders the
// selected cluster's runs flat, through `runsForSelected` below, while the
// Clusters view continues to show every registered cluster with its health. Two
// surfaces, two questions, one read -- rather than a second catalog that could
// disagree with this one about the same cluster in the same frame.
//
// The tree itself is a mapping onto VS Code's TreeItem vocabulary and nothing
// else. Everything that is a DECISION -- which instances exist, which runs
// belong to which, which of them the view is about, what a row says, which icon
// it carries -- is here, where it runs under bare `node --test` with no
// workbench, no cluster and no network.
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
// Refs: #4426 #4423 #3737 #3733

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { ConnectionState } from "../connection/manager.js";

import { readClustersFileSafe } from "../clusters/file.js";
import type { PresenceResult, PresenceVerdict } from "../clusters/presence.js";
import { defaultReceiptPath, readReceipt, type Receipt } from "../install/receipt.js";
import { failureGuidance } from "./installProgress.js";
import { compareVersions } from "../version/compare.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";
import {
  LOCAL_INSTANCE_NAME,
  instanceLabel,
  localInstance,
  remoteInstance,
  runsFromDeployments,
  sortInstances,
  type Instance,
  type Run,
  type RunItem,
  type RunItemStatus,
} from "./deployments.js";
import {
  currentDeploymentId,
  pendingDeploymentId,
  projectDeployments,
  projectNodeSpecs,
  rollbackTargetId,
} from "./deploymentHistory.js";
import { defaultRunsDir, listRuns, sortRunsNewestFirst } from "./runLog.js";

/** The live connection, as far as this view cares. */
export interface ConnectionFacts {
  clusterName: string;
  connected: boolean;
  /**
   * Where the connection to `clusterName` stands, in the vocabulary every
   * surface shares (`memql.connectionState`). Absent from a caller that only
   * knows connected-or-not, which then reads as `connected` or `unreachable`.
   */
  word?: ConnectionWord;
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
  /**
   * This extension's own build stamp (memql#5076), when it was packaged.
   *
   * Threaded from the host rather than read here: it lives at
   * `<extensionPath>/staged/buildinfo.json` and only the host knows that path.
   * Absent for an extension running out of a checkout, which was never packaged
   * and therefore has no commit to name.
   */
  buildStamp?: { commit: string; dirty: boolean };
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
        ? {
            registered: {
              name: registered.name,
              ...(registered.displayName !== undefined ? { displayName: registered.displayName } : {}),
              domain: registered.domain,
              ...(registered.version !== undefined ? { version: registered.version } : {}),
            },
          }
        : {}),
      connected:
        connection?.connected === true &&
        registered !== undefined &&
        connection.clusterName === registered.name,
      ...(inputs.buildStamp !== undefined ? { buildStamp: inputs.buildStamp } : {}),
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
      ...(cluster.displayName !== undefined ? { displayName: cluster.displayName } : {}),
      ...(cluster.domain !== undefined ? { domain: cluster.domain } : {}),
      // Reachability is only ever KNOWN for the cluster we hold a connection
      // to. For the rest the honest verdict is the one that says it does not
      // answer, because as far as this editor can tell, it does not.
      reachable: isConnected,
      connected: isConnected,
      ...(cluster.version !== undefined ? { registryVersion: cluster.version } : {}),
      ...(isConnected && remote !== undefined
        ? {
            deployments: remote.records,
            currentDeploymentId: remote.currentId,
            pendingDeploymentId: remote.pendingId,
            rollbackTargetId: remote.rollbackId,
          }
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
): Promise<
  | {
      records: ReturnType<typeof projectDeployments>;
      specs: ReturnType<typeof projectNodeSpecs>;
      currentId: string;
      pendingId: string;
      rollbackId: string;
    }
  | undefined
> {
  if (inputs.readDeployments === undefined) return undefined;
  try {
    const raw = await inputs.readDeployments();
    const records = projectDeployments(raw.deployments);
    return {
      records,
      specs: projectNodeSpecs(raw.specs),
      currentId: currentDeploymentId(records),
      // Resolved HERE, from the same read that resolves the version, for the
      // reason the one-pass comment above gives: two reads of the same rows let
      // the two answers disagree about the same cluster in the same frame.
      pendingId: pendingDeploymentId(records),
      rollbackId: rollbackTargetId(records),
    };
  } catch {
    // A cluster that stopped answering mid-read still has an instance row; it
    // simply has no history to show and no resolvable version, which renders
    // as the word "unknown".
    return undefined;
  }
}


// ---------------------------------------------------------------------------
// where this editor stands with a cluster
// ---------------------------------------------------------------------------

/**
 * The connection, in the one vocabulary every surface shares.
 *
 * The same six values as the `memql.connectionState` context key: `none` (no
 * cluster in hand, or a different one), `connecting`, `connected`, `signIn`
 * (the credential is missing, expired or was refused -- the fix is a sign-in,
 * whatever the cause), `unreachable`, and `notConfigured` (the cluster list
 * names no address to dial).
 */
export type ConnectionWord = "none" | "connecting" | "connected" | "signIn" | "unreachable" | "notConfigured";

/** The failure reasons whose fix is a sign-in rather than a look at the cluster. */
const SIGN_IN_REASONS = new Set([
  "missingCredential",
  "credentialExpired",
  "wrongTokenClass",
  "reauthenticationRequired",
]);

/**
 * Where the connection to `clusterName` stands.
 *
 * `none` for a state about a DIFFERENT cluster: the Deployments page for
 * `staging` is not "connected" because this editor holds `local`.
 */
export function connectionWordFor(state: ConnectionState, clusterName: string): ConnectionWord {
  if (state.status === "disconnected" || state.clusterName !== clusterName) return "none";
  switch (state.status) {
    case "connecting":
      return "connecting";
    case "connected":
      return "connected";
    case "error":
      if (SIGN_IN_REASONS.has(state.reason)) return "signIn";
      if (state.reason === "notConfigured") return "notConfigured";
      return "unreachable";
  }
}

/** The connection word for an instance, from the facts a catalog was built with. */
export function instanceConnectionWord(instance: Instance, connection: ConnectionFacts | undefined): ConnectionWord {
  if (connection === undefined || connection.clusterName !== instance.name) return "none";
  if (connection.word !== undefined) return connection.word;
  return connection.connected ? "connected" : "unreachable";
}

/** The dot beside a state word (kit `BarTone`). */
export type StateTone = "idle" | "live" | "busy" | "warn" | "error";

export type ClusterStateKey =
  | "notInstalled"
  | "unreceipted"
  | "notListed"
  | "connected"
  | "connecting"
  | "signIn"
  | "notSetUp"
  | "notRunning"
  | "cantReach"
  | "notConnected";

export interface ClusterState {
  key: ClusterStateKey;
  /** The page's words ("Not signed in"). */
  word: string;
  /** The heading's words, where the act is not beside it ("Sign in"). */
  heading: string;
  tone: StateTone;
}

const STATES: Readonly<Record<ClusterStateKey, Omit<ClusterState, "key">>> = {
  notInstalled: { word: "Not installed", heading: "Not installed", tone: "idle" },
  unreceipted: { word: "Not connected", heading: "Not connected", tone: "idle" },
  notListed: { word: "Not in your list", heading: "Not in your list", tone: "idle" },
  connected: { word: "Connected", heading: "Connected", tone: "live" },
  connecting: { word: "Connecting", heading: "Connecting", tone: "busy" },
  signIn: { word: "Not signed in", heading: "Sign in", tone: "warn" },
  notSetUp: { word: "Not set up", heading: "Not set up", tone: "warn" },
  notRunning: { word: "Not running", heading: "Not running", tone: "error" },
  cantReach: { word: "Can't reach", heading: "Can't reach", tone: "error" },
  notConnected: { word: "Not connected", heading: "Not connected", tone: "idle" },
};

/**
 * One cluster's state, in words: the connection first, what is on the machine
 * second.
 *
 * THE CONNECTION WINS because it is what the person can act on from here. A
 * local cluster that answers its front door but refuses this editor's
 * credential is "Not signed in", never "healthy" -- the old heading said
 * "not answering" for exactly that case, from a probe that could not tell the
 * difference, and offered nothing.
 *
 * The machine decides only what the connection cannot: nothing installed, a
 * cluster this editor did not install, a cluster missing from the list, and a
 * dropped connection to a local cluster whose front door is also silent
 * (`Not running`, which a Repair fixes) as against one that answers
 * (`Can't reach`).
 */
export function clusterState(instance: Instance, connection: ConnectionWord): ClusterState {
  const key = clusterStateKey(instance, connection);
  return { key, ...STATES[key] };
}

function clusterStateKey(instance: Instance, connection: ConnectionWord): ClusterStateKey {
  if (instance.kind === "local") {
    if (instance.presence === "absent") return "notInstalled";
    if (instance.presence === "present-unreceipted") return "unreceipted";
    if (instance.registered === false) return "notListed";
  }
  switch (connection) {
    case "connected":
      return "connected";
    case "connecting":
      return "connecting";
    case "signIn":
      return "signIn";
    case "notConfigured":
      return "notSetUp";
    case "unreachable":
      return instance.kind === "local" && instance.presence === "installed-unreachable" ? "notRunning" : "cantReach";
    case "none":
      return instance.kind === "local" && instance.presence === "installed-unreachable" ? "notRunning" : "notConnected";
  }
}

// ---------------------------------------------------------------------------
// the heading above the timeline
// ---------------------------------------------------------------------------

/**
 * The line beside the view's name: which cluster, how it stands, what it runs.
 *
 *   local · Connected · v0.23.5
 *   local · Connected · v0.23.5 · v0.24.0 available
 *   local · Sign in · main @ 3f2a9c1
 *   staging · Can't reach · v0.9.2
 *   local · Not installed
 *
 * The state word is the connection's (`clusterState`), shared with the page,
 * so the heading and the page cannot disagree about the same cluster. The
 * version is `versionLabel`, left out when nothing names one rather than
 * printed as "unknown".
 *
 * AN UPDATE IS SAID ONLY FOR A RELEASED CLUSTER. A cluster running the
 * checkout's build is not on the release its receipt names, so an update "to"
 * a newer release is a claim about a version it is not on.
 *
 * "" FOR NO SELECTION: the welcome is already saying what is going on.
 */
export function selectedViewDescription(
  instance: Instance | undefined,
  listing: ReleaseListing | undefined,
  connection: ConnectionWord = "none",
): string {
  if (instance === undefined) return "";
  const state = clusterState(instance, connection);
  const parts = [instanceLabel(instance), state.heading];
  if (state.key === "notInstalled") return parts.join(" · ");
  const label = (instance.versionLabel ?? "").trim();
  if (label !== "") parts.push(label);
  if (instance.imageSource !== "checkout") {
    const described = describeVersion({ recorded: instance.version, listing });
    if (described.upgradeAvailable && described.latest !== undefined) {
      parts.push(`${described.latest} available`);
    }
  }
  return parts.join(" · ");
}

// ---------------------------------------------------------------------------
// what a run row says
// ---------------------------------------------------------------------------

/** How a run reads, in its three tenses. */
export interface RunVerbs {
  /** It went through: "Updated". */
  done: string;
  /** It is going: "Updating". */
  doing: string;
  /** A thing that can fail or be stopped: "Update". */
  noun: string;
}

/**
 * The words for what a run did.
 *
 * A VERSION MOVE IS "UPDATED" ONLY WHEN IT MOVED FORWARD. The run log records
 * every move to a tag as `upgrade`, and a move back to an older release during
 * an incident is exactly the one that must not read as an upgrade; so the
 * direction is read off the two versions, and anything that is not provably
 * forward is a version change.
 */
export function runVerbs(run: Run): RunVerbs {
  switch (run.kind) {
    case "install":
      return { done: "Installed", doing: "Installing", noun: "Install" };
    case "repair":
      return { done: "Repaired", doing: "Repairing", noun: "Repair" };
    case "uninstall":
      return { done: "Uninstalled", doing: "Uninstalling", noun: "Uninstall" };
    case "rebuild":
      return { done: "Rebuilt", doing: "Rebuilding", noun: "Rebuild" };
    case "update":
      return { done: "Pulled and rebuilt", doing: "Pulling and rebuilding", noun: "Pull and rebuild" };
    case "upgrade":
      return compareVersions(run.fromVersion, run.toVersion) === "behind"
        ? { done: "Updated", doing: "Updating", noun: "Update" }
        : { done: "Changed version", doing: "Changing version", noun: "Version change" };
    case "rollout":
      return { done: "Deployed", doing: "Deploying", noun: "Deploy" };
  }
}

export type RunRowIcon = "running" | "succeeded" | "failed" | "cancelled" | "interrupted" | "replaced";

export interface RunRowStatus {
  icon: RunRowIcon;
  /** The dot a page draws for it. */
  tone: StateTone;
  /** What happened, as a verb: "Updated", "Install failed". */
  label: string;
  /** From where to where, and when: "v0.18.0 → v0.19.0 · 2d ago". */
  description: string;
  /** When it started, how long it took, and why it failed. */
  tooltip: string;
  /** The status in a word, for the run's own page ("Failed"). */
  statusWord: string;
}

/**
 * A run as a row.
 *
 * THE LABEL CARRIES THE OUTCOME ONLY WHEN IT IS NOT A SUCCESS. A tick and
 * "Updated" say it once; "Update failed" needs its word because a red dot on
 * its own could be any of four things. The description never repeats it.
 */
export function runRowStatus(run: Run, nowMs: number, opts: { prepared?: boolean } = {}): RunRowStatus {
  const verbs = runVerbs(run);
  // A REMOTE RECORD THAT IS PREPARED AND NOT SHIPPED is not "Deploying": the
  // deployment concept's `pending` reads as running (deployments.ts), and the
  // row would claim a deploy is under way that nobody has started.
  const prepared = opts.prepared === true && run.status === "running";
  const label = prepared ? "Prepared" : runLabel(run, verbs);
  const parts: string[] = [];
  const transition = versionTransition(run);
  if (transition !== "") parts.push(transition);
  if (run.status === "superseded") parts.push("replaced");
  const when = relativeTime(run.finishedAt ?? run.startedAt, nowMs);
  if (when !== "") parts.push(when);

  const lines: string[] = [];
  const started = formatWhen(run.startedAt, nowMs);
  const took = runDuration(run.startedAt, run.finishedAt);
  if (started !== "") lines.push(`Started ${started}${took === "" ? "" : ` · took ${took}`}`);
  const reason = runFailureReason(run);
  if (reason !== "") lines.push(reason);
  if (run.status === "interrupted") lines.push("It stopped when the editor closed.");

  return {
    icon: prepared ? "cancelled" : runRowIcon(run.status),
    tone: prepared ? "idle" : runTone(run.status),
    label,
    description: parts.join(" · "),
    tooltip: lines.join("\n"),
    statusWord: prepared ? "Prepared" : STATUS_WORDS[run.status],
  };
}

/**
 * What a run WAS, as a noun for its own page's title ("Update", "Rebuild",
 * "Rollback"); the outcome is the page's state word, said once, on its bar.
 */
export function runNoun(run: Run): string {
  return run.status === "rolled_back" ? "Rollback" : runVerbs(run).noun;
}

const STATUS_WORDS: Readonly<Record<Run["status"], string>> = {
  running: "Running",
  succeeded: "Succeeded",
  failed: "Failed",
  cancelled: "Cancelled",
  interrupted: "Interrupted",
  superseded: "Replaced",
  rolled_back: "Rolled back",
};

function runLabel(run: Run, verbs: RunVerbs): string {
  switch (run.status) {
    case "running":
      return verbs.doing;
    case "succeeded":
    case "superseded":
      return verbs.done;
    case "rolled_back":
      // A remote record that lands in `rolled_back` IS the rollback: the
      // deploy-control service writes a new record for it (deploymentHistory.ts).
      return "Rolled back";
    case "failed":
      return `${verbs.noun} failed`;
    case "cancelled":
      return `${verbs.noun} cancelled`;
    case "interrupted":
      return `${verbs.noun} interrupted`;
  }
}

/**
 * `v0.16.1 → v0.17.0`, or just the target when there is nothing to come from.
 *
 * An install has no `fromVersion` -- there was nothing there -- and rendering
 * a predecessor for it would invent one. A run with neither renders nothing.
 */
export function versionTransition(run: Run): string {
  const to = (run.toVersion ?? "").trim();
  const from = (run.fromVersion ?? "").trim();
  if (from !== "" && to !== "") return `${from} → ${to}`;
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
      // ITS OWN ICON: `cancelled` is a decision somebody made, `interrupted`
      // is work that stopped because the editor went away -- the row worth
      // re-running (memql#3886).
      return "interrupted";
    case "superseded":
    case "rolled_back":
      // Both LANDED and were later replaced: neither a failure nor the
      // version running now.
      return "replaced";
  }
}

function runTone(status: Run["status"]): StateTone {
  switch (status) {
    case "running":
      return "busy";
    case "succeeded":
      return "live";
    case "failed":
      return "error";
    case "interrupted":
      return "warn";
    default:
      return "idle";
  }
}

// ---------------------------------------------------------------------------
// one step of a recorded run
// ---------------------------------------------------------------------------

/**
 * What the run log's `detail` says, taken apart.
 *
 * runRecorder.ts writes `reason · exit N · <already-holds> · key=value ... ·
 * log=<file>`, each part only when it has something to say. A surface shows the
 * reason and offers the log; the rest belongs under Details.
 */
export interface ItemDetail {
  /** The step's own sentence about what went wrong, or "". */
  reason: string;
  exitCode?: number;
  /** The step found its work already done. */
  alreadyInPlace: boolean;
  /** The result's key=value pairs, verbatim. */
  fields: string;
  /** The saved output of a failed step, a file name in the runs directory. */
  logFile: string;
}

const ALREADY_HOLDS = "the condition dependents needed already holds";

export function parseItemDetail(detail: string | undefined): ItemDetail {
  const out: ItemDetail = { reason: "", alreadyInPlace: false, fields: "", logFile: "" };
  const reasons: string[] = [];
  for (const raw of (detail ?? "").split(" · ")) {
    const part = raw.trim();
    if (part === "") continue;
    const exit = /^exit (\d+)$/.exec(part);
    if (exit !== null) {
      out.exitCode = Number(exit[1]);
      continue;
    }
    if (part === ALREADY_HOLDS) {
      out.alreadyInPlace = true;
      continue;
    }
    const log = /^log=(\S+)$/.exec(part);
    if (log !== null) {
      out.logFile = log[1]!;
      continue;
    }
    if (/^[A-Za-z][\w.-]*=\S*(\s+[A-Za-z][\w.-]*=\S*)*$/.test(part)) {
      out.fields = out.fields === "" ? part : `${out.fields} ${part}`;
      continue;
    }
    reasons.push(part);
  }
  out.reason = reasons.join(" ");
  return out;
}

/**
 * Why a run failed, in one sentence, or "".
 *
 * READ OFF THE ITEMS, never stored beside them: the reason is whatever the
 * first failed step recorded, so it cannot disagree with the step list. A step
 * that failed without a sentence says what its exit code means instead of
 * printing the number.
 */
export function runFailureReason(run: Run): string {
  if (run.status !== "failed") return "";
  const failed = run.items.find((item) => item.status === "failed");
  if (failed === undefined) return "";
  return itemReason(failed);
}

/** A failed step's sentence: its own, or what its exit code means. */
export function itemReason(item: RunItem): string {
  const parsed = parseItemDetail(item.detail);
  if (parsed.reason !== "") return parsed.reason;
  if (parsed.exitCode !== undefined) return failureGuidance(parsed.exitCode).headline;
  return "";
}

/** One line of a run's step list: consecutive steps under one label are one line. */
export interface StepGroup {
  label: string;
  status: RunItemStatus;
  items: RunItem[];
}

/** Which status a group of steps shows: the worst of them. */
const GROUP_RANK: readonly RunItemStatus[] = ["failed", "running", "pending", "preserved", "ok", "skipped"];

/**
 * A run's items as the page lists them: in order, named in words, and the
 * three "Installing tools" steps as one line.
 *
 * `labels` maps a step id to its short label from the graph documents; an id
 * it does not name is shown as itself rather than dropped.
 */
export function stepGroups(items: readonly RunItem[], labels: ReadonlyMap<string, string>): StepGroup[] {
  const groups: StepGroup[] = [];
  for (const item of items) {
    const label = labels.get(item.label) ?? item.label;
    const last = groups[groups.length - 1];
    if (last !== undefined && last.label === label) {
      last.items.push(item);
      last.status = worse(last.status, item.status);
      continue;
    }
    groups.push({ label, status: item.status, items: [item] });
  }
  return groups;
}

function worse(a: RunItemStatus, b: RunItemStatus): RunItemStatus {
  return GROUP_RANK.indexOf(a) <= GROUP_RANK.indexOf(b) ? a : b;
}

// ---------------------------------------------------------------------------
// time
// ---------------------------------------------------------------------------

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * How long ago, coarsely: `2d ago` is read at a glance where a timestamp is not.
 *
 * An unparseable stamp renders as nothing, and one in the FUTURE as
 * `just now` -- clock skew between a cluster and this machine is ordinary.
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

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * A moment in this machine's own time: `27 Sep, 10:00`, with the year only
 * when it is not this one. Never an RFC3339 stamp; those are for Details.
 */
export function formatWhen(iso: string | undefined, nowMs: number): string {
  const at = Date.parse(iso ?? "");
  if (!Number.isFinite(at)) return "";
  const d = new Date(at);
  const year = d.getFullYear() === new Date(nowMs).getFullYear() ? "" : ` ${d.getFullYear()}`;
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${d.getDate()} ${MONTHS[d.getMonth()]}${year}, ${hh}:${mm}`;
}

/**
 * How long a run TOOK: `4m 12s`, `1h 3m`, `40s`.
 *
 * "" when it cannot be computed -- still running, interrupted, unparseable --
 * and the caller then omits the fact rather than printing a zero. A negative
 * elapsed (clock skew) reads as `0s`.
 */
export function runDuration(
  startedAt: string | undefined,
  finishedAt: string | undefined,
): string {
  const from = Date.parse(startedAt ?? "");
  const to = Date.parse(finishedAt ?? "");
  if (!Number.isFinite(from) || !Number.isFinite(to)) return "";
  const elapsed = Math.max(0, to - from);
  const seconds = Math.floor(elapsed / 1000);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${rest}s`;
  return `${rest}s`;
}

/**
 * How often a VISIBLE Deployments surface re-reads on its own.
 *
 * A run still going is polled briskly, because its row is the thing somebody
 * is watching; otherwise slowly, enough to notice a cluster that stopped or a
 * deploy somebody else started. Nothing polls while the surface is hidden.
 */
export const POLL_ACTIVE_MS = 10_000;
export const POLL_IDLE_MS = 60_000;

export function pollIntervalMs(runs: readonly Run[]): number {
  return runs.some((run) => run.status === "running") ? POLL_ACTIVE_MS : POLL_IDLE_MS;
}

// ---------------------------------------------------------------------------
// the title menu's context keys
// ---------------------------------------------------------------------------

/**
 * What the Deployments title menu is scoped by (memql#4426): what the selected
 * cluster IS -- local or remote, installed or not.
 *
 * NOT A CONNECTION KEY. `memql.connectionState`, `memql.clusterSelected` and
 * `memql.connected` describe the connection and the ConnectionManager publishes
 * them; this is a fact only the catalog computes, so the view publishes it.
 */
export const DEPLOYMENTS_INSTANCE_KEY = "memql.deploymentsInstance";
/** A checkout is recorded: Rebuild From Checkout and Open Local Checkout have something to act on. */
export const DEPLOYMENTS_HAS_CHECKOUT_KEY = "memql.deploymentsHasCheckout";
/** The checkout is on a branch: Update And Rebuild has something to pull. */
export const DEPLOYMENTS_HAS_BRANCH_KEY = "memql.deploymentsHasBranch";

/** The context value an instance carries. */
export function instanceContextValue(instance: Instance): string {
  if (instance.kind === "remote") return "memqlRemoteInstance";
  if (instance.presence === "absent") return "memqlLocalInstanceAbsent";
  if (instance.presence === "present-unreceipted") return "memqlLocalInstanceUnreceipted";
  return "memqlLocalInstance";
}

/** Every key the title menu reads, for the selection -- "" and false when there is none. */
export interface DeploymentsContextKeys {
  [DEPLOYMENTS_INSTANCE_KEY]: string;
  [DEPLOYMENTS_HAS_CHECKOUT_KEY]: boolean;
  [DEPLOYMENTS_HAS_BRANCH_KEY]: boolean;
}

export function deploymentsContextKeys(instance: Instance | undefined): DeploymentsContextKeys {
  const local = instance !== undefined && instance.kind === "local" && instance.presence !== "absent" && instance.presence !== "present-unreceipted";
  return {
    [DEPLOYMENTS_INSTANCE_KEY]: instance === undefined ? "" : instanceContextValue(instance),
    [DEPLOYMENTS_HAS_CHECKOUT_KEY]: local && (instance?.checkout ?? "") !== "",
    [DEPLOYMENTS_HAS_BRANCH_KEY]: local && (instance?.checkout ?? "") !== "" && (instance?.checkoutBranch ?? "") !== "",
  };
}

// ---------------------------------------------------------------------------
// the selected cluster's timeline (memql#4426)
// ---------------------------------------------------------------------------

/**
 * The instance the Deployments view is about, and its runs.
 *
 * NOTHING SELECTED yields no instance and no runs, so the provider returns
 * `[]` and the manifest's welcome renders over it -- VS Code draws welcome
 * content ONLY over a genuinely empty tree. A selection that names no instance
 * (removed from clusters.yaml by the Cockpit a moment ago) yields the same.
 */
export interface SelectedRuns {
  instance: Instance | undefined;
  /** Newest first. */
  runs: Run[];
}

export function runsForSelected(
  catalog: Catalog,
  selection: ConnectionFacts | undefined,
): SelectedRuns {
  if (selection === undefined) return { instance: undefined, runs: [] };
  const instance = catalog.instances.find((i) => i.name === selection.clusterName);
  if (instance === undefined) return { instance: undefined, runs: [] };
  return { instance, runs: sortRunsNewestFirst(catalog.runs.get(instance.name) ?? []) };
}

export { LOCAL_INSTANCE_NAME };
