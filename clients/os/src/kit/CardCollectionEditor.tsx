import { useId, type InputHTMLAttributes, type ReactNode, type RefObject } from "react";
import { Search, X } from "lucide-react";
import { Select } from "./controls";
import { AddButton } from "./AddButton";
import { IconButton } from "./IconButton";

/** The same search, filter and add line for small editable card collections. */
export function CardCollectionSearch({ label, mode, query, onQueryChange, inputRef, inputProps, disabled = false, add, clearLabel, picks }: {
  label: string;
  mode: { label: string; value: string; options: readonly (readonly [string, string])[]; onChange: (value: string) => void };
  query: string;
  onQueryChange: (query: string) => void;
  inputRef: RefObject<HTMLInputElement | null>;
  inputProps?: Omit<InputHTMLAttributes<HTMLInputElement>, "value" | "onChange" | "disabled">;
  disabled?: boolean;
  add?: { label: string; enabled: boolean; onClick: () => void };
  clearLabel: string;
  /**
   * What is picked so far, drawn as removable chips INSIDE the box, before the
   * typing, like the "To:" line of an email. With any picks the box is drawn
   * round the chips and the input together and is marked `data-picker-field`,
   * so a `kit/AnchoredPicker` list joins the whole box, not the bare input.
   */
  picks?: readonly { key: string; label: string; icon?: ReactNode; onRemove: () => void }[];
}) {
  const id = useId();
  const boxed = picks !== undefined && picks.length > 0;
  return <fieldset className="os-card-search" disabled={disabled}>
    <legend className="os-sr-only">{label}</legend>
    <div className="os-card-search-mode">
      <Select id={`${id}-mode`} label={mode.label} value={mode.value} onChange={mode.onChange}>
        {mode.options.map(([value, name]) => <option key={value} value={value}>{name}</option>)}
      </Select>
    </div>
    <div className="os-card-search-input" data-picker-field={boxed || undefined}>
      <Search size={14} aria-hidden />
      {boxed ? picks.map(pick => <span key={pick.key} className="os-chip os-chip-editable" data-tone="accent">
        {pick.icon}{pick.label}
        {/* The mousedown is kept from the input, so an open list stays open. */}
        <button type="button" className="os-chip-remove" aria-label={`Remove ${pick.label}`} title={`Remove ${pick.label}`}
          onMouseDown={event => event.preventDefault()} onClick={pick.onRemove}><X size={10} aria-hidden /></button>
      </span>) : null}
      <input ref={inputRef} className="os-input" placeholder="Search" {...inputProps} disabled={disabled} value={query} onChange={event => onQueryChange(event.target.value)} />
      {query ? <IconButton label={clearLabel} onClick={() => { onQueryChange(""); inputRef.current?.focus(); }}><X size={14} aria-hidden /></IconButton> : null}
    </div>
    {add ? <AddButton label={add.label} disabled={!add.enabled} onClick={add.onClick} /> : null}
  </fieldset>;
}

/** Card content opens its editor; independent actions remain separate targets. */
export function EditableCard({ label, descriptionId, onEdit, disabled, children, meta, actions }: {
  label: string;
  descriptionId?: string;
  onEdit: () => void;
  disabled?: boolean;
  children: ReactNode;
  meta?: ReactNode;
  actions: ReactNode;
}) {
  return <div className="os-editable-card">
    <button type="button" className="os-editable-card-open" aria-label={label} aria-describedby={descriptionId} title={label} disabled={disabled} onClick={onEdit}>{children}</button>
    {meta ? <span className="os-editable-card-meta">{meta}</span> : null}
    <div className="os-editable-card-actions">{actions}</div>
  </div>;
}

export function CardCollectionSkeleton({ label }: { label: string }) {
  return <div className="os-card-skeleton" role="status" aria-busy="true">
    <span className="os-sr-only">{label}</span>
    <div aria-hidden="true">{[0, 1].map(key => <div className="os-editable-card" key={key}>
      <div className="os-editable-card-open"><span className="os-skeleton-block os-skeleton-name" /><span className="os-skeleton-block os-skeleton-detail" /></div>
    </div>)}</div>
  </div>;
}
