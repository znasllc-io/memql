import { createContext, useContext, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";

import { useAuth } from "../auth/AuthProvider";
import { clusterDomainFor } from "../cluster/editorLink";
import { ArrangedPage } from "../pages/ArrangedPage";
import { Band, Button, EmptyState, ErrorNotice, Select, Skeleton } from "../ui";
import { AddMachine } from "./AddMachine";
import { LiveDegraded } from "./FleetFrame";
import { MachineCard } from "./MachineCard";
import { MACHINES_PAGE, MACHINES_PAGE_ID } from "./manifests";
import { RoutingPolicyEditor } from "./RoutingPolicyEditor";
import { fleetSurfaceById } from "./urls";
import { useMachines, type MachinesState } from "./useMachines";
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

// The page (epic memql#4661, task memql#4674).
//
// It is now an ARRANGEMENT: a manifest with a reading band the element library
// renders over v1:worker:registration, the pairing and routing controls as
// registered WIDGETS, and the machine cards as one more. Which means it is
// regenerable and versioned like every other page in the console, and its
// header, its version strip and its regenerate control are the shared ones.
//
// The cards stayed one widget rather than becoming elements, and that is a
// decision rather than a shortcut: a machine card carries an online dot
// derived from a 30-second window, the MERGE of two label maps with only one
// half editable, a rename, a revoke and an expandable call history. A display
// card has four slots. Decomposing it means designing the elements it would
// decompose into, which is element-library work rather than convergence work
// -- and is what phase 2 is for.
export function MachinesPage(): ReactNode {
  const state = useMachines();
  const surface = fleetSurfaceById("machines");
  if (surface === undefined) return null;

  return (
    <MachinesStateContext.Provider value={state}>
      <ArrangedPage
        manifest={MACHINES_PAGE}
        pageId={MACHINES_PAGE_ID}
        selectedRowId=""
        onSelect={() => {}}
        area="fleet"
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
            <Button onClick={state.reload}>Reload</Button>
          </>
        }
      />
    </MachinesStateContext.Provider>
  );
}

// The machines state, shared between the page's header actions and the body
// widget.
//
// A CONTEXT rather than two calls to useMachines(): the hook opens a live
// subscription, and calling it twice would open two -- which is not a
// performance detail but a correctness one, since the second subscription's
// events would repaint a list the first one already owns.
const MachinesStateContext = createContext<MachinesState | null>(null);

function useMachinesState(): MachinesState {
  const state = useContext(MachinesStateContext);
  if (state === null) {
    throw new Error("the machines widget must be rendered inside MachinesPage");
  }
  return state;
}

// MachinesBody is what the `fleetMachines` widget renders. Every behaviour the
// page had, unchanged -- see the header for why it is one widget.
export function MachinesBody(): ReactNode {
  const state = useMachinesState();
  const now = useNow();
  const { config } = useAuth();
  // The command palette's "Add machine" lands here with the form already open
  // (memql#4656). Read ONCE, as the initial state, rather than watched: a
  // person who then closes the form must not have it reopened under them by a
  // re-render, and the address they can still see says where they came from.
  //
  // It lives in the BODY rather than in the page shell because the body owns
  // the form -- the shell became an ArrangedPage in epic memql#4661 and holds
  // no form state at all.
  const [params] = useSearchParams();
  const [adding, setAdding] = useState(() => params.get("add") === "1");

  const domain = clusterDomainFor(config);
  const empty =
    !state.loading && state.error === "" && state.machines.length === 0;
  // A fleet with nothing in it should open on the thing to do about that,
  // rather than on an empty list with a button beside it.
  const showAdd = adding || empty;

  return (
    <>
        <LiveDegraded reason={state.liveDegraded} noun="machine" />

        <div className="flex justify-start">
          <Button
            pressed={showAdd}
            disabled={empty}
            onClick={() => setAdding((open) => !open)}
            title={empty ? "There is nothing here yet, so this panel stays open" : undefined}
          >
            Add a machine
          </Button>
        </div>

        {state.actionError === "" ? null : (
          <ErrorNotice
            sentence="That did not work."
            next="The machine is unchanged; try it again."
            detail={state.actionError}
          />
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
            <ErrorNotice
              sentence="Could not read the machines."
              next="Nothing is listed below. Do not read that as having no machines -- this read failed, so the answer is unknown."
              detail={state.error}
            />
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
    </>
  );
}
