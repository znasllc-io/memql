import { useCallback, useState } from "react";

import type { PipelineRow } from "../rows";

// THE CONNECT FLOW'S STATE, HELD ABOVE THE SECTION (design record D14; issue
// memql#5502: "State held above the section the way useAddMachineFlow holds the
// install draft"). DeployablesApp holds it, so leaving the rail and coming back
// -- to another tab, to the source page -- keeps every answer, and nothing is
// written until Connect pipeline is pressed.
//
// STUB: the connect rail (Task 9) fills this in. The fields below are the
// contract DeployablesSection and the source page already rely on.

export interface ConnectFlow {
  /** The source the open flow is over, or "" when none is open. */
  packageId: string;
  /** Open the flow over a source, prefilled from its pipeline when it has one; resumes a flow already open over the same source. */
  start: (packageId: string, existing: PipelineRow | null) => void;
  /** Close the flow and drop its answers. */
  reset: () => void;
}

export function useConnectFlow(): ConnectFlow {
  const [packageId, setPackageId] = useState("");
  const start = useCallback((id: string) => setPackageId(id), []);
  const reset = useCallback(() => setPackageId(""), []);
  return { packageId, start, reset };
}
