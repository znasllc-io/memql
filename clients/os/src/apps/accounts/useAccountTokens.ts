import { useCallback, useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { accountTokenFromRow, sortAccountTokens, type AccountTokenRow } from "./credentials";

// The credentials issued on behalf of one BILLING account
// (`v1:identity:account`, never the client registry), read on open.
//
// ===========================================================================
// AN ON-DEMAND READ, AND HERE THAT IS NOT A CHOICE
// ===========================================================================
// The ledger's four bands are on-demand because a live rollup would be four
// more subscriptions per open account. This one has no such trade to weigh:
// `v1:identity:identity` carries NO routing rule at all
// (component/node/routing.go forwards `v1:identity:user`,
// `v1:identity:invitation`, `v1:identity:account` and the two audit logs, and
// stops there). Under default-deny a subscription over it would seed
// correctly and then never move again, which is the failure mode the OS
// README names: correct on load, frozen after, and indistinguishable from
// working.
//
// So it re-reads, and the two writes beside it re-read on success. That is
// the ONLY thing that puts a fresh mint or a revocation on screen, which is
// why `reload` is returned rather than kept private.
//
// ===========================================================================
// THE READ'S GATE IS THE QUERY'S OWN
// ===========================================================================
// `accountTokensForAccount` filters `userId==actor.userId` -- the operator is
// the credential's subject, so an operator sees the credentials THEY issued
// and never a colleague's. The `accountId` conjunct narrows an
// already-authorized set; it is not the gate. Nothing here decides any of it.

/** One billing account's credentials, and the state of the read that fetched
 *  them. */
export interface AccountTokenFeed {
  tokens: AccountTokenRow[];
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence, verbatim. "" when the read worked. */
  error: string;
  /** When this browser last asked. "" before the first answer. */
  readAt: string;
  reload: () => void;
}

const EMPTY: Omit<AccountTokenFeed, "reload"> = {
  tokens: [],
  state: "idle",
  error: "",
  readAt: "",
};

export function useAccountTokens(accountId: string): AccountTokenFeed {
  const connection = useOsConnection();
  const [read, setRead] = useState(EMPTY);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || accountId === "") return;
    const controller = new AbortController();
    const signal = controller.signal;

    void (async () => {
      setRead({ ...EMPTY, state: "loading" });
      try {
        const result = await query.accountTokensForAccount({ accountId }, { signal });
        if (signal.aborted) return;
        setRead({
          tokens: sortAccountTokens((result.rows() as Row[]).map(accountTokenFromRow)),
          state: "ready",
          error: "",
          readAt: new Date().toISOString(),
        });
      } catch (err: unknown) {
        if (signal.aborted) return;
        setRead({
          tokens: [],
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
  }, [connection, accountId, nonce]);

  return useMemo(() => ({ ...read, reload }), [read, reload]);
}
