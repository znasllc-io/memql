// What a Deployments instance offers, given what it is and what state it is in.
//
// One table, read off design §5.2:
//
//   Action                 local, absent  local, installed          remote
//   Create deployment      install graph  stackCheckout + up        deploy-control Deploy
//   Repair                 --             re-run graph              --
//   Rebuild from checkout  --             k3d.dev over the checkout --
//   Update and rebuild     --             the same, latest first    --
//   Uninstall              --             uninstall graph           --
//   Cut version            --             --                        developer+
//   Promote                --             --                        admin+
//   Rollout action         --             --                        admin+
//   Rollback               --             --                        owner only
//
// TWO RULES DECIDE EVERY CELL.
//
//  1. PRESENCE DECIDES WHAT IS OFFERED, AND IT IS A REAL GATE -- but a gate on
//     this machine, not on anyone's authority. Install appears for `absent` and
//     for nothing else, because an install run over a cluster that already
//     exists is not a wasted click: it is a k3d cluster, a hosts block and a
//     trust-store CA rebuilt underneath a working parity stack. Uninstall is
//     the exact complement.
//
//  2. THE ROLE TIER IS A COURTESY, NEVER A CONTROL. Every tier below mirrors
//     `component/deploycontrol/service.go` and exists so the UI can hide an
//     action a caller cannot use. The real refusal arrives from the engine
//     naming the role required. src/deploy/actions.ts states this doctrine for
//     the deploy surface; this file extends it rather than amending it, and
//     takes its tiers from that catalog instead of re-spelling them, so the two
//     cannot drift apart.
//
// "Create deployment" is ONE action with three flows rather than three actions,
// and that is the design's whole claim: a deployment moves an instance to a
// version, and installing, upgrading a pinned checkout and shipping a remote
// release are the same verb reaching different machinery. The flow says which
// machinery; the label does not change, because the operator's intent did not.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3736 #3733

import {
  actionById,
  satisfiesTier,
  type DeployActionId,
  type RoleTier,
  type RoleVisibility,
} from "./actions.js";
import type { Instance, Run } from "../state/deployments.js";

export type InstanceActionId =
  | "createDeployment"
  | "repair"
  | "rebuildFromCheckout"
  | "updateAndRebuild"
  | "uninstall"
  | "cutVersion"
  | "rolloutAction"
  | "rollback";

/**
 * Which machinery an action reaches.
 *
 * Named after what runs, not after what it is called, so a panel switching on
 * this cannot accidentally route an upgrade into the full install graph -- the
 * one mistake here that rebuilds a working cluster.
 */
export type InstanceActionFlow =
  /** The full install graph, every step. */
  | "installGraph"
  /** `stackCheckout` at a chosen tag, then `clusterUp`. */
  | "upgradeToTag"
  /** The install graph again: every step verifies first and skips when satisfied. */
  | "repairGraph"
  /** The one-step rebuild graph: k3d.dev over the recorded checkout, image-source=checkout. */
  | "rebuildGraph"
  /** The two-step graph: bring the recorded checkout up to date, then rebuild from it. */
  | "updateRebuildGraph"
  /** The uninstall graph, behind its removal preview. */
  | "uninstallGraph"
  /** A deploy-control RPC against the target cluster. */
  | "deployControl";

export interface InstanceAction {
  id: InstanceActionId;
  label: string;
  detail: string;
  flow: InstanceActionFlow;
  /**
   * The engine tier this action MIRRORS. Present only for deploy-control
   * actions -- a local action runs on the operator's own machine and there is
   * no cluster role to mirror.
   */
  tier?: RoleTier;
  /** The deploy-control action driven, when `flow` is "deployControl". */
  deployAction?: DeployActionId;
  /**
   * Whether the action makes the operator type a phrase back.
   *
   * Read off `DEPLOY_ACTIONS` rather than decided here, so the run detail page
   * can draw the destructive one destructively without a second list of which
   * actions those are. Present only for deploy-control actions; a local action
   * that needs a confirmation gets it from the flow it opens.
   */
  typeToConfirm?: boolean;
}

/** "Create deployment", as the local machine's three states reach it. */
const CREATE_LOCAL_ABSENT: InstanceAction = {
  id: "createDeployment",
  label: "Create deployment",
  detail: "Install a local MemQL cluster on this machine.",
  flow: "installGraph",
};

const CREATE_LOCAL_INSTALLED: InstanceAction = {
  id: "createDeployment",
  label: "Create deployment",
  // Named as a MOVE rather than as "upgrade", because the tag list is not
  // filtered to newer tags: going back to a previous release is the same
  // operation and the same button, and calling it an upgrade would make the
  // one that matters during an incident read as the wrong control.
  detail: "Move this cluster to another release tag.",
  flow: "upgradeToTag",
};

const REPAIR: InstanceAction = {
  id: "repair",
  label: "Repair",
  // Re-running the graph IS the repair -- every step verifies first and skips
  // when already satisfied -- so this is the same graph the install runs, not a
  // second implementation that could disagree with it.
  detail: "Re-run the install steps. Anything already in place is left alone.",
  flow: "repairGraph",
};

/**
 * "Rebuild from checkout" (memql#4246) -- the only action that takes a cluster
 * OFF released images.
 *
 * A wizard install runs released images pulled at a tag, and the checkout it
 * cloned sits there inert. This is what makes it run: build the node images
 * from that checkout, import them, point the Application at them, restart.
 *
 * It is NOT a fourth "Create deployment" flow, and the label says so. The other
 * three move a cluster BETWEEN releases -- same lane, different version; this
 * changes which lane the cluster is in, which is a different question and gets
 * its own name rather than hiding inside a verb that means "move to a version".
 */
const REBUILD: InstanceAction = {
  id: "rebuildFromCheckout",
  label: "Rebuild from checkout",
  detail: "Build images from the recorded checkout, import them, and roll the cluster onto them.",
  flow: "rebuildGraph",
};

/**
 * "Update from origin and rebuild" (memql#4578) -- the same crossing as REBUILD,
 * with the latest code in it.
 *
 * IT IS NOT A MODE OF REBUILD, and the two live side by side because they
 * answer two different questions. "Test just what I have" is the offline one
 * and is what a developer wants mid-change; "test the latest with what I have"
 * is what they want before they push. Folding the second into the first as a
 * checkbox would put a network fetch and a moving working tree behind a button
 * whose label promises neither.
 *
 * IT IS OFFERED UNDER THE SAME GATE. A rebuild needs something to build from;
 * this needs the same thing and nothing more. Whether the checkout can actually
 * be moved -- a branch to move to, no merge half-finished, no colliding edits
 * -- is answered by the checklist and then by the run, both of which can say
 * WHY. Withholding the button on those grounds would be the panel guessing at a
 * refusal it is not the authority on.
 */
const UPDATE_AND_REBUILD: InstanceAction = {
  id: "updateAndRebuild",
  label: "Update from origin and rebuild",
  detail:
    "Bring the recorded checkout up to date, then build images from it and roll the cluster onto them.",
  flow: "updateRebuildGraph",
};

const UNINSTALL: InstanceAction = {
  id: "uninstall",
  label: "Uninstall",
  detail: "Remove it from this machine. You will see exactly what goes first.",
  flow: "uninstallGraph",
};

/**
 * The deploy-control actions a remote instance offers.
 *
 * The label and description come from DEPLOY_ACTIONS via `actionById`, so the
 * wording an operator reads here is the wording the deploy surface already
 * uses, and the tier is read off the same row the engine's gate is mirrored in.
 */
function fromDeployAction(
  id: InstanceActionId,
  deployAction: DeployActionId,
  overrides: { label?: string; detail?: string } = {},
): InstanceAction {
  const spec = actionById(deployAction);
  return {
    id,
    label: overrides.label ?? spec.label,
    detail: overrides.detail ?? spec.description,
    flow: "deployControl",
    tier: spec.tier,
    deployAction,
    typeToConfirm: spec.typeToConfirm,
  };
}

const REMOTE_ACTIONS: readonly InstanceAction[] = [
  fromDeployAction("createDeployment", "deploy", {
    label: "Create deployment",
    detail:
      "Ship the selected pending deployment record. Asynchronous: success means accepted and kicked off, not deployed.",
  }),
  fromDeployAction("cutVersion", "cutVersion"),
  fromDeployAction("rolloutAction", "rolloutAction"),
  fromDeployAction("rollback", "rollback"),
];

/**
 * The actions this instance offers.
 *
 * `visibility` is consulted for remote instances only, and an INDETERMINATE
 * role offers everything -- the same call src/deploy/actions.ts makes, for the
 * same reason: a caller whose role could not be read may well be an owner, and
 * hiding the surface would lock them out of something they are entitled to
 * while the engine would have refused anything they are not.
 *
 * A local instance ignores it entirely. Nothing here asks a cluster for
 * permission to change the machine it is running on.
 */
export function instanceActions(
  instance: Instance,
  visibility?: RoleVisibility,
): InstanceAction[] {
  if (instance.kind === "local") {
    // A REBUILD NEEDS SOMETHING TO BUILD FROM, and that is the whole gate on it
    // (memql#4246). `checkout` is `recordedStackDir`, which is "" for a machine
    // registered by hand and for an install that never reached the clone step.
    // Offering a button whose only possible outcome is a refusal teaches an
    // operator that the extension is broken.
    const hasCheckout = (instance.checkout ?? "") !== "";
    const rebuild = hasCheckout ? [REBUILD, UPDATE_AND_REBUILD] : [];
    return instance.presence === "absent"
      ? [CREATE_LOCAL_ABSENT]
      : // `installed-unreachable` gets the same set as `installed-healthy`:
        // something is on the machine either way, and repair is precisely the
        // action for the one that is not answering. Ordering puts it first
        // there, since an operator looking at a broken cluster came to fix it.
        instance.presence === "installed-unreachable"
        ? [REPAIR, CREATE_LOCAL_INSTALLED, ...rebuild, UNINSTALL]
        : [CREATE_LOCAL_INSTALLED, REPAIR, ...rebuild, UNINSTALL];
  }

  if (visibility === undefined || visibility.kind === "indeterminate") {
    return [...REMOTE_ACTIONS];
  }
  return REMOTE_ACTIONS.filter(
    (action) => action.tier === undefined || satisfiesTier(visibility.role, action.tier),
  );
}

/**
 * The same actions, ORDERED FOR THE RUN AN OPERATOR IS LOOKING AT
 * (memql#4427).
 *
 * THIS ADDS NO VERBS AND REMOVES NONE. It is `instanceActions` with one entry
 * moved to the front, and that constraint is the whole design: a detail page
 * that composed its own set would become a second authority on what an
 * instance offers, and the first thing a second authority does is offer a
 * button the first one withheld -- which is how the doctrine at the top of this
 * file ("never offer a button whose only outcome is a refusal") gets broken
 * without anybody editing the table it is written beside.
 *
 * THE TABLE, and it is read top to bottom:
 *
 *   remote instance          -- untouched. The deploy-control set arrives
 *                               already filtered by the caller's role and
 *                               already in the order deploy/actions.ts states;
 *                               rollback stays owner-only there, not here.
 *   local, run failed        -- Repair leads. Re-running the graph is what
 *                               answers a failed local run, and the operator
 *                               opened this page because of the failure.
 *   local, checkout lane     -- Rebuild From Checkout leads. The cluster is
 *                               running a developer's own build, so the verb
 *                               that refreshes it is the one they came for.
 *   otherwise                -- the instance's own order stands.
 *
 * FAILURE OUTRANKS THE LANE, deliberately. The lane is a standing fact about
 * the cluster and will still be true tomorrow; the failure is a fact about THIS
 * run, which is the thing the page is about. Leading with Rebuild over a failed
 * run would answer a question the operator did not ask.
 *
 * A LEAD THE INSTANCE DOES NOT OFFER IS SIMPLY NOT APPLIED. An `absent` local
 * instance offers only Create deployment, and a machine with no recorded
 * checkout is offered no rebuild (see `instanceActions`) -- so the reordering
 * finds nothing to move and the set is returned as it came. That is what makes
 * "no button whose only outcome is a refusal" hold here for free: this function
 * can only ever permute.
 */
export interface RunDetailActionsInput {
  instance: Instance;
  run: Run;
  visibility?: RoleVisibility;
  /**
   * The deploy-control actions the CLUSTER can currently take -- the ids from
   * `pipelineState().actions`. Remote instances only.
   *
   * WHY THE SET IS INTERSECTED AND NEVER UNIONED. `instanceActions` filters by
   * the caller's ROLE; the pipeline state answers a different question that the
   * role cannot -- whether this cluster has a deploy pipeline at all, and
   * whether its status read was even visible. An engine-only cluster refuses
   * every deploy-control action by design (pipelineState's header), so drawing
   * the role-permitted set over one would be a row of buttons whose only
   * outcome is a refusal -- the exact thing the doctrine at the top of this file
   * forbids. Intersecting can only remove, so no verb appears here that the
   * instance page would not also have offered.
   *
   * ABSENT MEANS "NOT ASKED", NOT "NOTHING OFFERED". The status read is
   * asynchronous and the page paints before it lands; treating the gap as an
   * empty pipeline would blank the actions for an instant on every open, which
   * reads as a cluster that lost its deploy console. Undefined therefore leaves
   * the role-gated set alone, which is the same call `roleVisibility`
   * indeterminate makes one question earlier.
   */
  pipelineOffers?: readonly DeployActionId[];
}

export function runDetailActions(input: RunDetailActionsInput): InstanceAction[] {
  const { instance, run } = input;
  const actions = instanceActions(instance, input.visibility);
  if (instance.kind === "remote") {
    const offers = input.pipelineOffers;
    if (offers === undefined) return actions;
    return actions.filter(
      (action) => action.deployAction !== undefined && offers.includes(action.deployAction),
    );
  }
  const lead: InstanceActionId | undefined =
    run.status === "failed"
      ? "repair"
      : instance.imageSource === "checkout"
        ? "rebuildFromCheckout"
        : undefined;
  if (lead === undefined) return actions;
  const at = actions.findIndex((action) => action.id === lead);
  if (at <= 0) return actions;
  return [actions[at], ...actions.slice(0, at), ...actions.slice(at + 1)];
}

/**
 * Which machinery moves THIS instance to a version (memql#3997).
 *
 * Read off the "Create deployment" rows above rather than restated, because
 * that is the claim: the upgrade button is not a fourth way to move a cluster,
 * it is the existing move with the target already decided. A second copy of
 * this mapping is how an upgrade would one day route a local instance into the
 * full install graph -- the one mistake the InstanceActionFlow doc calls out.
 *
 * `absent` has no answer and callers must not ask: nothing is installed, so
 * there is nothing to move. It returns the local flow rather than throwing,
 * and upgradeVerdict refuses on presence before it ever reaches here.
 */
export function moveFlowFor(instance: Instance): InstanceActionFlow {
  return instance.kind === "local"
    ? CREATE_LOCAL_INSTALLED.flow
    : (REMOTE_ACTIONS.find((a) => a.id === "createDeployment")?.flow ?? "deployControl");
}

/** Whether an instance offers a given action in its current state. */
export function offersAction(
  instance: Instance,
  id: InstanceActionId,
  visibility?: RoleVisibility,
): boolean {
  return instanceActions(instance, visibility).some((action) => action.id === id);
}
