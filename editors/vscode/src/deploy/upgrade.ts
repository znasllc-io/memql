// The one button that moves a cluster to the newest release (memql#3997).
//
// ONE BUTTON, ONE LABEL, ONE CONFIRMATION. "Create deployment" already moves a
// cluster to ANY tag, through a picker and a forecast, and it stays exactly as
// it is -- that is the control for choosing. This is the control for the case
// the whole epic is about: a newer release exists, and the operator wants THIS
// cluster on it. Making them open a picker to select the value the row just
// told them is the newest is asking them to repeat what the extension already
// knows.
//
// WHAT IT DECIDES, AND WHAT IT DOES NOT
//
// This module decides whether to draw the button, what the confirmation says,
// and whether a barrier turns that confirmation into a refusal. It runs
// nothing. The panel drives the machinery, so the wording and the gating are
// unit-testable and the run path stays the one that already exists.
//
// THE ENGINE IS THE AUTHORITY ON EVERY GATE. The role check here hides a button
// a caller cannot use; it does not decide anything. src/deploy/actions.ts
// states this doctrine and instanceActions.ts extends it, and the reason is
// worth repeating: a caller whose role could not be read may well be entitled,
// and hiding the surface would lock them out of something the engine would have
// allowed -- while the engine refuses anything they are not entitled to
// regardless of what this file drew.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3997 #3989

import { actionById, satisfiesTier, type RoleTier, type RoleVisibility } from "./actions.js";
import { moveFlowFor, type InstanceActionFlow } from "./instanceActions.js";
import { instanceLabel, type Instance } from "../state/deployments.js";
import { barriersCrossed, type UpgradeBarrier } from "../version/barriers.js";
import { returnsToReleasedImages } from "../state/imageLane.js";
import type { VersionDescription } from "../version/describe.js";

/** Which machinery a move reaches, and between which two versions. */
export interface UpgradeTarget {
  /** The cluster as the confirmation names it (instanceLabel), never its key. */
  instanceName: string;
  /**
   * What the cluster records today.
   *
   * Non-empty in an `offer`, by construction rather than by check: an offer
   * needs `upgradeAvailable`, which needs the recorded version to have parsed.
   * The empty case is still spelled ("an unrecorded version") because the type
   * cannot say so and a future caller may reach it another way.
   */
  from: string;
  /** The release it would move to. */
  to: string;
  flow: InstanceActionFlow;
}

export type UpgradeVerdict =
  /** Draw the button. `confirmation` is what the operator reads before it runs. */
  | {
      kind: "offer";
      target: UpgradeTarget;
      label: string;
      /** The confirmation's title: "Update local". */
      title: string;
      /** The one question the operator answers, and the lane warning when it applies. */
      confirmation: string;
      /** What the operator re-types. The target version, never the word "yes". */
      phrase: string;
    }
  /**
   * Draw the button and REFUSE when it is pressed. Not a warning: the move
   * across this barrier can leave a cluster running with an empty graph and no
   * error anywhere, and a warning is something an operator clicks past.
   */
  | {
      kind: "refused";
      target: UpgradeTarget;
      label: string;
      message: string;
      barriers: readonly UpgradeBarrier[];
      /** The runbook the refusal points at, repo-relative. */
      docHref: string;
    }
  /** Draw nothing. `reason` is for tests and logs, not for an operator. */
  | { kind: "none"; reason: string };

/**
 * The tier the REMOTE path actually needs, read off the actions it invokes
 * rather than written down here.
 *
 * WHY THIS IS DERIVED RATHER THAN THE `owner` memql#3997 NAMES.
 *
 * The calls this path makes are `cutVersion` and `deploy`, and DEPLOY_ACTIONS
 * records BOTH as `developer`. A button gated at owner would therefore be a UI
 * gate stricter than the gate it mirrors -- which is a second authority, and
 * epic memql#3989's "What must not regress" ends with "The engine stays the
 * authority on every gate". Deriving honours that more exactly than the literal
 * reading of "owner-gated as a courtesy only" would; the phrase is about the
 * gate being non-authoritative, and a non-authoritative gate that refuses what
 * the authority allows is the one shape it must not take.
 *
 * AND THIS EXACT MISTAKE HAS A BUG NUMBER. Before memql#3331, satisfiesTier
 * approximated the developer tier as "admin or above", and the comment above it
 * records what that cost: "it hid cut/deploy from a developer who was entitled
 * to them" (actions.ts:136-140). Hardcoding `owner` here would hide the SAME
 * TWO CALLS from the SAME ROLE again, in a new place, months after that was
 * fixed.
 *
 * So: if either action's tier changes upstream, this follows without an edit,
 * and there is no second copy to disagree with the engine.
 */
function remoteTiers(): readonly RoleTier[] {
  return [actionById("cutVersion").tier, actionById("deploy").tier];
}

export interface UpgradeVerdictInput {
  instance: Instance;
  /** The version verdict from version/describe.ts. */
  version: VersionDescription;
  /** The caller's cluster role. Consulted for remote instances only. */
  visibility?: RoleVisibility;
}

/**
 * Whether this instance is offered a move to the newest release, and what
 * happens when it is taken.
 *
 * GATED ON `upgradeAvailable`, WHICH IS TRUE ONLY IN THE `behind` STATE. Every
 * other state leaves it off rather than offering an action nothing supports:
 * `current` has nowhere to go, `ahead` would be offered a move BACKWARDS (a
 * locally built cluster, and calling that an upgrade would be a lie),
 * `notComparable` and `unfetched` do not know, and "we do not know" is not a
 * reason to move a cluster.
 */
export function upgradeVerdict(input: UpgradeVerdictInput): UpgradeVerdict {
  const { instance, version } = input;

  if (instance.presence === "absent") {
    // Nothing to move. Installing is a different verb with different questions,
    // and instanceActions.ts already offers it.
    return { kind: "none", reason: "nothing is installed" };
  }
  if (!version.upgradeAvailable || version.latest === undefined) {
    return { kind: "none", reason: `no newer release is known (${version.state})` };
  }

  const from = (instance.version ?? "").trim();
  const to = version.latest;
  const target: UpgradeTarget = {
    instanceName: instanceLabel(instance),
    from,
    to,
    flow: moveFlowFor(instance),
  };

  if (instance.kind === "remote" && !mayDriveRemote(input.visibility)) {
    return { kind: "none", reason: "the caller's role cannot cut and deploy" };
  }

  const crossing = barriersCrossed(from, to);
  if (crossing.kind === "blocked") {
    return {
      kind: "refused",
      target,
      label: upgradeLabel(to),
      message: refusalMessage(target, crossing.barriers, crossing.direction),
      barriers: crossing.barriers,
      // Every barrier in a crossing points at the same runbook today. If that
      // ever stops being true, the refusal names each barrier's own href in the
      // message; this field is the one the caller LINKS, so it takes the first.
      docHref: crossing.barriers[0]?.docHref ?? "",
    };
  }
  if (crossing.kind === "undetermined") {
    // UNREACHABLE BY CONSTRUCTION, and handled anyway. `upgradeAvailable` is
    // true only when compareVersions returned "behind", which requires both
    // sides to parse -- so a crossing computed from the same two values cannot
    // fail to place them. Refusing rather than proceeding is the fail-closed
    // direction if that ever stops holding, and costs nothing while it does.
    return {
      kind: "refused",
      target,
      label: upgradeLabel(to),
      message: `${to} can't be applied automatically.`,
      barriers: [],
      docHref: "",
    };
  }

  return {
    kind: "offer",
    target,
    label: upgradeLabel(to),
    title: `Update ${instanceLabel(instance)}`,
    confirmation: confirmationMessage(instance, target),
    // The TARGET, never the word "yes": re-typing the version forces the
    // operator to look at what they are moving to. Same call the deploy
    // surface's confirmationPhrase makes for promote and rollback.
    phrase: to,
  };
}

function mayDriveRemote(visibility: RoleVisibility | undefined): boolean {
  // An INDETERMINATE role offers everything, for the reason at the top of this
  // file: a caller whose role could not be read may well be entitled.
  if (visibility === undefined || visibility.kind === "indeterminate") return true;
  return remoteTiers().every((tier) => satisfiesTier(visibility.role, tier));
}

function upgradeLabel(to: string): string {
  return `Update to ${to}`;
}

/**
 * The one question the confirmation asks.
 *
 * SHORT, because it sits in a one-line input box above "Type v0.24.0 to
 * confirm". What matters to the decision is WHERE the cluster moves and, for a
 * remote one, that it deploys at once; how the move is carried out is the
 * run's business and shows on its progress screen.
 */
function confirmationMessage(instance: Instance, target: UpgradeTarget): string {
  const from = target.from === "" ? "" : ` from ${target.from}`;
  const head =
    target.flow === "upgradeToTag"
      ? `Update ${target.instanceName}${from} to ${target.to}?`
      : `Update ${target.instanceName} to ${target.to}? It deploys now.`;
  // THE LANE CROSSING, IN THE CONFIRMATION AN OPERATOR ACTUALLY READS
  // (memql#4246): a move over a cluster running checkout-built images returns
  // it to released ones, and nothing else on this path would say so.
  return instance.imageSource === "checkout" ? `${head} ${returnsToReleasedImages(instanceLabel(instance), "")}` : head;
}

/**
 * The refusal, which has to leave the operator able to act.
 *
 * One sentence; the page offers the runbook beside it (`docHref`) rather than
 * quoting a repository path into the sentence.
 */
function refusalMessage(
  target: UpgradeTarget,
  _barriers: readonly UpgradeBarrier[],
  direction: "forward" | "backward",
): string {
  return direction === "backward"
    ? `${target.instanceName} can't be moved back to ${target.to} automatically.`
    : `${target.to} needs manual upgrade steps.`;
}
