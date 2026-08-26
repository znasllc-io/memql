import { useState, type ReactNode } from "react";

import { useCluster } from "../cluster/ClusterProvider";
import { Button, type ButtonSize } from "../ui";

// The Run affordance a queued goal has been waiting for.
//
// `queued` is, by the concept's own definition, "planning complete, tasks
// emitted, waiting for a human to click Run" (dsl/planner/concepts.memql),
// and `startPlan`'s own header names "the user clicking Run" as its caller.
// This console never shipped that click, so every userGoal that finished
// planning parked at queued forever -- to the person watching, exactly as if
// the system had hung.
//
// This deliberately renders ONLY beside a goal at `queued`. It does not step
// over the estimate / approval / budget gates NewGoalDialog's header defends:
// a goal those gates park sits at `awaitingFeedback`, a different status with
// its own conversation, and this button does not exist there.
//
// No optimistic status flip: the goal chrome and the goals list both fold
// live plan events (useGoalWorld, useGoals), so the row's own update event
// moves the badge -- the same way it would if an automation had started the
// plan on the caller's behalf.
export function RunGoalButton({
  planId,
  size,
}: {
  planId: string;
  size?: ButtonSize;
}): ReactNode {
  const { query } = useCluster();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function run(): Promise<void> {
    if (busy || query === null) return;
    setBusy(true);
    setError("");
    try {
      await query.startPlan({ planId });
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }

  return (
    <span className="inline-flex items-center gap-2">
      <Button
        tone="primary"
        {...(size === undefined ? {} : { size })}
        onClick={() => void run()}
        busy={busy}
        busyLabel="Starting…"
      >
        Run
      </Button>
      {error === "" ? null : (
        <span className="text-xs text-danger" role="alert">
          {error}
        </span>
      )}
    </span>
  );
}
