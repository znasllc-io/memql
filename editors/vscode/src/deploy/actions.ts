// The deploy-action catalog: what each action is, which role tier the ENGINE
// requires for it, and which ones demand a typed confirmation.
//
// THE ENGINE IS THE AUTHORITY. Nothing in this file is a gate. Every tier here
// is a mirror of `component/deploycontrol/service.go` -- authorizeDeploy /
// authorize / authorizeOwner -- kept so the UI can hide an action a caller
// cannot use, which is a courtesy, not a control. The real refusal arrives as a
// DeployControlError carrying PERMISSION_DENIED from the same gate the unary
// path runs (memql#3311's parity test pins the two together). If this table
// ever drifts from the service, the consequence is a button that turns out to
// be refused -- never an action that should have been refused and was not.
//
// The tiers are read off the SERVICE, not off the issue's summary table, and
// the two do not fully agree. Recorded here because the difference is
// load-bearing:
//
//   suggest_version, cut_version, deploy   -> authorizeDeploy (developer+)
//   promote, rollout_action, get_status    -> authorize       (admin+)
//   rollback_deployment                    -> authorizeOwner  (owner ONLY)
//
// The issue's matrix says "View: any". The SHIPPED gate on GetDeploymentStatus
// is owner/admin (#728), and #3311 deliberately preserved that parity rather
// than loosening the read through the new surface. So a developer-or-below
// caller can be refused the STATUS read while still seeing topology and
// deployment history, which are ordinary concept rows and never went near this
// gate. The panel degrades to a message for that case rather than to an empty
// pane.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { Role } from "@znasllc-io/memql-sdk-core/client";

/** The five actions the DevOps surface drives. */
export type DeployActionId =
  | "cutVersion"
  | "deploy"
  | "rollback"
  | "promote"
  | "rolloutAction";

/**
 * The role tiers the deploy-control service enforces.
 *
 * Named after the SERVICE's helpers rather than after roles, so the mapping
 * from a tier to the set of roles that satisfy it lives in exactly one place
 * (satisfiesTier) instead of being re-spelled per action.
 */
export type RoleTier = "developer" | "admin" | "owner";

export interface DeployActionSpec {
  id: DeployActionId;
  /** The label a button carries. */
  label: string;
  /** The verb an outcome line names. Matches the service's audit action
   *  suffix so an operator can grep the audit log with what the UI told them. */
  verb: string;
  tier: RoleTier;
  /** True when the action requires the operator to type a phrase back. */
  typeToConfirm: boolean;
  description: string;
}

/**
 * The catalog.
 *
 * `rolloutAction` is one entry rather than two because promote and abort are
 * one RPC discriminated by an argument -- and because their confirmation
 * requirements differ (abort is type-to-confirm, promote is immediate), which
 * is a property of the chosen sub-action, not of the entry. See
 * rolloutRequiresConfirmation.
 */
export const DEPLOY_ACTIONS: readonly DeployActionSpec[] = [
  {
    id: "cutVersion",
    label: "Cut version",
    verb: "cut_version",
    tier: "developer",
    typeToConfirm: false,
    description:
      "Create a new pending deployment record at the next version, ready to deploy.",
  },
  {
    id: "deploy",
    label: "Deploy",
    verb: "deploy",
    tier: "developer",
    typeToConfirm: false,
    description:
      "Ship the selected pending deployment record. Asynchronous: success means accepted and kicked off, not deployed.",
  },
  {
    id: "promote",
    label: "Promote staging to prod",
    verb: "promote",
    tier: "admin",
    typeToConfirm: true,
    description:
      "Digest-copy the validated staging release into the prod overlay. No rebuild.",
  },
  {
    id: "rollback",
    label: "Roll back",
    verb: "rollback_deployment",
    tier: "owner",
    typeToConfirm: true,
    description:
      "Redeploy the selected succeeded deployment's stored image digest as a new deployment record.",
  },
  {
    id: "rolloutAction",
    label: "Rollout promote / abort",
    verb: "rollout_action",
    tier: "admin",
    typeToConfirm: false,
    description:
      "Promote or abort an in-flight Argo Rollout. Abort requires a typed confirmation.",
  },
] as const;

export function actionById(id: DeployActionId): DeployActionSpec {
  const found = DEPLOY_ACTIONS.find((a) => a.id === id);
  // Unreachable through the typed surface; a throw rather than a silent
  // fallback so a future id added to the union but not to the table fails
  // loudly at the call site instead of resolving to some other action.
  if (found === undefined) throw new Error(`unknown deploy action: ${id}`);
  return found;
}

/**
 * Whether a resolved cluster role satisfies a tier.
 *
 * `developer` is deliberately absent from the switch: the TS SDK's role enum
 * does not carry it (`UserRoleWire` has no USER_ROLE_DEVELOPER, so
 * roleFromWire maps it to ""), which is why an indeterminate role is a
 * first-class case in roleVisibility below rather than a bug to paper over.
 */
export function satisfiesTier(role: Role, tier: RoleTier): boolean {
  switch (tier) {
    case "owner":
      return role === "owner";
    case "admin":
      return role === "owner" || role === "admin";
    case "developer":
      // No `developer` value can reach here, so this is "admin or above" in
      // practice -- a narrower answer than the engine's, which also admits a
      // developer. Narrower is the safe direction for a hint: it hides a
      // button a developer could have used (recoverable, and the
      // indeterminate branch covers the real developer case), where wider
      // would show one that is certain to be refused.
      return role === "owner" || role === "admin";
    default:
      return false;
  }
}

/** How the panel should treat the caller's role. */
export type RoleVisibility =
  /** Role resolved; show exactly the actions the tier admits. */
  | { kind: "resolved"; role: Role }
  /**
   * Role could not be resolved -- the access read failed, or the caller holds
   * a role the TS SDK's enum cannot name (`developer`). Actions are OFFERED
   * with a notice, because hiding them would lock a genuine developer out of
   * cut and deploy, which the engine would have allowed. This is the one place
   * the "never rely on hiding as the gate" rule has teeth: the engine refuses
   * anything this caller may not do, and the refusal names the required role.
   */
  | { kind: "indeterminate"; reason: string };

export function roleVisibility(role: Role | undefined, reason = ""): RoleVisibility {
  if (role === undefined || role === "") {
    return {
      kind: "indeterminate",
      reason:
        reason !== ""
          ? reason
          : "Your cluster role could not be determined. Actions are offered, but the engine decides -- a refusal will name the role required.",
    };
  }
  return { kind: "resolved", role };
}

/**
 * The actions to render for a given visibility.
 *
 * A "resolved" writer or reader gets NOTHING, which is the acceptance
 * criterion the issue states: a non-admin sees the read surface and none of
 * the actions.
 */
export function visibleActions(visibility: RoleVisibility): DeployActionSpec[] {
  if (visibility.kind === "indeterminate") return [...DEPLOY_ACTIONS];
  return DEPLOY_ACTIONS.filter((action) => satisfiesTier(visibility.role, action.tier));
}

/** The role requirement, phrased the way the service phrases it. */
export function tierDescription(tier: RoleTier): string {
  switch (tier) {
    case "owner":
      return "owner";
    case "admin":
      return "owner or admin";
    case "developer":
      return "developer, admin, or owner";
    default:
      return "";
  }
}

// -----------------------------------------------------------------------------
// Type-to-confirm
// -----------------------------------------------------------------------------

/**
 * The phrase an operator must type back before a destructive action runs.
 *
 * It is the action's TARGET, not the word "yes" and not the action's name:
 * re-typing the version being pushed to production, or the deployment being
 * rolled back to, forces the operator to look at what they selected. A
 * confirmation that can be satisfied without reading the target confirms
 * nothing. This mirrors the cockpit and the portal (deployment-console.md,
 * "Confirmation").
 *
 * `target` empty yields an empty phrase, and requireConfirmation treats that
 * as "cannot be confirmed" rather than "no confirmation needed" -- an action
 * with nothing identifiable to re-type must not proceed unchallenged.
 */
export function confirmationPhrase(id: DeployActionId, target: string): string {
  switch (id) {
    case "promote":
    case "rollback":
      return target;
    case "rolloutAction":
      return target;
    default:
      return "";
  }
}

/**
 * Whether a rollout sub-action needs confirming.
 *
 * `abort` tears down an in-flight rollout and is irreversible from the
 * console; `promote` advances one that is already going where it was told to.
 * Only the first is challenged -- confirming both would train the operator to
 * type through the prompt, which is how a type-to-confirm stops working.
 */
export function rolloutRequiresConfirmation(subAction: string): boolean {
  return subAction === "abort";
}

/**
 * Compare a typed confirmation against the expected phrase.
 *
 * Surrounding whitespace is forgiven (a paste picks it up); case and content
 * are not. An empty expectation is never satisfiable -- see confirmationPhrase.
 */
export function confirmationMatches(expected: string, typed: string): boolean {
  if (expected === "") return false;
  return typed.trim() === expected.trim();
}
