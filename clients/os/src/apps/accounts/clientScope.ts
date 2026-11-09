import { useEffect, useMemo, useState } from "react";

import { useSession } from "../../chrome/access";
import type { AccessGroup, ProfileAccess } from "../../modules/profile/access";
import { selectableOrganizations, untitledAccountName, type AccountRow } from "./rows";
import { useAccountOptionsFeed } from "./tie";

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
//     of exactly those.
//   - Anybody else gets no control at all, and reads everything in scope:
//     a member of one organization, whose scope IS that organization. A
//     control with one option is furniture, and a control listing other
//     organizations would disclose them.
//
// An organization is named the same way for both: by its account row, else
// by the group that grants it, else as the Accounts app names an unnamed one.
//
// PRESENTATION, NOT PERMISSION. The reads narrow on the server (spec D11): a
// request naming another client's organization answers nothing. So a stored
// choice that is not among the options falls back to All rather than being
// sent, and "All" sends NO ids, which the reads take as everything the caller
// reaches.
//
// A STAFF MEMBER'S CHOICE IS KEPT UNTIL THE ACCOUNT LIST HAS ANSWERED. Only
// that read names every organization; before it answers, the list holds at
// most the organizations the person's own groups name. Validating the stored
// choice against that partial list dropped it, so the page's FIRST read asked
// for every client, the opposite of what its person had chosen, and the
// choice came back when the list did. So until the first answer the scope is
// the stored choice with no control. That is safe for the reason above: an id
// the caller may not read answers nothing, never another client's figures.
// A member's choices come from MyAccess, not from the list, so a member is
// answered at once. A list that answers with an error counts as answered (the
// control then offers what it could read), so a choice nothing can confirm is
// never held forever behind a hidden control.
// Regression: test/accounts/clientScope.test.tsx, "before the account list
// has answered".
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

/**
 * An organization's name, the same way for staff and for members: its account
 * row's name, else the name the granting group carries, else the Accounts
 * app's spelling of an unnamed account, which keeps the id in sight.
 */
function organizationLabel(id: string, accounts: readonly AccountRow[], groups: readonly AccessGroup[]): string {
  const row = accounts.find((account) => account.id === id)?.name.trim() ?? "";
  if (row !== "") return row;
  const group = groups.find((candidate) => candidate.accountId === id && candidate.accountName.trim() !== "")?.accountName.trim() ?? "";
  return group !== "" ? group : untitledAccountName(id);
}

function optionsFor(access: ProfileAccess | null, accounts: readonly AccountRow[]): ScopeOption[] {
  if (access === null) return [];
  const groups = access.groups ?? [];
  const label = (id: string) => ({ value: id, label: organizationLabel(id, accounts, groups) });
  if (access.everyAccount === true) {
    return [{ value: ALL, label: "All clients" }, ...selectableOrganizations(accounts).map((account) => label(account.id)).sort(byLabel)];
  }
  const ids = memberIds(access);
  if (ids.length < 2) return [];
  return [{ value: ALL, label: "All my clients" }, ...ids.map(label).sort(byLabel)];
}

/**
 * The scope a person's statistics read under, given who they are, the
 * account list the page holds, the choice stored for this app, and whether
 * that list has answered yet (see the comment at the top of this file).
 */
export function clientScopeFor(access: ProfileAccess | null, accounts: readonly AccountRow[], stored: string, accountsAnswered = true): Omit<ClientScope, "set"> {
  if (access?.everyAccount === true && !accountsAnswered) {
    return { showControl: false, value: stored, options: [], accountIds: stored === ALL ? [] : [stored] };
  }
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
  const { access } = useSession();
  const feed = useAccountOptionsFeed(mayChoose(access));
  // Answered once the list has been read, and still answered while a later
  // refresh is out or the connection drops, so the control does not vanish
  // from under the person every time the stream blinks.
  const answeredNow = feed.state === "live" || feed.state === "degraded";
  const [answeredOnce, setAnsweredOnce] = useState(false);
  useEffect(() => {
    if (answeredNow) setAnsweredOnce(true);
  }, [answeredNow]);
  const answered = answeredNow || answeredOnce;
  const scope = useMemo(() => clientScopeFor(access, feed.accounts, stored, answered), [access, feed.accounts, stored, answered]);
  return useMemo(
    () => ({
      ...scope,
      set: (value: string) => onStore(scope.options.some((option) => option.value === value) ? value : ALL),
    }),
    [scope, onStore],
  );
}
