import { getRowByConceptAndId, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, usePagedCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { groupFromRow, membershipFromRow } from "./rows";

// The groups feed, and the two membership reads.
//
// ===========================================================================
// THERE IS NO MEMBERSHIP FEED, AND THAT IS THE RULE (design record, D5)
// ===========================================================================
// `v1:identity:groupMembership` is every membership in the cluster: one row
// per person per group, forever, including the removed ones the concept keeps
// as history. A cluster-wide collection over it would seed all of them to
// render ONE page -- the Deployables timeline argument, and the reason the
// account rollups are on-demand reads rather than four more subscriptions.
//
// So the two reads below are SCOPED: `membersOfGroup` for an opened group and
// `groupsForUser` for an opened person, each seeded with its own argument.
// They are still LiveCollections, because both concepts broadcast created and
// updated to browser subscribers (component/node/routing.go) -- and a
// subscription is scoped by CONCEPT and cannot be scoped by argument, which is
// exactly what `inScope` is for: it says about an arriving event what the
// read's own filter said server-side. Without it, adding somebody to any group
// in the cluster would fold a row into the page you happen to have open.
//
// The reads go through the GENERATED builders (`make sdk-gen`), which is what
// keeps the argument names in step with the DSL: a query whose argument was
// renamed fails to compile here rather than answering nothing at runtime.

export const GROUP_CONCEPT = "v1:identity:group";
export const MEMBERSHIP_CONCEPT = "v1:identity:groupMembership";

/**
 * Every group in the cluster, live.
 *
 * `includeArchived: true` and narrowed on the read side, for the reason the
 * people feed passes no `active`: the show-archived setting is a view over
 * rows already here, and seeding filtered would make the toggle re-run the
 * read and re-baseline every arrival cue -- so flipping it would announce the
 * whole list as new.
 */
export function useGroups(enabled = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "users:groups" : null, (connection) => ({
    concept: GROUP_CONCEPT,
    actions: ["created", "updated"],
    seed: async (cursor, signal) => {
      const result = await connection.query.groupsAll({ includeArchived: true }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, GROUP_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));
}

export function useGroupDirectory(search: string, includeArchived: boolean) {
  const needle = search.trim();
  return usePagedCollection<Row>(`users:groups:directory:${JSON.stringify([needle, includeArchived])}`, connection => ({
    concept: GROUP_CONCEPT,
    actions: ["created", "updated"],
    seed: async (cursor, signal) => {
      const result = await connection.query.groupsBrowse({ search: needle || undefined, includeArchived }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (groupId, signal) => (await connection.query.groupById({ groupId }, { signal })).rows()[0] ?? null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(a["id"]).localeCompare(String(b["id"])),
    inScope: raw => {
      const group = groupFromRow(raw);
      return (includeArchived || group.status === "active") && (!needle || [group.name, group.description].some(text => text.toLowerCase().includes(needle.toLowerCase())));
    },
  }));
}

export function useGroupRecord(groupId: string) {
  return useLiveCollection<Row>(groupId ? `users:group:${groupId}` : null, connection => ({
    concept: GROUP_CONCEPT,
    eventInScope: id => id === groupId,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.groupById({ groupId }, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => (await connection.query.groupById({ groupId: id }, { signal })).rows()[0] ?? null,
    inScope: raw => groupFromRow(raw).id === groupId,
  }));
}

/**
 * The memberships of ONE group, live.
 *
 * Keyed on the group so opening a second one seeds its own rows rather than
 * appending to the first's, and `inScope` keeps every other group's events out.
 */
export function useMembersOfGroup(groupId: string): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(`users:members:${groupId}`, (connection) => ({
    concept: MEMBERSHIP_CONCEPT,
    actions: ["created", "updated"],
    seed: async (cursor, signal) => {
      if (groupId === "") return { rows: [], nextCursor: "" };
      const result = await connection.query.membersOfGroup(
        { groupId, includeRemoved: false },
        { signal, ...(cursor ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, MEMBERSHIP_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    // The read's own filter, said again about an arriving event: this group,
    // and still a member. A removal writes a new version with `removed`, which
    // this drops -- which is what takes the row off the page live.
    inScope: (row) => {
      const membership = membershipFromRow(row);
      return membership.groupId === groupId && membership.status === "active";
    },
  }));
}

/** The memberships of ONE person, live. The person page's Groups panel. */
export function useGroupsForUser(userId: string): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(`users:memberships:${userId}`, (connection) => ({
    concept: MEMBERSHIP_CONCEPT,
    actions: ["created", "updated"],
    seed: async (cursor, signal) => {
      if (userId === "") return { rows: [], nextCursor: "" };
      const result = await connection.query.groupsForUser(
        { userId, includeRemoved: false },
        { signal, ...(cursor ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, MEMBERSHIP_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    inScope: (row) => {
      const membership = membershipFromRow(row);
      return membership.userId === userId && membership.status === "active";
    },
  }));
}

/** Re-read ONE group through the authorized read path, for its page on open. */
export async function rereadGroup(query: QueryClient, groupId: string, signal?: AbortSignal): Promise<Row | null> {
  return getRowByConceptAndId(query, GROUP_CONCEPT, groupId, signal ? { signal } : {});
}
