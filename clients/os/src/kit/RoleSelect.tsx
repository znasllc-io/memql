import { useId } from "react";
import { Select } from "./controls";
import { roleDisplayName } from "./RankMark";

export interface RoleChoice { value: string; label?: string; disabled?: boolean; reason?: string }
export const accountRoleChoices: readonly RoleChoice[] = ["developer", "admin", "member", "viewer"].map(value => ({ value }));

/** One quiet control for roles across account, user and invitation settings. */
export function RoleSelect({ value, onChange, label = "Role", options = accountRoleChoices, disabled = false }: {
  value: string; onChange: (role: string) => void; label?: string; options?: readonly RoleChoice[]; disabled?: boolean;
}) {
  const id = useId();
  return <fieldset className="os-role-select" disabled={disabled}><Select id={id} label={label} value={value} onChange={next => { if (!disabled) onChange(next); }}>
    {!options.some(option => option.value === value) ? <option value={value} disabled>{roleDisplayName(value)}</option> : null}
    {options.map(option => <option key={option.value} value={option.value} disabled={option.disabled} data-hint={option.reason}>{option.label ?? roleDisplayName(option.value)}</option>)}
  </Select></fieldset>;
}
