import { useEffect, useState } from "react";
import type { AccessSummary } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "./ClusterProvider";

// Who the CLUSTER thinks you are.
//
// Read over the connection (MyAccessMsg -> userId / primaryEmail /
// clusterRole) rather than decoded out of the JWT the portal is holding. Both
// would usually agree, and the difference is the whole point: the JWT is a
// claim this page happens to carry, while the stream's answer is the identity
// the server actually resolved for THIS connection. If they ever diverge --
// a rotated token, a revoked session, a role changed since the token was
// minted, a proxy that swapped the credential -- the header should show what
// the cluster is acting on, not what the browser believes.
//
// It also keeps the token out of one more code path: rendering the signed-in
// user needs no access to the credential at all.

export interface MyAccessState {
  access: AccessSummary | null;
  loading: boolean;
  error: string;
}

// enabled defaults to true. Pass false to skip the round trip entirely from a
// caller that will not render the result -- a parameter rather than a
// conditional call, because hook order cannot vary between renders.
export function useMyAccess(enabled = true): MyAccessState {
  const { query } = useCluster();
  const [access, setAccess] = useState<AccessSummary | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (query === null || !enabled) {
      // Cleared rather than kept: a stale identity next to a dead connection
      // reads as "still signed in to that cluster", which is the one thing
      // this widget must never imply.
      setAccess(null);
      setLoading(false);
      setError("");
      return;
    }

    let live = true;
    setLoading(true);
    setError("");

    void query
      .getMyAccess()
      .then((summary) => {
        if (live) setAccess(summary);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, enabled]);

  return { access, loading, error };
}
