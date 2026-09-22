import { useState } from "react";
import { Fingerprint } from "lucide-react";
import { Wizard } from "../kit/Wizard";
import { Field, Select } from "../kit/controls";
import { oauthFields, value, type IdentityData } from "./nativeIdentity";

export function OwnershipWizard({ data, busy, error, submit }: {
  data: IdentityData; busy: boolean; error: string;
  submit: (path: string, form: Record<string, string>) => void;
}) {
  const [step, setStep] = useState(0);
  const [form, setForm] = useState<Record<string, string>>(() => ({
    ...oauthFields(data), domain: value(data, "PrefillDomain"),
    owner_first_name: value(data, "PrefillOwnerFirstName"), owner_last_name: value(data, "PrefillOwnerLastName"),
    owner_email: value(data, "PrefillOwnerEmail"), owner_phone: value(data, "PrefillOwnerPhone"),
    owner_primary_role: value(data, "PrefillOwnerPrimaryRole"), owner_gender: value(data, "PrefillOwnerGender"),
    owner_birthdate: value(data, "PrefillOwnerBirthdate"), brand_name: value(data, "PrefillOrgName"),
    internal_domains: value(data, "PrefillInternalDomains"), internal_default_role: "",
    registration_mode: value(data, "PrefillMode") || "invite_only",
    registration_domains: value(data, "PrefillRegistrationDomains"), access_request_notify_emails: value(data, "PrefillNotifyEmails"),
  }));
  const field = (name: string, label: string, type = "text", required = false) => <Field label={label}><input className="os-input" aria-label={label}
    type={type} required={required} value={form[name] || ""} onChange={e => setForm(f => ({ ...f, [name]: e.target.value }))} /></Field>;
  const select = (name: string, label: string, options: [string, string][]) => <Field label={label}><Select
    id={name} label={label} value={form[name] || ""} onChange={next => setForm(f => ({ ...f, [name]: next }))}>
    {options.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
  </Select></Field>;
  const valid = step === 0 ? Boolean((form.owner_first_name || "").trim() && (form.owner_last_name || "").trim() && /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(form.owner_email || ""))
    : step === 1 ? Boolean((form.domain || "").trim()) : true;
  const bodies = [
    <div className="os-identity-fields">{field("owner_first_name", "First name", "text", true)}{field("owner_last_name", "Last name", "text", true)}
      {field("owner_email", "Owner email", "email", true)}<p>Verify this address to become the cluster owner.</p>
      {field("owner_phone", "Phone number (optional)", "tel")}{field("owner_primary_role", "Role at organization (optional)")}
      {select("owner_gender", "Gender (optional)", [["", "Choose"], ["female", "Female"], ["male", "Male"], ["nonbinary", "Nonbinary"], ["other", "Other"], ["prefer_not_to_say", "Prefer not to say"]])}
      {field("owner_birthdate", "Date of birth (optional)", "date")}</div>,
    <div className="os-identity-fields">{field("domain", "Cluster domain", "text", true)}<p>The hostname suffix for this installation, such as memql.localhost.</p>
      {field("brand_name", "Organization name (optional)")}{field("internal_domains", "Internal email domains (comma-separated)")}
      <p>People with an internal email domain receive the default cluster role. External people receive a personal partition.</p></div>,
    <div className="os-identity-fields">{select("registration_mode", "New accounts", [["open", "Open registration"], ["domain_restricted", "Approved email domains"], ["invite_only", "Invitation only"], ["waitlist", "Access requests"]])}
      {field("registration_domains", "Approved email domains (comma-separated)")}
      {field("access_request_notify_emails", "Notify these emails about access requests (comma-separated)")}</div>,
    <div className="os-identity-fields"><p>Claim <strong>{form.domain}</strong> for <strong>{form.owner_first_name} {form.owner_last_name}</strong>.</p>
      <p>We’ll send a single-use verification link to <strong>{form.owner_email}</strong>. Ownership is granted only after verification.</p>
      <p>Once signed in, OS will continue with the existing inference setup.</p></div>,
  ];
  return <Wizard icon={<Fingerprint />} title="Welcome to MemQL OS" lead="Set up this installation and verify cluster ownership."
    label="Ownership setup" open={String(step)} onOpen={id => setStep(Number(id))}
    steps={["Cluster owner", "Your installation", "Account access", "Verify ownership"].map((name, i) => ({ id: String(i), name,
      state: i < step ? "done" : i === step ? "open" : "ahead", body: bodies[i] }))}
    notices={error ? <p role="alert">{error}</p> : undefined}
    status={{ word: busy ? "Sending verification" : valid ? "Your turn" : "Complete the required fields", tone: busy ? "busy" : "none" }}
    acts={busy ? [] : [...(step > 0 ? [{ label: "Back", text: true, onAct: () => setStep(step - 1) }] : []),
      ...(valid ? [{ label: step === 3 ? "Send verification link" : "Continue", tone: "primary" as const,
        onAct: () => step === 3 ? submit("/setup", form) : setStep(step + 1) }] : [])]} />;
}
