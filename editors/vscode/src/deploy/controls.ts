// The buttons the deploy-control actions draw, each carrying the exact request
// it sends.
//
// THE CATALOG NAMES ACTIONS; A BUTTON NEEDS A TARGET. deploy/actions.ts lists
// four actions, and three of them take an argument the catalog cannot know:
// which version to cut, which deployment to roll back to, which Argo Rollout
// to promote or abort. The panel used to fill those in at the click, and got
// all three wrong in the same way -- by filling in a constant:
//
//   - Cut version sent `bump: "patch"` every time, so a minor or major cut
//     could not be made from the panel at all, and `previewNextVersion` -- the
//     read that says what each bump would produce -- had no caller.
//   - Rollout promote / abort sent `promote` with an EMPTY rollout name, which
//     the SDK refuses before sending (`rolloutAction: rollout is required`),
//     so the button could not succeed and abort was never offered.
//   - Roll back sent the newest `succeeded` deployment, which is usually the
//     one the cluster is running.
//
// So the argument is decided HERE, when the page is built, from facts the page
// has already read, and every button is one fully-resolved request. Each
// button's label names its target before anything is pressed ("Prepare 1.5.0",
// "Roll back to 1.4.1", a Promote on the rollout's own row) and the click sends
// exactly that. WHERE a control is drawn -- the action bar, a rollout's row,
// the list of versions to prepare -- is deploy/instanceActions.ts's to decide;
// WHAT it sends is only ever decided here.
//
// THE KEY IS THE WHOLE CONTRACT WITH THE WEBVIEW. A button posts its `key`;
// the panel re-expands the controls from its own current facts and runs the one
// whose key matches, byte for byte. (On the page a control is a kit act,
// `data-act="deploy" data-value="<key>"`, posting `{ type: "deploy", value }`.) Nothing the page posts reaches a request
// except the choice of a control this module built, which is what the
// untrusted postMessage channel should be allowed to decide. A key carries its
// target (`cutVersion:minor:1.5.0`, `rollback:<id>`), so a click on a button
// whose target has since changed matches nothing and runs nothing, rather than
// running the new target under the old label.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { RolloutStatus } from "@znasllc-io/memql-sdk-core/deploy";

import {
  DEPLOY_ACTIONS,
  actionById,
  confirmationPhrase,
  rolloutRequiresConfirmation,
  type DeployActionId,
} from "./actions.js";
import type { DeployActionRequest, VersionPreview } from "./controller.js";
import type { Instance, Run } from "../state/deployments.js";

/** What the controls are resolved from. Every field is a read the page made. */
export interface DeployControlFacts {
  /** `pendingDeploymentId` and `rollbackTargetId`, from the catalog. */
  instance: Pick<Instance, "pendingDeploymentId" | "rollbackTargetId">;
  /** The history, for the version a target id carries. */
  runs: readonly Run[];
  /** Every rollout the status read reported; `rolloutsInFlight` narrows. */
  rollouts: readonly RolloutStatus[];
  /** The version preview, or undefined before it has been read. */
  preview: VersionPreview | undefined;
}

export interface DeployControl {
  /** What the button posts, and the only thing the panel matches on. */
  key: string;
  action: DeployActionId;
  /** The words on the button, naming its target ("Prepare 1.5.0"). */
  label: string;
  /** What it does, in a sentence: the button's tooltip. */
  detail: string;
  /**
   * A quiet word drawn beside the label, when the label alone leaves a choice
   * unexplained: the bump a cut takes ("minor"). Absent otherwise.
   */
  note?: string;
  /** Drawn as the destructive style: the action makes someone type it back. */
  destructive: boolean;
  /**
   * The phrase the operator types back before the request is sent; "" when
   * none is asked for. Always the TARGET (confirmationPhrase), and never ""
   * on a destructive control that carries a request.
   */
  confirm: string;
  /** The request the button sends. Absent exactly when `refusal` is set. */
  request?: DeployActionRequest;
  /**
   * Why the button has nothing to send -- nothing cut, nothing to roll back
   * to. The button is still drawn, as Deploy's has been, and the page says
   * why beside it; a click reports this instead of sending anything.
   */
  refusal?: string;
}

/** The bumps CutVersion accepts, in the order they are drawn. */
export const CUT_BUMPS = ["patch", "minor", "major"] as const;
export type CutBump = (typeof CUT_BUMPS)[number];

/**
 * The Argo Rollouts phases a promote or abort acts on, compared without regard
 * to case or surrounding space (the engine relays Argo's own string).
 *
 * `Paused` is a rollout waiting for exactly this -- a blue-green preview
 * awaiting promotion, a canary at a pause step -- and `Progressing` is one
 * still moving that an operator may advance or cancel. `Healthy` is finished,
 * with nothing to promote and nothing an abort should touch, and `Degraded` is
 * one that has already failed or been aborted, which Argo recovers with
 * `retry`, a verb this surface does not have. Drawing either would draw a
 * button whose best outcome is nothing happening.
 */
const IN_FLIGHT_PHASES = new Set(["progressing", "paused"]);

/** The rollouts a promote or abort can act on, as the status read reported them. */
export function rolloutsInFlight(rollouts: readonly RolloutStatus[]): RolloutStatus[] {
  return rollouts.filter(
    (rollout) => rollout.name.trim() !== "" && IN_FLIGHT_PHASES.has(rollout.phase.trim().toLowerCase()),
  );
}

/**
 * The controls for a set of actions, in the order given.
 *
 * An action can expand to several controls (a cut per bump, a promote and an
 * abort per rollout in flight) or to none (no rollout in flight). Deploy and
 * Roll back expand to exactly one, with a `refusal` in place of a request when
 * there is no record to name.
 */
export function deployControls(
  actions: readonly DeployActionId[],
  facts: DeployControlFacts,
): DeployControl[] {
  return actions.flatMap((id) => {
    switch (id) {
      case "deploy":
        return [deployControl(facts)];
      case "cutVersion":
        return cutControls(facts.preview);
      case "rollback":
        return [rollbackControl(facts)];
      case "rolloutAction":
        return rolloutControls(facts.rollouts);
      default:
        return [];
    }
  });
}

/**
 * The control a posted key names, resolved against the facts NOW.
 *
 * Expanded over every action in the catalog rather than over what the page
 * drew. Which actions a page draws is a courtesy (deploy/actions.ts), and the
 * engine gates every one of them; what this lookup guards is that the request
 * is one this module built, not that the page drew it.
 */
export function deployControlByKey(key: string, facts: DeployControlFacts): DeployControl | undefined {
  return deployControls(
    DEPLOY_ACTIONS.map((action) => action.id),
    facts,
  ).find((control) => control.key === key);
}

/** The action a posted key belongs to, whether or not it still resolves. */
export function actionOfKey(key: string): DeployActionId | undefined {
  return DEPLOY_ACTIONS.find((action) => key === action.id || key.startsWith(`${action.id}:`))?.id;
}

/**
 * Deploy ships the PENDING record the page rendered (memql#4017), never
 * `runs[0]`: `deploy` moves whatever it is given pending -> in_progress without
 * checking what it moves from (component/deploycontrol/deploy.go), so the
 * newest record re-shipped a landed deployment whenever one was newest. The key
 * stays the bare id because the target is already fixed on the instance when
 * the page is built (`Instance.pendingDeploymentId`) and printed above the
 * button.
 */
function deployControl(facts: DeployControlFacts): DeployControl {
  const spec = actionById("deploy");
  const target = (facts.instance.pendingDeploymentId ?? "").trim();
  const base = { key: spec.id, action: spec.id, label: spec.label, detail: spec.description, destructive: false, confirm: "" };
  if (target === "") {
    return { ...base, refusal: "Nothing is prepared, so there is nothing to deploy." };
  }
  const version = versionOf(facts.runs, target);
  return {
    ...base,
    label: version === "" ? spec.label : `Deploy ${version}`,
    detail: version === "" ? spec.description : `Deploy ${version}, the version prepared last.`,
    request: { id: "deploy", deploymentId: target },
  };
}

/**
 * One cut per bump, naming the version it prepares when the preview gave one.
 *
 * "PREPARE" IS THE USER'S WORD FOR A CUT: it creates the record Deploy then
 * ships. The label names the version ("Prepare 1.5.0") and the bump is its
 * quiet note, so the three read as a choice between versions, not between
 * semver terms.
 *
 * THE VERSION IS SENT, NOT ONLY THE BUMP. The button says `Prepare 1.5.0`, and a
 * bump alone asks the engine to compute the version again at the click -- from
 * a current version that may have moved since the page was read. Naming it is
 * the upgrade path's rule (runRemoteUpgrade): the operator chose a specific
 * release. The bump rides along because the engine records it in the audit
 * detail, where it says which of the three was chosen.
 *
 * WITHOUT A PREVIEW THE CUT IS NOT BLOCKED. A failed read, or a current version
 * the engine cannot parse (its proposals come back empty), falls back to the
 * bump alone and a label that says so, and the engine decides -- or refuses in
 * its own words. A preview is a courtesy on the Cut button, never a gate.
 */
function cutControls(preview: VersionPreview | undefined): DeployControl[] {
  const suggestion = preview?.suggestion ?? null;
  return CUT_BUMPS.map((bump): DeployControl => {
    const next = suggestion === null ? "" : nextVersionFor(suggestion, bump);
    const base = { action: "cutVersion" as const, destructive: false, confirm: "" };
    if (next === "") {
      return {
        ...base,
        key: `cutVersion:${bump}`,
        label: `Prepare next ${bump}`,
        detail: `Prepare the next ${bump} version for deployment. The cluster works out the number.`,
        request: { id: "cutVersion", bump, version: "" },
      };
    }
    const current = suggestion?.currentVersion ?? "";
    return {
      ...base,
      key: `cutVersion:${bump}:${next}`,
      label: `Prepare ${next}`,
      note: bump,
      detail: `Prepare ${next}, the next ${bump} version${current === "" ? "" : ` after ${current}`}, for deployment.`,
      request: { id: "cutVersion", bump, version: next },
    };
  });
}

function nextVersionFor(
  suggestion: NonNullable<VersionPreview["suggestion"]>,
  bump: CutBump,
): string {
  switch (bump) {
    case "patch":
      return suggestion.nextPatch;
    case "minor":
      return suggestion.nextMinor;
    case "major":
      return suggestion.nextMajor;
  }
}

function rollbackControl(facts: DeployControlFacts): DeployControl {
  const spec = actionById("rollback");
  const target = (facts.instance.rollbackTargetId ?? "").trim();
  if (target === "") {
    return {
      key: spec.id,
      action: spec.id,
      label: spec.label,
      detail: spec.description,
      destructive: true,
      confirm: "",
      refusal: "No earlier release is in this cluster's history, so there is nothing to roll back to.",
    };
  }
  const version = versionOf(facts.runs, target);
  return {
    key: `${spec.id}:${target}`,
    action: spec.id,
    label: version === "" ? spec.label : `Roll back to ${version}`,
    detail: `Deploy ${version === "" ? "the earlier release" : version} again (${target}).`,
    destructive: true,
    confirm: confirmationPhrase(spec.id, target),
    request: { id: "rollback", toDeploymentId: target },
  };
}

/**
 * A promote and an abort for each rollout in flight, and nothing when none is.
 *
 * PER ROLLOUT, because the RPC acts on one by name and a cluster runs one per
 * engine node type. The confirmation is the rollout's NAME, for the reason
 * confirmationPhrase gives: re-typing what is being aborted is what makes the
 * operator read it. It used to be the cluster's name, which confirms nothing
 * about which rollout goes.
 */
function rolloutControls(rollouts: readonly RolloutStatus[]): DeployControl[] {
  return rolloutsInFlight(rollouts).flatMap((rollout) =>
    (["promote", "abort"] as const).map((subAction): DeployControl => {
      const confirm = rolloutRequiresConfirmation(subAction)
        ? confirmationPhrase("rolloutAction", rollout.name)
        : "";
      return {
        key: `rolloutAction:${subAction}:${rollout.name}`,
        action: "rolloutAction",
        label: `${subAction === "promote" ? "Promote" : "Abort"} ${rollout.name}`,
        detail:
          subAction === "promote"
            ? `Let the ${rollout.name} rollout continue.`
            : `Stop the ${rollout.name} rollout and keep the release it was replacing.`,
        destructive: confirm !== "",
        confirm,
        request: { id: "rolloutAction", rollout: rollout.name, subAction },
      };
    }),
  );
}

/** The version a history row carries, or "" when the row is not in the read. */
export function versionOf(runs: readonly Run[], id: string): string {
  return runs.find((run) => run.id === id)?.toVersion ?? "";
}
