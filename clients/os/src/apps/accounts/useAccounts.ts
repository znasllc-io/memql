import { useCallback, useEffect, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { ACCOUNT_CONCEPT, accountFromRow, type AccountRow } from "./rows";

// The client registry's one feed, and the four rollups a detail view reads.
//
// ===========================================================================
// BOTH VERBS ARE LIVE, AND THE SEED IS WHY THAT MATTERS MORE THAN USUAL
// ===========================================================================
// component/node/routing.go carries `graph.node.created.v1:accounts:account`
// AND `graph.node.updated.v1:accounts:account`. Unlike `v1:identity:user` --
// which broadcasts creates and deliberately not updates, because the row
// churns on lastSeenAt -- an account carries no field that moves on a timer,
// so there is nothing to strobe and both verbs are affordable.
//
// The rule the README states is to read the routing table before deciding
// what a feed does, and the case that makes it load-bearing here is the boot
// seed: `v1:accounts:account:self` is materialized by whichever bff replica
// wins the startup race, which is not the replica any browser is attached to.
// Without the broadcast, the first-run card would keep asking for a company
// name on a cluster that already had a row waiting.

/**
 * Every account this caller may see, live.
 *
 * NO ARGUMENTS, and `clientAccountsAll`'s `includeArchived` is deliberately
 * not passed: the show-archived toggle is a view over rows already here (see
 * settings.ts). Seeding filtered would make the toggle re-run the read and
 * re-baseline every arrival cue, so flipping it would announce the whole list
 * as new.
 *
 * The gate is the query's own -- the concept's composite tier is ANDed into
 * the read whatever this code asks for -- so a caller sees exactly their own
 * accounts, or every one if they are a cluster owner. Nothing here decides
 * that.
 */
export function useAccounts(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("accounts:registry", (connection) => ({
    concept: ACCOUNT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.clientAccountsAll({ includeArchived: true }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, ACCOUNT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

// ---------------------------------------------------------------------------
// The four rollups
// ---------------------------------------------------------------------------

/**
 * One rollup band's state.
 *
 * `error` is the SERVER's sentence, kept verbatim and rendered in surface.
 * It is a first-class state rather than an empty list, because the two are
 * different answers and the difference is the interesting one: a caller below
 * the invitation gate gets a refusal, and rendering that as "0 guest invites"
 * would be this window inventing a fact about a client.
 */
export interface Rollup {
  rows: Row[];
  state: "idle" | "loading" | "ready" | "error";
  error: string;
  readAt: string;
}

const EMPTY_ROLLUP: Rollup = { rows: [], state: "idle", error: "", readAt: "" };

export interface AccountRollups {
  sites: Rollup;
  files: Rollup;
  domains: Rollup;
  invites: Rollup;
  reload: () => void;
}

/**
 * The four populations tied to one account, read on open.
 *
 * ON-DEMAND READS, NOT LIVE FEEDS, and the honest move is to say which. Three
 * of the four concepts DO broadcast -- site, artifact and invitation all carry
 * rules -- so a live rollup is buildable. It is deliberately not built:
 *
 *   - It would be four more subscriptions per OPEN ACCOUNT, over concepts the
 *     Files, Deployables and Users apps already hold feeds for. The cost lands
 *     on the mesh, and the benefit is a count changing under somebody reading
 *     a client's profile.
 *   - `v1:knowledge:knowledgeDomain` carries NO rule (#4809 is the tier
 *     question; nothing broadcasts it either), so one of the four could never
 *     be live. A ledger where three bands move and the fourth silently does
 *     not is worse than one where none do -- the reader has no way to tell
 *     which kind of band they are looking at.
 *
 * So all four are read together, they print WHEN they were read, and they
 * re-read on demand. That is the same call the Training app made for the
 * knowledge side, for the same reason.
 */
export function useAccountRollups(accountId: string): AccountRollups {
  const connection = useOsConnection();
  const [sites, setSites] = useState<Rollup>(EMPTY_ROLLUP);
  const [files, setFiles] = useState<Rollup>(EMPTY_ROLLUP);
  const [domains, setDomains] = useState<Rollup>(EMPTY_ROLLUP);
  const [invites, setInvites] = useState<Rollup>(EMPTY_ROLLUP);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || accountId === "") return;
    const controller = new AbortController();
    const signal = controller.signal;

    // EACH BAND SETTLES ON ITS OWN. A single Promise.all would let one
    // refusal -- the invitation gate is the one that will actually happen --
    // decide the state of three reads that succeeded, and a client's
    // deployables would disappear because somebody is not an admin.
    async function run(
      read: () => Promise<{ rows: () => Row[] }>,
      set: (r: Rollup) => void,
    ): Promise<void> {
      set({ ...EMPTY_ROLLUP, state: "loading" });
      try {
        const result = await read();
        if (signal.aborted) return;
        set({ rows: result.rows(), state: "ready", error: "", readAt: new Date().toISOString() });
      } catch (err: unknown) {
        if (signal.aborted) return;
        set({
          rows: [],
          state: "error",
          // VERBATIM. A refusal on the invitation rollup is the server saying
          // reading invitations is owner and admin only, which is the most
          // useful sentence this surface can carry.
          error: err instanceof Error ? err.message : String(err),
          readAt: new Date().toISOString(),
        });
      }
    }

    void run(() => query.sitesForAccount({ accountId }, { signal }), setSites);
    void run(() => query.libraryItemsForAccount({ accountId }, { signal }), setFiles);
    void run(() => query.domainsForAccount({ accountId }, { signal }), setDomains);
    void run(() => query.invitationsForAccount({ accountId }, { signal }), setInvites);

    return () => controller.abort();
  }, [connection, accountId, nonce]);

  return { sites, files, domains, invites, reload };
}

/**
 * Re-read ONE account through the authorized read path.
 *
 * Best-effort by contract: the caller renders what it has when this returns
 * null rather than failing the surface. Used after a write so a save lands on
 * screen even if the broadcast is slow -- not INSTEAD of the broadcast, which
 * is what carries the change to every other browser.
 */
export async function rereadAccount(
  connection: ReturnType<typeof useOsConnection>,
  accountId: string,
  signal?: AbortSignal,
): Promise<AccountRow | null> {
  const query = connection?.query ?? null;
  if (query === null) return null;
  const row = await getRowByConceptAndId(query, ACCOUNT_CONCEPT, accountId, signal ? { signal } : {});
  return row ? accountFromRow(row as Row) : null;
}
