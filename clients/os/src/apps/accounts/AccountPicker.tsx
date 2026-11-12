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
 * A single-account picker: sites, invitations, knowledge domains, files and goals.
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
