import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { automationFromRow, isAutomation, type AutomationRow } from "./automations";

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
// ledger.
//
// ===========================================================================
// ONE PAGE, AND IT SAYS SO WHEN THERE ARE MORE
// ===========================================================================
// `cataloguedConstructsForOwner` carries `paginate 50` and the generated
// method takes no cursor argument, so this reads one page. A catalog at the
// bound is reported as "the first 50", never as the whole of it: a count
// presented as a total when it is a page is the same class of lie as a spend
// figure presented as measured when it was absent.
const PAGE_BOUND = 50;

export interface AutomationsRead {
  automations: AutomationRow[];
  /** How many catalogued constructs the page held, of every kind. */
  scanned: number;
  /** True when the page came back AT its bound, so there may be more. */
  bounded: boolean;
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence, verbatim. "" when the last read worked. */
  error: string;
  /** When this window last looked. The whole point of an on-demand read. */
  readAt: string;
  read: () => void;
}

const IDLE: Omit<AutomationsRead, "read"> = {
  automations: [],
  scanned: 0,
  bounded: false,
  state: "idle",
  error: "",
  readAt: "",
};

export function useAutomations(): AutomationsRead {
  const connection = useOsConnection();
  const [state, setState] = useState<Omit<AutomationsRead, "read">>(IDLE);
  const [nonce, setNonce] = useState(1);

  const read = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) {
      setState({ ...IDLE, state: "error", error: "Not connected to the cluster." });
      return;
    }
    const controller = new AbortController();
    const signal = controller.signal;
    setState((prev) => ({ ...prev, state: "loading", error: "" }));

    void (async () => {
      try {
        const result = await query.cataloguedConstructsForOwner({}, { signal });
        if (signal.aborted) return;
        const rows = result.rows() as Row[];
        setState({
          // FILTERED IN THE BROWSER, deliberately: `cataloguedConstructsForOwner`
          // returns every kind, and a second DSL query for one filter would be
          // a new construct fanning out to five generated artifacts for a
          // predicate a browser can apply exactly.
          automations: rows.filter(isAutomation).map(automationFromRow),
          scanned: rows.length,
          bounded: rows.length >= PAGE_BOUND,
          state: "ready",
          error: "",
          readAt: new Date().toISOString(),
        });
      } catch (err: unknown) {
        if (signal.aborted) return;
        setState({
          ...IDLE,
          state: "error",
          // VERBATIM. A refusal here is the server's own sentence and is the
          // most useful thing this panel can carry.
          error: err instanceof Error ? err.message : String(err),
          readAt: new Date().toISOString(),
        });
      }
    })();

    return () => controller.abort();
  }, [connection, nonce]);

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
