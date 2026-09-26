import { useState } from "react";
import { Field, Select } from "../kit/controls";

const TITLES = ["Owner", "Founder", "CEO", "COO", "CTO", "Manager", "Engineer", "Designer", "Consultant"];

/** A profile title only. Authorization continues to come from assigned roles. */
export function OrganizationTitle({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const [other, setOther] = useState(() => value !== "" && !TITLES.includes(value));
  const label = "Title in organization (optional)";
  return <>
    <Field label={label}><Select id="organization-title" label={label} value={other ? "other" : value}
      onChange={next => { setOther(next === "other"); onChange(next === "other" ? "" : next); }}>
      <option value="">Choose</option>
      {TITLES.map(title => <option key={title} value={title}>{title}</option>)}
      <option value="other">Other</option>
    </Select></Field>
    {other ? <Field label="Your title"><input className="os-input" aria-label="Your title" maxLength={80}
      value={value} onChange={event => onChange(event.target.value)} /></Field> : null}
  </>;
}
