import { useMemo } from "react";

import { useSessionIfPresent } from "../../chrome/access";
import type { ProfileAccess } from "../../modules/profile/access";
import { selectableOrganizations } from "../email/organizations";
import { accountName, type AccountRow } from "./rows";
import { useAccountOptions } from "./tie";

// ===========================================================================
// THE CLIENT A STATISTICS PAGE READS UNDER (spec D13)
// ===========================================================================
// Websites and email figures are read for a client -- an organization -- or
// for every client the person reaches. Which choice they get follows from the
// organization scope the server resolved for them (MyAccess: `everyAccount`
// or `accountIds`, the same resolution the row gate uses):
//
//   - Staff (`everyAccount`) choose All clients or any active organization.
//   - Somebody in more than one organization chooses All my clients or one
//     of exactly those, named from the account list, else from the group
//     that grants it, else by its id.
//   - Anybody else gets no control at all, and reads everything in scope:
//     a member of one organization, whose scope IS that organization. A
//     control with one option is furniture, and a control listing other
//     organizations would disclose them.
//
// PRESENTATION, NOT PERMISSION. The reads narrow on the server (spec D11): a
// request naming another client's organization answers nothing. So a stored
// choice that is not among the options falls back to All rather than being
// sent, and "All" sends NO ids, which the reads take as everything the caller
// reaches.
//
// IT LIVES WITH THE ACCOUNT CONCEPT, beside the tie picker (tie.tsx says
// why): the kit is the shell's vocabulary, and a client picker is one domain's
// surface that the statistics pages mount.

export interface ScopeOption {
  value: string;
  label: string;
}

export interface ClientScope {
  /** Whether there is a choice to offer. */
  showControl: boolean;
  /** The chosen organization's id, or "" for all of them. */
  value: string;
  options: readonly ScopeOption[];
  /** What a statistics read sends: none for all, else the one chosen. */
  accountIds: readonly string[];
  /** Remember a choice; one the options do not hold is remembered as All. */
  set: (value: string) => void;
}

/** The value meaning every client in scope. */
const ALL = "";

const NO_CHOICE: Omit<ClientScope, "set"> = Object.freeze({ showControl: false, value: ALL, options: Object.freeze([]), accountIds: Object.freeze([]) });

function byLabel(a: ScopeOption, b: ScopeOption): number {
  return a.label.localeCompare(b.label, "en", { sensitivity: "base" }) || a.value.localeCompare(b.value);
}

/** The ids of the organizations a person belongs to, once each. */
function memberIds(access: ProfileAccess): string[] {
  return [...new Set((access.accountIds ?? []).map((id) => id.trim()).filter((id) => id !== ""))];
}

/** A member's organization by name: the account row's, else the granting group's, else its id. */
function memberLabel(id: string, accounts: readonly AccountRow[], access: ProfileAccess): string {
  const row = accounts.find((account) => account.id === id);
  if (row !== undefined && row.name.trim() !== "") return row.name.trim();
  const group = (access.groups ?? []).find((candidate) => candidate.accountId === id && candidate.accountName.trim() !== "");
  return group !== undefined ? group.accountName.trim() : id;
}

function optionsFor(access: ProfileAccess | null, accounts: readonly AccountRow[]): ScopeOption[] {
  if (access === null) return [];
  if (access.everyAccount === true) {
    const organizations = selectableOrganizations(accounts).map((account) => ({ value: account.id, label: accountName(account) }));
    return [{ value: ALL, label: "All clients" }, ...organizations.sort(byLabel)];
  }
  const ids = memberIds(access);
  if (ids.length < 2) return [];
  const organizations = ids.map((id) => ({ value: id, label: memberLabel(id, accounts, access) }));
  return [{ value: ALL, label: "All my clients" }, ...organizations.sort(byLabel)];
}

/**
 * The scope a person's statistics read under, given who they are, the
 * account list the page holds, and the choice stored for this app.
 */
export function clientScopeFor(access: ProfileAccess | null, accounts: readonly AccountRow[], stored: string): Omit<ClientScope, "set"> {
  const options = optionsFor(access, accounts);
  // One option is no choice: staff on a cluster with no organization yet.
  if (options.length < 2) return NO_CHOICE;
  const value = options.some((option) => option.value === stored) ? stored : ALL;
  return { showControl: true, value, options, accountIds: value === ALL ? [] : [value] };
}

/** Whether a choice could be offered at all, so the account list is read only when it could. */
function mayChoose(access: ProfileAccess | null): boolean {
  return access !== null && (access.everyAccount === true || memberIds(access).length > 1);
}

/**
 * The client scope for this session: `stored` is the app's remembered choice
 * and `onStore` remembers a new one (each app keeps its own, in its settings).
 */
export function useClientScope(stored: string, onStore: (value: string) => void): ClientScope {
  const access = useSessionIfPresent()?.access ?? null;
  const accounts = useAccountOptions(mayChoose(access));
  const scope = useMemo(() => clientScopeFor(access, accounts, stored), [access, accounts, stored]);
  return useMemo(
    () => ({
      ...scope,
      set: (value: string) => onStore(scope.options.some((option) => option.value === value) ? value : ALL),
    }),
    [scope, onStore],
  );
}
