import type { ReactNode } from "react";

// Checkbox and RadioGroup -- the two primitives whose absence is why seven
// pages hand-rolled a native control with a label row beside it, and why no
// two of those rows agreed: items-start here, items-center there, gap-2 in one
// file and gap-1.5 in the next, the label text-sm in one and text-xs in
// another. None of them was wrong on its own page.
//
// TWO DECISIONS WORTH KNOWING:
//
// 1. The control stays a NATIVE <input>. A drawn box would have to
//    re-implement focus, the space key, indeterminate state, form
//    participation and the platform's own high-contrast handling; the only
//    thing it would buy is a bespoke tick. `accent-color` recolours the native
//    control to the brand accent in every current engine, which is the whole
//    of what a drawn box was going to be for.
//
// 2. Alignment is `items-start`, and the box gets a small top margin rather
//    than the row getting `items-center`. Centring reads better only while
//    every label is one line; the moment one wraps -- and these labels carry
//    sentences, because a checkbox that changes what a machine may do has to
//    say so -- a centred box floats into the middle of a paragraph. Top
//    alignment keeps it beside the FIRST line, which is the line it labels.

// The box's top margin centres it on the first line of label text: the control
// is 1rem and the text-sm line box is ~1.25rem, so half the difference is 2px.
const BOX = "mt-0.5 size-4 shrink-0 accent-accent disabled:cursor-not-allowed";

const ROW = "flex items-start gap-2 text-sm text-fg";

export function Checkbox({
  checked,
  onChange,
  label,
  hint,
  disabled = false,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label: ReactNode;
  hint?: string;
  disabled?: boolean;
}): ReactNode {
  return (
    <label className={ROW + (disabled ? " cursor-not-allowed opacity-40" : " cursor-pointer")}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
        className={BOX}
      />
      <span className="min-w-0">
        {label}
        {hint === undefined ? null : <span className="mt-0.5 block text-xs text-subtle">{hint}</span>}
      </span>
    </label>
  );
}

export interface RadioOption {
  value: string;
  label: ReactNode;
  hint?: string;
  disabled?: boolean;
}

/**
 * RadioGroup renders one exclusive choice.
 *
 * `name` is required and must be unique on the page: it is what makes the
 * options exclusive to the BROWSER, and therefore what makes arrow-key
 * navigation move between exactly these options and no others. Two groups
 * sharing a name is not a styling bug, it is one group.
 *
 * The wrapper is a <fieldset>/<legend> pair rather than a div with a label,
 * because a group of radios has a group-level question ("Which release?") and
 * <legend> is the only element that announces one.
 */
export function RadioGroup({
  name,
  legend,
  value,
  onChange,
  options,
  disabled = false,
}: {
  name: string;
  legend?: string;
  value: string;
  onChange: (next: string) => void;
  options: readonly RadioOption[];
  disabled?: boolean;
}): ReactNode {
  return (
    <fieldset className="flex flex-col gap-2" disabled={disabled}>
      {legend === undefined ? null : (
        <legend className="mb-1 block h-4 truncate text-xs leading-4 font-medium text-muted">
          {legend}
        </legend>
      )}
      {options.map((option) => {
        const off = disabled || option.disabled === true;
        return (
          <label
            key={option.value}
            className={ROW + (off ? " cursor-not-allowed opacity-40" : " cursor-pointer")}
          >
            <input
              type="radio"
              name={name}
              value={option.value}
              checked={value === option.value}
              disabled={off}
              onChange={(event) => onChange(event.target.value)}
              className={BOX}
            />
            <span className="min-w-0">
              {option.label}
              {option.hint === undefined ? null : (
                <span className="mt-0.5 block text-xs text-subtle">{option.hint}</span>
              )}
            </span>
          </label>
        );
      })}
    </fieldset>
  );
}
