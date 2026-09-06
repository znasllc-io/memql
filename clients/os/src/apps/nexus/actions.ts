import { useCallback, useState } from "react";

import { useOsConnection } from "../../live/connection";
import { idTail } from "./rows";

// Every write the Nexus app makes, and the busy/error pair each one owns.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// Every `v1:work:*` concept declares the composite tier
// (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so a person acts on their
// own goals and the engine decides which those are. The builtins repeat their
// own gate in Go -- a builtin's annotation set carries no `@requiresRank`, so
// the floor is the handler's, and these are caller-scoped rather than ranked.
// None of that is decided in a browser.
//
// ===========================================================================
// A REFUSAL IS THE SERVER'S OWN SENTENCE AND IT RENDERS BESIDE THE CONTROL
// ===========================================================================
// Never a toast; this shell has none. Each write owns its own error slot
// rather than sharing one, because the create form, the run page's acts and
// the approval's decide buttons are three different places on screen -- and a
// shared slot puts a refusal under a control somebody is looking at naming a
// failure they did not cause.
//
// THE EXECUTORS ARE BEING WRITTEN IN PARALLEL WITH THIS SURFACE. Until
// `integration.work.*` is wired, every one of these calls answers with the
// engine's own "no such executor" sentence, and that is what the surface will
// show -- verbatim, in place, with the act still offered. That is the correct
// behaviour and not a stopgap: a client that assumed success would report a
// goal as accepted that no run exists for.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface WriteState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  reset: () => void;
}

const NOT_CONNECTED = "Not connected to the cluster, so nothing was written.";

// ---------------------------------------------------------------------------
// createGoal
// ---------------------------------------------------------------------------

export interface NewGoalFacts {
  statement: string;
  accountIds: string[];
}

export interface CreateGoalState extends WriteState {
  /** The goal the last successful call opened, so the surface can say which. */
  createdGoalId: string;
  /** The run it opened alongside it -- a goal always gets one immediately. */
  createdRunId: string;
  create: (facts: NewGoalFacts) => Promise<string>;
}

/**
 * Accept a goal.
 *
 * `createGoal` opens the goal AND its first run in `compiling` and dispatches
 * compile, so ONE call is the whole act -- there is no client-side follow-up
 * write to get half-done, which is the property the Deployables app records
 * about `packageDeploy`.
 *
 * `requestedVia` is `"nexus"`, which is the enum member for this shell's own
 * surfaces: the design record names Nexus as the goal-facing app on MemQL OS
 * and this app is the first half of it. Guessing `"api"` would file every
 * goal a person typed as one a program submitted.
 */
export function useCreateGoal(): CreateGoalState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [createdGoalId, setCreatedGoalId] = useState("");
  const [createdRunId, setCreatedRunId] = useState("");

  const create = useCallback(
    async (facts: NewGoalFacts): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return "";
      }
      const statement = facts.statement.trim();
      if (statement === "") {
        // The one rule a browser can answer, answered here rather than sent.
        // `statement` is `string!`, so the server would refuse it too -- but a
        // round trip to be told what this form already knows is a round trip
        // somebody waits for.
        setError("Say what you want done first.");
        return "";
      }
      setBusy(true);
      setError("");
      setCreatedGoalId("");
      setCreatedRunId("");
      try {
        const result = await query.createGoal({
          statement,
          requestedVia: "nexus",
          ...(facts.accountIds.length > 0 ? { accountIds: facts.accountIds } : {}),
        });
        const reply = result.rows()[0] ?? null;
        const goalId = typeof reply?.["goalId"] === "string" ? (reply["goalId"] as string) : "";
        const runId = typeof reply?.["runId"] === "string" ? (reply["runId"] as string) : "";
        setCreatedGoalId(goalId);
        setCreatedRunId(runId);
        // NOTHING IS INSERTED LOCALLY. `v1:work:goal` broadcasts, so the row
        // arrives on the feed the list already draws, with the arrival cue,
        // exactly like a goal a responsibility raised. A local insert would
        // put a row on screen the cluster had not confirmed, and the two would
        // differ in whatever the optimistic copy guessed wrong -- the status,
        // the origin, the id.
        return goalId;
      } catch (err: unknown) {
        setError(describe(err));
        return "";
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    createdGoalId,
    createdRunId,
    create,
    reset: () => {
      setError("");
      setCreatedGoalId("");
      setCreatedRunId("");
    },
  };
}

// ---------------------------------------------------------------------------
// cancelGoal
// ---------------------------------------------------------------------------

export interface CancelGoalState extends WriteState {
  cancel: (goalId: string, reason: string) => Promise<boolean>;
}

/**
 * Close a goal and ask its runs to stop.
 *
 * CANCELLATION IS REQUESTED, NOT DONE, and the surface says so where the act
 * is offered: a run notices at its next step boundary, so a step already in
 * flight finishes and is journaled rather than being abandoned mid-effect. A
 * button that implied an immediate stop would have somebody refreshing to find
 * out why the run is still moving.
 */
export function useCancelGoal(): CancelGoalState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const cancel = useCallback(
    async (goalId: string, reason: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return false;
      }
      setBusy(true);
      setError("");
      try {
        const trimmed = reason.trim();
        await query.cancelGoal({ goalId, ...(trimmed === "" ? {} : { reason: trimmed }) });
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, error, cancel, reset: () => setError("") };
}

// ---------------------------------------------------------------------------
// replayRun and forkRun
// ---------------------------------------------------------------------------

export interface DeriveRunState extends WriteState {
  /** The NEW run the last successful call opened. Both verbs make one. */
  derivedRunId: string;
  replay: (runId: string) => Promise<string>;
  fork: (runId: string, atStepKey: string) => Promise<string>;
}

/**
 * Replay a run, or fork it at a step.
 *
 * ONE HOOK FOR BOTH, because they are one act with two settings from this
 * surface's side -- each reads a source run and returns a NEW run id, each
 * leaves the source untouched, and each lands the person on a different run.
 * Two hooks would mean two busy flags and two error slots for one control
 * cluster on one bar.
 *
 * `policy` is left UNSENT rather than sent as `"strict"`: strict is the
 * declared default and the builtin applies it, so naming it here would put a
 * second copy of a default in a browser -- the copy that goes stale.
 */
export function useDeriveRun(): DeriveRunState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [derivedRunId, setDerivedRunId] = useState("");

  const call = useCallback(
    async (fn: () => Promise<{ rows: () => Record<string, unknown>[] }>): Promise<string> => {
      setBusy(true);
      setError("");
      setDerivedRunId("");
      try {
        const result = await fn();
        const reply = result.rows()[0] ?? null;
        const runId = typeof reply?.["runId"] === "string" ? (reply["runId"] as string) : "";
        setDerivedRunId(runId);
        return runId;
      } catch (err: unknown) {
        setError(describe(err));
        return "";
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  const replay = useCallback(
    async (runId: string): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return "";
      }
      return call(() => query.replayRun({ runId }));
    },
    [connection, call],
  );

  const fork = useCallback(
    async (runId: string, atStepKey: string): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return "";
      }
      if (atStepKey.trim() === "") {
        // `atStepKey` is `string!` on the builtin, and a fork with no step to
        // diverge at is not a thing -- it is a replay. Saying so beats a
        // refusal that reads like the fork failed.
        setError("Pick the step to fork at first.");
        return "";
      }
      return call(() => query.forkRun({ runId, atStepKey }));
    },
    [connection, call],
  );

  return {
    busy,
    error,
    derivedRunId,
    replay,
    fork,
    reset: () => {
      setError("");
      setDerivedRunId("");
    },
  };
}

// ---------------------------------------------------------------------------
// decideApproval
// ---------------------------------------------------------------------------

export type Decision = "approved" | "rejected" | "answered";

export interface DecideApprovalState extends WriteState {
  /** The approval currently being decided, so one row's buttons go busy and
   *  not every row's. An inbox is many independent decisions. */
  deciding: string;
  decide: (
    approvalId: string,
    decision: Decision,
    answer?: Record<string, unknown>,
  ) => Promise<boolean>;
}

/**
 * Decide an approval and resume the run parked on it.
 *
 * THE BUSY FLAG IS PER APPROVAL, not per hook. Every other write in this app
 * acts on the one thing on screen; this one is issued from a queue, and a
 * shared boolean would grey out every row in the inbox because somebody
 * approved one of them.
 *
 * A refusal here is the one worth reading in full: the builtin refuses a
 * decision whose artifact changed since it was raised, because an approval is
 * a decision about a specific command, patch, message or draft and never
 * carries to a modified one. The server's sentence names that; a paraphrase
 * would drop the only fact that tells somebody to look again before deciding.
 */
export function useDecideApproval(): DecideApprovalState {
  const connection = useOsConnection();
  const [deciding, setDeciding] = useState("");
  const [error, setError] = useState("");

  const decide = useCallback(
    async (
      approvalId: string,
      decision: Decision,
      answer?: Record<string, unknown>,
    ): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return false;
      }
      setDeciding(idTail(approvalId));
      setError("");
      try {
        await query.decideApproval({
          approvalId,
          decision,
          ...(answer === undefined ? {} : { answer }),
        });
        // Nothing is patched locally: the decision broadcasts, and the row
        // leaves the pending feed on its own update. Watching it go IS the
        // confirmation, which is why there is no success message here.
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setDeciding("");
      }
    },
    [connection],
  );

  return {
    busy: deciding !== "",
    error,
    deciding,
    decide,
    reset: () => setError(""),
  };
}
