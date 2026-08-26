import { useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";

import { useAuth } from "../auth/AuthProvider";
import { clusterDomainFor } from "../cluster/editorLink";
import { Band, Button, Callout, Container, EmptyState, Select, Skeleton } from "../ui";
import { AddMachine } from "./AddMachine";
import { FleetFrame, LiveDegraded } from "./FleetFrame";
import { MachineCard } from "./MachineCard";
import { RoutingPolicyEditor } from "./RoutingPolicyEditor";
import { fleetSurfaceById } from "./urls";
import { useMachines } from "./useMachines";
import { useNow } from "./useNow";

// /fleet/machines (memql#4355): the computers registered to this cluster as
// workers, and where work lands on them.
//
// ===========================================================================
// THE SCOPE TOGGLE IS A COURTESY, NOT A GATE
// ===========================================================================
// Same standing rule as every gated screen in this portal. The all-machines
// read opens `actor.isClusterOwner==true`, evaluated in the engine against the
// auth envelope, so a non-owner who reached the toggle would get an empty set
// whatever this code renders. isClusterOwner only decides what the page
// OFFERS -- and the hook clamps the scope besides, because an empty table
// captioned "every machine in this cluster" reads as "there are none", which
// is the wrong sentence for "you may not ask".
//
// ===========================================================================
// NO ROW LIST COMPONENT HERE, AND THAT IS A DEPARTURE WORTH STATING
// ===========================================================================
// Sites and Artifacts render through RowList over the concept's @displayCard,
// which is right for a population you scan. A machine is not scanned -- it is
// READ: the online dot, the merge of two label maps, an editable half of that
// merge, a rename, a revoke and an expandable call history. A display card has
// four slots. Hand-composing here does not weaken portal_render_path_test.go's
// rule either: that rule polices the concept-AGNOSTIC browse path, and this is
// a designed surface for one concept, which is exactly the category the rule
// carves out.

export function MachinesPage(): ReactNode {
  const state = useMachines();
  const now = useNow();
  const { config } = useAuth();
  // The command palette's "Add machine" lands here with the form already
  // open (memql#4656). Read once, as the initial state, rather than watched:
  // a person who then CLOSES the form must not have it reopened under them by
  // a re-render, and the address they can still see says where they came
  // from.
  const [params] = useSearchParams();
  const [adding, setAdding] = useState(() => params.get("add") === "1");

  const surface = fleetSurfaceById("machines");
  if (surface === undefined) return null;

  const domain = clusterDomainFor(config);
  const empty =
    !state.loading && state.error === "" && state.machines.length === 0;
  // A fleet with nothing in it should open on the thing to do about that,
  // rather than on an empty list with a button beside it.
  const showAdd = adding || empty;

  return (
    <Container>
      <FleetFrame
        surface={surface}
        actions={
          <>
            {state.isClusterOwner ? (
              <Select
                value={state.scope}
                onChange={(next) => state.setScope(next === "all" ? "all" : "mine")}
                ariaLabel="Whose machines"
              >
                <option value="mine">My machines</option>
                <option value="all">Every machine in this cluster</option>
              </Select>
            ) : null}
            <Button
              pressed={showAdd}
              disabled={empty}
              onClick={() => setAdding((open) => !open)}
              title={empty ? "There is nothing here yet, so this panel stays open" : undefined}
            >
              Add a machine
            </Button>
            <Button onClick={state.reload}>Reload</Button>
          </>
        }
      >
        <LiveDegraded reason={state.liveDegraded} noun="machine" />

        {state.actionError === "" ? null : (
          <Callout tone="danger" title="That did not work">
            {state.actionError}
          </Callout>
        )}

        {showAdd ? (
          <Band title="Add a machine">
            <AddMachine domain={domain} machineCount={state.machines.length} />
          </Band>
        ) : null}

        <Band
          title={state.scope === "all" ? "Every machine" : "Your machines"}
          meta={
            state.loading
              ? "Loading…"
              : `${state.machines.length} ${state.machines.length === 1 ? "machine" : "machines"} loaded`
          }
        >
          {state.error !== "" ? (
            <Callout tone="danger" title="Could not read the machines">
              {state.error} Nothing is listed rather than an empty table -- an empty list here
              would read as &ldquo;you have no machines&rdquo;, which is not what happened.
            </Callout>
          ) : state.loading && state.machines.length === 0 ? (
            <Skeleton variant="rows" rows={3} />
          ) : empty ? (
            <EmptyState
              // The exemplar first-run empty (memql#4651). `firstRun` is true
              // only for YOUR OWN list: "no machine has ever registered" on
              // the all-cluster scope is an operator's observation about a
              // cluster, not the product introducing itself to a person.
              firstRun={state.scope !== "all"}
              statement={
                state.scope === "all"
                  ? "No machine has ever registered against this cluster."
                  : "You have no machines yet. Add one to run work on a computer you own -- mint a token above and run the command it gives you on that computer."
              }
            />
          ) : (
            <ul className="flex flex-col gap-3">
              {state.machines.map((machine) => (
                <li key={machine.id}>
                  <MachineCard
                    machine={machine}
                    now={now}
                    showOwner={state.scope === "all"}
                    busy={state.busyId === machine.id}
                    onRename={(displayName) => state.rename(machine.id, displayName)}
                    onSetLabels={(labels) => state.setOperatorLabels(machine.id, labels)}
                    onRevoke={(reason) => state.revoke(machine.id, reason)}
                  />
                </li>
              ))}
            </ul>
          )}
        </Band>

        <Band
          title="Routing"
          meta="Applies to every call dispatched on your behalf"
        >
          <RoutingPolicyEditor />
        </Band>
      </FleetFrame>
    </Container>
  );
}
