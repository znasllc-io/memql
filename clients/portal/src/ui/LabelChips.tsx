import { useState, type KeyboardEvent, type ReactNode } from "react";

import { Badge } from "./Badge";
import { X } from "./icons";

// Free-text labels on a thing, editable in place -- the one new vocabulary
// piece the Artifacts page needed (a person types their own labels; their
// agents write the same field through a tool). One Badge chip per label
// with a small remove control, and one text input that adds a label on
// Enter.
//
// onAdd/onRemove rather than onChange(next: string[]): the caller almost
// always backs this with one mutation per add or remove (an artifact-label
// attach/detach call, here), so it already knows which single label
// changed. Handing back a whole next[] would make it re-diff two arrays to
// recover the very fact it started with.
//
// Duplicate/blank rejection lives HERE rather than in the caller: the
// contract is "onAdd only ever fires with something worth adding," so a
// caller wiring this to a mutation never needs its own guard.

export function LabelChips({
  labels,
  onAdd,
  onRemove,
  busy = false,
  readOnly = false,
}: {
  labels: string[];
  onAdd: (label: string) => void;
  onRemove: (label: string) => void;
  // Disables every control while an add/remove from a previous interaction
  // is still in flight -- the same meaning `busy` carries on Button, minus
  // the label swap (there is no single action here to relabel).
  busy?: boolean;
  // Renders the chips with no remove control and no add input -- the same
  // list, read-only (e.g. labels on a row the viewer does not own).
  readOnly?: boolean;
}): ReactNode {
  const [draft, setDraft] = useState("");

  function commit(): void {
    const next = draft.trim();
    if (next === "" || labels.includes(next)) return;
    onAdd(next);
    setDraft("");
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    if (event.key !== "Enter") return;
    event.preventDefault();
    commit();
  }

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {labels.map((label) => (
        <Badge key={label} tone="neutral">
          {label}
          {readOnly ? null : (
            <button
              type="button"
              disabled={busy}
              onClick={() => onRemove(label)}
              aria-label={`Remove label ${label}`}
              className="-mr-1 ml-1 inline-flex rounded-full p-0.5 text-subtle hover:bg-raised hover:text-fg disabled:cursor-not-allowed disabled:opacity-40"
            >
              <X size={11} aria-hidden="true" />
            </button>
          )}
        </Badge>
      ))}
      {readOnly ? null : (
        // The placeholder is the ONLY visible affordance for an Enter-only
        // add path, so it has to name the key, not just describe the field
        // ("Add a label" alone left a typed label with no visible way to
        // tell it commits). w-48 borrows Field.tsx's own `grow` minimum
        // (min-w-48) rather than inventing a new width -- wide enough that
        // the longer placeholder is readable and a realistic label
        // ("customer-onboarding-v2") doesn't scroll out of view while it's
        // being typed. Fixed rather than w-full/flex-1: this row wraps a
        // mix of small chips, and a growing input would swing width
        // unpredictably with how many chips preceded it on the line.
        <label className="inline-flex">
          <span className="sr-only">Add a label</span>
          <input
            type="text"
            value={draft}
            disabled={busy}
            placeholder="Add a label, press Enter"
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={handleKeyDown}
            className="w-48 rounded border border-line bg-surface px-2 py-1 text-xs text-fg placeholder:text-subtle disabled:cursor-not-allowed disabled:opacity-40"
          />
        </label>
      )}
    </div>
  );
}
