import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

import { ChevronDown } from "./icons";

// The form vocabulary: one inset style for every text-shaped input, and one
// Field wrapper that owns the label / hint / error lines. Before this file
// the portal carried two Fields and three input recipes that disagreed on
// background, radius and padding; a form should not reveal which page it was
// written on.
//
// Inputs take (value, onChange: string) rather than the raw event -- every
// existing call site already worked at that level, and the event is noise to
// a form that only wants the text.
//
// -----------------------------------------------------------------------------
// WHY THE HEIGHT IS A TOKEN AND THE PADDING IS NOT (memql#4503/#4504)
// -----------------------------------------------------------------------------
//
// This file used to say `px-3 py-2 text-sm` and let the height fall out of it:
// ~38px for an input, while Button sm landed on ~34px and Button xs on ~26px.
// Nothing was wrong with any one of those numbers. What was wrong is that a
// form row is precisely where two of them meet, so every row in the portal was
// built out of controls that could not line up -- and a native <select> was
// worse than merely different, because it adds its own per-platform chrome on
// top of whatever padding it is given, so the same class string measured
// differently on Linux and macOS.
//
// So the vertical size is now stated once, as `h-control` (--memql-control-h in
// brand/tokens.css, shared with the identity pages), and the padding here is
// HORIZONTAL ONLY. Give a line control vertical padding again and it stops
// being the same height as the button beside it, which is the whole defect.

// The inset LOOK -- border, ground, text colour, disabled treatment. No
// vertical metric: the two recipes below add it, differently, on purpose.
const INSET =
  "w-full rounded border border-line bg-surface px-3 text-sm text-fg " +
  "placeholder:text-subtle disabled:cursor-not-allowed disabled:opacity-40";

// A control that occupies exactly one line: input, select. Sits on the control
// line so anything else on that line -- another field, a Button sm -- agrees.
const INSET_LINE = INSET + " h-control";

// A control whose height is its content: textarea. It keeps the vertical
// padding the line controls traded away, because there is no single line for
// it to sit on.
const INSET_BLOCK = INSET + " py-2";

// The label line is a FIXED box, and that is what makes two Fields side by side
// start their controls at the same y.
//
// The alternative -- letting the label size itself -- is what produced the
// reported bug together with `items-end`: a one-line label beside a two-line
// one, or a field with a hint beside one without, pushed its neighbour's
// control off the line. Top-alignment (see FormRow) fixes the hint half;
// this fixes the label half.
//
// `leading-4` is stated rather than inherited: --memql-text-xs declares a font
// size and no line-height, so `text-xs` alone leaves the line box to whatever
// the cascade says, which is not a number this layout can be built on.
const LABEL_LINE = "block h-4 truncate text-xs leading-4 font-medium text-muted";

// The offset a Field's control sits at inside the Field: the label line plus
// the column gap. FormActions reproduces it so its buttons top-align with the
// controls beside them -- exported so the two cannot drift.
export const FIELD_LABEL_OFFSET = "h-4";

/**
 * useTruncationTitle sets a native tooltip on a label ONLY when the text is
 * actually clipped.
 *
 * Setting `title` unconditionally would be simpler and is the obvious move,
 * but it puts a tooltip duplicating visible text on every label in the portal
 * -- noise for a pointer user and, worse, a second announcement of the same
 * words for a screen reader. Measuring means the tooltip exists exactly where
 * it adds something: the label the reader cannot fully see.
 */
function useTruncationTitle(text: string): {
  ref: React.RefObject<HTMLSpanElement | null>;
  title: string | undefined;
} {
  const ref = useRef<HTMLSpanElement>(null);
  const [clipped, setClipped] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (el === null) return;
    setClipped(el.scrollWidth > el.clientWidth);
  }, [text]);
  return { ref, title: clipped ? text : undefined };
}

export function Field({
  label,
  hint,
  error,
  grow = false,
  children,
}: {
  label: string;
  hint?: string;
  error?: string;
  // Let the field flex inside a wrapping form row.
  grow?: boolean;
  children: ReactNode;
}): ReactNode {
  const { ref, title } = useTruncationTitle(label);
  return (
    <label className={"flex flex-col gap-1 " + (grow ? "min-w-48 flex-1" : "")}>
      <span ref={ref} className={LABEL_LINE} {...(title === undefined ? {} : { title })}>
        {label}
      </span>
      {children}
      {hint === undefined ? null : <span className="text-xs text-subtle">{hint}</span>}
      {error === undefined ? null : (
        <span className="text-xs text-danger" role="alert">
          {error}
        </span>
      )}
    </label>
  );
}

export function TextInput({
  value,
  onChange,
  placeholder,
  type = "text",
  disabled = false,
  // A datalist id. Present so a combo box is still a TextInput rather than a
  // page's own copy of the inset recipe (which is what artifacts/ carried).
  list,
  // For an input with no visible <label> around it -- a search box whose
  // caption is the button beside it, say. A Field-wrapped input needs none.
  ariaLabel,
  // A field that DRIVES something with the keyboard: the command palette's
  // query box moves a selection with the arrows and commits it with Enter
  // (memql#4656). Optional, and absent everywhere else -- a form field's keys
  // belong to the form.
  onKeyDown,
  autoFocus = false,
  // The listbox this field steers, when it steers one. Emitting the ARIA trio
  // here rather than at the call site is what keeps the pattern correct in
  // the one place it is written down: a bare input over a list of options is
  // a control a screen reader cannot follow at all.
  combobox,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  type?: string;
  disabled?: boolean;
  list?: string;
  ariaLabel?: string;
  onKeyDown?: (event: KeyboardEvent<HTMLInputElement>) => void;
  autoFocus?: boolean;
  combobox?: { listId: string; activeId?: string };
}): ReactNode {
  return (
    <input
      type={type}
      value={value}
      disabled={disabled}
      {...(placeholder === undefined ? {} : { placeholder })}
      {...(list === undefined ? {} : { list })}
      {...(ariaLabel === undefined ? {} : { "aria-label": ariaLabel })}
      {...(onKeyDown === undefined ? {} : { onKeyDown })}
      {...(autoFocus ? { autoFocus: true } : {})}
      {...(combobox === undefined
        ? {}
        : {
            role: "combobox",
            "aria-expanded": true,
            "aria-controls": combobox.listId,
            ...(combobox.activeId === undefined
              ? {}
              : { "aria-activedescendant": combobox.activeId }),
          })}
      onChange={(event) => onChange(event.target.value)}
      className={INSET_LINE}
    />
  );
}

export function Textarea({
  value,
  onChange,
  placeholder,
  rows = 4,
  disabled = false,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  rows?: number;
  disabled?: boolean;
}): ReactNode {
  return (
    <textarea
      value={value}
      rows={rows}
      disabled={disabled}
      {...(placeholder === undefined ? {} : { placeholder })}
      onChange={(event) => onChange(event.target.value)}
      className={INSET_BLOCK + " font-mono"}
    />
  );
}

/**
 * Select -- a native <select> with the platform's own chrome removed.
 *
 * `appearance-none` is the load-bearing part. A native select does not merely
 * paint a different arrow per platform; it sizes itself, adding chrome on top
 * of the box the CSS asked for. That is why a select and an input carrying the
 * identical class string still measured differently on Linux and macOS, and no
 * amount of matching padding could have fixed it.
 *
 * With the appearance gone the arrow has to be drawn back, and it is drawn as
 * a real SVG element rather than a background-image data URI for one reason:
 * an SVG in a data URI is a separate document and cannot see `currentColor`,
 * so its arrow would be a hard-coded colour and would be wrong in one of the
 * two themes. The wrapper is `relative` so the chevron can be parked over the
 * right-hand padding that `pr-8` reserves for it.
 */
export function Select({
  value,
  onChange,
  disabled = false,
  ariaLabel,
  children,
}: {
  value: string;
  onChange: (next: string) => void;
  disabled?: boolean;
  ariaLabel?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <span className="relative block w-full">
      <select
        value={value}
        disabled={disabled}
        {...(ariaLabel === undefined ? {} : { "aria-label": ariaLabel })}
        onChange={(event) => onChange(event.target.value)}
        className={INSET_LINE + " appearance-none pr-8"}
      >
        {children}
      </select>
      <ChevronDown
        aria-hidden="true"
        className="pointer-events-none absolute top-1/2 right-2.5 size-4 -translate-y-1/2 text-muted"
      />
    </span>
  );
}
