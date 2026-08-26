import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { clusterDomainFor } from "../cluster/editorLink";
import { AddMachine } from "../fleet/AddMachine";
import { useMachines } from "../fleet/useMachines";

// Pairing a machine, as a widget (epic memql#4661).
//
// AddMachine needs two things the arrangement cannot hand it -- the cluster's
// own domain, and how many machines are listed right now (it reports a
// registration when that count grows past what it captured at mint time). Both
// come from hooks, which is exactly why this adapter exists rather than the
// registry calling AddMachine directly: a widget's inputs are its own business,
// and threading them through the arrangement would put fleet-specific
// parameters into a generic grammar.
export function AddMachineWidget({ onChanged }: { onChanged: () => void }): ReactNode {
  const { config } = useAuth();
  const state = useMachines();
  // The page reloads itself from the same live subscription the list reads,
  // so a pairing shows up without this being called. Accepted and unused
  // rather than absent, because the registry hands every widget the same
  // props and a widget that took a different set would make the registry's
  // one shape into two.
  void onChanged;
  return <AddMachine domain={clusterDomainFor(config)} machineCount={state.machines.length} />;
}
