import type { DeploymentStatus } from "@znasllc-io/memql-sdk-core/deploy";
import type { ConceptLike, RowLike } from "@znasllc-io/memql-view-kit";

// Turning the deploy console's typed payloads into row sets an element can
// render.
//
// ===========================================================================
// WHY THIS EXISTS AT ALL
// ===========================================================================
// Almost everything the portal shows is a CONCEPT ROW: it arrives with a
// concept id, a display card, and a shape the element library already knows
// how to profile. DeploymentStatus is not that. It is the live answer from
// DeployControlService -- resolved image digests, the Argo CD app's sync and
// health, per-rollout progressive-delivery state, the last gate's legs -- and
// none of it is persisted as graph rows (memql#3311's header says so
// explicitly: deployment HISTORY is v1:cluster:deployment and is read as
// ordinary rows; this is the current state, computed).
//
// So it has to be ADAPTED, and the honest adaptation is to make it speak the
// same contract as everything else: a flat row set plus a ConceptLike naming
// which fields are the primary, the secondary and the status. Then the gate
// legs render through the checklist and the digests through the table -- the
// same renderers, with the same theming and the same keyboard behaviour, and
// no bespoke markup anywhere in the deployments view.
//
// THE DESCRIPTORS ARE NOT CONCEPTS, and their ids say so: `deploy.gateLeg`,
// not `v1:deploy:gateLeg`. Nothing in the DSL declares these, no query
// returns them, and an id in memQL's `{version}:{domain}:{entity}` grammar
// would be a claim that one does.
//
// This module lives OUTSIDE src/views/ deliberately. Reshaping data is not
// laying out a page, and keeping it out means the view modules contain no
// iteration over rows at all -- which is what
// portal_view_composition_test.go checks for.

export const GATE_LEG_CONCEPT: ConceptLike = {
  id: "deploy.gateLeg",
  entity: "gate check",
  // `passed` is the status slot, which is what makes the checklist bind its
  // `done` requirement without the view naming a field.
  displayCard: { primary: "name", secondary: "detail", status: "passed" },
};

export const COMPONENT_CONCEPT: ConceptLike = {
  id: "deploy.component",
  entity: "image",
  displayCard: { primary: "name", secondary: "repo", tertiary: "digest" },
};

export const ROLLOUT_CONCEPT: ConceptLike = {
  id: "deploy.rollout",
  entity: "rollout",
  displayCard: { primary: "name", secondary: "kind", status: "phase" },
};

// gateLegRows: one row per leg of the last deploy gate.
export function gateLegRows(status: DeploymentStatus | null): RowLike[] {
  if (!status) return [];
  return status.gateResult.legs.map((leg) => ({
    id: leg.name,
    name: leg.name,
    passed: leg.passed,
    detail: leg.detail,
  }));
}

// componentRows: one row per image the environment is pinned to.
export function componentRows(status: DeploymentStatus | null): RowLike[] {
  if (!status) return [];
  return status.components.map((component) => ({
    id: component.name,
    name: component.name,
    repo: component.repo,
    digest: component.digest,
  }));
}

// rolloutRows: one row per Argo Rollout mid-flight.
//
// canaryWeight and currentStep are carried as numbers so the stat strip and
// the table read them as measures rather than as text -- a weight of 20 that
// arrives as "20" sorts lexically, which puts 100 before 20.
export function rolloutRows(status: DeploymentStatus | null): RowLike[] {
  if (!status) return [];
  return status.rollouts.map((rollout) => ({
    id: rollout.name,
    name: rollout.name,
    kind: rollout.kind,
    phase: rollout.phase,
    activeColor: rollout.activeColor,
    previewColor: rollout.previewColor,
    canaryWeight: rollout.canaryWeight,
    currentStep: rollout.currentStep,
    latestAnalysisResult: rollout.latestAnalysisResult,
  }));
}
