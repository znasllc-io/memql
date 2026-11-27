import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Check } from "lucide-react";
import { AnchoredPicker } from "./AnchoredPicker";
import { CardCollectionSearch } from "./CardCollectionEditor";

export interface PickerChoice { id: string; label: string; description?: string; kind?: string }

/** Search, select several, then commit once. Selection survives filtering and
 * closing the popup; the caller clears it only after a successful save. */
export function MultiPicker({ choices, selected, onChange, label, disabled = false, single = false, add }: {
  choices: readonly PickerChoice[];
  selected: readonly string[];
  onChange: (ids: string[]) => void;
  label: string;
  disabled?: boolean;
  single?: boolean;
  add?: { label: string; onClick: () => void };
}) {
  const id = useId(), input = useRef<HTMLInputElement>(null), owner = useRef<HTMLDivElement>(null), popup = useRef<HTMLDivElement>(null);
  const [query, setQuery] = useState(""), [kind, setKind] = useState("all"), [open, setOpen] = useState(false), [active, setActive] = useState(0), [limit, setLimit] = useState(20);
  const kinds = useMemo(() => [...new Set(choices.map(choice => choice.kind).filter((value): value is string => !!value))], [choices]);
  const matches = useMemo(() => choices.filter(choice => (kind === "all" || choice.kind === kind) && `${choice.label} ${choice.description ?? ""}`.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase())), [choices, query, kind]);
  const visible = matches.slice(0, limit), current = Math.min(active, visible.length - 1);
  const listId = `${id}-choices`;
  useEffect(() => { if (open) document.getElementById(`${listId}-${current}`)?.scrollIntoView?.({ block: "nearest" }); }, [open, current, listId]);
  function toggle(key: string) {
    onChange(selected.includes(key) ? selected.filter(value => value !== key) : single ? [key] : [...selected, key]);
    if (single) setOpen(false);
  }
  function keyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Escape") { setOpen(false); event.stopPropagation(); return; }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault(); setOpen(true); setActive(open ? Math.max(0, Math.min(visible.length - 1, current + (event.key === "ArrowDown" ? 1 : -1))) : event.key === "ArrowDown" ? 0 : Math.max(0, visible.length - 1));
    } else if (event.key === "Enter") {
      event.preventDefault(); if (open && visible[current]) toggle(visible[current].id); else setOpen(true);
    } else if (event.key === "Tab") setOpen(false);
  }
  return <div className="os-multi-picker" ref={owner}>
    <CardCollectionSearch label={label} query={query} inputRef={input} disabled={disabled}
      mode={kinds.length ? { label: `${label} filter`, value: kind, options: [["all", "All"], ...kinds.map(value => [value, value] as const)], onChange: value => { setKind(value); setActive(0); setLimit(20); } } : undefined}
      onQueryChange={value => { setQuery(value); setActive(0); setLimit(20); setOpen(true); }} clearLabel={`Clear ${label.toLowerCase()} search`}
      picks={selected.map(key => ({ key, label: choices.find(choice => choice.id === key)?.label ?? key, onRemove: () => onChange(selected.filter(value => value !== key)) }))}
      add={add ? { ...add, enabled: selected.length > 0 && !disabled } : undefined}
      inputProps={{ id, placeholder: `Search ${label.toLowerCase()}`, "aria-label": label, role: "combobox", "aria-autocomplete": "list", "aria-expanded": open && !disabled, "aria-controls": open && !disabled ? listId : undefined,
        "aria-activedescendant": open && !disabled && current >= 0 ? `${listId}-${current}` : undefined, autoComplete: "off", onFocus: () => setOpen(true), onClick: () => setOpen(true), onKeyDown: keyDown }} />
    {open && !disabled ? <AnchoredPicker anchor={input} owner={owner} popupRef={popup} onClose={() => setOpen(false)}>
      <div id={listId} role="listbox" aria-label={`${label} choices`} aria-multiselectable={!single}>
        {visible.map((choice, index) => <button key={choice.id} id={`${listId}-${index}`} type="button" role="option" aria-selected={selected.includes(choice.id)}
          className="os-multi-picker-option" data-active={index === current || undefined} tabIndex={-1} onMouseDown={event => event.preventDefault()} onPointerMove={() => setActive(index)} onClick={() => toggle(choice.id)}>
          <span><strong>{choice.label}</strong>{choice.description ? <small>{choice.description}</small> : null}</span>
          {selected.includes(choice.id) ? <Check size={15} aria-hidden /> : null}
        </button>)}
      </div>
      {!matches.length ? <p className="os-caption os-multi-picker-empty">No matches.</p> : null}
      {matches.length > limit ? <button type="button" className="os-multi-picker-more" onClick={() => { setLimit(limit + 20); input.current?.focus(); }}>Show more</button> : null}
    </AnchoredPicker> : null}
  </div>;
}
