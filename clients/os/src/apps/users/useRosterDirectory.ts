import { queryPage, buildSearchUsers, buildPendingUserInvitations, getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useMergedPagedCollections } from "../../live/useMergedPagedCollections";
import { usePagedCollection } from "../../live/useLiveCollection";
import { usePeople, USER_CONCEPT } from "./usePeople";
import { useInvites, INVITATION_CONCEPT } from "./useInvites";
import { foldRoster, type RosterFilter } from "./roster";
import { invitationFromRow, invitationIsPending, personFromRow } from "./rows";

// Recent is a bounded merge of two ordered streams: ten of each are enough
// to decide the first ten combined rows. Advancing both frontiers before
// exposing another ten prevents an unseen invitation outranking a shown user.
// Name/last-seen are explicit alternate directory views. They preserve their
// existing computed-name ordering and read the projected catalogs on demand.
export function useRosterDirectory(filter: RosterFilter, memberIds?: ReadonlySet<string>, enabled = true) {
  const recent = filter.sort === "recent";
  const members = memberIds ? [...memberIds].sort() : undefined;
  const ready = enabled && (filter.group === "" || memberIds !== undefined);
  const key = JSON.stringify([filter.search, filter.role, filter.group, filter.state, filter.showDeactivated, filter.sort, members]);
  const usersPage = usePagedCollection<Row>(ready && recent ? `users:roster:people:${key}` : null, connection => ({
    concept: USER_CONCEPT,
    actions: ["created"],
    seed: async (cursor, signal) => {
      const result = await queryPage(connection.query, "searchUsers", buildSearchUsers({ search: filter.search || undefined, role: filter.role || undefined, state: filter.state || undefined, showDeactivated: filter.showDeactivated, memberIds: members }), { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, USER_CONCEPT, id, { signal }) as Row | null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(a["id"]).localeCompare(String(b["id"])),
    inScope: raw => foldRoster([personFromRow(raw)], [], filter, new Date(), memberIds).length > 0,
  }));
  const invitesPage = usePagedCollection<Row>(ready && recent ? `users:roster:invites:${key}` : null, connection => ({
    concept: INVITATION_CONCEPT,
    actions: ["created", "updated"],
    seed: async (cursor, signal) => {
      const result = await queryPage(connection.query, "pendingUserInvitations", buildPendingUserInvitations({ unexpired: true, search: filter.search || undefined, role: filter.role || undefined, groupId: filter.group || undefined, state: filter.state || undefined }), { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, INVITATION_CONCEPT, id, { signal }) as Row | null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(a["id"]).localeCompare(String(b["id"])),
    inScope: raw => invitationIsPending(invitationFromRow(raw)) && foldRoster([], [invitationFromRow(raw)], filter, new Date(), memberIds).length > 0,
  }));
  const allUsers = usePeople(ready && !recent);
  const allInvites = useInvites(ready && !recent);
  const users = recent ? usersPage : allUsers;
  const invites = recent ? invitesPage : allInvites;
  const pagination = useMergedPagedCollections(`users:roster:${key}`, users, invites,
    (userRows, inviteRows) => foldRoster(userRows.map(personFromRow), inviteRows.map(invitationFromRow), filter, new Date(), memberIds),
    { manual: recent });
  return { users, invites, ...pagination, visible: pagination.limit };
}
