import { useSessionIfPresent } from "../../chrome/access";
import type { ProfileAccess } from "../../modules/profile/access";
import { accountIsArchived, SELF_ACCOUNT_ID, type AccountRow } from "./rows";

/** Default only from the server's resolved scope. A multi-organization member
 * chooses explicitly; list order and a role's display name carry no authority. */
export function defaultOrganization(accounts: readonly AccountRow[], access: ProfileAccess | null): string {
  const active = accounts.filter((account) => !accountIsArchived(account));
  if (access?.everyAccount === true) {
    return active.some((account) => account.id === SELF_ACCOUNT_ID) ? SELF_ACCOUNT_ID : "";
  }
  const allowed = new Set(access?.accountIds ?? access?.groups?.map((group) => group.accountId) ?? []);
  const choices = active.filter((account) => allowed.has(account.id));
  return choices.length === 1 ? choices[0]!.id : "";
}

export function useDefaultOrganization(accounts: readonly AccountRow[]): string {
  return defaultOrganization(accounts, useSessionIfPresent()?.access ?? null);
}

export function organizationChosen(accounts: readonly AccountRow[], accountId: string): boolean {
  return accountId !== "" && accounts.some((account) => account.id === accountId && !accountIsArchived(account));
}
