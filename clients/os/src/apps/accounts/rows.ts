import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten, stringsOf } from "../../kit/rows";

// The wire rows the Accounts app renders, projected into the shapes its
// surfaces read.
//
// PURE, and separate from every component, for the reason apps/users/rows.ts
// is: a projection asserted through render() is asserted through three layers
// that can each fail for unrelated reasons. Everything here is a function of a
// row and is unit-testable with no browser, no cluster and no React.

/** The literal id of the owner's own company (design D3). */
export const SELF_ACCOUNT_ID = "v1:accounts:account:self";

export const ACCOUNT_CONCEPT = "v1:accounts:account";

export interface AccountRow {
  id: string;
  name: string;
  domain: string;
  primaryContactName: string;
  primaryContactEmail: string;
  notes: string;
  /** `active` | `archived`. "" only on a row written before status existed. */
  status: string;
  /**
   * When a human last stated these facts. EMPTY IS A QUESTION, not a flag:
   * on the seeded `self` row it means nobody has answered the first-run card
   * yet, which is the one thing that card is gated on (D7).
   */
  configuredAt: string;
  ownerUserId: string;
  createdAt: string;
}

export function accountFromRow(row: Row): AccountRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    name: rowString(flat, "name"),
    domain: rowString(flat, "domain"),
    primaryContactName: rowString(flat, "primaryContactName"),
    primaryContactEmail: rowString(flat, "primaryContactEmail"),
    notes: rowString(flat, "notes"),
    // NOT defaulted to "active". A row whose status the fold has not seen is
    // a row we do not know the status of, and rendering that as active would
    // put an archived client back in the default list on the strength of a
    // guess. `accountIsArchived` reads the absence as not-archived, which is
    // the same answer the ENGINE's `isNotArchived` trait gives -- one place,
    // stated once.
    status: rowString(flat, "status"),
    configuredAt: rowString(flat, "configuredAt"),
    ownerUserId: rowString(flat, "ownerUserId"),
    createdAt: rowString(flat, "createdAt"),
  };
}

export function accountIsArchived(account: AccountRow): boolean {
  return account.status === "archived";
}

export function accountIsSelf(account: AccountRow): boolean {
  return account.id === SELF_ACCOUNT_ID;
}

/**
 * Whether the first-run setup card should stand in for the app (D7).
 *
 * TWO CONDITIONS, AND BOTH MATTER. The row must be the `self` singleton --
 * no other account can ever raise this card, because no other account was
 * filled in by a boot -- and its `configuredAt` must be absent, which is what
 * "nobody has answered yet" looks like. `updateClientAccount` stamps the
 * field, so saving the card is what retires it; nothing clears a flag and
 * nothing is remembered in this browser.
 *
 * The self row being MISSING is deliberately not the card's condition. A
 * cluster whose seed has not run yet has nothing to prepopulate and nothing
 * to save into; the app says the registry is still coming up instead of
 * offering a form that would write a second self row.
 */
export function needsFirstRun(account: AccountRow | null): boolean {
  if (account === null) return false;
  return accountIsSelf(account) && account.configuredAt.trim() === "";
}

/**
 * What a PERSON would call a change to an account, for the arrival cue.
 *
 * A HEARTBEAT IS NOT NEWS (clients/os/README.md), and this row is the easy
 * case for once: `v1:accounts:account` carries no field that moves on a timer
 * -- no lastSeenAt, no freshness, nothing the engine churns. So every field a
 * surface renders can be named here honestly, and the cue fires on exactly
 * what it looks like it fires on: a rename, a domain correction, a new
 * contact, an archive.
 *
 * `configuredAt` is deliberately ABSENT. It moves on every save, so naming it
 * would make the cue fire twice for one edit -- once for the field somebody
 * changed and once for the timestamp that changing it stamped.
 */
export function accountFingerprint(account: AccountRow): string {
  return [
    account.name,
    account.domain,
    account.primaryContactName,
    account.primaryContactEmail,
    account.notes,
    account.status,
  ].join("|");
}

/**
 * The name to show, never blank.
 *
 * A blank cell is indistinguishable from a cell that failed to render, and an
 * account with no name is a real state -- the seed writes a starter, and a
 * row mid-edit can arrive with anything. The id's tail is the fallback because
 * it is the one thing the row always has and the one thing that distinguishes
 * two otherwise-empty rows from each other.
 */
export function accountName(account: AccountRow): string {
  const name = account.name.trim();
  if (name !== "") return name;
  const tail = account.id.split(":").pop() ?? "";
  return tail === "" ? "Untitled account" : `Untitled account (${tail})`;
}

// ---------------------------------------------------------------------------
// The tie readers
// ---------------------------------------------------------------------------

/**
 * The accounts a Library row is labelled with (`accountIds`, a list).
 *
 * ABSENCE AND THE EMPTY LIST ARE ONE ANSWER, and they have to be: every
 * artifact promoted before this field existed carries no key at all, so a
 * filter that distinguished them would silently hide the entire pre-existing
 * Library from the untied view. This is the `folderId` lesson applied to a
 * list -- the honest reader is the fold, not the filter.
 */
export function accountIdsOf(row: Row): string[] {
  return stringsOf(flatten(row), "accountIds");
}

/** The single account a site, invitation or knowledge domain is tied to. */
export function accountIdOf(row: Row): string {
  return rowString(flatten(row), "accountId");
}

/**
 * Resolve an account id to a name from a snapshot the caller already holds.
 *
 * PRESENTATION ONLY, and it never fetches. Every tie surface in the OS holds
 * the `clientAccountsAll` snapshot for its picker anyway, so resolving from
 * that list costs nothing; a per-row read would put one request per rendered
 * row on a list somebody is scrolling.
 *
 * An id that resolves to NOTHING keeps its id rather than rendering blank or
 * dropping the tie. An account can be archived, or created by somebody whose
 * rows this caller cannot read -- the tie is still true, and "tied to
 * something you cannot see" is a more useful thing to show than nothing.
 */
export function accountNameFrom(accounts: AccountRow[], accountId: string): string {
  const id = accountId.trim();
  if (id === "") return "";
  const found = accounts.find((a) => a.id === id);
  return found ? accountName(found) : id;
}
