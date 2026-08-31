import { useCallback, useEffect, useState } from "react";

import { useOsConnection } from "../../../live/connection";
import { invocationFromRow, type InvocationRow } from "../rows";

// One machine's recent calls, read ON DEMAND.
//
// ===========================================================================
// THIS ONE IS A QUERY, AND THAT IS A DESIGN DECISION, NOT AN OVERSIGHT
// ===========================================================================
// Every other Fleet feed in the OS is a LiveList, because
// v1:worker:registration, v1:worker:routingPolicy and v1:workbench:workspace
// all carry broadcast routing rules (component/node/routing.go) and their
// events cross replicas to a browser subscriber. v1:worker:invocation is
// deliberately EXCLUDED from that set on volume grounds -- one row per tool
// call, which on a busy fleet is orders of magnitude more traffic than the
// rest of the concept list combined -- and routing.go says so in as many
// words.
//
// So subscribing here would not be "live": it would be a subscription that
// silently receives nothing in the only topology that ships, and a panel that
// renders an empty list rather than an honest "as of when". The read is
// therefore explicit -- it runs when the panel opens and when the operator
// asks again -- and the surface says when it last ran.
//
// ===========================================================================
// invocationsForWorker IS THE SELF-SCOPED READ
// ===========================================================================
// v1:worker:invocation declares no row tier, so the caller scope lives in the
// query's FILTER: `workerId==args.workerId && ownerUserId==actor.userId`. The
// operator variant (invocationsForWorkerAsOperator, `actor.isClusterOwner`)
// belongs to a cluster-wide machines view, which this app does not have --
// the Machines section is the CALLER's registrations. Calling the operator
// variant here would answer identically for every machine a person owns and
// differently for none of them.

export interface InvocationsState {
  invocations: InvocationRow[];
  loading: boolean;
  error: string;
  /** When the read last completed, or null before the first one. The panel
   *  renders it: a list with no subscription behind it has to say how old
   *  it is, or it reads as live. */
  readAt: Date | null;
  refresh: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useInvocations(workerId: string, enabled: boolean): InvocationsState {
  const connection = useOsConnection();
  const [invocations, setInvocations] = useState<InvocationRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [readAt, setReadAt] = useState<Date | null>(null);
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (!enabled || connection === null || workerId === "") return;
    let live = true;
    setLoading(true);
    setError("");

    void connection.query
      .invocationsForWorker({ workerId })
      .then((result) => {
        if (!live) return;
        setInvocations(result.rows().map(invocationFromRow));
        setReadAt(new Date());
      })
      .catch((err: unknown) => {
        if (!live) return;
        // The rows already on screen are KEPT. They were true when they were
        // read, and blanking them on a failed refresh replaces a stale
        // answer with no answer -- which is strictly less information.
        setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [connection, workerId, enabled, epoch]);

  const refresh = useCallback(() => setEpoch((n) => n + 1), []);

  return { invocations, loading, error, readAt, refresh };
}
