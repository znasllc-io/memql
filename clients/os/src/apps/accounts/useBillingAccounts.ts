import { useCallback, useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { billingAccountFromRow, sortBillingAccounts, type BillingAccountRow } from "./billing";

// The caller's billing accounts (`v1:identity:account`), read on open.
//
// ===========================================================================
// AN ON-DEMAND READ, AND THE ROUTING TABLE IS WHY
// ===========================================================================
// The client registry beside this one IS live, and its hook says why:
// component/node/routing.go forwards BOTH `graph.node.created` and
// `graph.node.updated` for `v1:accounts:account`. This concept is not that
// case. The table carries exactly one rule for it --
// `graph.node.created.v1:identity:account` -- and no update rule at all.
//
// A collection over it would therefore seed correctly, fold a create, and then
// never move again for a rename, a suspension or an archive. That is worse
// than not being live: "correct on load, frozen after" is the failure the OS
// README names, and a list where one verb arrives and the other silently does
// not gives a reader no way to tell which kind of list they are looking at.
//
// So it reads, it prints WHEN it read, and it re-reads on demand -- the same
// call `useAccountTokens` makes one level down, where the concept
// (`v1:identity:identity`) carries no rule at all.
//
// ===========================================================================
// THE GATE IS THE QUERY'S OWN
// ===========================================================================
// `accounts` filters `ownerUserId==actor.userId`, and the concept declares
// `@rowAuthz(owner="ownerUserId")` with NO cluster-owner escape -- so a caller
// sees exactly the billing accounts that are theirs, and an admin sees a
// colleague's no more than anyone else does. Nothing here decides any of it,
// and no role is checked in this file: a boolean edited in a browser changes
// none of it, and a second copy of the rule would be free to disagree.
//
// EVERY STATUS IS ASKED FOR. `accounts` takes an optional `status` and it is
// deliberately not passed: a suspended or archived billing account's
// credentials still exist and still work until they are revoked, so narrowing
// the read would hide the one surface that can revoke them.

/** The caller's billing accounts, and the state of the read that fetched them. */
export interface BillingAccountFeed {
  accounts: BillingAccountRow[];
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence, verbatim. "" when the read worked. */
  error: string;
  /** When this browser last asked. "" before the first answer. */
  readAt: string;
  reload: () => void;
}

const EMPTY: Omit<BillingAccountFeed, "reload"> = {
  accounts: [],
  state: "idle",
  error: "",
  readAt: "",
};

export function useBillingAccounts(): BillingAccountFeed {
  const connection = useOsConnection();
  const [read, setRead] = useState(EMPTY);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) return;
    const controller = new AbortController();
    const signal = controller.signal;

    void (async () => {
      setRead({ ...EMPTY, state: "loading" });
      try {
        const result = await query.accounts({}, { signal });
        if (signal.aborted) return;
        setRead({
          accounts: sortBillingAccounts((result.rows() as Row[]).map(billingAccountFromRow)),
          state: "ready",
          error: "",
          readAt: new Date().toISOString(),
        });
      } catch (err: unknown) {
        if (signal.aborted) return;
        setRead({
          accounts: [],
          state: "error",
          // VERBATIM. A refusal here is the cluster's own account of why, and
          // a sentence composed in this browser would be a guess printed in
          // the server's voice.
          error: err instanceof Error ? err.message : String(err),
          readAt: new Date().toISOString(),
        });
      }
    })();

    return () => controller.abort();
  }, [connection, nonce]);

  return useMemo(() => ({ ...read, reload }), [read, reload]);
}
