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
// Each is now offered only with its target in hand.
//
// WHAT A DEPLOY ACT SENDS IS NOT DECIDED HERE. A remote cluster's deploy
// controls -- Deploy, Prepare, Roll back, Promote and Abort -- are built by
// deploy/controls.ts, which resolves each one's target from what the page read
// and gives it a KEY. This file decides only WHERE each is drawn: Deploy and
// Roll back on the bar, a rollout's Promote and Abort on that rollout's row,
// the versions to prepare as a short list (`remoteChoices`). Every one posts
// `{ type: "deploy", value: <key> }`, and the panel re-expands the controls
// from its own facts and runs the one whose key matches.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3736 #3733

import { satisfiesTier, type DeployActionId, type RoleVisibility } from "./actions.js";
import type { VersionPreview } from "./controller.js";
import { deployControls, type DeployControl, type DeployControlFacts } from "./controls.js";
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

/**
 * What a page act posts as its `type`. `deploy` is every deploy control
 * (deploy/controls.ts), its `value` the control's key.
 */
export type PageActId = LocalActId | "update" | "connect" | "signIn" | "deploy" | "showRun" | "more";

/** One act on a bar: what the page posts is `{ type: id, value }`. */
export interface PageAct {
  id: PageActId;
  label: string;
  /** The act's target: a version, or a deploy control's key. */
  value?: string;
  /** The one button. Absent is a text act. */
  tone?: "primary" | "danger";
  /** What it does, as a tooltip, when the label says less. */
  title?: string;
  /** Its full name for assistive tech, when the label leans on its row. */
  ariaLabel?: string;
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
  /** The next-version proposals the Prepare choices name; undefined before (or without) the read. */
  preview?: VersionPreview;
}

/**
 * The facts a remote page's deploy controls are resolved from. The panel
 * resolves a click from the same four fields, so what a key names on the page
 * is what it names at the click.
 */
export function remoteControlFacts(i: Pick<RemoteBarInput, "instance" | "runs" | "pipeline" | "preview">): DeployControlFacts {
  return { instance: i.instance, runs: i.runs, rollouts: i.pipeline?.rollouts ?? [], preview: i.preview };
}

/**
 * The deploy controls of one action this page may draw: the role admits it,
 * the pipeline is there, and the control has a target to send. A control with
 * nothing to send (nothing prepared, nothing to roll back to) is ABSENT here;
 * the panel still refuses its key, in the control's own sentence, if one is
 * posted anyway.
 */
function drawable(
  i: { pipeline?: PipelineState | undefined; visibility?: RoleVisibility | undefined },
  facts: DeployControlFacts,
  id: DeployActionId,
): DeployControl[] {
  const pipeline = i.pipeline;
  if (pipeline === undefined || pipeline.kind !== "present" || !admits(i.visibility, pipeline, id)) return [];
  return deployControls([id], facts).filter((control) => control.request !== undefined);
}

/**
 * A deploy control as a page act. One that asks for a typed confirmation is
 * labelled with an ellipsis, as every act that asks for more is.
 */
export function controlAct(control: DeployControl, over: { label?: string; tone?: "primary" | "danger" } = {}): PageAct {
  const label = over.label ?? control.label;
  return {
    id: "deploy",
    label: control.confirm === "" ? label : `${label}…`,
    value: control.key,
    ...(over.tone === undefined ? {} : { tone: over.tone }),
    ...(control.detail === "" ? {} : { title: control.detail }),
    ...(label === control.label ? {} : { ariaLabel: control.label }),
  };
}

/** The deploy-control acts a remote cluster's bar offers, with the update. */
function remoteActs(i: RemoteBarInput): { primary: PageAct | undefined; texts: PageAct[] } {
  const pipeline = i.pipeline;
  if (pipeline === undefined) return { primary: undefined, texts: [] };
  const texts: PageAct[] = [];
  let primary: PageAct | undefined;

  // Deploy ships the record that is PREPARED AND NOT SHIPPED, by id -- and is
  // absent when there is none, rather than drawn beside a line saying so.
  const facts = remoteControlFacts(i);
  const deploy = drawable(i, facts, "deploy")[0];
  if (deploy !== undefined) primary = controlAct(deploy, { tone: "primary" });
  // The update prepares the named release and ships it, under one
  // confirmation. Not where the cluster has no deploy pipeline: both calls
  // would be refused.
  if (i.upgrade.kind === "offer" && pipeline.kind !== "notConfigured") {
    const update: PageAct = { id: "update", label: `${i.upgrade.label}…`, value: i.upgrade.target.to };
    if (primary === undefined) primary = { ...update, tone: "primary" };
    else texts.push(update);
  }
  // Roll back names the release it returns to (deploymentHistory's
  // rollbackTargetId), never the one running.
  const rollback = drawable(i, facts, "rollback")[0];
  if (rollback !== undefined) texts.push(controlAct(rollback));
  return { primary, texts };
}

/** A rollout part-way through, as a row carrying its own acts. */
export interface RolloutRow {
  name: string;
  /** Argo's phase, as reported ("Paused"). */
  phase: string;
  /** Promote, then Abort. */
  acts: PageAct[];
}

/** A version the cluster can be prepared at, as one choice. */
export interface VersionChoice {
  act: PageAct;
  /** The bump, as a quiet note ("minor"); "" when the label already says it. */
  note: string;
}

/**
 * The acts a remote page draws in its BODY, because each is about one item
 * rather than the cluster: a rollout's Promote and Abort on that rollout's row,
 * and the versions to prepare as a short list -- three of them, which would
 * fill the bar on their own.
 */
export interface RemoteChoices {
  /**
   * The rollouts in flight, one row each. Undefined when the group is not
   * drawn at all: the role cannot promote, or the cluster reported no
   * rollouts. Empty when it reported some and none is part-way through.
   */
  rollouts?: RolloutRow[];
  /** A choice per bump, in patch, minor, major order; empty when Prepare is not offered. */
  versions: VersionChoice[];
  /** Why the versions name only the bump: the preview read failed. "" otherwise. */
  previewMessage: string;
}

/** The remote page's per-item acts: rollout rows and versions to prepare. */
export function remoteChoices(i: RemoteBarInput): RemoteChoices {
  const none: RemoteChoices = { versions: [], previewMessage: "" };
  if (clusterState(i.instance, i.connection).key !== "connected") return none;
  const pipeline = i.pipeline;
  if (pipeline === undefined || pipeline.kind !== "present") return none;

  const facts = remoteControlFacts(i);
  const versions = drawable(i, facts, "cutVersion").map((control) => ({
    act: controlAct(control),
    note: control.note ?? "",
  }));
  const previewMessage = versions.length === 0 ? "" : (i.preview?.message ?? "");

  let rollouts: RolloutRow[] | undefined;
  if (admits(i.visibility, pipeline, "rolloutAction") && pipeline.rollouts.length > 0) {
    const controls = drawable(i, facts, "rolloutAction");
    rollouts = pipeline.rollouts
      .filter((rollout) => controls.some((control) => rolloutOf(control) === rollout.name))
      .map((rollout) => ({
        name: rollout.name,
        phase: rollout.phase,
        // The row names the rollout, so its acts say only the verb; their
        // accessible names keep the whole of it ("Promote bff").
        acts: controls
          .filter((control) => rolloutOf(control) === rollout.name)
          .map((control) => controlAct(control, { label: subActionOf(control) === "abort" ? "Abort" : "Promote" })),
      }));
  }
  return { ...(rollouts === undefined ? {} : { rollouts }), versions, previewMessage };
}

function rolloutOf(control: DeployControl): string {
  return control.request?.id === "rolloutAction" ? control.request.rollout : "";
}

function subActionOf(control: DeployControl): string {
  return control.request?.id === "rolloutAction" ? control.request.subAction : "";
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
 *   the remote run Roll back returns to -> Roll back to it;
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
  // Roll back is offered on the page of the run it returns to -- the one
  // deploymentHistory.rollbackTargetId names -- and on no other: the same
  // control the cluster's bar draws, key and all.
  if (i.connection === "connected" && i.run.id === (i.instance.rollbackTargetId ?? "")) {
    const facts: DeployControlFacts = { instance: i.instance, runs: [i.run], rollouts: [], preview: undefined };
    const rollback = drawable(i, facts, "rollback")[0];
    if (rollback !== undefined) return { ...base, acts: [controlAct(rollback, { tone: "danger" })], more: [] };
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
 * the page never drew -- or an update aimed at a different version than the
 * one named on the button -- is dropped here rather than run. A deploy
 * control is not narrowed here: the panel resolves its key against a fresh
 * expansion of deploy/controls.ts, which is the stricter check.
 */
export function barOffers(barValue: PageBar, id: string, value: string | undefined): PageAct | undefined {
  return [...barValue.acts, ...barValue.more].find(
    (act) => act.id === id && act.id !== "more" && (act.value ?? "") === (value ?? ""),
  );
}
