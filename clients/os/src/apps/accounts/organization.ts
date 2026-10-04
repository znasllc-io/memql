import { useSessionIfPresent } from "../../chrome/access";
import type { ProfileAccess } from "../../modules/profile/access";
import { accountIsArchived, accountIsSelf, type AccountRow } from "./rows";

/** Operators default to the cluster's own organization. Other defaults come
 * from the server's resolved membership scope; list order carries no authority. */
export function defaultOrganization(accounts: readonly AccountRow[], access: ProfileAccess | null): string {
  const active = accounts.filter((account) => !accountIsArchived(account));
  if (access?.everyAccount === true) {
    return active.find(accountIsSelf)?.id ?? "";
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
