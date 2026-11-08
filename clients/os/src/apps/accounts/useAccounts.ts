import { useCallback, useEffect, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection, usePagedCollection } from "../../live/useLiveCollection";
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

/** Recent/search pages are retained for the authenticated browser session. */
export function useAccounts(search = "", includeArchived = false) {
  const needle = search.trim();
  return usePagedCollection<Row>(`accounts:registry:${JSON.stringify([needle, includeArchived])}`, connection => ({
    concept: ACCOUNT_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.clientAccountsBrowse({ search: needle || undefined, includeArchived }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (accountId, signal) => (await connection.query.clientAccountById({ accountId }, { signal })).rows()[0] ?? null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(a["id"]).localeCompare(String(b["id"])),
    inScope: raw => {
      const account = accountFromRow(raw);
      return (includeArchived || account.status !== "archived") && (!needle || [account.name, account.domain, account.primaryContactName, account.primaryContactEmail, account.notes].some(value => value.toLowerCase().includes(needle.toLowerCase())));
    },
  }));
}

/** The first-run gate and deep links must not depend on the loaded list page. */
export function useAccountRecord(accountId: string) {
  return useLiveCollection<Row>(accountId ? `accounts:record:${accountId}` : null, connection => ({
    concept: ACCOUNT_CONCEPT,
    eventInScope: id => id === accountId,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.clientAccountById({ accountId }, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => (await connection.query.clientAccountById({ accountId: id }, { signal })).rows()[0] ?? null,
    inScope: row => accountFromRow(row).id === accountId,
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

// ---------------------------------------------------------------------------
// The People band (epic memql#5167, section C)
// ---------------------------------------------------------------------------

/** What the People band draws: how many people, in how many groups. */
export interface AccountPeople {
  /** Distinct people across this client's groups. */
  people: number;
  /** The groups, named, in the order the read returned them. */
  groups: { id: string; name: string }[];
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence, verbatim. */
  error: string;
  readAt: string;
}

const EMPTY_PEOPLE: AccountPeople = {
  people: 0,
  groups: [],
  state: "idle",
  error: "",
  readAt: "",
};

/**
 * How many people reach one client's work, read on demand with the other bands.
 *
 * TWO READS, AND THE SECOND ONE FANS OUT: `groupsForAccount` answers which
 * groups grant this client, and `membersOfGroup` answers each group's members.
 * There is no cluster-wide membership feed to sum instead -- one row per person
 * per group in the cluster, forever -- and the fan-out is bounded by the number
 * of groups ONE client has, which is one plus whatever an operator made.
 *
 * DISTINCT PEOPLE, not summed memberships: somebody in two of a client's
 * groups is one person, and "5 in 2 groups" for three people would be a number
 * this window invented.
 *
 * IT DOES NOT COUNT THE STANDING STAFF, and that is the honest answer rather
 * than a gap. Developer rank and above are members of every account's group by
 * RULE, with no rows anywhere -- so adding them would mean this band deciding
 * who the cluster's staff are, on a screen about a client. The Users app says
 * the rule where it belongs, on the group's own page.
 *
 * A REFUSAL IS NOT A ZERO, the ledger's standing rule: the reads carry
 * `@requiresRank("admin")`, so below that the band prints the server's sentence
 * rather than claiming the client has nobody.
 */
export function useAccountPeople(accountId: string): AccountPeople & { reload: () => void } {
  const connection = useOsConnection();
  const [state, setState] = useState<AccountPeople>(EMPTY_PEOPLE);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || accountId === "") return;
    const controller = new AbortController();
    const signal = controller.signal;
    let alive = true;
    setState({ ...EMPTY_PEOPLE, state: "loading" });

    void (async () => {
      try {
        const groupsResult = await query.groupsForAccount({ accountId }, { signal });
        const groups = (groupsResult.rows() as Record<string, unknown>[])
          .map((row) => ({
            id: typeof row["id"] === "string" ? row["id"] : "",
            name: typeof row["name"] === "string" ? row["name"] : "",
          }))
          .filter((g) => g.id !== "");

        const members = await Promise.all(
          groups.map((group) =>
            query.membersOfGroup({ groupId: group.id, includeRemoved: false }, { signal }),
          ),
        );
        if (!alive) return;

        const people = new Set<string>();
        for (const result of members) {
          for (const row of result.rows() as Record<string, unknown>[]) {
            const userId = typeof row["userId"] === "string" ? row["userId"] : "";
            const status = typeof row["status"] === "string" ? row["status"] : "active";
            if (userId !== "" && status !== "removed") people.add(userId);
          }
        }
        setState({
          people: people.size,
          groups,
          state: "ready",
          error: "",
          readAt: new Date().toISOString(),
        });
      } catch (err: unknown) {
        if (!alive || signal.aborted) return;
        setState({
          ...EMPTY_PEOPLE,
          state: "error",
          error: err instanceof Error ? err.message : String(err),
          readAt: new Date().toISOString(),
        });
      }
    })();

    return () => {
      alive = false;
      controller.abort();
    };
  }, [connection, accountId, nonce]);

  return { ...state, reload };
}
