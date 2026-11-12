import { useCallback, useEffect, useMemo, useState } from "react";
import { getRowByConceptAndId, renderMemQLValue, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useReading, type Reading } from "../../cluster/reading";
import { useOsConnection } from "../../live/connection";
import { assignmentStatusFromRows, inventoryFromRows, type AssignmentStatus, type Inventory } from "./inventory";
import { useLiveCollection, usePagedCollection } from "../../live/useLiveCollection";
import { ACCOUNT_CONCEPT, accountFromRow, type AccountRow } from "./rows";

// The client registry's one feed, and the reads an account's page makes.
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
// The guests band
// ---------------------------------------------------------------------------

/**
 * One band's state.
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

/**
 * The guest invitations filed under one account, read on open.
 *
 * THE ONE ROW-NAMING BAND THAT SURVIVED THE INVENTORY (account row ownership,
 * section H). The inventory counts invitations under People, but it cannot say
 * WHO was invited, and an invitation is a person who is not yet in any of the
 * client's groups -- so the People band, which counts members, never shows
 * them. The other four bands (deployables, files, knowledge, campaigns) named
 * rows the inventory now counts, and each of those is listed in its own app.
 *
 * ON-DEMAND, like the inventory beside it: a ledger where one band moves and
 * the rest do not is worse than one where none do.
 */
export function useAccountGuests(accountId: string): Rollup & { reload: () => void } {
  const connection = useOsConnection();
  const [state, setState] = useState<Rollup>(EMPTY_ROLLUP);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || accountId === "") return;
    const controller = new AbortController();
    const signal = controller.signal;
    setState({ ...EMPTY_ROLLUP, state: "loading" });
    query
      .invitationsForAccount({ accountId }, { signal })
      .then((result) => {
        if (signal.aborted) return;
        setState({ rows: result.rows(), state: "ready", error: "", readAt: new Date().toISOString() });
      })
      .catch((err: unknown) => {
        if (signal.aborted) return;
        // VERBATIM. A refusal here is the server saying reading invitations
        // needs a higher rank, which is the most useful sentence this band can
        // carry.
        setState({
          rows: [],
          state: "error",
          error: err instanceof Error ? err.message : String(err),
          readAt: new Date().toISOString(),
        });
      });
    return () => controller.abort();
  }, [connection, accountId, nonce]);

  return { ...state, reload };
}

// ---------------------------------------------------------------------------
// The inventory and the assignment status (account row ownership, section H)
// ---------------------------------------------------------------------------

// BY NAME, NOT BY GENERATED METHOD, until the SDK carries them. Both are `@sdk`
// builtins; `executeNamed` with the builtin call is what a generated method
// does underneath (cluster/updates/InstallationUpdates.tsx calls its builtins
// the same way).

/**
 * What one account holds, read on demand.
 *
 * NEVER LIVE, AND IT SAYS SO. The answer is one grouped count over every
 * concept, not a feed of any one, so no event keeps it current; the page
 * prints when it was read and offers a re-read.
 *
 * `enabled` is the caller holding `execute app:accounts/inventory`. A
 * non-holder asks nothing: the builtin would refuse them, and the page draws
 * no inventory for them at all.
 */
export function useAccountInventory(accountId: string, enabled: boolean): Reading<Inventory> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (!enabled || query === null || accountId === "") return null;
    return async (signal: AbortSignal) => {
      const result = await query.executeNamed(
        "accountInventory",
        `builtin accountInventory(accountId: ${renderMemQLValue(accountId)})`,
        { signal },
      );
      return inventoryFromRows(result.rows());
    };
  }, [enabled, query, accountId]);
  return useReading<Inventory>(`accounts:inventory:${accountId}:${enabled}`, read);
}

/**
 * How far the assignment of existing rows has got, cluster-wide, read on
 * demand. Drawn on the primary account's page only.
 */
export function useAccountAssignmentStatus(enabled: boolean): Reading<AssignmentStatus> {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const read = useMemo(() => {
    if (!enabled || query === null) return null;
    return async (signal: AbortSignal) => {
      const result = await query.executeNamed(
        "accountAssignmentStatus",
        "builtin accountAssignmentStatus()",
        { signal },
      );
      return assignmentStatusFromRows(result.rows());
    };
  }, [enabled, query]);
  return useReading<AssignmentStatus>(`accounts:assignmentStatus:${enabled}`, read);
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
