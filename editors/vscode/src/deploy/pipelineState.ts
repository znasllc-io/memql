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
// THE ENGINE'S OWN WORDS ARE CARRIED, NOT PARAPHRASED. A refusal names a
// reason, sometimes a role; a paraphrase is one more thing that can be wrong,
// and the operator may need to match it against a log line.
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
  /** The heading for the section. */
  title: string;
  /**
   * What to say beneath it. For the two failure states this is the ENGINE's
   * sentence, prefixed by nothing and reworded by nothing.
   */
  detail: string;
  /**
   * The actions to render. Empty for both failure states, because an action
   * whose own status read was refused is an action the engine will refuse too
   * -- and one that reported no pipeline has nothing to act on.
   */
  actions: DeployActionSpec[];
}

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
      title: "Deployment status is not visible at your role",
      detail: read.message,
      actions: [],
    };
  }
  if (read.reason === "unavailable" || read.status === null) {
    return {
      kind: "notConfigured",
      title: "No deploy pipeline is configured for this cluster",
      detail:
        read.message === ""
          ? "The cluster did not answer the deployment-status read."
          : read.message,
      actions: [],
    };
  }
  return {
    kind: "present",
    title: "Deploy",
    detail:
      "The engine decides every one of these. What is hidden here is hidden as a courtesy; " +
      "a refusal will name the role required.",
    actions: visibleActions(visibility),
  };
}
