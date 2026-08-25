import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useLive } from "../cluster/useLive";

// Who is still outstanding on an invitation (memql#4271).
//
// The read gates ITSELF: pendingUserInvitations carries requiresOwnerOrAdmin as
// a top-level conjunct in its own DSL filter, so the engine empties the result
// for a caller below the floor whatever this code renders. The `enabled` flag
// is a courtesy -- it stops the portal issuing a read it knows will come back
// empty -- and never the authorization.
//
// LIVE THROUGH THE STORE (memql#4539). It used to bump an epoch on every CDC
// event and re-run the whole read, which is a read per invitation issued
// anywhere in the cluster; the rows fold now. A pending invitation that gets
// ACCEPTED leaves the list the same way, because the read's own membership
// predicate is re-applied to every folded row: `status` is what "pending"
// means, and an accepted row no longer satisfies it.

export interface PendingInvitationsState {
  rows: Row[];
  loading: boolean;
  error: string;
  reload: () => void;
}

const INVITATION_CONCEPT = "v1:identity:invitation";

// A row belongs on this list only while it is still outstanding. The read says
// so server-side; this says the same thing about an arriving event, which is
// what stops an acceptance from folding straight back in as an update.
function isPending(row: Row): boolean {
  const payload = (row as { payload?: unknown }).payload;
  const source =
    payload && typeof payload === "object" && !Array.isArray(payload)
      ? (payload as Record<string, unknown>)
      : (row as Record<string, unknown>);
  const status = String(source["status"] ?? "");
  return status === "" || status === "pending";
}

export function usePendingInvitations(enabled: boolean): PendingInvitationsState {
  const { query, status } = useCluster();
  const connected = query !== null && enabled && status === "connected";

  const live = useLive<Row>(connected ? "people:pendingInvitations" : null, () => ({
    concept: INVITATION_CONCEPT,
    actions: ["created", "updated"],
    paged: false,
    seed: async (_cursor, signal) => {
      if (query === null) return { rows: [], nextCursor: "" };
      const result = await query.pendingUserInvitations({}, { signal });
      return { rows: result.rawNodes(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      if (query === null) return null;
      return getRowByConceptAndId(query, INVITATION_CONCEPT, rowId, { signal });
    },
    inScope: isPending,
  }));

  return {
    rows: connected ? live.rows : [],
    loading: connected && live.state === "seeding",
    error: live.error,
    reload: live.reload,
  };
}
