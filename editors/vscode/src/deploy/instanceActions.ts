// What a Deployments instance offers, given what it is and what state it is in.
//
// One table, read off design §5.2:
//
//   Action             local, absent   local, installed        remote
//   Create deployment  install graph   stackCheckout + up      deploy-control Deploy
//   Repair             --              re-run graph            --
//   Uninstall          --              uninstall graph         --
//   Cut version        --              --                      developer+
//   Promote            --              --                      admin+
//   Rollout action     --              --                      admin+
//   Rollback           --              --                      owner only
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
import type { Instance } from "../state/deployments.js";

export type InstanceActionId =
  | "createDeployment"
  | "repair"
  | "uninstall"
  | "cutVersion"
  | "promote"
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
}

/** "Create deployment", as the local machine's three states reach it. */
const CREATE_LOCAL_ABSENT: InstanceAction = {
  id: "createDeployment",
  label: "Create deployment",
  detail: "Install a local memQL cluster on this machine.",
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
  };
}

const REMOTE_ACTIONS: readonly InstanceAction[] = [
  fromDeployAction("createDeployment", "deploy", {
    label: "Create deployment",
    detail:
      "Ship the selected pending deployment record. Asynchronous: success means accepted and kicked off, not deployed.",
  }),
  fromDeployAction("cutVersion", "cutVersion"),
  fromDeployAction("promote", "promote"),
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
    return instance.presence === "absent"
      ? [CREATE_LOCAL_ABSENT]
      : // `installed-unreachable` gets the same set as `installed-healthy`:
        // something is on the machine either way, and repair is precisely the
        // action for the one that is not answering. Ordering puts it first
        // there, since an operator looking at a broken cluster came to fix it.
        instance.presence === "installed-unreachable"
        ? [REPAIR, CREATE_LOCAL_INSTALLED, UNINSTALL]
        : [CREATE_LOCAL_INSTALLED, REPAIR, UNINSTALL];
  }

  if (visibility === undefined || visibility.kind === "indeterminate") {
    return [...REMOTE_ACTIONS];
  }
  return REMOTE_ACTIONS.filter(
    (action) => action.tier === undefined || satisfiesTier(visibility.role, action.tier),
  );
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
