import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { automationFromRow, isAutomation, type AutomationRow } from "./automations";
import {
  ladderPolicyFromRow,
  procedureFromRow,
  type LadderPolicy,
  type ProcedureRow,
} from "./ladder";

// The automations catalog: a READ, not a feed, and the surface says so.
//
// ===========================================================================
// THE ROUTING RULE WAS CHECKED, NOT ASSUMED
// ===========================================================================
// `v1:authoring:construct` carries NO broadcast routing rule in
// `component/node/routing.go` -- the authoring domain forwards
// `authoring.promote.*` and `authoring.demote.*`, which are the runtime's own
// channels and not graph node events. So a `useLiveCollection` over it would
// render "Loading from the cluster" and then a list that silently never moved,
// which is WORSE than a plain read: the caption would be claiming wiring that
// is not there.
//
// That check is the OS README's standing rule -- read the routing rules before
// deciding a concept is dark -- and this is what checking it produced. The
// section prints when it looked and offers to look again, which is the call
// the Training app made for the knowledge side and Accounts made for its
// ledger. The learned procedures are the same concept, and the ladder policy
// (`v1:authoring:ladderPolicy`) is a seeded singleton with no rule either, so
// all three reads share the one "Look again".
//
// ===========================================================================
// THREE READS, THREE ANSWERS
// ===========================================================================
// The authored catalog, the learned procedures and the policy row settle
// independently. A refusal on one is that read's own sentence, shown beside
// what did arrive -- a cluster whose learned-procedure read fails still lists
// every authored automation, and a policy nobody published is an ABSENCE the
// procedure page says out loud, never a reason to hide the procedures.
//
// ===========================================================================
// ONE PAGE, AND IT SAYS SO WHEN THERE ARE MORE
// ===========================================================================
// `cataloguedConstructsForOwner` carries `paginate 50` and
// `learnedProceduresForOwner` `paginate 100`, and neither generated method
// takes a cursor, so each reads one page. A page at its bound is reported as
// "the first N", never as the whole: a count presented as a total when it is
// a page is the same class of lie as a spend figure presented as measured
// when it was absent.
const PAGE_BOUND = 50;
export const PROCEDURE_PAGE_BOUND = 100;

export interface AutomationsRead {
  automations: AutomationRow[];
  /** How many catalogued constructs the page held, of every kind. */
  scanned: number;
  /** True when the page came back AT its bound, so there may be more. */
  bounded: boolean;
  /** The learned procedures, every rung, retired included. */
  procedures: ProcedureRow[];
  proceduresBounded: boolean;
  /** The learned-procedure read's own refusal, verbatim. "" when it worked. */
  proceduresError: string;
  /** The ladder's values, or null when the cluster published no row. */
  policy: LadderPolicy | null;
  /** The policy read's own refusal, verbatim. "" when it worked. */
  policyError: string;
  state: "idle" | "loading" | "ready" | "error";
  /** The catalog read's refusal, verbatim. "" when the last read worked. */
  error: string;
  /** When this window last looked. The whole point of an on-demand read. */
  readAt: string;
  read: () => void;
}

type ReadState = Omit<AutomationsRead, "read">;

const IDLE: ReadState = {
  automations: [],
  scanned: 0,
  bounded: false,
  procedures: [],
  proceduresBounded: false,
  proceduresError: "",
  policy: null,
  policyError: "",
  state: "idle",
  error: "",
  readAt: "",
};

function describe(reason: unknown): string {
  return reason instanceof Error ? reason.message : String(reason);
}

/**
 * The catalog, read while `active`.
 *
 * HELD BY THE APP ROOT, NOT BY A SECTION, because two sections read it: the
 * Automations list, and an approval card that names the procedure it would
 * promote -- whose parameters are only called by their goal input keys once
 * the procedure itself is in hand. `active` is the app's answer to "does any
 * visible surface need this": it reads when that turns true, which is every
 * time somebody arrives at Automations, and once when a promotion waits in the
 * Approvals queue.
 */
export function useAutomations(active = true): AutomationsRead {
  const connection = useOsConnection();
  const [state, setState] = useState<ReadState>(IDLE);
  const [nonce, setNonce] = useState(1);

  const read = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (!active) return;
    const query = connection?.query ?? null;
    if (query === null) {
      setState({ ...IDLE, state: "error", error: "Not connected to the cluster." });
      return;
    }
    const controller = new AbortController();
    const signal = controller.signal;
    setState((prev) => ({ ...prev, state: "loading", error: "" }));

    void (async () => {
      // EACH CALL IS ITS OWN PROMISE, including the throw that happens before
      // one exists: a read that fails synchronously must settle as that read's
      // refusal, not take the other two down with it.
      const [catalog, learned, policy] = await Promise.allSettled([
        (async () => query.cataloguedConstructsForOwner({}, { signal }))(),
        (async () => query.learnedProceduresForOwner({}, { signal }))(),
        (async () => query.ladderPolicyCurrent({}, { signal }))(),
      ]);
      if (signal.aborted) return;
      const next: ReadState = { ...IDLE, readAt: new Date().toISOString(), state: "ready" };

      if (catalog.status === "fulfilled") {
        const rows = catalog.value.rows() as Row[];
        // FILTERED IN THE BROWSER, deliberately: `cataloguedConstructsForOwner`
        // returns every kind, and a second DSL query for one filter would be
        // a new construct fanning out to five generated artifacts for a
        // predicate a browser can apply exactly.
        next.automations = rows.filter(isAutomation).map(automationFromRow);
        next.scanned = rows.length;
        next.bounded = rows.length >= PAGE_BOUND;
      } else {
        // VERBATIM. A refusal here is the server's own sentence and is the
        // most useful thing this panel can carry.
        next.state = "error";
        next.error = describe(catalog.reason);
      }

      if (learned.status === "fulfilled") {
        const rows = learned.value.rows() as Row[];
        next.procedures = rows.map(procedureFromRow).filter((p) => p.id !== "");
        next.proceduresBounded = rows.length >= PROCEDURE_PAGE_BOUND;
      } else {
        next.proceduresError = describe(learned.reason);
      }

      if (policy.status === "fulfilled") {
        // AT MOST ONE ROW, pinned to its literal id. None is a cluster that
        // has not published the ladder's values -- null, never the defaults.
        next.policy = ladderPolicyFromRow((policy.value.rows() as Row[])[0] ?? null);
      } else {
        next.policyError = describe(policy.reason);
      }

      setState(next);
    })();

    return () => controller.abort();
  }, [connection, nonce, active]);

  return { ...state, read };
}

export interface SetAutomationStatusState {
  busy: string;
  error: string;
  set: (constructId: string, status: "active" | "retired") => Promise<boolean>;
  reset: () => void;
}

/**
 * Arm or retire one catalogued automation.
 *
 * NOTHING NEW IS INVENTED FOR THIS. `setConstructStatus` is the authoring
 * catalog's own verb and the gate the engine already runs is the gate: the
 * mutation writes `ownerUserId: actor.userId`, so a person can only move their
 * own catalog, and `v1:authoring:construct` decides the rest.
 *
 * NEVER OFFERED ON A LEARNED PROCEDURE. A procedure is served through the
 * ladder and nothing else -- `procedureStep` refuses outside a replay -- so
 * arming one would register a construct no goal can run, and retiring one is
 * the ladder's own sweep.
 *
 * THE BUSY FLAG IS PER CONSTRUCT, not per hook, for the reason the approvals
 * queue's is: this is issued from a list, and a shared boolean would grey out
 * every row because somebody retired one of them.
 */
export function useSetAutomationStatus(): SetAutomationStatusState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  const set = useCallback(
    async (constructId: string, status: "active" | "retired"): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(constructId);
      setError("");
      try {
        await query.setConstructStatus({ constructId, status });
        return true;
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
        return false;
      } finally {
        setBusy("");
      }
    },
    [connection],
  );

  return { busy, error, set, reset: () => setError("") };
}
