import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";

// `v1:identity:account` -- the BILLING accounts a credential is issued
// against, projected out of the rows `accounts` returns.
//
// ===========================================================================
// TWO CONCEPTS SHARE THE WORD "ACCOUNT", AND THIS FILE IS ABOUT THE OTHER ONE
// ===========================================================================
// The rest of this app is `v1:accounts:account` -- the CLIENT REGISTRY (epic
// memql#4800): a company this instance's owner does work for. Everything in
// this file is `v1:identity:account` -- the PAYING account of the
// account-isolation model (docs/internal/design/account-isolation-model.md), a
// billing and attribution subject that nothing authenticates as.
//
// `dsl/accounts/concepts.memql` states the relationship between them in one
// line: "They share a word and nothing else -- no field, no reference, no
// lifecycle." There is no link between the two concepts in either direction.
//
// THAT IS NOT A PEDANTIC DISTINCTION HERE, IT IS THE WHOLE REASON THIS FILE
// EXISTS. `mintAccountToken` gates on `query accountById`, which binds
// `v1:identity:account` and filters `ownerUserId==actor.userId`
// (component/grpc/account_token_handlers.go). Handing it a client-registry id
// resolves ZERO ROWS, and zero rows IS the refusal -- so a credentials surface
// built over the client list would be refused on every single mint, with a
// permission error as the only clue. The projection therefore has its own
// type, its own name and its own file, because the one thing that must never
// happen is an `AccountRow` and a `BillingAccountRow` being interchangeable.
//
// PURE, and separate from the section, for the reason rows.ts is: a projection
// asserted through render() is asserted through three layers that can each
// fail for unrelated reasons.

export const BILLING_ACCOUNT_CONCEPT = "v1:identity:account";

/**
 * The billing account, in this app's words.
 *
 * THE TWO `@pii` FIELDS ARE DELIBERATELY ABSENT. `accountFull` projects
 * `primaryContactName` and `primaryContactEmail`, both `@pii` on the concept
 * -- personal data about a third party, not about a MemQL principal. This
 * surface's job is credentials, and a contact's name and address have no part
 * in issuing or revoking one. Leaving them out of the PROJECTION rather than
 * out of the markup is the point: a field that is never read cannot be
 * rendered by a later edit that was only trying to add a line.
 */
export interface BillingAccountRow {
  id: string;
  name: string;
  /** `active` | `suspended` | `archived`. "" when the row did not project it. */
  status: string;
  description: string;
  /** The operator's own id for this customer upstream of MemQL. Opaque. */
  externalRef: string;
  /** Stamped by `archiveAccount`; "" while active or suspended. */
  archivedAt: string;
  updatedAt: string;
  createdAt: string;
}

export function billingAccountFromRow(row: Row): BillingAccountRow {
  const flat = flatten(row);
  return {
    id: rowString(flat, "id"),
    name: rowString(flat, "name"),
    // NOT defaulted to "active", for the reason rows.ts states about the
    // client registry: a status the read did not project is a status we do not
    // know, and rendering that as active would claim a fact off a guess.
    status: rowString(flat, "status"),
    description: rowString(flat, "description"),
    externalRef: rowString(flat, "externalRef"),
    archivedAt: rowString(flat, "archivedAt"),
    updatedAt: rowString(flat, "updatedAt"),
    createdAt: rowString(flat, "createdAt"),
  };
}

/**
 * Whether this billing account is closed.
 *
 * IT IS NOT A REASON TO HIDE THE ROW. A closed account's credentials still
 * exist and still work until they are revoked, so the one place they can be
 * revoked must not disappear the moment somebody tidies the list. The read
 * asks for every status for exactly that reason.
 */
export function billingAccountIsArchived(account: BillingAccountRow): boolean {
  return account.status === "archived";
}

export function billingAccountIsSuspended(account: BillingAccountRow): boolean {
  return account.status === "suspended";
}

/**
 * The name to show, never blank.
 *
 * `name` is `string!` on the concept, so a blank one should not exist -- but a
 * blank cell is indistinguishable from a cell that failed to render, and the
 * id's tail is the one thing every row has and the one thing that tells two
 * otherwise-empty rows apart.
 */
export function billingAccountName(account: BillingAccountRow): string {
  const name = account.name.trim();
  if (name !== "") return name;
  const tail = account.id.split(":").pop() ?? "";
  return tail === "" ? "Unnamed billing account" : `Unnamed billing account (${tail})`;
}

/**
 * Newest first, agreeing with the server rather than reordering it.
 *
 * `accounts` sorts `row.createdAt` descending and paginates 50. This restates
 * that order client-side so the list is stable across a re-read -- the same
 * promise `sortAccountTokens` makes for the credentials beneath each row, and
 * for the same reason: a fold that moved rows between reads would have
 * somebody hunting for the one they were just looking at.
 */
export function sortBillingAccounts(accounts: BillingAccountRow[]): BillingAccountRow[] {
  return [...accounts].sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}
