import { useMemo } from "react";

import { Chip, Select } from "../../kit";
import { accountIsArchived, accountName, type AccountRow } from "./rows";

// THE ONE ACCOUNT PICKER IN THE OS.
//
// It lives in this app because this app owns the concept, and every tie
// surface -- the Deployables site detail, the Files inspector, the Users
// invite flow -- imports it rather than growing its own. Four hand-rolled
// pickers would be four places to disagree about what an archived client
// looks like, whether an unresolvable id renders, and what "no account"
// is called.
//
// PRESENTATION OVER ENGINE TRUTH, throughout. The options come from the
// caller's own `clientAccountsAll` snapshot; nothing here reads, and nothing
// here decides what a person may see. Organization ownership uses the
// required mode; optional labels keep their existing empty choice. The engine
// authorizes both the selected organization and the target resource.

/** What "no account" is called, in one place. */
export const NO_ACCOUNT_LABEL = "No client";

/**
 * A single-account picker: sites, invitations, knowledge domains.
 *
 * ARCHIVED ACCOUNTS ARE OFFERED, and marked. Filing a client away must not
 * make the sites already tied to them unpickable -- somebody correcting a tie
 * on an old deployable is exactly the person who needs the archived name in
 * the list. What they must not do is pick one by accident, which is what the
 * suffix is for.
 *
 * AN UNRESOLVABLE CURRENT VALUE KEEPS ITS PLACE. If `value` names an account
 * this caller cannot read -- archived elsewhere, or created by somebody whose
 * rows they do not see -- the option is synthesized from the id rather than
 * dropped. Dropping it would make the select fall back to its first option and
 * silently RE-TIE the row to a different client the moment somebody saved
 * anything else on the form.
 */
export function AccountPicker({
  value,
  onChange,
  onCommit,
  accounts,
  id,
  label,
  emptyLabel = NO_ACCOUNT_LABEL,
  disabled = false,
  required = false,
  requiredLabel = "Choose an organization",
}: {
  value: string;
  onChange: (next: string) => void;
  accounts: AccountRow[];
  id: string;
  label: string;
  /** Explicit choice, including confirming the current account. */
  onCommit?: () => void;
  /**
   * The empty option for optional labels and scopes. Required ownership
   * uses requiredLabel and cannot be cleared.
   */
  emptyLabel?: string;
  disabled?: boolean;
  /** Organization ownership requires an explicit, active account. */
  required?: boolean;
  /** Wording for the required, unselected prompt in the calling flow. */
  requiredLabel?: string;
}) {
  const options = useMemo(() => {
    const known = accounts.filter((a) => !required || !accountIsArchived(a) || a.id === value).map((a) => ({
      id: a.id,
      label: accountIsArchived(a) ? `${accountName(a)} (archived)` : accountName(a),
    }));
    const held = value.trim();
    if (held !== "" && !known.some((o) => o.id === held)) {
      known.unshift({ id: held, label: `${held} (not visible to you)` });
    }
    return known;
  }, [accounts, value, required]);

  return (
    <Select value={value} onChange={onChange} onCommit={onCommit} id={id} label={label}>
      {/* Optional labels can be cleared; required ownership uses a prompt. */}
      <option value="" disabled={required || disabled}>{required ? requiredLabel : emptyLabel}</option>
      {options.map((o) => (
        <option key={o.id} value={o.id} disabled={disabled || (required && !accounts.some((account) => account.id === o.id && !accountIsArchived(account)))}>
          {o.label}
        </option>
      ))}
    </Select>
  );
}

/**
 * A multi-account picker: the Library index's `accountIds` list.
 *
 * A LIST OF TOGGLES RATHER THAN A MULTI-SELECT. A native `<select multiple>`
 * loses every selection the moment somebody clicks without holding a modifier
 * -- which is the single most destructive interaction available on a control
 * whose whole job is "one or two accounts". Toggles cost more vertical space
 * and cannot do that.
 *
 * ARCHIVED ACCOUNTS APPEAR ONLY IF ALREADY SELECTED. Unlike the single picker,
 * this one is a labelling surface rather than a correction surface: somebody
 * filing a new document should not be offered clients that have been filed
 * away, but a document already labelled with one must keep showing that label
 * -- and must be able to remove it.
 */
export function AccountLabelPicker({
  selected,
  onChange,
  accounts,
  label,
  disabled = false,
}: {
  selected: string[];
  onChange: (next: string[]) => void;
  accounts: AccountRow[];
  label: string;
  disabled?: boolean;
}) {
  const offered = useMemo(() => {
    const held = new Set(selected);
    const known = accounts.filter((a) => !accountIsArchived(a) || held.has(a.id));
    const unresolvable = selected
      .filter((id) => !known.some((a) => a.id === id))
      .map((id) => ({ id, name: id, archived: false, unknown: true }));
    return [
      ...known.map((a) => ({
        id: a.id,
        name: accountName(a),
        archived: accountIsArchived(a),
        unknown: false,
      })),
      ...unresolvable,
    ];
  }, [accounts, selected]);

  function toggle(accountId: string) {
    onChange(
      selected.includes(accountId)
        ? selected.filter((id) => id !== accountId)
        : [...selected, accountId],
    );
  }

  if (offered.length === 0) {
    return (
      <p className="os-caption">
        No clients yet. The Accounts app is where they are added.
      </p>
    );
  }

  return (
    <div className="os-account-labels" role="group" aria-label={label}>
      {offered.map((o) => {
        const on = selected.includes(o.id);
        return (
          <button
            key={o.id}
            type="button"
            className="os-account-label"
            data-on={on || undefined}
            aria-pressed={on}
            disabled={disabled}
            onClick={() => toggle(o.id)}
          >
            {o.name}
            {o.archived ? <span className="os-caption"> archived</span> : null}
            {o.unknown ? <span className="os-caption"> not visible to you</span> : null}
          </button>
        );
      })}
    </div>
  );
}

/**
 * The tie, as a read-only chip.
 *
 * Renders NOTHING for an untied row rather than an empty chip or a dash: a
 * site with no client is the ordinary case, and a column of "--" on a list
 * where most rows have no tie is noise pretending to be data. The places that
 * need an explicit absence say so in words instead.
 */
export function AccountChip({ name }: { name: string }) {
  if (name.trim() === "") return null;
  return (
    <Chip tone="accent" title="Organization">
      {name}
    </Chip>
  );
}
