import { useState } from "react";
import { Fingerprint } from "lucide-react";
import { Mark } from "../chrome/Mark";
import { OrganizationTitle } from "./OrganizationTitle";
import { SetupTheme } from "./SetupTheme";
import { JoiningPolicyFields, JOINING_MODES, useJoiningPolicy, type JoiningPolicy } from "./JoiningPolicy";
import { Wizard } from "../kit/Wizard";
import { Field } from "../kit/controls";
import { oauthFields, value, type IdentityData } from "./nativeIdentity";

export function OwnershipWizard({ data, busy, error, submit }: {
  data: IdentityData; busy: boolean; error: string;
  submit: (path: string, form: Record<string, string>) => void;
}) {
  const local = data.Local === true;
  const [step, setStep] = useState(0);
  const [form, setForm] = useState<Record<string, string>>(() => ({
    ...oauthFields(data), domain: value(data, "PrefillDomain"),
    owner_first_name: value(data, "PrefillOwnerFirstName"), owner_last_name: value(data, "PrefillOwnerLastName"),
    owner_email: value(data, "PrefillOwnerEmail"), owner_phone: value(data, "PrefillOwnerPhone"),
    owner_primary_role: value(data, "PrefillOwnerPrimaryRole"), brand_name: value(data, "PrefillOrgName"),
    internal_domains: value(data, "PrefillInternalDomains"), internal_default_role: value(data, "PrefillInternalDefaultRole") || "writer",
    registration_mode: value(data, "PrefillMode") || "invite_only",
    registration_domains: value(data, "PrefillRegistrationDomains"), access_request_notify_emails: value(data, "PrefillNotifyEmails"),
  }));
  const joining = useJoiningPolicy({
    registrationMode: form.registration_mode || "invite_only", registrationDomains: form.registration_domains || "",
    accessRequestNotifyEmails: form.access_request_notify_emails || "", internalDomains: form.internal_domains || "",
    internalDefaultRole: form.internal_default_role || "writer",
  }, next => setForm(held => {
    const keys: Record<keyof JoiningPolicy, string> = { registrationMode: "registration_mode", registrationDomains: "registration_domains",
      accessRequestNotifyEmails: "access_request_notify_emails", internalDomains: "internal_domains", internalDefaultRole: "internal_default_role" };
    return { ...held, ...Object.fromEntries(Object.entries(next).map(([key, value]) => [keys[key as keyof JoiningPolicy], value])) };
  }));
  const field = (name: string, label: string, type = "text", required = false) => <Field label={label}><input className="os-input" aria-label={label}
    type={type} required={required} maxLength={name === "brand_name" ? 200 : undefined} value={form[name] || ""} onChange={e => setForm(f => ({ ...f, [name]: e.target.value }))} /></Field>;
  const valid = step === 0 ? Boolean((form.brand_name || "").trim() && (form.owner_first_name || "").trim() && (form.owner_last_name || "").trim() && /^[^\s@]+@[^\s@]+$/.test(form.owner_email || ""))
    : step === 1 ? Boolean((form.domain || "").trim())
    : step === 2 ? !joining.problem : true;
  const bodies = [
    <div className="os-identity-fields">{field("brand_name", "Organization name", "text", true)}
      <p>Owns this cluster and is the default for new resources. This organization cannot be deleted.</p>
      {field("owner_first_name", "First name", "text", true)}{field("owner_last_name", "Last name", "text", true)}
      {field("owner_email", "Owner email", "email", true)}<p>{local ? "Contact information only. This local installation does not verify email; your passkey will prove access." : "Verify this address, then register a passkey to become the cluster owner."}</p>
      {field("owner_phone", "Phone number (optional)", "tel")}<OrganizationTitle value={form.owner_primary_role || ""} onChange={next => setForm(f => ({ ...f, owner_primary_role: next }))} />
      </div>,
    <div className="os-identity-fields">{field("domain", "Cluster domain", "text", true)}<p>The hostname suffix for this installation, such as memql.localhost.</p></div>,
    <JoiningPolicyFields policy={joining} local={local} />,
    <div className="os-identity-fields"><p>Claim <strong>{form.domain}</strong> for <strong>{form.brand_name}</strong>, with <strong>{form.owner_first_name} {form.owner_last_name}</strong> as cluster owner.</p>
      <p>{local ? "No email is sent. You must create a passkey to finish setup and sign in to this local installation." : <>We’ll send a single-use verification link to <strong>{form.owner_email}</strong>. After verifying your email, you must register a passkey to finish setup.</>}</p>
      <p>How people join: <strong>{JOINING_MODES.find(([mode]) => mode === form.registration_mode)?.[1]}</strong>. You can change this in cluster settings.</p>
      <p>Once signed in, OS will continue with the existing inference setup.</p></div>,
  ];
  return <Wizard leadingIcon={<Mark />} icon={<Fingerprint />} title="Welcome to MemQL OS" lead="Set up this installation and verify cluster ownership."
    asideFooter={<SetupTheme />}
    label="Ownership setup" open={String(step)} onOpen={id => setStep(Number(id))}
    steps={["Organization and owner", "Your installation", "Joining the cluster", local ? "Register passkey" : "Verify email", ...(local ? [] : ["Register passkey"])].map((name, i) => ({ id: String(i), name,
      state: i < step ? "done" : i === step ? "open" : "ahead", body: bodies[i] }))}
    notices={error ? <p role="alert">{error}</p> : undefined}
    status={{ word: busy ? (local ? "Preparing passkey setup" : "Sending verification") : valid ? "Your turn" : step === 2 ? joining.problem : "Complete the required fields", tone: busy ? "busy" : "none" }}
    acts={busy ? [] : [...(step > 0 ? [{ label: "Back", text: true, onAct: () => setStep(step - 1) }] : []),
      ...(valid ? [{ label: step === 3 ? (local ? "Continue to passkey" : "Send verification link") : "Continue", tone: "primary" as const,
        onAct: () => {
          if (step === 2 && !joining.commit()) return;
          if (step === 3) submit("/setup", form);
          else setStep(step + 1);
        } }] : [])]} />;
}
