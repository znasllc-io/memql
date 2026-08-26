import type { ReactNode } from "react";

import { ArtifactsBody } from "../artifacts/ArtifactsPage";
import { DeployablesBody } from "../deployables/DeployablesPage";
import { MachinesBody } from "../fleet/MachinesPage";
import { WorkbenchesBody } from "../fleet/WorkbenchesPage";

// The four converged pages' interactive bodies, as widgets (epic memql#4661,
// task memql#4674).
//
// ===========================================================================
// WHY THESE ARE ONE WIDGET EACH RATHER THAN A HANDFUL OF ELEMENTS
// ===========================================================================
// Spec D6 says the arrangement system is the PAGE system, and it does not say
// every part of a page is an element. Each of these four bodies is a rich
// per-row READING that no element in the library expresses and that a table
// would be a downgrade from:
//
//   a machine card carries an online dot derived from a 30-second window, the
//   MERGE of two label maps with only one half editable, a rename, a revoke
//   and an expandable call history. A display card has four slots;
//
//   a workbench list groups per REPLICA, because "why is this workbench node
//   full" is the question, and a flat table of workspaces cannot ask it;
//
//   Artifacts and Deployables carry an upload, a search, a create form and a
//   confirm dialog each -- controls, which is exactly what a widget is for.
//
// So what converged is the LAYOUT: each page is now a manifest with a reading
// band the element library renders, the controls as registered widgets, and
// the body as one more. Each page is therefore regenerable, versioned and
// consistent with every other page -- which is the property D6 is actually
// about -- and none of them lost a behaviour on the way.
//
// PHASE 2 (the filed follow-up) is where the bodies decompose further, and the
// honest reason it is not here is that decomposing a machine card means
// designing the elements it would decompose INTO, which is element-library
// work rather than convergence work.

export function MachinesWidget(): ReactNode {
  return <MachinesBody />;
}

export function WorkbenchesWidget(): ReactNode {
  return <WorkbenchesBody />;
}

export function ArtifactsWidget(): ReactNode {
  return <ArtifactsBody />;
}

export function DeployablesWidget(): ReactNode {
  return <DeployablesBody />;
}
