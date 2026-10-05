import { useState } from "react";
import { Field, Select } from "../kit/controls";
import { EntryList, listEntries, validEmailDomain } from "../kit/EntryList";

export interface JoiningPolicy {
  registrationMode: string;
  registrationDomains: string;
  accessRequestNotifyEmails: string;
  internalDomains: string;
  internalDefaultRole: string;
}

export const JOINING_MODES = [
  ["invite_only", "Invitation only", "An admin invites each person. An internal email domain does not bypass the invitation."],
  ["domain_restricted", "Approved email domains", "People at these domains can sign up or be invited. To admit someone from another domain, add their domain first."],
  ["waitlist", "Admin approval", "People request access. An admin reviews each request and sends an invitation if approved."],
  ["open", "Open registration", "Anyone with a verified email address can create an account."],
] as const;

type ListKey = "registrationDomains" | "accessRequestNotifyEmails" | "internalDomains";
const EMPTY_ENTRIES: Record<ListKey, string> = { registrationDomains: "", accessRequestNotifyEmails: "", internalDomains: "" };
const LABELS: Record<ListKey, string> = {
  registrationDomains: "Approved email domains", accessRequestNotifyEmails: "Notify about requests (optional)", internalDomains: "Internal email domains (optional)",
};

function entryProblem(key: ListKey, text: string): string {
  if (!text.trim()) return "";
  return (key === "accessRequestNotifyEmails" ? /^[^\s,@]+@[^\s,@]+$/.test(text.trim()) : validEmailDomain(text.trim()))
    ? "" : key === "accessRequestNotifyEmails" ? "Enter one email address, such as admin@example.com." : "Use a domain like example.com, without @ or https://.";
}

/** Both setup and settings commit pending entries through the same validation. */
export function useJoiningPolicy(value: JoiningPolicy, onChange: (next: Partial<JoiningPolicy>) => void) {
  const [drafts, setDrafts] = useState(EMPTY_ENTRIES);
  const [errors, setErrors] = useState(EMPTY_ENTRIES);
  const active: ListKey[] = ["internalDomains",
    ...(value.registrationMode === "domain_restricted" ? ["registrationDomains" as const] : []),
    ...(["waitlist", "domain_restricted"].includes(value.registrationMode) ? ["accessRequestNotifyEmails" as const] : [])];
  const effective = { ...value };
  for (const key of active) effective[key] = listEntries([value[key], drafts[key]].join(",")).join(", ");
  const problem = active.map(key => entryProblem(key, drafts[key])).find(Boolean)
    || (value.registrationMode === "domain_restricted" && !effective.registrationDomains ? "Add at least one approved email domain." : "");
  const reset = () => { setDrafts(EMPTY_ENTRIES); setErrors(EMPTY_ENTRIES); };
  return {
    value, effective, problem, pending: Object.values(drafts).some(text => text.trim()), onChange, reset,
    commit: () => {
      if (problem) return false;
      onChange(effective);
      reset();
      return true;
    },
    entry: (key: ListKey) => ({
      entries: listEntries(value[key]), label: LABELS[key], draft: drafts[key], error: errors[key],
      placeholder: key === "accessRequestNotifyEmails" ? "admin@example.com" : "example.com",
      addLabel: key === "accessRequestNotifyEmails" ? "Add notification email" : key === "registrationDomains" ? "Add approved domain" : "Add internal domain",
      onDraft: (text: string) => { setDrafts(held => ({ ...held, [key]: text })); setErrors(held => ({ ...held, [key]: "" })); },
      onAdd: () => {
        const error = entryProblem(key, drafts[key]);
        setErrors(held => ({ ...held, [key]: error }));
        if (error) return;
        onChange({ [key]: listEntries([value[key], drafts[key]].join(",")).join(", ") });
        setDrafts(held => ({ ...held, [key]: "" }));
      },
      onRemove: (entry: string) => onChange({ [key]: listEntries(value[key]).filter(item => item !== entry).join(", ") }),
    }),
  };
}

export function JoiningPolicyFields({ policy, local = false }: { policy: ReturnType<typeof useJoiningPolicy>; local?: boolean }) {
  const { value, onChange } = policy;
  const [defaultsOpen, setDefaultsOpen] = useState(Boolean(value.internalDomains));
  return <div className="os-joining-policy">
    <Field label="How people join"><Select id="joining-policy" label="How people join" value={value.registrationMode}
      onChange={registrationMode => onChange({ registrationMode })}>
      {JOINING_MODES.map(([mode, label]) => <option key={mode} value={mode}>{label}</option>)}
    </Select></Field>
    <p>{JOINING_MODES.find(([mode]) => mode === value.registrationMode)?.[2]}</p>
    {value.registrationMode === "domain_restricted" ? <EntryList {...policy.entry("registrationDomains")} /> : null}
    {["waitlist", "domain_restricted"].includes(value.registrationMode) ? <EntryList {...policy.entry("accessRequestNotifyEmails")}
      hint="Leave empty to notify cluster owners and admins. Review requests in Users." /> : null}
    <details className="os-joining-defaults" open={defaultsOpen} onToggle={event => setDefaultsOpen(event.currentTarget.open)}>
      <summary>Internal team defaults</summary>
      <p>In every joining mode, these domains assign a starting role after someone is admitted. Invitations can specify a different role.</p>
      <EntryList {...policy.entry("internalDomains")} />
      <Field label="Default internal role"><Select id="joining-internal-role" label="Default internal role" value={value.internalDefaultRole}
        onChange={internalDefaultRole => onChange({ internalDefaultRole })}>
        {["reader", "writer", "admin", "developer", "owner"].map(role => <option key={role} value={role}>{role[0]!.toUpperCase() + role.slice(1)}</option>)}
      </Select></Field>
      <p>Other domains start as Readers. Existing accounts keep their roles.</p>
    </details>
    {local ? <p>This installation uses passkeys only. These settings do not enable email sign-up locally.</p> : null}
  </div>;
}
