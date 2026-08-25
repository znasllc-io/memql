import type { AccessSummary } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "./ClusterProvider";
import { useLiveValue } from "./useLive";

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
//
// ONE READ PER CONNECTION, HOWEVER MANY CALLERS (memql#4539). Fifteen modules
// call this hook and several of them mount on every page, so landing anywhere
// used to issue fourteen identical MyAccessMsg round trips -- and the only
// thing wrong with each of those call sites was that there were fourteen of
// them. It now rides the SDK's LiveValue: a shared, connection-scoped,
// in-flight-deduped read, keyed so every caller joins the same answer.

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
  const { value, state, error } = useLiveValue<AccessSummary>(
    query !== null && enabled ? "cluster:myAccess" : null,
    async (signal) => {
      if (query === null) return null;
      return query.getMyAccess({ signal });
    },
  );

  return {
    // Cleared rather than kept when there is no connection: a stale identity
    // next to a dead one reads as "still signed in to that cluster", which is
    // the one thing this widget must never imply.
    access: query === null || !enabled ? null : value,
    loading: query !== null && enabled && state === "seeding",
    error,
  };
}
