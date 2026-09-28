// Which of three states a remote instance's deploy pipeline is in.
//
// NONE OF THEM IS AN ERROR, and that is the whole design. A row of buttons
// that turn out to be refused would be the error; naming the state is not.
//
//   PRESENT      -- the status read answered. The actions are offered.
//   NOT CONFIGURED -- the read failed for a reason that is not the role gate,
//                   and the commonest reason by far is that the cluster has no
//                   deploy pipeline at all. `992deb41` moved the orchestration
//                   (scripts/release/promote.sh, the ArgoCD Applications) out
//                   of this repository and into the product repo, and
//                   `deploycontrol/driver.go` refuses `docker-local` outright,
//                   so an engine-only cluster refuses every deploy-control
//                   action by design. That is the state anyone running THIS
//                   repository is in, which makes it a state to render rather
//                   than a fault to report.
//   NOT VISIBLE  -- `GetDeploymentStatus` is owner/admin gated (#728, parity
//                   preserved by memql#3311 and the docs corrected to match by
//                   memql#3332), so a developer legitimately cannot see status
//                   while still seeing history -- which is ordinary concept
//                   rows and never went near that gate.
//
// THE ENGINE'S OWN WORDS ARE CARRIED, NOT PARAPHRASED -- and not SHOWN on the
// page either. A refusal names a reason, sometimes a role, in words written
// for a log; the page says one short line and keeps the engine's sentence for
// the tooltip and Details, where somebody matching it against a log finds it.
//
// AND THIS IS NEVER A GATE. `deploy/actions.ts` states the doctrine and this
// extends it: hiding an action a role cannot use is a courtesy. The engine
// refuses independently through the same gate the unary path runs, and the
// refusal is surfaced naming the role required.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3740 #3733

import type { StatusRead } from "./controller.js";
import { visibleActions, type DeployActionSpec, type RoleVisibility } from "./actions.js";

export type PipelineStateKind = "present" | "notConfigured" | "notVisible";

export interface PipelineState {
  kind: PipelineStateKind;
  /**
   * The one line a page says about it, or "" when there is nothing to say
   * (`present`). Short on purpose: the reason behind it is `engineMessage`,
   * which belongs in a tooltip or Details, not on the page.
   */
  line: string;
  /** The ENGINE's own sentence for a failed read, verbatim; "" for `present`. */
  engineMessage: string;
  /**
   * The actions the caller's role admits. Empty for both failure states: an
   * action whose own status read was refused, or that found no pipeline, has
   * nothing to act on.
   */
  actions: DeployActionSpec[];
  /**
   * The Argo Rollouts that are part-way through, by name -- the only ones a
   * promote or an abort can act on. Empty when none is, and always for the
   * failure states. Promote and abort are offered per rollout named here and
   * never with a blank name, which the engine refuses.
   */
  rollouts: string[];
}

/** The rollout phases in which a promote or an abort has something to act on. */
const IN_FLIGHT_PHASES = new Set(["paused", "progressing"]);

/**
 * Classify the status read.
 *
 * The role is consulted ONLY for which actions to draw in the `present` case.
 * It does not decide the state: a developer whose read was refused lands in
 * `notVisible` because the ENGINE said so, not because this table concluded it
 * -- which is what keeps the courtesy from quietly becoming the control.
 */
export function pipelineState(read: StatusRead, visibility: RoleVisibility): PipelineState {
  if (read.reason === "permissionDenied") {
    return {
      kind: "notVisible",
      line: "Your role can't view deployment status.",
      engineMessage: read.message,
      actions: [],
      rollouts: [],
    };
  }
  if (read.reason === "unavailable" || read.status === null) {
    return {
      kind: "notConfigured",
      line: "Deployments aren't set up for this cluster.",
      engineMessage: read.message,
      actions: [],
      rollouts: [],
    };
  }
  return {
    kind: "present",
    line: "",
    engineMessage: "",
    actions: visibleActions(visibility),
    rollouts: (read.status.rollouts ?? [])
      .filter((rollout) => rollout.name.trim() !== "" && IN_FLIGHT_PHASES.has(rollout.phase.trim().toLowerCase()))
      .map((rollout) => rollout.name.trim()),
  };
}
