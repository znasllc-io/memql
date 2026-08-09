import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ConceptLike, RowLike } from "@znasllc-io/memql-view-kit";

// Adapting an account's credential list into a row set an element can render.
//
// Same reasoning as src/deploy/rows.ts, and the same shape of exception: an
// account_token row IS a v1:identity:identity row, but the portal never
// browses it as one. It arrives through a named query
// (accountTokensForAccount) in the accountTokenSummary shape, the concept
// registry's display card for `identity` describes a generic credential
// (label / identityType / userId), and none of that is what an operator
// reading a customer's credential list needs to see.
//
// So the descriptor here names the three facts that matter for THIS list --
// what the credential is called, which account it is bound to, and whether it
// is still live -- and the ordinary table element renders it with the same
// theming, the same status badge and the same keyboard behaviour as every
// other list in the app.
//
// THE DESCRIPTOR IS NOT A CONCEPT, and its id says so: `accounts.token`, not
// `v1:identity:identity`. Claiming the concept id would put a second,
// disagreeing display card in front of rows the registry already describes.
//
// This module lives OUTSIDE src/views/ deliberately: reshaping data is not
// laying out a page, and keeping the iteration here is what lets the view
// modules contain none -- which portal_view_composition_test.go checks.

export const ACCOUNT_TOKEN_CONCEPT: ConceptLike = {
  id: "accounts.token",
  entity: "account token",
  // `state` is the status slot rather than the raw `active` boolean: a badge
  // reading "true" tells an operator nothing, and "revoked" is the word they
  // are scanning for.
  displayCard: { primary: "label", secondary: "issued", tertiary: "expires", status: "state" },
};

// A credential as the token panel needs it. Derived from accountTokenSummary,
// which projects no keyHash -- there is no digest to omit here because none
// arrives.
export interface AccountTokenView {
  id: string;
  label: string;
  active: boolean;
  accountId: string;
  createdAt: string;
}

function text(row: Row, key: string): string {
  const value = row[key];
  return typeof value === "string" ? value : "";
}

function flag(row: Row, key: string, fallback: boolean): boolean {
  const value = row[key];
  if (typeof value === "boolean") return value;
  if (typeof value === "string") return value.toLowerCase() === "true";
  return fallback;
}

// accountTokenViews normalizes the query result. `active` defaults to TRUE
// when absent, matching the concept's own default -- the alternative
// (defaulting to revoked) would show a live credential as dead, and an
// operator who believes a credential is revoked when it is not is the worse
// of the two errors.
export function accountTokenViews(rows: readonly Row[]): AccountTokenView[] {
  return rows.map((row) => ({
    id: text(row, "id"),
    label: text(row, "label"),
    active: flag(row, "active", true),
    accountId: text(row, "accountId"),
    createdAt: text(row, "createdAt"),
  }));
}

// accountTokenRows projects the views into the flat rows the table renders.
export function accountTokenRows(tokens: readonly AccountTokenView[]): RowLike[] {
  return tokens.map((token) => ({
    id: token.id,
    label: token.label || "(unlabelled)",
    state: token.active ? "active" : "revoked",
    issued: token.createdAt || "unknown",
    // Displayed as a word rather than left blank: an empty cell reads as
    // "unknown", and "this credential does not expire" is a fact the operator
    // should be able to see at a glance in a list they are auditing.
    expires: "no expiry",
  }));
}

// A customer as the action bar needs it. The Customers view already renders
// the population; this is only what an ACTION has to know about the one row an
// operator selected.
export interface SelectedAccount {
  id: string;
  name: string;
  status: string;
  description: string;
  primaryContactName: string;
  primaryContactEmail: string;
  externalRef: string;
}

export function selectedAccount(rows: readonly Row[], rowId: string): SelectedAccount | null {
  if (rowId === "") return null;
  const match = rows.find((row) => text(row, "id") === rowId);
  if (match === undefined) return null;
  return {
    id: text(match, "id"),
    name: text(match, "name"),
    status: text(match, "status"),
    description: text(match, "description"),
    primaryContactName: text(match, "primaryContactName"),
    primaryContactEmail: text(match, "primaryContactEmail"),
    externalRef: text(match, "externalRef"),
  };
}
