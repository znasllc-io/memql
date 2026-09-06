import { useCallback, useState } from "react";

import { useOs } from "../chrome/state";
import { useOsConnection } from "../live/connection";

// ASK-TO-GOAL: the handoff from asking to having it done.
//
// ===========================================================================
// WHY THE HANDOFF IS A FIRST-CLASS ACT AND NOT A COPY-PASTE
// ===========================================================================
// Ask is the OS's prompt surface and a GOAL is what replaces the chat prompt
// as the unit of intent (epic memql#4785's own framing). Somebody who asks
// "how do I reconcile the September ledger" and reads the answer has, half the
// time, decided they want it DONE. Making them retype the sentence into
// another app is the seam where the idea gets lost.
//
// ===========================================================================
// `requestedVia` IS "ask", AND THAT MATTERS
// ===========================================================================
// The enum has a member for this surface, so a goal that arrived from Ask is
// filed as one. Writing "nexus" here -- which is what the Nexus app's own
// composer writes -- would make the two indistinguishable, and the question
// this field exists to answer is exactly which surface a goal came through.
//
// ===========================================================================
// NOTHING IS INSERTED LOCALLY
// ===========================================================================
// `v1:work:goal` broadcasts, so the row arrives on the feed the goals list
// already draws, with the arrival cue, exactly like a goal a responsibility
// raised. Opening the app on the new goal is a NAVIGATION, not a render of a
// row this browser invented.

export interface MakeGoalState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  make: (prompt: string) => Promise<void>;
  reset: () => void;
}

export function useMakeGoal(): MakeGoalState {
  const connection = useOsConnection();
  const { actions } = useOs();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const make = useCallback(
    async (prompt: string): Promise<void> => {
      const query = connection?.query ?? null;
      const statement = prompt.trim();
      if (statement === "") return;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return;
      }
      setBusy(true);
      setError("");
      try {
        const result = await query.createGoal({ statement, requestedVia: "ask" });
        const reply = result.rows()[0] ?? null;
        const goalId = typeof reply?.["goalId"] === "string" ? (reply["goalId"] as string) : "";
        // OPEN-BY-ID FIRST, which focuses an existing Nexus window rather than
        // opening a second one, and is the path that carries the `canOpen`
        // admission check. A refusal carries no window id, so there is nothing
        // to navigate and doing nothing is correct.
        const effect = actions.openApp("nexus", "goals", goalId === "" ? {} : { goalId });
        if (effect.kind === "focused-existing") {
          actions.navigateSection(effect.windowId, "goals");
        }
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [connection, actions],
  );

  return { busy, error, make, reset: () => setError("") };
}
