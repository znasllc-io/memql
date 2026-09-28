// What a cluster offers, given what it is and where this editor stands with it
// -- the ONE authority on which acts a Deployments surface draws.
//
// Two questions, answered here and nowhere else:
//
//  1. WHICH LIFECYCLE ACTS ARE LEGAL for the local cluster (`localActs`). The
//     title menu's `when` clauses, the palette commands and the page all narrow
//     against this, so no surface offers an act another one withholds.
//
//     Action               absent   installed        unreceipted
//     Install              yes      --               --
//     Change version       --       yes              --
//     Repair               --       yes              --
//     Rebuild from ckout   --       with a checkout  --
//     Pull and rebuild     --       with a branch    --
//     Uninstall            --       yes              yes (a delete)
//     Reconnect            --       when not listed  --
//     Connect (adopt)      --       --               yes
//
//  2. WHAT A PAGE'S ACTION BAR SAYS AND OFFERS (`localOverviewBar`,
//     `remoteOverviewBar`, `runDetailBar`): the state in words and the acts
//     legal from it, at most three with one primary. Every other legal act
//     goes behind "More" (a quick pick) rather than being dropped, so the cap
//     costs a click and never a capability.
//
// TWO RULES DECIDE EVERY CELL.
//
//  - PRESENCE IS A REAL GATE -- on this machine, not on anyone's authority.
//    Install appears for `absent` and for nothing else, because an install run
//    over a cluster that exists rebuilds a k3d cluster, a hosts block and a
//    trust-store CA underneath a working stack. Uninstall is the complement.
//
//  - THE ROLE TIER IS A COURTESY, NEVER A CONTROL. Every tier mirrors
//    `component/deploycontrol/service.go` so a page can hide what a caller
//    cannot use; the engine refuses on its own, naming the role required
//    (src/deploy/actions.ts states the doctrine).
//
// AN ACT WHOSE ONLY OUTCOME IS A REFUSAL IS ABSENT. That is what put three bugs
// right here: Deploy offered with nothing prepared, a rollout promote with no
// rollout to name, and a rollback aimed at the deployment already running.
// Each is now offered only with its target in hand, and carries it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3736 #3733

import { satisfiesTier, type DeployActionId, type RoleVisibility } from "./actions.js";
import type { PipelineState } from "./pipelineState.js";
import type { UpgradeVerdict } from "./upgrade.js";
import type { Instance, Run } from "../state/deployments.js";
import {
  clusterState,
  runRowStatus,
  type ConnectionWord,
  type StateTone,
} from "../state/deploymentsCatalog.js";

/**
 * Which machinery moves a cluster to a version: the install graph re-run at a
 * tag on this machine, or a deploy-control cut-and-ship against a remote.
 * Named after what runs, so a caller cannot route an update into the full
 * install graph -- the one mistake that rebuilds a working cluster.
 */
export type InstanceActionFlow = "upgradeToTag" | "deployControl";

export function moveFlowFor(instance: Instance): InstanceActionFlow {
  return instance.kind === "local" ? "upgradeToTag" : "deployControl";
}

// ---------------------------------------------------------------------------
// the local cluster's lifecycle
// ---------------------------------------------------------------------------

export type LocalActId =
  | "install"
  | "adopt"
  | "reconnect"
  | "repair"
  | "uninstall"
  | "changeVersion"
  | "rebuildFromCheckout"
  | "updateAndRebuild";

/**
 * The lifecycle acts legal for a local cluster right now.
 *
 * A REBUILD NEEDS SOMETHING TO BUILD FROM (a recorded checkout), and a PULL
 * NEEDS SOMETHING TO PULL (a branch) -- a strictly narrower condition, because
 * a release install has a checkout pinned to a tag and nothing to update it to
 * (memql#5073). A cluster found here that no install recorded can only be
 * adopted as it is, or deleted: repair and change version have nothing to
 * reverse or replay (memql#5118).
 */
export function localActs(instance: Instance): LocalActId[] {
  if (instance.kind !== "local") return [];
  if (instance.presence === "absent") return ["install"];
  if (instance.presence === "present-unreceipted") return ["adopt", "uninstall"];
  const acts: LocalActId[] = ["changeVersion", "repair", "uninstall"];
  const hasCheckout = (instance.checkout ?? "") !== "";
  if (hasCheckout) acts.push("rebuildFromCheckout");
  if (hasCheckout && (instance.checkoutBranch ?? "") !== "") acts.push("updateAndRebuild");
  if (instance.registered === false) acts.push("reconnect");
  return acts;
}

export function offersLocal(instance: Instance, id: LocalActId): boolean {
  return localActs(instance).includes(id);
}

// ---------------------------------------------------------------------------
// what an action bar offers
// ---------------------------------------------------------------------------

export type PageActId =
  | LocalActId
  | "update"
  | "connect"
  | "signIn"
  | "deploy"
  | "cutVersion"
  | "rolloutPromote"
  | "rolloutAbort"
  | "rollback"
  | "showRun"
  | "more";

/** One act on a bar: what the page posts is `{ type: id, value }`. */
export interface PageAct {
  id: PageActId;
  label: string;
  /** The act's target: a version, a deployment id, a rollout name. */
  value?: string;
  /** The one button. Absent is a text act. */
  tone?: "primary" | "danger";
}

export interface PageBar {
  /** The state in words: "Connected", "Not signed in". */
  state: string;
  /** What it means, in one clause, when the word needs it. */
  detail?: string;
  tone: StateTone;
  /** At most three, the button last. */
  acts: PageAct[];
  /** The legal acts that did not fit, offered from the "More" act. */
  more: PageAct[];
}

/** The "More" act, when there is more than fits. */
const MORE: PageAct = { id: "more", label: "More…" };

/**
 * At most three acts, the button last; the rest behind More.
 *
 * `texts` are in priority order. When everything fits it is drawn as given;
 * when it does not, the button and the first text stay and the rest go behind
 * More -- so the cap on the bar is never a cap on what can be done.
 */
function fit(primary: PageAct | undefined, texts: readonly PageAct[]): { acts: PageAct[]; more: PageAct[] } {
  const room = primary === undefined ? 3 : 2;
  if (texts.length <= room) return { acts: [...texts, ...(primary === undefined ? [] : [primary])], more: [] };
  const keep = texts.slice(0, room - 1);
  return { acts: [...keep, MORE, ...(primary === undefined ? [] : [primary])], more: texts.slice(room - 1) };
}

function bar(state: ReturnType<typeof clusterState>, primary: PageAct | undefined, texts: PageAct[]): PageBar {
  return { state: state.word, tone: state.tone, ...fit(primary, texts) };
}

const CHANGE_VERSION: PageAct = { id: "changeVersion", label: "Change version…" };
const PULL_AND_REBUILD: PageAct = { id: "updateAndRebuild", label: "Pull and rebuild…" };
const REBUILD: PageAct = { id: "rebuildFromCheckout", label: "Rebuild from checkout…", tone: "primary" };

export interface LocalBarInput {
  instance: Instance;
  connection: ConnectionWord;
  upgrade: UpgradeVerdict;
}

/**
 * The local cluster page's bar.
 *
 * THE PRIMARY IS WHAT THE STATE ASKS FOR. Not installed: Install. Found but
 * not ours: Connect to it as it is. Not in the list: Reconnect. Signed out:
 * Sign in. Not running: Repair. Connected: Rebuild when it runs your own
 * build (that is what refreshes it), otherwise Update when a newer release
 * exists, otherwise nothing -- a cluster that needs nothing is not handed a
 * button.
 *
 * Repair, Uninstall and Rebuild on a released cluster are not on the bar; they
 * are in the Deployments title menu and the palette, which open the same flows.
 */
export function localOverviewBar(i: LocalBarInput): PageBar {
  const { instance } = i;
  const state = clusterState(instance, i.connection);
  const acts = localActs(instance);
  const change = acts.includes("changeVersion") ? [CHANGE_VERSION] : [];
  switch (state.key) {
    case "notInstalled":
      return bar(state, { id: "install", label: "Install", tone: "primary" }, []);
    case "unreceipted":
      return bar(state, { id: "adopt", label: "Connect", tone: "primary" }, [{ id: "uninstall", label: "Delete…" }]);
    case "notListed":
      return bar(state, { id: "reconnect", label: "Reconnect", tone: "primary" }, change);
    case "signIn":
      return bar(state, { id: "signIn", label: "Sign in", tone: "primary" }, change);
    case "notRunning":
      return bar(state, { id: "repair", label: "Repair", tone: "primary" }, change);
    case "notConnected":
    case "cantReach":
      return bar(state, { id: "connect", label: "Connect", tone: "primary" }, change);
    case "connecting":
      return bar(state, undefined, []);
    case "notSetUp":
      return bar(state, undefined, change);
    case "connected": {
      const texts: PageAct[] = [];
      if (acts.includes("updateAndRebuild")) texts.push(PULL_AND_REBUILD);
      texts.push(...change);
      let primary: PageAct | undefined;
      if (instance.imageSource === "checkout" && acts.includes("rebuildFromCheckout")) primary = REBUILD;
      else if (i.upgrade.kind === "offer") primary = { id: "update", label: `${i.upgrade.label}…`, value: i.upgrade.target.to, tone: "primary" };
      return bar(state, primary, texts);
    }
  }
}

// ---------------------------------------------------------------------------
// a remote cluster
// ---------------------------------------------------------------------------

/**
 * The deployment a rollback lands on: the newest SUCCEEDED run that is not the
 * one running now.
 *
 * NOT the newest succeeded run, which is the current deployment itself: the
 * engine's RollbackDeployment redeploys the target's digest as a new record,
 * so aiming it there re-ships what is already running. Undefined when there is
 * no current deployment to roll back from, or nothing older that landed.
 */
export function rollbackTarget(runs: readonly Run[], currentId: string | undefined): Run | undefined {
  const current = (currentId ?? "").trim();
  if (current === "") return undefined;
  const at = runs.findIndex((run) => run.id === current);
  if (at < 0) return undefined;
  return runs.slice(at + 1).find((run) => run.status === "succeeded");
}

/** Whether the caller's role admits an action -- an indeterminate role admits everything. */
function admits(visibility: RoleVisibility | undefined, pipeline: PipelineState, id: DeployActionId): boolean {
  if (!pipeline.actions.some((action) => action.id === id)) return false;
  if (visibility === undefined || visibility.kind === "indeterminate") return true;
  const spec = pipeline.actions.find((action) => action.id === id);
  return spec !== undefined && satisfiesTier(visibility.role, spec.tier);
}

export interface RemoteBarInput {
  instance: Instance;
  connection: ConnectionWord;
  upgrade: UpgradeVerdict;
  /** Undefined until the status read lands. */
  pipeline: PipelineState | undefined;
  visibility?: RoleVisibility;
  /** Newest first. */
  runs: readonly Run[];
}

/** A deploy-control act a remote cluster offers, with its target. */
function remoteActs(i: RemoteBarInput): { primary: PageAct | undefined; texts: PageAct[] } {
  const pipeline = i.pipeline;
  if (pipeline === undefined) return { primary: undefined, texts: [] };
  const texts: PageAct[] = [];
  let primary: PageAct | undefined;

  // Deploy ships the record that is PREPARED AND NOT SHIPPED, by id -- and is
  // absent when there is none, rather than drawn beside a line saying so.
  const pending = (i.instance.pendingDeploymentId ?? "").trim();
  if (pipeline.kind === "present" && pending !== "" && admits(i.visibility, pipeline, "deploy")) {
    const version = i.runs.find((run) => run.id === pending)?.toVersion ?? "";
    primary = { id: "deploy", label: version === "" ? "Deploy" : `Deploy ${version}`, value: pending, tone: "primary" };
  }
  // The update cuts at the named release and ships it, under one confirmation.
  // Not where the cluster has no deploy pipeline: both calls would be refused.
  if (i.upgrade.kind === "offer" && pipeline.kind !== "notConfigured") {
    const update: PageAct = { id: "update", label: `${i.upgrade.label}…`, value: i.upgrade.target.to };
    if (primary === undefined) primary = { ...update, tone: "primary" };
    else texts.push(update);
  }
  if (pipeline.kind === "present" && admits(i.visibility, pipeline, "cutVersion")) {
    const cut: PageAct = { id: "cutVersion", label: "Prepare next version" };
    if (primary === undefined) primary = { ...cut, tone: "primary" };
    else texts.push(cut);
  }
  if (pipeline.kind === "present" && admits(i.visibility, pipeline, "rollback")) {
    const target = rollbackTarget(i.runs, i.instance.currentDeploymentId);
    if (target !== undefined) {
      const to = target.toVersion ?? "";
      texts.push({ id: "rollback", label: to === "" ? "Roll back…" : `Roll back to ${to}…`, value: target.id });
    }
  }
  // Promote and abort only NAME a rollout that is part-way through: the engine
  // refuses a blank one, which is all the old button ever sent.
  if (pipeline.kind === "present" && admits(i.visibility, pipeline, "rolloutAction")) {
    for (const rollout of pipeline.rollouts) {
      texts.push({ id: "rolloutPromote", label: `Promote ${rollout}`, value: rollout });
      texts.push({ id: "rolloutAbort", label: `Abort ${rollout}…`, value: rollout });
    }
  }
  return { primary, texts };
}

/** The remote cluster page's bar. */
export function remoteOverviewBar(i: RemoteBarInput): PageBar {
  const state = clusterState(i.instance, i.connection);
  switch (state.key) {
    case "signIn":
      return bar(state, { id: "signIn", label: "Sign in", tone: "primary" }, []);
    case "notConnected":
      return bar(state, { id: "connect", label: "Connect", tone: "primary" }, []);
    case "cantReach":
      return bar(state, { id: "connect", label: "Retry", tone: "primary" }, []);
    case "connected": {
      const { primary, texts } = remoteActs(i);
      return bar(state, primary, texts);
    }
    default:
      return bar(state, undefined, []);
  }
}

// ---------------------------------------------------------------------------
// one run
// ---------------------------------------------------------------------------

export interface RunDetailBarInput {
  instance: Instance;
  run: Run;
  connection: ConnectionWord;
  pipeline?: PipelineState;
  visibility?: RoleVisibility;
  /** A local run is going on this machine now (not this one: that shows live). */
  runInFlight: boolean;
}

/**
 * A recorded run's bar: its outcome in a word, and only the acts about THIS
 * run.
 *
 * A READ-ONLY RECORD OFFERS NAVIGATION, NOT THE CLUSTER'S WHOLE SET. The old
 * detail page repeated every instance act, and its Roll back ignored the run
 * on screen. Now:
 *
 *   a local run that failed or was interrupted -> Retry, which is the act the
 *     run came from (a version change to the same version, the same rebuild,
 *     a repair), and only when that act is legal now;
 *   a remote run that landed and is not the one running -> Roll back to it;
 *   a run going now elsewhere on this machine -> Show progress, and nothing
 *     that could start a second run beside it.
 */
export function runDetailBar(i: RunDetailBarInput): PageBar {
  const row = runRowStatus(i.run, 0, { prepared: i.run.id === (i.instance.pendingDeploymentId ?? "") });
  const base = { state: row.statusWord, tone: row.tone };
  if (i.instance.kind === "local") {
    if (i.runInFlight) return { ...base, acts: [{ id: "showRun", label: "Show current run" }], more: [] };
    if (i.run.status !== "failed" && i.run.status !== "interrupted") return { ...base, acts: [], more: [] };
    const retry = retryFor(i.instance, i.run);
    return { ...base, acts: retry === undefined ? [] : [retry], more: [] };
  }
  const pipeline = i.pipeline;
  if (
    i.connection === "connected" &&
    pipeline !== undefined &&
    pipeline.kind === "present" &&
    i.run.status === "succeeded" &&
    i.run.id !== (i.instance.currentDeploymentId ?? "") &&
    admits(i.visibility, pipeline, "rollback")
  ) {
    const to = i.run.toVersion ?? "";
    return {
      ...base,
      acts: [{ id: "rollback", label: to === "" ? "Roll back to this…" : `Roll back to ${to}…`, value: i.run.id, tone: "danger" }],
      more: [],
    };
  }
  return { ...base, acts: [], more: [] };
}

/** The act that re-attempts a local run, labelled Retry, when it is legal now. */
function retryFor(instance: Instance, run: Run): PageAct | undefined {
  const legal = localActs(instance);
  const act = (id: LocalActId, value?: string): PageAct | undefined =>
    legal.includes(id) ? { id, label: "Retry", tone: "primary", ...(value === undefined ? {} : { value }) } : undefined;
  switch (run.kind) {
    case "install":
      return legal.includes("install") ? act("install") : act("repair");
    case "repair":
      return act("repair");
    case "uninstall":
      return act("uninstall");
    case "upgrade":
      return (run.toVersion ?? "") === "" ? undefined : act("changeVersion", run.toVersion);
    case "rebuild":
      return act("rebuildFromCheckout");
    case "update":
      return act("updateAndRebuild");
    case "rollout":
      return undefined;
  }
}

/**
 * Whether a posted act is one the bar offered, target and all.
 *
 * The webview is a separate process and its messages are untrusted: an act
 * the page never drew -- or a rollback aimed at a different deployment than
 * the one named on the button -- is dropped here rather than run.
 */
export function barOffers(barValue: PageBar, id: string, value: string | undefined): PageAct | undefined {
  return [...barValue.acts, ...barValue.more].find(
    (act) => act.id === id && act.id !== "more" && (act.value ?? "") === (value ?? ""),
  );
}
