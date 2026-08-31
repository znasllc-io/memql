import { useState } from "react";

import { Button, Chip, Chips, type ChipTone } from "../../kit";
import { chipsFromMap, parseLabelChip, type LabelMap } from "./labels";

// A controlled label-map editor: chips in, chips out, no writes of its own.
//
// The two label maps this app edits -- a machine's `operatorLabels` and a
// routing policy's require/prefer sets -- are the same VALUE with different
// save moments. A machine's labels save per chip (the row is the truth, and a
// half-edited set is a real state); a policy's save with the form (the row is
// replaced wholesale, and a half-edited set is not). Keeping the widget
// controlled is what lets both exist without two editors that look the same
// and behave differently.
//
// `key=value` is the chip form throughout, which is what
// docs/public/operate/workers-runbook.md's worker.yaml already shows, so
// there is one spelling to learn rather than two.

export function MapEditor({
  value,
  onChange,
  busy = false,
  label,
  idPrefix,
  tone = "accent",
}: {
  value: LabelMap;
  onChange: (next: LabelMap) => void;
  busy?: boolean;
  /** Accessible name for the chip list and the input. */
  label: string;
  /** Unique across the surface: two editors on one screen must not share an
   *  input id, or a label points at the wrong control. */
  idPrefix: string;
  tone?: ChipTone;
}) {
  const [draft, setDraft] = useState("");
  const [hint, setHint] = useState("");
  const chips = chipsFromMap(value);
  const inputId = `${idPrefix}-draft`;

  function commit() {
    const text = draft.trim();
    if (text === "") return;
    const pair = parseLabelChip(text);
    if (pair === null) {
      // Refused rather than guessed: a bare word is a value with no key, and
      // inventing one is how a routing rule ends up matching something
      // nobody wrote.
      setHint("A label is a pair: write key=value.");
      return;
    }
    setHint("");
    setDraft("");
    // The same key twice is a REPLACEMENT, not a duplicate -- the map it
    // becomes has one entry, so showing two chips would misreport what will
    // be sent.
    onChange({ ...value, [pair.key]: pair.value });
  }

  function remove(key: string) {
    const next = { ...value };
    delete next[key];
    onChange(next);
  }

  return (
    <div className="os-fleet-labeledit">
      <Chips label={label}>
        {chips.length === 0 ? (
          <span className="os-caption">None set.</span>
        ) : (
          chips.map((chip) => {
            const pair = parseLabelChip(chip);
            const key = pair?.key ?? chip;
            return (
              <span key={key} className="os-chip-editable" role="listitem">
                <Chip tone={tone}>{chip}</Chip>
                <button
                  type="button"
                  className="os-chip-remove"
                  aria-label={`Remove ${chip}`}
                  disabled={busy}
                  onClick={() => remove(key)}
                >
                  &times;
                </button>
              </span>
            );
          })
        )}
      </Chips>
      {/* A nested <form> is invalid HTML, so this is a div: the policy editor
          mounts two of these INSIDE its own form, and a submit here would
          otherwise submit that one. Enter is handled explicitly instead. */}
      <div className="os-form-row">
        <label className="os-sr-only" htmlFor={inputId}>
          {label}
        </label>
        <input
          id={inputId}
          className="os-input"
          value={draft}
          disabled={busy}
          placeholder="key=value"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== "Enter") return;
            e.preventDefault();
            commit();
          }}
        />
        <Button onClick={commit} disabled={busy || draft.trim() === ""}>
          Add
        </Button>
      </div>
      {hint ? <p className="os-caption">{hint}</p> : null}
    </div>
  );
}
