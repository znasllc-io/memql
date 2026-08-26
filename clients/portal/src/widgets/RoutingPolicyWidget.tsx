import type { ReactNode } from "react";

import { RoutingPolicyEditor } from "../fleet/RoutingPolicyEditor";

// The fleet's routing policy, as a widget (epic memql#4661).
//
// A pure re-export in adapter clothing: RoutingPolicyEditor already takes no
// props and owns its own read and write. The adapter exists so the registry
// has one shape for every entry -- a registry where some ids point at
// components and others at wrappers is a registry whose next entry has to
// decide which kind it is.
export function RoutingPolicyWidget({ onChanged }: { onChanged: () => void }): ReactNode {
  void onChanged;
  return <RoutingPolicyEditor />;
}
