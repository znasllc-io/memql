import { ownsMachine } from "../../fleet/machines/sharing";
import { isRevoked, type MachineRow } from "../../fleet/rows";

// WHICH MACHINES TAKE A PIPELINE STEP (epic memql#5479; design record,
// "Compute confirmed reads only what is enforced"). One rule, read by the
// connect flow's Compute stop and by Settings > Pipelines' fleet line, so the
// two can never disagree about whether "your fleet" exists.
//
// REPORTED, never the merge with the operator's labels. Allowing pipelines is
// the machine's own policy.yaml: the cockpit drops an operator-set `pipelines`
// label at register, and an operator label cannot grant what the machine
// itself refuses. And not "online": a machine that allows pipelines and is
// asleep is still a place a step can run once it wakes, and the runner says so
// if it does not.
//
// THE OWNER'S, NOT EVERY ROW THE FEED HOLDS. The runner sends a step to its
// pipeline owner's machines, and the machines feed folds whatever its
// subscription is sent -- a cluster owner's is admitted to every registration
// row, so it can hold a colleague's machine, which is not one of "your
// machines" and which no step of this pipeline would ever reach.

/** The label a machine's cockpit reports when its policy lets it run pipeline
 *  steps -- the one the runner's router requires -- and the value that means it does. */
export const PIPELINES_LABEL = "pipelines";
export const PIPELINES_ALLOWED = "allowed";

/** Whether a machine would take a pipeline step: not revoked, and its REPORTED labels say so. */
export function allowsPipelines(machine: Pick<MachineRow, "revokedAt" | "reportedLabels">): boolean {
  return !isRevoked(machine) && machine.reportedLabels[PIPELINES_LABEL] === PIPELINES_ALLOWED;
}

/** How many of `ownerUserId`'s own machines allow pipelines. */
export function machinesAllowingPipelines(
  machines: readonly Pick<MachineRow, "ownerUserId" | "revokedAt" | "reportedLabels">[],
  ownerUserId: string,
): number {
  return machines.filter((m) => ownsMachine(m, ownerUserId) && allowsPipelines(m)).length;
}
