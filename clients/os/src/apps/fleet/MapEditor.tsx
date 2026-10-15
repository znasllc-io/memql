import { useEffect, useRef, useState } from "react";
import { Trash2 } from "lucide-react";
import { Button, Notice } from "../../kit";
import { CardCollectionSearch, EditableCard } from "../../kit/CardCollectionEditor";
import { CardCarousel } from "../../kit/CardCarousel";
import { IconButton } from "../../kit/IconButton";
import type { LabelMap } from "./labels";

type Draft = { key: string; value: string; original: string | null };

// One editor for operator labels and routing predicates. The parent owns the
// save moment: machine edits return a receipt; a route updates its form draft.
export function MapEditor({ value, onChange, busy = false, label, idPrefix }: {
  value: LabelMap;
  /** A false receipt keeps the draft for retry; the parent displays its error. */
  onChange: (next: LabelMap) => void | boolean | Promise<boolean>;
  busy?: boolean;
  label: string;
  idPrefix: string;
}) {
  const [query, setQuery] = useState("");
  const [searchBy, setSearchBy] = useState("label");
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState("");
  const [revealKey, setRevealKey] = useState<string>();
  const [focusRequest, setFocusRequest] = useState(0);
  const search = useRef<HTMLInputElement>(null);
  const locked = useRef(false);
  const restoreSearch = useRef(false);
  const disabled = busy || saving;
  const name = query.trim();
  const needle = name.toLocaleLowerCase();
  const entries = Object.entries(value).sort(([a], [b]) => a.localeCompare(b));
  const matches = entries.filter(([key, text]) => (searchBy === "label" ? key : text).toLocaleLowerCase().includes(needle));
  const validName = name !== "" && !name.includes("=");
  const canAdd = searchBy === "label" && validName && !Object.hasOwn(value, name);
  const conflict = draft !== null && (Object.hasOwn(value, draft.key) ? value[draft.key] : null) !== draft.original;
  const hintId = `${idPrefix}-hint`;

  useEffect(() => {
    if (restoreSearch.current && !disabled && !draft) {
      restoreSearch.current = false;
      search.current?.focus();
    }
  }, [disabled, draft, focusRequest]);

  function begin(key: string, original: string | null) {
    if (disabled || draft) return;
    setFailure("");
    setDraft({ key, value: original ?? "", original });
  }

  function cancel() {
    restoreSearch.current = true;
    setDraft(null);
    setFailure("");
  }

  async function change(next: LabelMap) {
    if (disabled || locked.current) return false;
    locked.current = true;
    setSaving(true);
    setFailure("");
    try { return (await onChange(next)) !== false; }
    catch { setFailure("The labels could not be saved. Try again."); return false; }
    finally { locked.current = false; setSaving(false); }
  }

  async function save() {
    if (!draft || conflict) return;
    const key = draft.key;
    if (await change({ ...value, [key]: draft.value.trim() })) {
      restoreSearch.current = true;
      setDraft(null);
      setQuery("");
      setRevealKey(key);
    }
  }

  async function remove(key: string) {
    const next = { ...value };
    delete next[key];
    setRevealKey(undefined);
    // Restore focus after the pressed card disappears, including a last page.
    restoreSearch.current = true;
    await change(next);
    setFocusRequest(request => request + 1);
  }

  return <div className="os-fleet-labeledit" aria-busy={disabled}>
    <CardCollectionSearch label={`Find or add ${label.toLowerCase()}`} query={query} inputRef={search}
      onQueryChange={next => { setQuery(next); setRevealKey(undefined); setFailure(""); }}
      mode={{ label: `Search ${label.toLowerCase()} by`, value: searchBy, options: [["label", "Label"], ["value", "Value"]], onChange: next => { setSearchBy(next); setFailure(""); } }}
      disabled={disabled || draft !== null} clearLabel="Clear label search"
      add={searchBy === "label" ? { label: canAdd ? `Add label ${name}` : "Add label", enabled: canAdd, onClick: () => begin(name, null) } : undefined}
      inputProps={{ id: `${idPrefix}-search`, "aria-label": `Search ${label.toLowerCase()}`, "aria-describedby": hintId,
        onKeyDown: event => {
          if (event.key === "Escape") { event.preventDefault(); setQuery(""); }
          if (event.key !== "Enter" || event.nativeEvent.isComposing) return;
          event.preventDefault();
          if (canAdd) begin(name, null);
          else if (searchBy === "label" && Object.hasOwn(value, name)) begin(name, value[name]!);
        },
      }} />
    <p className="os-caption fleet-label-hint" id={hintId}>Search by label or value. Enter a new label name to add it.</p>
    {searchBy === "label" && name.includes("=") ? <p className="os-caption" role="status">Enter the label name first, then its value.</p> : null}
    {draft ? <div className="os-card-draft" role="group" aria-label={`${draft.original === null ? "Add" : "Edit"} ${draft.key}`}>
      <label htmlFor={`${idPrefix}-value`}>Value for <strong>{draft.key}</strong></label>
      <div className="os-form-row">
        <input autoFocus id={`${idPrefix}-value`} className="os-input" value={draft.value} disabled={disabled}
          onChange={event => setDraft({ ...draft, value: event.target.value })}
          onKeyDown={event => {
            if (event.key === "Enter" && !event.nativeEvent.isComposing) { event.preventDefault(); void save(); }
            if (event.key === "Escape" && !disabled) { event.preventDefault(); cancel(); }
          }} />
      </div>
      {conflict && !disabled ? <Notice tone="warn" sentence="This label changed elsewhere." next="Cancel and reopen it to edit the current value." /> : null}
      <div className="os-panel-actions">
        <Button disabled={disabled} onClick={cancel}>Cancel</Button>
        <Button tone="primary" disabled={conflict || (draft.original !== null && draft.value.trim() === draft.original)} busy={disabled} busyLabel="Saving..." onClick={() => { void save(); }}>{draft.original === null ? "Add" : "Save"}</Button>
      </div>
    </div> : null}
    {failure ? <Notice tone="error" sentence={failure} /> : null}
    {matches.length ? <CardCarousel key={`${searchBy}:${query}`} items={matches} itemKey={([key]) => key} revealKey={revealKey} minCardWidth={240}
      label={`${label} cards`} listLabel={label} noun="label" renderItem={([key, text]) => <EditableCard
        label={`Edit ${key}`} descriptionId={`${idPrefix}-card-${encodeURIComponent(key)}`} disabled={disabled || draft !== null} onEdit={() => begin(key, text)}
        actions={<IconButton className="os-card-remove" label={`Remove ${key}=${text}`} disabled={disabled || draft !== null} onClick={() => { void remove(key); }}><Trash2 size={15} aria-hidden /></IconButton>}>
        <strong>{key}</strong><span id={`${idPrefix}-card-${encodeURIComponent(key)}`}>{text || "No value"}</span>
      </EditableCard>} /> : !draft ? <p className="os-caption" role="status">{entries.length ? "No matching labels." : "No routing labels."}</p> : null}
  </div>;
}
