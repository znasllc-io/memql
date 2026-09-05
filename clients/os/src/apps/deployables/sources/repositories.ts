import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";

// The repositories a GitHub grant can see, as the picker reads them (epic
// memql#4915, design section A step 3).
//
// ===========================================================================
// A READING, NOT A FEED
// ===========================================================================
// `sourceRepositories` asks GitHub, through the cluster, what this person's
// installations hold right now. Nothing broadcasts a repository, and nothing
// could: the rows are not in this graph. So the picker prints WHEN it read
// and offers to look again, and this module has no subscription, no
// fingerprint and no arrival cue -- captioning liveness that is not there is
// the failure clients/os/README.md names for the on-demand surfaces.
//
// Everything here is pure -- a reply in, groups out -- so the picker's
// behaviour (what is grouped under whom, what a search matches, where a
// pending organization lands) is provable without rendering anything.

/** One repository the grant can reach. */
export interface RepositoryRow {
  /** `owner/name`, and the identity of the row: the picker keys on it. */
  fullName: string;
  owner: string;
  name: string;
  /** The clone URL the Source stop will carry. */
  url: string;
  private: boolean;
  /** GitHub's own word: `public`, `private`, `internal`. */
  visibility: string;
  defaultBranch: string;
  pushedAt: string;
  /** Which installation answered for it, so a later call can name one. */
  installationId: string;
}

/** One installation the grant reaches: an account or an organization. */
export interface InstallationRow {
  id: string;
  login: string;
  /** `User` or `Organization`, as GitHub spells them. */
  accountType: string;
  /** `all` or `selected`. */
  repositorySelection: string;
  suspended: boolean;
}

/** An organization whose installation an owner has not approved yet. */
export interface PendingInstallation {
  login: string;
}

/** One page of the answer. */
export interface RepositoryPage {
  repositories: RepositoryRow[];
  installations: InstallationRow[];
  pending: PendingInstallation[];
  /** The next page to ask for, or 0 when this was the last. */
  nextPage: number;
  /** `ok`, or the refusal code the read stopped on. */
  reason: string;
}

export const EMPTY_PAGE: RepositoryPage = {
  repositories: [],
  installations: [],
  pending: [],
  nextPage: 0,
  reason: "",
};

// ---------------------------------------------------------------------------
// Reading the reply
// ---------------------------------------------------------------------------

/**
 * A member list, whether it arrives as JSON text or as a decoded array.
 *
 * `packages/rows.ts` has the same two-shape reader for the analysis report,
 * and this is deliberately not that one promoted to the kit: `listOf` CASTS
 * its members, while every reader below PROJECTS -- each field named, each
 * default stated -- which is what keeps an added engine field from arriving
 * in this app unconsidered. Two different jobs that happen to start the same
 * way.
 */
function membersOf(row: Record<string, unknown>, key: string): unknown[] {
  const raw = row[key];
  if (Array.isArray(raw)) return raw;
  if (typeof raw === "string" && raw !== "") {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (Array.isArray(parsed)) return parsed;
    } catch {
      // An unreadable list renders as no list. Never a thrown parse error:
      // one malformed member must not blank the whole picker, and an empty
      // picker already says "this connection reaches no repositories yet".
    }
  }
  return [];
}

function text(member: unknown, key: string): string {
  if (member === null || typeof member !== "object") return "";
  const v = (member as Record<string, unknown>)[key];
  return typeof v === "string" ? v : "";
}

/**
 * A boolean that may have crossed the wire as a word.
 *
 * The engine renders a scalar boolean as the STRING "true" on a builtin's
 * reply row (`packages/calls.ts`), and a member nested in a list arrives
 * decoded. Both spellings mean the same thing and anything else means false,
 * which is the reading that asserts least: `private` decides whether a chip
 * is drawn, and drawing none for a value this build cannot read is quieter
 * than claiming a repository is public.
 */
function flag(member: unknown, key: string): boolean {
  if (member === null || typeof member !== "object") return false;
  const v = (member as Record<string, unknown>)[key];
  return v === true || v === "true";
}

function count(row: Record<string, unknown>, key: string): number {
  const v = row[key];
  if (typeof v === "number" && Number.isFinite(v)) return Math.trunc(v);
  if (typeof v === "string" && v.trim() !== "") {
    const parsed = Number(v);
    if (Number.isFinite(parsed)) return Math.trunc(parsed);
  }
  return 0;
}

/**
 * Owner and name, from whichever of the three fields the reply carries.
 *
 * `fullName` is the identity, and the two halves are derived from it when
 * they are absent rather than left blank -- a group header reading nothing
 * and a row reading `acme/widget` would be one repository shown as two
 * mistakes. When the halves are present they WIN, because an owner that
 * contains a slash is a thing GitHub does not have but a parser should not
 * assume.
 */
function namesOf(member: unknown): { fullName: string; owner: string; name: string } {
  const fullName = text(member, "fullName").trim();
  let owner = text(member, "owner").trim();
  let name = text(member, "name").trim();
  if (owner === "" || name === "") {
    const cut = fullName.indexOf("/");
    if (cut > 0) {
      if (owner === "") owner = fullName.slice(0, cut);
      if (name === "") name = fullName.slice(cut + 1);
    }
  }
  return { fullName: fullName || (owner !== "" && name !== "" ? `${owner}/${name}` : ""), owner, name };
}

/**
 * The picker's reading of one `sourceRepositories` reply row.
 *
 * A member with no name at all is DROPPED: the picker keys on `fullName` and
 * a row with none could not be chosen, so rendering it would be a line that
 * does nothing when clicked.
 */
export function repositoryPageFrom(raw: Row | undefined | null): RepositoryPage {
  if (!raw) return { ...EMPTY_PAGE };
  const row = flatten(raw) as Record<string, unknown>;
  const repositories: RepositoryRow[] = [];
  for (const member of membersOf(row, "repositories")) {
    const { fullName, owner, name } = namesOf(member);
    if (fullName === "") continue;
    repositories.push({
      fullName,
      owner,
      name,
      url: text(member, "url"),
      private: flag(member, "private"),
      visibility: text(member, "visibility"),
      defaultBranch: text(member, "defaultBranch"),
      pushedAt: text(member, "pushedAt"),
      installationId: text(member, "installationId"),
    });
  }
  const installations: InstallationRow[] = [];
  for (const member of membersOf(row, "installations")) {
    const id = text(member, "id");
    if (id === "") continue;
    installations.push({
      id,
      login: text(member, "login"),
      accountType: text(member, "accountType"),
      repositorySelection: text(member, "repositorySelection"),
      suspended: flag(member, "suspended"),
    });
  }
  const pending: PendingInstallation[] = [];
  for (const member of membersOf(row, "pending")) {
    const login = text(member, "login").trim();
    // A pending entry with no login says only "something is waiting", which
    // names nobody to ask -- the entire content of this state.
    if (login === "") continue;
    pending.push({ login });
  }
  return {
    repositories,
    installations,
    pending,
    nextPage: count(row, "nextPage"),
    reason: text(row, "reason"),
  };
}

// ---------------------------------------------------------------------------
// Grouping, ordering and search
// ---------------------------------------------------------------------------

/** One owner's block in the picker: their repositories, or the sentence. */
export interface RepositoryGroup {
  owner: string;
  repositories: RepositoryRow[];
  /** Waiting for an owner of this organization to approve the app. */
  pending: boolean;
}

/** Whether a repository answers what somebody typed. Case-insensitive over
 *  the FULL name, so "acme/wid" and "WIDGET" both find `acme/widget`. */
export function matchesSearch(repo: Pick<RepositoryRow, "fullName">, search: string): boolean {
  const needle = search.trim().toLowerCase();
  if (needle === "") return true;
  return repo.fullName.toLowerCase().includes(needle);
}

function byName(a: string, b: string): number {
  return a.toLowerCase().localeCompare(b.toLowerCase());
}

/**
 * The picker's list: filtered, grouped by owner, ordered.
 *
 * PENDING GROUPS COME LAST, and that is the one ordering decision here. A
 * group with nothing in it is not a choice -- it is a note about a choice
 * that is not available yet -- and interleaving it alphabetically would put
 * a line nobody can act on in the middle of the ones they can. When the
 * approval lands the group moves up into the pickable half, which is exactly
 * the news somebody pressed Look again for.
 *
 * A pending organization is matched against the SEARCH by its login, so
 * typing the name of the organization you are waiting on finds the sentence
 * explaining why it has no repositories rather than hiding it.
 */
export function groupRepositories(
  page: Pick<RepositoryPage, "repositories" | "pending">,
  search: string,
): RepositoryGroup[] {
  const byOwner = new Map<string, RepositoryRow[]>();
  for (const repo of page.repositories) {
    if (!matchesSearch(repo, search)) continue;
    const held = byOwner.get(repo.owner);
    if (held) held.push(repo);
    else byOwner.set(repo.owner, [repo]);
  }

  const groups: RepositoryGroup[] = [...byOwner.entries()]
    .map(([owner, repositories]) => ({
      owner,
      repositories: repositories.slice().sort((a, b) => byName(a.name || a.fullName, b.name || b.fullName)),
      pending: false,
    }))
    .sort((a, b) => byName(a.owner, b.owner));

  const needle = search.trim().toLowerCase();
  const pending = page.pending
    .filter((p) => needle === "" || p.login.toLowerCase().includes(needle))
    // An organization that already answered with repositories is not
    // pending: the same login must never render twice, once as rows and
    // once as a sentence saying it has none.
    .filter((p) => !byOwner.has(p.login))
    .map((p) => ({ owner: p.login, repositories: [], pending: true }))
    .sort((a, b) => byName(a.owner, b.owner));

  return [...groups, ...pending];
}

/** How many repositories a set of groups holds, for the footer's count. */
export function repositoryCount(groups: readonly RepositoryGroup[]): number {
  return groups.reduce((n, g) => n + g.repositories.length, 0);
}
