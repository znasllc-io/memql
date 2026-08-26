import { useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";
import { Button, Dialog, Field, Textarea } from "../ui";
import { nexusPath } from "./urls";

// Starting a goal from the console (memql#4528).
//
// ===========================================================================
// THIS REVERSES A DECISION THIS FILE'S NEIGHBOURS USED TO STATE
// ===========================================================================
// The index page's header (GoalsPage today) said "NOTHING IN THIS CONSOLE
// CREATES ONE" and its empty state told the reader goals come from asking an
// agent. That was true and it is not any more; both have been rewritten
// rather than left standing, because a superseded argument sitting beside its
// replacement reads as the live one.
//
// ===========================================================================
// NO NEW WIRE SURFACE
// ===========================================================================
// `createPlan` (dsl/planner/mutations.memql) is the single client-reachable
// write path for plan creation, and this calls it through the generated
// QueryClient like every other portal write. No proto, no HTTP, no DSL change.
//
// ===========================================================================
// THIS BUTTON STARTS THE PLANNING LIFECYCLE, NOT SPEND
// ===========================================================================
// There is deliberately NO `startPlan` call here. createPlan lands the row and
// the planner picks it up to SIZE the goal; the estimate / approval / budget
// gates in docs/public/ai/llm-cost-control.md then decide whether anything is
// spent, and a goal over the threshold parks in awaitingFeedback for the
// person who asked. Adding startPlan would step over all of that from a
// button, which is exactly the bypass that doc exists to prevent.
//
// The startPlan click DOES exist -- one lifecycle stage later. A goal whose
// planning finished parks at `queued`, which the concept defines as "waiting
// for a human to click Run", and RunGoal.tsx renders exactly that click,
// exactly there. Two buttons, two stages: this one starts sizing, that one
// starts spend.
//
// ===========================================================================
// THE PICKUP, VERIFIED AGAINST CODE RATHER THAN AGAINST THE COMMENT
// ===========================================================================
// createPlan's own comment claimed plans land in status 'queued' so a
// `routePlanOnQueue` automation fires. Both halves were wrong: the stamp
// writes status: "planning", and no automation by that name exists anywhere in
// the tree. What actually picks a plan up is the planner integration's
// subscription to `graph.node.created.v1:planner:plan` ->
// PlannerAgentLoop.HandlePlanCreated (integrations/planner/agent_loop.go),
// which accepts status "planning" OR "queued" and skips only the kinds another
// dispatcher claims -- trainSpecialist, embedDomainItems, adHocAction,
// scopeElevation, agentInvocation. A userGoal in "planning" is none of those,
// so a plan created here is picked up exactly as a chat-created one is. The
// stale COMMENT was fixed in the same change; the stamp was not touched,
// because the stamp is what works.

// The synthetic space this console's goals are filed under.
//
// v1:planner:plan.partitionId is a `v1:cognition:space.id` and is @required --
// the Tasks page is per-space and budget rollups scope to one. The console is
// not a space and has no chat surface behind it, so this follows the sentinel
// convention the other space-less callers already use
// (integrations/planner/refresh_cron.go's "system:knowledge-refresh",
// reactive_loop.go's "system:reactive-loop"): a well-formed row that does not
// pretend to belong to a conversation that never happened. Provenance stays
// honest elsewhere -- triggerSource keeps its stamped "user.explicit" default
// and requestedBy names the person who typed the goal.
export const CONSOLE_PARTITION_ID = "system:portal-console";

// The one name for this affordance, in both places a person needs it: the
// goal chrome's header and the empty state. One component rather than two call
// sites wiring a button to a dialog, so the label, the copy and the write
// cannot drift between them.
export function NewGoalAction({ tone = "quiet" }: { tone?: "primary" | "quiet" }): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button tone={tone} onClick={() => setOpen(true)}>
        New goal
      </Button>
      <NewGoalDialog open={open} onClose={() => setOpen(false)} />
    </>
  );
}

export function NewGoalDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}): ReactNode {
  const { query } = useCluster();
  const { access } = useMyAccess();
  const navigate = useNavigate();
  const [goal, setGoal] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const userId = access?.userId ?? "";
  const text = goal.trim();

  function close(): void {
    if (busy) return;
    setGoal("");
    setError("");
    onClose();
  }

  async function submit(): Promise<void> {
    if (text === "" || busy) return;
    if (query === null) {
      setError("Not connected to a cluster. See the connection state in the rail footer.");
      return;
    }
    // requestedBy is @required AND it is what makes the goal YOURS: Nexus
    // filters the map on requestedBy == your own user id (the memql#4366
    // residual), so a blank one would write a goal nothing in this console
    // could ever show you again. Refusing beats writing that row.
    if (userId === "") {
      setError("This connection has not resolved a user yet, so the goal has nobody to belong to.");
      return;
    }

    setBusy(true);
    setError("");
    // The id is minted HERE, the same way the composer mints a view id, so
    // the navigation below needs no second read to learn where it landed.
    const planId = newShortId();
    try {
      await query.createPlan({
        planId,
        partitionId: CONSOLE_PARTITION_ID,
        kind: "userGoal",
        goal: text,
        requestedBy: userId,
        // Nothing to carry. `input` is @required and the dispatchers that
        // read it (trainSpecialist, embedDomainItems) claim kinds this is
        // not; for a userGoal the goal TEXT is the whole input.
        input: {},
      });
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
      return;
    }
    setBusy(false);
    setGoal("");
    onClose();
    // Land on the new goal's map so the operator watches it materialize,
    // rather than on a list they then have to find it in.
    navigate(nexusPath(planId));
  }

  return (
    <Dialog open={open} onClose={close} labelledBy="new-goal-dialog-title">
      <form
        // text-left EXPLICITLY. This dialog is rendered from wherever its
        // button is, and one of those places is EmptyState -- whose container
        // is text-center, which the dialog inherits through the portal-less
        // <dialog>. Without this the same dialog reads centred from the empty
        // state and left-aligned from the goal header, which is a difference
        // no test in jsdom can see.
        className="flex flex-col gap-3 p-5 text-left"
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <h2 id="new-goal-dialog-title" className="text-base font-semibold">
          New goal
        </h2>
        <p className="text-sm text-muted">
          Say what you want done, the way you would say it to a colleague. The planner
          sizes the goal before it spends anything, and asks you first if it is big.
        </p>
        <Field
          label="Goal"
          hint="One or two sentences. You can add detail once it starts."
          {...(error === "" ? {} : { error })}
        >
          <Textarea
            value={goal}
            onChange={setGoal}
            rows={4}
            disabled={busy}
            placeholder="Draft the Q3 board update from the deployment and audit history"
          />
        </Field>
        <div className="mt-2 flex justify-end gap-2">
          <Button tone="quiet" onClick={close} disabled={busy}>
            Cancel
          </Button>
          <Button
            tone="primary"
            type="submit"
            busy={busy}
            busyLabel="Starting…"
            disabled={text === ""}
          >
            Start the goal
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
