import { useId, useRef } from "react";
import { X } from "lucide-react";
import { AddButton } from "./AddButton";

export function listEntries(value: string): string[] {
  return [...new Set(value.split(",").map(domain => domain.trim().toLowerCase()).filter(Boolean))];
}

export function validEmailDomain(domain: string): boolean {
  return domain.length <= 253 && domain.split(".").every(label =>
    /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/i.test(label));
}

/** One entry at a time, with a controlled draft so navigation can commit it. */
export function EntryList({ entries, label, placeholder, hint, addLabel, draft, error, onDraft, onAdd, onRemove }: {
  entries: string[]; label: string; placeholder: string; hint?: string; addLabel: string;
  draft: string; error: string;
  onDraft: (value: string) => void; onAdd: () => void; onRemove: (domain: string) => void;
}) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const add = () => { onAdd(); input.current?.focus(); };
  return <div className="os-form-field os-entry-list">
    <label className="os-form-field-label" htmlFor={id}>{label}</label>
    <div className="os-form-row">
      <input ref={input} id={id} className="os-input" placeholder={placeholder} value={draft}
        autoCapitalize="none" autoComplete="off" spellCheck={false} maxLength={254}
        aria-invalid={Boolean(error)} aria-describedby={[hint ? `${id}-hint` : "", error ? `${id}-error` : ""].filter(Boolean).join(" ") || undefined}
        onChange={event => onDraft(event.target.value)}
        onKeyDown={event => {
          if (event.key === "Enter" && !event.nativeEvent.isComposing) { event.preventDefault(); add(); }
        }} />
      <AddButton label={addLabel} onClick={add} disabled={!draft.trim()}
        style={{ minWidth: "var(--os-control-h)", minHeight: "var(--os-control-h)" }} />
    </div>
    {error ? <p className="os-entry-list-error" id={`${id}-error`} role="alert">{error}</p> : null}
    {entries.length ? <ul className="os-chips os-entry-list-list" aria-label={label}>
      {entries.map(domain => <li className="os-chip" key={domain}>
        <span>{domain}</span>
        <button type="button" className="os-chip-remove" aria-label={`Remove ${domain}`} title={`Remove ${domain}`}
          onClick={() => { onRemove(domain); input.current?.focus(); }}><X size={12} aria-hidden /></button>
      </li>)}
    </ul> : null}
    {hint ? <p id={`${id}-hint`}>{hint}</p> : null}
  </div>;
}
