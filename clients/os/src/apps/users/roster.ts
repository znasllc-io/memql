import {
  invitationHasExpired,
  personIsDim,
  personName,
  type InvitationRow,
  type PersonRow,
} from "./rows";

// The People list, as a function of the two feeds. PURE, and separate from the
// component for the reason apps/users/rows.ts is: a join asserted through
// render() is asserted through three layers that can each fail for unrelated
// reasons.
//
// ===========================================================================
// ONE LIST, TWO FEEDS, AND NO "INVITES" SECTION
// ===========================================================================
// An invitation is a person who has not arrived (design record, D1). The app
// used to answer "who is on this cluster" in two places -- a roster and an
// Invites section -- so somebody looking for a colleague had to know whether
// that colleague had accepted yet, which is the one thing they were trying to
// find out.
//
// The join is `useTwoFeedView`, whose header states why a `useLiveView`
// transform reading a second feed silently drops rows. What arrives here is
// both snapshots, and what leaves is one ordered list.

export type RosterRow =
  | { kind: "person"; id: string; person: PersonRow }
  | { kind: "invited"; id: string; invite: InvitationRow };

export type RosterState = "active" | "invited" | "deactivated";

export interface RosterFilter {
  /** Free text over the name and the address. */
  search: string;
  /** A role slug, or "" for every role. */
  role: string;
  /** A group id, or "" for every group. Answered by the members read. */
  group: string;
  /** One state, or "" for all of them. */
  state: RosterState | "";
  /** The setting: deactivated people are hidden unless this is on. */
  showDeactivated: boolean;
  /** Recent by row version, computed name, or last seen. */
  sort: "recent" | "name" | "lastSeen";
}

export const DEFAULT_ROSTER_FILTER: RosterFilter = {
  search: "",
  role: "",
  group: "",
  state: "",
  showDeactivated: false,
  sort: "recent",
};

/** Whether the filter is narrowing anything, for the Refine chips. */
export function filterIsNarrowing(filter: RosterFilter): boolean {
  return (
    filter.search.trim() !== "" || filter.role !== "" || filter.group !== "" || filter.state !== ""
  );
}

export function rosterStateOf(row: RosterRow): RosterState {
  if (row.kind === "invited") return "invited";
  return personIsDim(row.person) ? "deactivated" : "active";
}

/** The address a row is reached at, whichever kind it is. */
export function rosterEmail(row: RosterRow): string {
  return row.kind === "person" ? row.person.primaryEmail : row.invite.inviteeEmail;
}

/** What the row is called. An invited person is their address until they arrive. */
export function rosterName(row: RosterRow): string {
  if (row.kind === "person") return personName(row.person);
  const named = row.invite.inviteeName.trim();
  return named !== "" ? named : row.invite.inviteeEmail;
}

/** The role the row holds, or the one the invitation grants. */
export function rosterRole(row: RosterRow): string {
  return row.kind === "person" ? row.person.role : row.invite.inviteeRole;
}

/**
 * The arrival fingerprint.
 *
 * LAST SEEN IS NOT IN IT, and that is the rule this app states out loud: the
 * engine churns `lastSeenAt` for every person forever, so naming it would turn
 * the list into a strobe on a timer -- the standing badge the cue exists not to
 * be. What is left is what a person would call a change: a rename, a role
 * change, a sign-in policy flip, an account deactivated, an invitation
 * accepted or re-sent to different groups.
 */
export function rosterFingerprint(row: RosterRow): string {
  if (row.kind === "person") {
    const p = row.person;
    return `${p.displayName}|${p.primaryEmail}|${p.role}|${p.signInPolicy}|${p.active}|${p.suspendedAt}`;
  }
  const i = row.invite;
  return `${i.inviteeEmail}|${i.inviteeRole}|${i.status}|${i.deliveryState}|${i.groupIds.join(",")}`;
}

/**
 * The roster: both feeds, narrowed and ordered.
 *
 * `memberIds` is the answer to the GROUP facet and is passed in rather than
 * read here: memberships are read per opened group (`membersOfGroup`), never as
 * a cluster-wide feed, so the facet costs exactly one read of the ONE group
 * somebody picked. With no group picked it is undefined and no membership is
 * consulted at all.
 */
export function foldRoster(
  users: readonly PersonRow[],
  invites: readonly InvitationRow[],
  filter: RosterFilter,
  now: Date,
  memberIds?: ReadonlySet<string>,
): RosterRow[] {
  const rows: RosterRow[] = [
    ...users.filter((p) => p.id !== "").map((person) => ({ kind: "person" as const, id: person.id, person })),
    ...invites
      // AN EXPIRED INVITATION IS NOT A PERSON WHO IS ARRIVING. The row stays in
      // the cluster as history and the feed still carries it; it leaves this
      // list because "who can reach this cluster, and who is on their way" has
      // no room for a link that no longer works.
      .filter((invite) => invite.id !== "" && !invitationHasExpired(invite, now))
      .map((invite) => ({ kind: "invited" as const, id: invite.id, invite })),
  ];

  const needle = filter.search.trim().toLowerCase();
  const narrowed = rows.filter((row) => {
    const state = rosterStateOf(row);
    if (!filter.showDeactivated && state === "deactivated") return false;
    if (filter.state !== "" && state !== filter.state) return false;
    if (filter.role !== "" && rosterRole(row) !== filter.role) return false;
    if (filter.group !== "") {
      const inGroup =
        row.kind === "person"
          ? (memberIds?.has(row.person.id) ?? false)
          : row.invite.groupIds.includes(filter.group);
      if (!inGroup) return false;
    }
    if (needle !== "") {
      const hay = `${rosterName(row)} ${rosterEmail(row)}`.toLowerCase();
      if (!hay.includes(needle)) return false;
    }
    return true;
  });

  return narrowed.sort((a, b) => {
    if (filter.sort === "recent") {
      const at = a.kind === "person" ? a.person.createdAt : a.invite.createdAt;
      const bt = b.kind === "person" ? b.person.createdAt : b.invite.createdAt;
      return bt.localeCompare(at) || a.id.localeCompare(b.id);
    }
    if (filter.sort === "lastSeen") {
      // AN INVITED PERSON HAS NEVER BEEN SEEN, so they sort to the end rather
      // than to the top: "" parses as NaN and would otherwise land wherever
      // the comparator happened to put it.
      const at = a.kind === "person" ? Date.parse(a.person.lastSeenAt) : NaN;
      const bt = b.kind === "person" ? Date.parse(b.person.lastSeenAt) : NaN;
      if (Number.isFinite(at) && Number.isFinite(bt)) return bt - at;
      if (Number.isFinite(at)) return -1;
      if (Number.isFinite(bt)) return 1;
    }
    return rosterName(a).localeCompare(rosterName(b));
  });
}

/** Days left on an invitation, floored at zero. "" when it never expires. */
export function daysLeft(invite: InvitationRow, now: Date): string {
  if (invite.expiresAt === "") return "";
  const at = Date.parse(invite.expiresAt);
  if (!Number.isFinite(at)) return "";
  const days = Math.max(0, Math.ceil((at - now.getTime()) / 86_400_000));
  return days === 1 ? "1 day left" : `${days} days left`;
}
