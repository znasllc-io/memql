import { SELF_ACCOUNT_ID, accountIsArchived, accountNameFrom, type AccountRow } from "../accounts/rows";

// WHOSE EMAIL IT IS (memql#6033).
//
// An organization is the unit email belongs to: its senders, its sending
// domain, its testing audience, its package. Email names organizations in a
// handful of places, and each used to answer the same three questions for
// itself -- which ones can I choose between, what is this one called, is this
// the cluster's own -- and the last of them as a bare `"self"` in six files.
// They are answered here once.

/**
 * The organizations a person can choose between: the active ones the list they
 * were handed holds. The list is what `useAccountOptions()` reads, which is
 * already bounded by what this person may read, so this adds only "not filed
 * away" and "has an id" (a row mid-write has none).
 */
export function selectableOrganizations(accounts: readonly AccountRow[]): AccountRow[] {
  return accounts.filter((account) => account.id !== "" && !accountIsArchived(account));
}

/**
 * An organization's name, from a list the page already holds. An id the list
 * does not hold keeps its id rather than rendering blank: the tie is still
 * true, and an archived or unreadable organization is still the one it is.
 */
export function organizationName(accounts: readonly AccountRow[], accountId: string): string {
  return accountNameFrom(accounts, accountId);
}

/**
 * The cluster's own organization, which may send from the cluster's sender.
 * The one place Email spells it (audit F3): every other organization has to
 * name a sender of its own before it can send, because the engine refuses a
 * campaign that names none ("choose a sending identity belonging to this
 * organization"). `SELF_ACCOUNT_ID` is the BARE id the wire carries, not the
 * canonical one a server-side file spells.
 */
export function isOperatorOrganization(accountId: string): boolean {
  return accountId === SELF_ACCOUNT_ID;
}
