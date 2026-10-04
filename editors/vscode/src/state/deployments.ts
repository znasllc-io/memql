// The instance and run model: what an operator deploys to, and what changed it.
//
// The Deployments view has two levels and this file is both of them. An
// INSTANCE is a MemQL you operate -- the local cluster on this machine, or a
// remote one you can reach. A RUN is something that moved an instance's
// deployed state. Everything the tree, the instance page and the action
// catalog read comes from here.
//
// THREE PROPERTIES ARE LOAD-BEARING.
//
//  1. AN INSTANCE IS DERIVED, NEVER DECLARED. There is no new registry file.
//     The local instance is resolved from evidence already on the machine (the
//     install receipt, a `local: true` row in clusters.yaml, the front-door
//     probe -- all of it via clusters/presence.ts); a remote instance is a
//     clusters.yaml row. The extension cannot CREATE a remote cluster, so a
//     "declared but not installed" remote row would be a row you can only look
//     at.
//
//  2. LOCAL AND REMOTE RUNS SHARE A HEADER, NOT A GRANULARITY. A local run's
//     items are capability-script executions; a remote run's are per-tier
//     `v1:cluster:deploymentNodeSpec` rows. The asymmetry is stated rather than
//     hidden -- the panel labels them "Steps" and "Node types" -- because
//     pretending a node-type spec is a step would invent a step log the data
//     does not contain.
//
//  3. A VERSION IS SHOWN FROM WHAT WAS RECORDED, OR NOT AT ALL. `versionLabel`
//     is the tag, the branch and commit, the checkout build or the registry's
//     recorded release -- whichever names what is running. The word "unknown"
//     used to fill the gap (`displayVersion`), and it put a false claim in the
//     Deployments heading for every branch install, whose commit was recorded
//     all along. A surface with no label now leaves the version out; the word
//     survives only where a sentence needs a noun (a forecast's "from").
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3736 #3733

import type { PresenceVerdict } from "../clusters/presence.js";
import {
  recordedDomain,
  recordedImageSource,
  recordedRebuild,
  recordedStackBranch,
  recordedStackCommit,
  recordedStackDir,
  recordedStackTag,
  type ImageSource,
  type Receipt,
  type RecordedRebuild,
} from "../install/receipt.js";
import {
  indexDeployments,
  resolveTierVersion,
  shortenDigest,
  type DeploymentNodeSpec,
  type DeploymentRecord,
} from "./deploymentHistory.js";

/**
 * The name the local instance carries when nothing in clusters.yaml names it.
 *
 * A machine with a receipt but no registry row still has an instance to show
 * -- that is the whole point of the presence pass -- and it needs a key the
 * tree can select on. The registered name wins when there is one, so an
 * operator who called their local cluster something else sees that.
 */
export const LOCAL_INSTANCE_NAME = "local";

export type InstanceKind = "local" | "remote";

/**
 * A MemQL an operator deploys to.
 *
 * `presence` is the same three-valued verdict clusters/presence.ts produces for
 * the local machine, reused for remote instances by way of `remotePresence`.
 * `connected` is a different question -- whether THIS editor currently holds a
 * live session -- and the two are independent: a healthy cluster you have
 * signed out of is `installed-healthy` and not connected.
 */
export interface Instance {
  /** The clusters.yaml slot key, or "local" for an unregistered local install. */
  name: string;
  /**
   * What a surface calls it (instanceLabel): the name the Clusters view shows
   * -- the list's display name -- or, for a local install with no list entry,
   * its domain. `name` is a KEY, and it is "local" for every local cluster, so
   * a page titled by it names nothing a person recognises.
   */
  label?: string;
  kind: InstanceKind;
  domain?: string;
  presence: PresenceVerdict;
  /**
   * Local: the release tag the receipt's `stackCheckout` step recorded, or --
   * when the receipt records no checkout at all -- the release clusters.yaml
   * says the cluster reported. Never set for a branch or commit install.
   * Remote: the current deployment's version.
   * Absent when it could not be resolved -- render it with displayVersion.
   */
  version?: string;
  /**
   * What a surface prints for the version (memql#4246 over the owner's
   * "local · not answering · unknown"): the release tag when there is one,
   * `main @ 3f2a9c1` for a branch install, `Your build 3f2a9c1` for a cluster
   * running images built from the checkout, the registry's recorded release
   * when the receipt names none. Absent when nothing names a version at all,
   * and a surface then says NOTHING about the version rather than printing
   * the word "unknown" -- the old rule, which put a false claim in the one
   * heading the owner read.
   *
   * `version` stays the tag, because it is what gets COMPARED (an upgrade is
   * offered from it, a forecast is computed from it); this is only what is
   * shown.
   */
  versionLabel?: string;
  connected: boolean;
  /**
   * Local: whether clusters.yaml carries a `local: true` row for it. A local
   * cluster that is installed but NOT in the list is reconnected, not signed
   * in to -- there is no row to sign in against.
   */
  registered?: boolean;
  /**
   * Remote: the deployment the cluster is running now
   * (deploymentHistory.currentDeploymentId). A rollback targets an OLDER
   * landed record than this one; targeting this one re-ships what is already
   * running.
   */
  currentDeploymentId?: string;
  /**
   * Remote: the record the Deploy button ships -- the newest CUT-but-unshipped
   * deployment (memql#4017). Absent when nothing is cut, when the records could
   * not be read, and always for a local instance, which has no deploy pipeline.
   *
   * It lives on the instance rather than being re-derived at the click so that
   * what ships is the record the page RENDERED. Deriving it again later means
   * deriving it from a catalog that may have moved -- which is right almost
   * always and wrong for whichever operator loses a race with a second cut.
   */
  pendingDeploymentId?: string;
  /**
   * Remote: the record Roll back returns to
   * (`deploymentHistory.rollbackTargetId`). Absent when nothing earlier
   * qualifies, when the records could not be read, and always for a local
   * instance. On the instance for the reason `pendingDeploymentId` is: what is
   * rolled back to is the record the page named.
   */
  rollbackTargetId?: string;
  /**
   * Local only (memql#4246). `checkout` is the directory the install cloned
   * (`recordedStackDir`); `imageSource` is which lane set the images last,
   * `released` or `checkout` (`recordedImageSource`); `rebuild` is the last
   * rebuild's own facts, present exactly when `imageSource === "checkout"`
   * (`recordedRebuild`). All three are DERIVED from the same receipt every
   * other local fact comes from -- there is no second place that decides a
   * local instance's image source.
   */
  imageSource?: ImageSource;
  checkout?: string;
  rebuild?: RecordedRebuild;
  /**
   * The branch the recorded checkout can be brought up to date with, or absent
   * when there is none (memql#5073).
   *
   * ABSENT IS THE COMMON CASE AND NOT A GAP: a release install is pinned to a
   * tag on purpose, and `recordedStackBranch` answers "" for it deliberately.
   * It is what tells "Update from origin and rebuild" apart from "Rebuild from
   * checkout" -- the second works on a pinned install, the first cannot,
   * because there is nothing to update TO.
   *
   * NOT DERIVABLE FROM `imageSource`, which is the trap it exists to avoid: a
   * release user who runs Rebuild from checkout becomes `imageSource=checkout`
   * while their checkout stays pinned to a tag.
   */
  checkoutBranch?: string;
  /**
   * The commit the install's `stackCheckout` step recorded (memql#5076).
   *
   * Distinct from `version`, which is the release TAG a tag install pinned. A
   * from-source install has no tag and this is the only thing that names what
   * is on disk -- and it is what the extension's own build commit is compared
   * against, in version/checkoutSkew.ts.
   */
  checkoutCommit?: string;
  /**
   * The commit THIS EXTENSION was packaged from, and whether that build carried
   * uncommitted edits (memql#5076).
   *
   * NOT FROM THE RECEIPT, unlike every other field here, and that is the point:
   * a receipt describes the install, and the question this answers is about the
   * thing driving it. It is injected by the panel from
   * version/buildStamp.readBuildStamp, and is ABSENT for an extension running
   * out of a checkout in the Extension Development Host -- which was never
   * packaged and therefore has no commit to name.
   */
  extensionCommit?: string;
  extensionDirty?: boolean;
}

/**
 * What kind of change a run made.
 *
 * The first four are local: an install graph run under one of its four verbs.
 * `rebuild` is also local, and outside the install graph entirely -- a
 * `k3d.dev` rebuild-from-checkout run (memql#4246), which is how a local
 * cluster starts running a developer's own edits instead of a release.
 * `update` is `rebuild` with the checkout brought up to date first
 * (memql#4578) -- a separate kind rather than a flavour of `rebuild`, because
 * the run log is what an operator reads to answer "what did I do to this
 * cluster", and "rebuilt from my checkout" and "moved my checkout and then
 * rebuilt" are two different answers.
 * `rollout` is every remote run, because a remote instance's runs are read
 * from `v1:cluster:deployment` and that concept records one kind of event.
 */
export type RunKind =
  | "install"
  | "upgrade"
  | "repair"
  | "uninstall"
  | "rebuild"
  | "update"
  | "rollout";

/**
 * Where a run ended up.
 *
 * The head three plus `cancelled` are reachable locally. `superseded` and
 * `rolled_back` are REMOTE-ONLY -- they are two of the six values of the
 * deployment concept's own status enum, and neither has a local analogue
 * because nothing supersedes a local install and there is no local rollback.
 *
 * `cancelled` is the exact mirror: local-only, because the deployment concept
 * has no such status. An operator can abandon an install; a deployment record
 * that stops advancing stays `in_progress`.
 *
 * `interrupted` is local-only for the same reason and is NOT a synonym for
 * either neighbour (memql#3886). A local run's record is rewritten after every
 * step, so a run whose extension host exits mid-flight leaves a file that says
 * `running` and always will -- nothing is left to write to it. Those records
 * rendered as a live spinner for work that ended hours earlier, which reads as
 * "still going" and makes an operator wait instead of retry.
 *
 * It is not `failed`: nothing failed, and filing it under failure would put a
 * healthy step's record in the list an operator scans for defects. It is not
 * `cancelled` either: that means somebody decided to stop, and the whole point
 * of this state is that nobody decided anything -- the editor went away.
 */
export type RunStatus =
  | "running"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "interrupted"
  | "superseded"
  | "rolled_back";

/**
 * What happened to one item within a run.
 *
 * The six states are the install executor's, carried over whole. `preserved`
 * is the one that cannot be folded into either success or failure: it means the
 * uninstall KEPT something because the operator already had it before MemQL
 * ever ran -- a k3d cluster, a mkcert CA, a checkout. Rounding it to "ok" tells
 * the operator the artifact is gone when it is still there; rounding it to
 * "skipped" loses the reason. The two-tier model that stops an uninstall
 * deleting a developer's own cluster is only legible if this state survives to
 * the surface that reports it.
 */
export type RunItemStatus = "pending" | "running" | "ok" | "failed" | "skipped" | "preserved";

export interface RunItem {
  /** An install-graph step id (local), or a nodeType (remote). */
  label: string;
  status: RunItemStatus;
  detail?: string;
  /** The item's latest transition: its start while running, its finish once settled. */
  at?: string;
  /**
   * When the step started, kept beside `at` once it has finished, so the record
   * holds the step's real duration (state/runProgress.ts weighs the next run's
   * progress bar by it). Absent on records written before it was kept.
   */
  startedAt?: string;
}

export interface Run {
  /** Local: extension-minted, see runLog.mintRunId. Remote: the deploymentId. */
  id: string;
  /** The Instance.name this run belongs to. */
  instance: string;
  kind: RunKind;
  fromVersion?: string;
  toVersion?: string;
  startedAt: string;
  finishedAt?: string;
  status: RunStatus;
  items: RunItem[];
}

/** The statuses that mean a run is over and will not change again. */
const TERMINAL_RUN_STATUSES = new Set<RunStatus>([
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
  "superseded",
  "rolled_back",
]);

export function runIsTerminal(status: RunStatus): boolean {
  return TERMINAL_RUN_STATUSES.has(status);
}

/**
 * A version, as a surface should print it.
 *
 * The word rather than the empty string, for the reason state/topology.ts gave
 * when it drew a node with no resolvable deployment: a blank is read as a fact
 * about the instance ("it has no version") when it is a fact about the read
 * ("we could not resolve one"). The two ask for different next actions.
 */
export function displayVersion(version: string | undefined): string {
  const value = (version ?? "").trim();
  return value === "" ? "unknown" : value;
}

// ---------------------------------------------------------------------------
// the local instance
// ---------------------------------------------------------------------------

export interface LocalInstanceInput {
  /** The verdict from clusters/presence.ts. */
  presence: PresenceVerdict;
  /** The install receipt, when one was read. Null when nothing installed from here. */
  receipt: Receipt | null;
  /** The `local: true` clusters.yaml row, when one is registered. */
  registered?: { name?: string; displayName?: string; domain?: string; version?: string };
  /** Whether this editor currently holds a live session against it. */
  connected: boolean;
  /**
   * This extension's own build stamp, when it was packaged (memql#5076).
   *
   * Threaded in rather than read here, for the reason every other module in
   * this directory is free of `vscode` imports: reading it needs the extension
   * PATH, which only the host knows.
   */
  buildStamp?: { commit: string; dirty: boolean };
}

/**
 * The local instance -- derived whether or not anything is installed.
 *
 * A machine with NO local cluster still yields one, carrying `absent`. It is
 * the local machine's entry in the catalog, and `absent` is a real verdict
 * about it rather than a missing one: the instance page opens on it, the
 * upgrade machinery reads it, and `instanceActions` uses exactly that verdict
 * to decide that the only thing on offer is an install.
 *
 * IT IS NO LONGER A ROW, and the argument that used to be written here went
 * with it (memql#4426). The Deployments view rendered a `local` row on every
 * machine, installed or not, so an operator with nothing had somewhere to
 * start; that row is what suppressed the view's welcome, and the welcome now
 * carries the install offer itself, alongside the Clusters welcome and the view
 * title menu. So this function no longer exists to guarantee a row -- it exists
 * because the local machine is an instance, and "nothing is installed" is
 * something to say about it.
 *
 * Domain and version are read back from what a previous run recorded rather
 * than defaulted. A default here would claim the machine is at a version it may
 * not be at -- and `recordedStackTag` exists precisely because assuming the
 * extension's own default tag once turned a repair into a silent upgrade.
 */
export function localInstance(input: LocalInstanceInput): Instance {
  const registeredName = (input.registered?.name ?? "").trim();
  const domain = (input.registered?.domain ?? "").trim() || recordedDomain(input.receipt);
  const tag = recordedStackTag(input.receipt);
  const checkout = recordedStackDir(input.receipt);
  const imageSource = recordedImageSource(input.receipt);
  const rebuild = recordedRebuild(input.receipt);
  const checkoutBranch = recordedStackBranch(input.receipt);
  const checkoutCommit = recordedStackCommit(input.receipt);
  // THE REGISTRY'S RECORDED RELEASE WHEN THE RECEIPT NAMES NOTHING. A cluster
  // with no receipt, or one whose receipt never recorded its checkout, has no
  // better answer than what the version learners wrote into clusters.yaml --
  // a fact about this cluster, which "unknown" is not.
  //
  // NEVER FOR A BRANCH OR COMMIT INSTALL. Its receipt names what it runs (the
  // label says `main @ 3f2a9c1`), and what the cluster reports is the nearest
  // release, not the code it serves. `version` is what an update is COMPARED
  // from, so borrowing the reported release would offer "Update to v0.24.0"
  // over a checkout of main -- a move to a tag, worded as an upgrade, from a
  // version the cluster is not on.
  const fromSource = tag === "" && (checkoutBranch !== "" || checkoutCommit !== "");
  const version = tag !== "" ? tag : fromSource ? "" : (input.registered?.version ?? "").trim();
  const label = localVersionLabel({
    tag,
    registryVersion: (input.registered?.version ?? "").trim(),
    branch: checkoutBranch,
    commit: checkoutCommit,
    ...(imageSource === "checkout" && rebuild !== undefined ? { rebuild } : {}),
  });
  // The Clusters view's own rule for a listed cluster (displayLabel); the
  // domain for one that is installed and not listed; the words when there is
  // neither.
  const displayName = (input.registered?.displayName ?? "").trim();
  const shown = displayName !== "" ? displayName : registeredName !== "" ? registeredName : domain !== "" ? domain : "Local cluster";
  return {
    name: registeredName !== "" ? registeredName : LOCAL_INSTANCE_NAME,
    label: shown,
    kind: "local",
    ...(domain !== "" ? { domain } : {}),
    presence: input.presence,
    ...(version !== "" ? { version } : {}),
    ...(label !== "" ? { versionLabel: label } : {}),
    connected: input.connected,
    registered: input.registered !== undefined,
    ...(checkout !== "" ? { checkout } : {}),
    ...(imageSource !== "" ? { imageSource } : {}),
    ...(rebuild !== undefined ? { rebuild } : {}),
    ...(checkoutBranch !== "" ? { checkoutBranch } : {}),
    ...(checkoutCommit !== "" ? { checkoutCommit } : {}),
    ...(input.buildStamp !== undefined ? { extensionCommit: input.buildStamp.commit } : {}),
    ...(input.buildStamp?.dirty ? { extensionDirty: true } : {}),
  };
}

/**
 * The version a local cluster shows, from what was recorded (see
 * `Instance.versionLabel`).
 *
 * ORDER IS WHAT IS RUNNING, MOST SPECIFIC FIRST. A rebuild from the checkout
 * outranks the tag the install pinned, because the tag is no longer what the
 * cluster serves. A branch install names its branch and commit rather than a
 * registry version the cluster once reported, because the commit is exact and
 * the reported version of a branch build is not a release.
 */
export function localVersionLabel(i: {
  tag: string;
  registryVersion: string;
  branch: string;
  commit: string;
  rebuild?: RecordedRebuild;
}): string {
  if (i.rebuild !== undefined && i.rebuild.commit.trim() !== "") {
    return `Your build ${i.rebuild.commit.trim().slice(0, 7)}`;
  }
  if (i.tag !== "") return i.tag;
  // A recorded commit is what runs: the reported release of a commit build is
  // the nearest tag, which the cluster is not on.
  const commit = i.commit.slice(0, 7);
  if (commit !== "") return i.branch !== "" ? `${i.branch} @ ${commit}` : commit;
  return i.registryVersion;
}

// ---------------------------------------------------------------------------
// remote instances
// ---------------------------------------------------------------------------

/**
 * A remote instance's presence, from whether it answers.
 *
 * A REMOTE IS NEVER `absent`. A clusters.yaml row is an operator's assertion
 * that a cluster is there; the extension cannot install one and so can never know
 * that it is not. "Declared but never reached" and "was reachable and now is
 * not" are the same actionable state -- it does not answer -- and collapsing
 * them keeps the verdict to the question the surface can actually resolve.
 */
export function remotePresence(reachable: boolean): PresenceVerdict {
  return reachable ? "installed-healthy" : "installed-unreachable";
}

export interface RemoteInstanceInput {
  /** The clusters.yaml slot key. */
  name: string;
  /** The list's display name, when it has one. */
  displayName?: string;
  domain?: string;
  /** Whether the cluster answered -- a live session, or a probe. */
  reachable: boolean;
  connected: boolean;
  /**
   * The deployment records read for this cluster, and the id of the current
   * one (deploymentHistory.currentDeploymentId). Empty when history has not
   * loaded or nothing has landed: the version then resolves to unknown, which
   * `displayVersion` renders as itself.
   */
  deployments?: DeploymentRecord[];
  currentDeploymentId?: string;
  /**
   * The record the Deploy button ships
   * (`deploymentHistory.pendingDeploymentId`). Empty when nothing is cut, and
   * absent when no records were read at all -- both land as no target on the
   * instance, because there is no record to name in either case.
   */
  pendingDeploymentId?: string;
  /**
   * The record Roll back returns to (`deploymentHistory.rollbackTargetId`),
   * empty or absent exactly as `pendingDeploymentId` is.
   */
  rollbackTargetId?: string;
  /**
   * The release clusters.yaml records for it, learned the last time this
   * editor could ask. Shown when the deployment records cannot be read -- a
   * cluster this editor is not connected to -- rather than no version at all.
   */
  registryVersion?: string;
}

/**
 * A remote instance, versioned by whichever deployment is current.
 *
 * The version is the CURRENT deployment's, not the newest record's: a deploy in
 * flight has not landed, and reporting the version it is heading towards would
 * tell an operator mid-deploy that the cluster is already there. Which record
 * is current is `deploymentHistory.currentDeploymentId`'s judgement and is not
 * re-derived here.
 *
 * The ship and rollback targets are the same arrangement: `pendingDeploymentId`
 * decides which record is cut-but-unshipped, `rollbackTargetId` which record
 * Roll back returns to, and this carries the answers. Derivations over one
 * record list, none of them made twice.
 */
export function remoteInstance(input: RemoteInstanceInput): Instance {
  const current = (input.currentDeploymentId ?? "").trim();
  const deployed =
    current === ""
      ? ""
      : (indexDeployments(input.deployments ?? []).get(current)?.version ?? "");
  const version = deployed !== "" ? deployed : (input.registryVersion ?? "").trim();
  const domain = (input.domain ?? "").trim();
  const pending = (input.pendingDeploymentId ?? "").trim();
  const rollback = (input.rollbackTargetId ?? "").trim();
  const displayName = (input.displayName ?? "").trim();
  return {
    name: input.name,
    label: displayName !== "" ? displayName : input.name,
    kind: "remote",
    ...(domain !== "" ? { domain } : {}),
    presence: remotePresence(input.reachable),
    ...(version !== "" ? { version, versionLabel: version } : {}),
    connected: input.connected,
    ...(current !== "" ? { currentDeploymentId: current } : {}),
    ...(pending !== "" ? { pendingDeploymentId: pending } : {}),
    ...(rollback !== "" ? { rollbackTargetId: rollback } : {}),
  };
}

/** The name a surface gives an instance: its label, else its key. */
export function instanceLabel(instance: Instance): string {
  const label = (instance.label ?? "").trim();
  return label !== "" ? label : instance.name;
}

/**
 * Instances in catalog order: local first, then remote by name.
 *
 * Local is pinned to the top rather than sorted with the rest because it is the
 * one instance that is always present -- including as `absent`, which is a
 * verdict about this machine and not an absence from the list.
 *
 * "The order the TREE renders them" until memql#4426, and it is worth saying
 * what changed: the Deployments view now renders one cluster's runs rather than
 * every instance, so nothing reads this order to lay out rows any more. It is
 * still the order the catalog hands instances back in, which is what a lookup
 * by name and the instance page both walk, so a stable, stated order is still
 * worth having -- it just no longer decides what an operator sees first.
 */
export function sortInstances(instances: readonly Instance[]): Instance[] {
  return [...instances].sort((a, b) => {
    if (a.kind !== b.kind) return a.kind === "local" ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
}

// ---------------------------------------------------------------------------
// remote runs, from deployment rows
// ---------------------------------------------------------------------------

/**
 * A deployment's status, as a run status.
 *
 * `pending` and `in_progress` both collapse to `running`: they are the deploy
 * concept's two pre-terminal states and the distinction between "cut" and
 * "shipping" is the instance page's to draw from the record, not a run outcome.
 * The other four are one-for-one with the concept's enum.
 *
 * AN UNRECOGNISED STATUS READS AS `running`, which is the only value that
 * asserts no outcome. The alternatives both invent one: `failed` manufactures
 * an alarm about a deploy that may have been perfectly fine, and `succeeded`
 * closes the book on one that may not have been. A row still moving is the
 * reading that invites the operator to go and look, which is what they should
 * do with a status this model has never seen.
 */
export function remoteRunStatus(deploymentStatus: string): RunStatus {
  switch (deploymentStatus) {
    case "succeeded":
      return "succeeded";
    case "failed":
      return "failed";
    case "superseded":
      return "superseded";
    case "rolled_back":
      return "rolled_back";
    case "pending":
    case "in_progress":
    default:
      return "running";
  }
}

/**
 * The status a remote run's node-type item carries.
 *
 * THERE IS NO PER-TIER OUTCOME IN THE DATA. A `deploymentNodeSpec` row is a
 * DECLARATION -- version, replicas, digest -- and the SDK exposes no `asOf`, so
 * a deployment's status transitions are not readable either (design §2.5).
 * What exists is one verdict for the whole deploy, and this function repeats it
 * per tier rather than pretending each tier was observed separately.
 *
 * `superseded` and `rolled_back` both map to `ok` because both describe a
 * deploy that LANDED and was later replaced. The tier did its work; what
 * happened afterwards is the run header's story, and marking the tier itself
 * failed would blame it for a subsequent decision.
 */
export function remoteItemStatus(status: RunStatus): RunItemStatus {
  switch (status) {
    case "running":
      return "running";
    case "failed":
      return "failed";
    case "cancelled":
      return "skipped";
    case "succeeded":
    case "superseded":
    case "rolled_back":
      return "ok";
    default:
      return "pending";
  }
}

/** The one-line description of a tier within a remote run. */
export function nodeSpecDetail(
  spec: DeploymentNodeSpec,
  deployment: DeploymentRecord | undefined,
): string {
  const resolved = resolveTierVersion(spec, deployment);
  const parts: string[] = [];
  if (resolved.version === "") {
    parts.push("version unknown");
  } else {
    // Saying WHICH of the two happened, because engine-as-spine makes a pinned
    // tier and an inherited one print the same string otherwise -- and the
    // difference is exactly what an operator is looking for when a single node
    // type is behaving differently from the rest.
    parts.push(`${resolved.version} (${resolved.inherited ? "inherited" : "pinned"})`);
  }
  parts.push(`${spec.replicas} ${spec.replicas === 1 ? "replica" : "replicas"}`);
  const digest = shortenDigest(spec.imageDigest);
  if (digest !== "") parts.push(`digest ${digest}`);
  return parts.join(" · ");
}

export interface RemoteRunsInput {
  instance: string;
  /** Projected deployment records (deploymentHistory.projectDeployments). */
  deployments: readonly DeploymentRecord[];
  /** Projected per-tier specs (deploymentHistory.projectNodeSpecs). */
  specs: readonly DeploymentNodeSpec[];
}

/**
 * A remote instance's runs, newest first.
 *
 * Nothing local is written for these: they ARE `v1:cluster:deployment` rows,
 * read the same way the concept browser reads any row. The run log (state/runLog.ts)
 * is for local runs alone, and mirroring remote deployments into it would create
 * a second, staler answer to a question the cluster already answers.
 *
 * The sort is re-derived rather than inherited from the caller, so a list that
 * was filtered or re-ordered upstream cannot silently produce a history in the
 * wrong order.
 */
export function runsFromDeployments(input: RemoteRunsInput): Run[] {
  const byId = indexDeployments([...input.deployments]);
  const specsByDeployment = new Map<string, DeploymentNodeSpec[]>();
  for (const spec of input.specs) {
    const bucket = specsByDeployment.get(spec.deploymentId);
    if (bucket === undefined) specsByDeployment.set(spec.deploymentId, [spec]);
    else bucket.push(spec);
  }

  const runs = input.deployments.map((record) => {
    const status = remoteRunStatus(record.status);
    const itemStatus = remoteItemStatus(status);
    const items: RunItem[] = (specsByDeployment.get(record.deploymentId) ?? [])
      .map((spec) => ({
        label: spec.nodeType,
        status: itemStatus,
        detail: nodeSpecDetail(spec, record),
        ...(spec.updatedAt !== "" ? { at: spec.updatedAt } : {}),
      }))
      .sort((a, b) => a.label.localeCompare(b.label));

    const from = byId.get(record.previousDeploymentId)?.version ?? "";
    const run: Run = {
      id: record.deploymentId,
      instance: input.instance,
      kind: "rollout",
      startedAt: record.createdAt,
      status,
      items,
    };
    if (from !== "") run.fromVersion = from;
    if (record.version !== "") run.toVersion = record.version;
    // `updatedAt` is the most recent status TRANSITION, so it is a finish time
    // only once the run has finished. On a deploy still in flight it is the
    // moment it last advanced, and reporting that as "finished" would draw a
    // running deploy as a completed one.
    if (runIsTerminal(status) && record.updatedAt !== "") run.finishedAt = record.updatedAt;
    return run;
  });

  return runs.sort((a, b) => {
    if (a.startedAt !== b.startedAt) return a.startedAt > b.startedAt ? -1 : 1;
    // A total order even for two records cut in the same instant, so the list
    // does not shuffle between renders for no reason an operator can see.
    return a.id.localeCompare(b.id);
  });
}

// ---------------------------------------------------------------------------
// local runs
// ---------------------------------------------------------------------------

/** A run in its opening state: started, nothing recorded yet. */
export function newLocalRun(input: {
  id: string;
  instance: string;
  kind: RunKind;
  startedAt: string;
  fromVersion?: string;
  toVersion?: string;
}): Run {
  const run: Run = {
    id: input.id,
    instance: input.instance,
    kind: input.kind,
    startedAt: input.startedAt,
    status: "running",
    items: [],
  };
  if (input.fromVersion !== undefined && input.fromVersion !== "") run.fromVersion = input.fromVersion;
  if (input.toVersion !== undefined && input.toVersion !== "") run.toVersion = input.toVersion;
  return run;
}

/**
 * A run with one item recorded, replacing any earlier record of the same label.
 *
 * UPSERT BY LABEL, because a step gets RETRIED: the install executor re-runs a
 * failed step and the operator retries a whole wave. Appending would leave the
 * failure and the success both in the record, and a reader deciding which one
 * counts would be re-implementing this rule somewhere less visible.
 *
 * Returns a new object rather than mutating, so a caller holding the previous
 * value (a renderer mid-frame) is unaffected.
 */
export function upsertRunItem(run: Run, item: RunItem): Run {
  const at = run.items.findIndex((existing) => existing.label === item.label);
  const items = [...run.items];
  if (at >= 0) items[at] = item;
  else items.push(item);
  return { ...run, items };
}

/**
 * The run's terminal status, from what its items did.
 *
 * ANY FAILURE FAILS THE RUN, and nothing else does. A `preserved` item is not a
 * failure -- it is the uninstall correctly declining to remove something the
 * operator already had -- and neither is a `skipped` one, which is a step whose
 * verify already held. A run with an item still `pending` or `running` has not
 * finished and this returns "running", so a caller cannot accidentally close a
 * record that is still open.
 */
export function settleRunStatus(run: Run): RunStatus {
  if (run.items.some((i) => i.status === "failed")) return "failed";
  if (run.items.some((i) => i.status === "pending" || i.status === "running")) return "running";
  return "succeeded";
}
