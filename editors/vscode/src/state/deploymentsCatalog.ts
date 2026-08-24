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

import { readClustersFileSafe } from "../clusters/file.js";
import type { PresenceResult, PresenceVerdict } from "../clusters/presence.js";
import { defaultReceiptPath, readReceipt, type Receipt } from "../install/receipt.js";
import { describeVersion } from "../version/describe.js";
import type { ReleaseListing } from "../version/releaseCache.js";
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
  pendingDeploymentId,
  projectDeployments,
  projectNodeSpecs,
} from "./deploymentHistory.js";
import { defaultRunsDir, listRuns, sortRunsNewestFirst } from "./runLog.js";

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
        ? {
            deployments: remote.records,
            currentDeploymentId: remote.currentId,
            pendingDeploymentId: remote.pendingId,
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
/**
 * A checkout-mode instance's version text, or "" for every other instance.
 *
 * A CHECKOUT-MODE INSTANCE NAMES THE CHECKOUT, NOT THE RECORDED RELEASE
 * (memql#4246). `instance.version` is still whichever tag the ORIGINAL install
 * checked out -- a repair or upgrade would replay it -- but that is not what is
 * running right now. Printing it would tell an operator their v0.17.0 cluster
 * is healthy while it is actually serving whatever was in the checkout at the
 * last rebuild, uncommitted edits included.
 *
 * Its own function since memql#4426, because two surfaces now say it: the tree
 * row, and the Deployments view description above the timeline. A second copy
 * of this rule is how one of them would go on printing a release tag for a
 * cluster running a developer's own build.
 *
 * A count the envelope did not carry is LEFT OUT, never invented: printing
 * "0 uncommitted files" from an unreported field is a claim that the tree was
 * clean.
 */
export function checkoutVersionText(instance: Instance): string {
  if (instance.imageSource !== "checkout" || instance.rebuild === undefined) return "";
  const { rebuild } = instance;
  const shortCommit = rebuild.commit.slice(0, 7);
  const dirty = rebuild.dirtyCount;
  return `checkout ${shortCommit}${dirty !== undefined && dirty > 0 ? ` (${dirty} uncommitted)` : ""}`;
}

export type InstanceRowIcon = "healthy" | "unreachable" | "absent";

export interface InstanceRowStatus {
  icon: InstanceRowIcon;
  /** The right-hand text on the row. */
  description: string;
  tooltip: string;
}

/**
 * @param listing the release listing, or undefined when nothing has been
 * fetched. REQUIRED rather than optional: a caller with no listing is making a
 * decision ("render without availability"), and an optional parameter would let
 * a new call site make it by omission -- which presents as the availability
 * clause silently never appearing on one surface.
 */
export function instanceRowStatus(
  instance: Instance,
  listing: ReleaseListing | undefined,
): InstanceRowStatus {
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
  //
  // WHICH IS WHY `short` IS NOT USED ON ITS OWN (memql#3996). describeVersion
  // returns "" for an unrecorded version, because a Clusters-tree row that
  // appended "unknown" to every cluster on a fresh install would be noise.
  // These rows made the opposite call first and it still holds here: this row
  // already prints a version for every instance, so falling silent for one
  // instance would read as a fact about that instance.
  const version = displayVersion(instance.version);
  const described = describeVersion({ recorded: instance.version, listing });
  // `short` carries the availability clause in the one state that has one --
  // `v0.18.0 - v0.19.0 available` -- and is otherwise just the version. Taking
  // the wording from there rather than re-composing it is what keeps this row
  // and the Clusters tree from saying "available" two different ways.
  let versionText = described.short === "" ? version : described.short;
  // A CHECKOUT-MODE INSTANCE NAMES THE CHECKOUT, NOT THE RECORDED RELEASE
  // (memql#4246). `instance.version` is still whichever tag the ORIGINAL
  // install checked out -- a repair or upgrade would replay it -- but that is
  // not what is running right now. Printing it here would tell an operator
  // their v0.17.0 cluster is healthy while it is actually serving whatever
  // was in the checkout at the last rebuild, uncommitted edits included.
  let checkoutTooltip = "";
  const checkoutText = checkoutVersionText(instance);
  if (checkoutText !== "" && instance.rebuild !== undefined) {
    const { rebuild } = instance;
    const shortCommit = rebuild.commit.slice(0, 7);
    const dirty = rebuild.dirtyCount;
    versionText = checkoutText;
    checkoutTooltip =
      `\nRunning images built from the checkout at ${shortCommit}` +
      (dirty === undefined ? ". " : ` (${dirty} uncommitted files when it was built). `) +
      "An install, upgrade or repair returns it to released images.";
  }
  // The presence verdict stays FIRST in the tooltip. It is what an operator
  // opened the row for; the version is context beneath it, and a tooltip that
  // led with the version would bury the reason the row is amber.
  if (instance.presence === "installed-unreachable") {
    return {
      icon: "unreachable",
      description: `not answering - ${versionText}`,
      tooltip:
        (instance.kind === "local"
          ? `A local cluster is installed but is not answering. Version ${version}.`
          : `${instance.name} is not answering. Version ${version}.`) +
        `\n${described.sentence}` +
        checkoutTooltip,
    };
  }
  return {
    icon: "healthy",
    description: `healthy - ${versionText}`,
    tooltip: `${instance.name} is healthy. Version ${version}.\n${described.sentence}` + checkoutTooltip,
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

/**
 * How long a run TOOK, as the detail page prints it (memql#4427).
 *
 * A different question from `relativeTime`, which answers "how long ago", and
 * kept beside it so the two read as one vocabulary rather than as two authors.
 * Coarse in the same way and for the same reason -- `4m 12s` is read at a
 * glance -- but it keeps seconds, because a run's duration is often under a
 * minute and "0m" would be a worse answer than none.
 *
 * "" WHEN IT CANNOT BE COMPUTED, and the caller omits the fact rather than
 * printing a placeholder. A run still in flight has no finish, an interrupted
 * one never got a finish written, and an unparseable stamp is a fact about the
 * record. None of the three is a duration of zero, and printing one would be
 * the same class of lie `displayVersion` exists to prevent.
 *
 * A NEGATIVE ELAPSED READS AS `0s`, not as a negative duration: clock skew
 * between a cluster and this machine is ordinary, and a run that finished a
 * moment "before" it started did not run backwards.
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
 * The context key the Deployments view title menu is scoped by (memql#4426).
 *
 * A KEY RATHER THAN A ROW'S `contextValue`, because there is no longer a row.
 * The instance actions moved to the view TITLE menu when the wrapper row went,
 * and `view/title` clauses are evaluated with no `viewItem` in scope -- so the
 * only way to keep "Uninstall is offered for an installed local cluster and for
 * nothing else" is to publish the same vocabulary as a key.
 *
 * IT IS NOT A THIRD CONNECTION KEY. `memql.clusterSelected` and
 * `memql.connected` describe the CONNECTION and only the ConnectionManager
 * publishes them (design D1). This describes what the selected cluster IS --
 * local or remote, installed or not -- which is a fact only the catalog
 * computes, so the Deployments view is its one publisher for the same reason
 * the manager is theirs.
 */
export const DEPLOYMENTS_INSTANCE_KEY = "memql.deploymentsInstance";

/**
 * The context value an instance carries, which is what scopes its menu entries.
 *
 * The vocabulary is unchanged from when it labelled a row: the three values
 * exist because the three kinds of instance offer different actions, and an
 * absent local cluster can only be created where an installed one can also be
 * repaired, rebuilt and uninstalled.
 */
export function instanceContextValue(instance: Instance): string {
  if (instance.kind === "remote") return "memqlRemoteInstance";
  return instance.presence === "absent" ? "memqlLocalInstanceAbsent" : "memqlLocalInstance";
}

/**
 * The same value for the SELECTION, including when there is not one.
 *
 * "" when nothing is selected, which is what every `==` clause in the manifest
 * fails against -- so a title menu over an unselected view offers none of the
 * instance actions, and the welcome is the only thing on screen. Distinguished
 * from `instanceContextValue` so no caller has to invent a value for the
 * absent case; inventing one is how "no cluster" would come to mean "local".
 */
export function selectedInstanceContext(instance: Instance | undefined): string {
  return instance === undefined ? "" : instanceContextValue(instance);
}

// ---------------------------------------------------------------------------
// the selected cluster's timeline (memql#4426)
// ---------------------------------------------------------------------------

/**
 * The instance the Deployments view is about, and its runs.
 *
 * WHAT CHANGED, AND WHY THE SHAPING IS HERE. The view used to render every
 * registered instance as a top-level row with its runs nested underneath, which
 * put a wrapper row -- `local`, most often -- between an operator and the only
 * list they came for. It now renders ONE cluster's runs, flat: the one this
 * editor has in hand. The instance itself does not vanish; it moves to the view
 * description, where it is a heading rather than a row to expand.
 *
 * Pure, and in this file rather than inline in the provider, for the reason the
 * header states about every other decision here: the failure mode of the
 * Deployments view is a BLANK PANEL, and no unit test of a TreeDataProvider
 * would see one. Driven here, "a selection that names no instance yields no
 * rows" is an assertion.
 *
 * NOTHING SELECTED IS NOT AN ERROR, and it is not an empty cluster either. It
 * yields no instance and no runs, so the provider returns `[]` and the
 * manifest's welcome renders over it -- which is the whole mechanism, since VS
 * Code draws welcome content ONLY over a genuinely empty tree.
 *
 * A SELECTION THAT NAMES NO INSTANCE yields the same nothing rather than a
 * complaint. It is an ordinary race: `clusters.yaml` is shared with the MemQL
 * Cockpit, so a cluster can be removed there between this editor selecting it
 * and this catalog being read, and a synthetic error row would suppress the
 * welcome that correctly describes what is left.
 */
export interface SelectedRuns {
  /** The selected cluster's instance, or undefined when there is no selection. */
  instance: Instance | undefined;
  /** Its runs, newest first. Empty when there is no selection. */
  runs: Run[];
}

export function runsForSelected(
  catalog: Catalog,
  selection: ConnectionFacts | undefined,
): SelectedRuns {
  if (selection === undefined) return { instance: undefined, runs: [] };
  const instance = catalog.instances.find((i) => i.name === selection.clusterName);
  if (instance === undefined) return { instance: undefined, runs: [] };
  // RE-DERIVED, not inherited. `listRuns` and `runsFromDeployments` each sort
  // their own output already, so this is a second application of an order that
  // is usually right -- and that is the point: a list filtered or re-ordered
  // upstream cannot silently produce a history running backwards, and the
  // comparator is the shared one rather than a third spelling of it.
  return { instance, runs: sortRunsNewestFirst(catalog.runs.get(instance.name) ?? []) };
}

/**
 * The line above the timeline: which cluster this is, how it is, what it runs.
 *
 *   local · healthy · v0.19.1
 *   local · healthy · v0.19.1 · update v0.20.0 available
 *   staging · not answering · v0.9.2
 *   local · not installed
 *
 * THE INSTANCE FACTS, PROMOTED OUT OF A ROW. `TreeView.description` is the API
 * made for exactly this -- a subtitle beside the view's own name -- and it is
 * what lets the runs be the only rows. An operator reading it learns the three
 * things the wrapper row told them, in the place a heading belongs.
 *
 * IT SAYS "update vX available", NOT the row's "vX available". The row appends
 * the clause to a version and reads `v0.18.0 - v0.19.0 available`, which is one
 * field. Here the version is its own segment, and a bare `v0.19.0 available`
 * beside `v0.18.0` would read as two versions with no statement about either.
 *
 * A CHECKOUT-MODE CLUSTER GETS NO AVAILABILITY CLAUSE, which is the same call
 * `instanceRowStatus` makes: the recorded release is not what the cluster is
 * running, so "an update to it is available" is a claim about a version it is
 * not on.
 *
 * "" FOR NO SELECTION, and the caller clears the description rather than
 * printing something. The welcome is already saying what is going on.
 */
export function selectedViewDescription(
  instance: Instance | undefined,
  listing: ReleaseListing | undefined,
): string {
  if (instance === undefined) return "";
  if (instance.presence === "absent") {
    // No version segment, exactly as the row makes no version claim about a
    // machine with nothing installed.
    return `${instance.name} · not installed`;
  }
  const parts = [
    instance.name,
    instance.presence === "installed-unreachable" ? "not answering" : "healthy",
  ];
  const checkout = checkoutVersionText(instance);
  if (checkout !== "") {
    parts.push(checkout);
    return parts.join(" · ");
  }
  parts.push(displayVersion(instance.version));
  const described = describeVersion({ recorded: instance.version, listing });
  if (described.upgradeAvailable && described.latest !== undefined) {
    parts.push(`update ${described.latest} available`);
  }
  return parts.join(" · ");
}

export { LOCAL_INSTANCE_NAME };
