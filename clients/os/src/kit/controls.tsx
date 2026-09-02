import { useEffect, useRef, useState, type ReactNode } from "react";
import { ArrowUpDown, ChevronDown, Search, X } from "lucide-react";

// The OS's shared controls.
//
// ===========================================================================
// WHY THESE LIVE IN kit/ AND NOT IN THE APP THAT NEEDED THEM FIRST
// ===========================================================================
// Fleet was the first real app, and it arrived with fifty-five `os-fleet-*`
// classes -- among them a button, an input, a select, a panel, a chip, a
// checkbox and three different notice boxes. Every one of those is a control
// any app needs, so the next app would have written `os-users-button` beside
// them, and the shell would have drifted into one look per app while every
// individual app looked internally consistent. That is what "nobody went over
// the design" looks like from the inside: not a wrong colour, a second
// vocabulary.
//
// So the generic half is here, named for what it IS rather than where it was
// born, and the app keeps only what is genuinely about ITS subject -- a
// machine line, a routing record, a workspace card.
//
// The rule for adding to this file: a control earns a place here when a
// SECOND surface needs it. Promoting on the first use invents an abstraction
// from one example; waiting for the third means the second one has already
// forked.

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

export type ButtonTone = "quiet" | "primary" | "danger";

/**
 * One action.
 *
 * `tone` is about consequence, not emphasis: `primary` is the one thing this
 * surface is FOR, `danger` is a thing that cannot be undone, `quiet` is
 * everything else. A surface with two primaries has not decided what it is
 * for.
 */
export function Button({
  children,
  onClick,
  tone = "quiet",
  type = "button",
  disabled = false,
  busy = false,
  busyLabel,
  ariaLabel,
  ariaExpanded,
}: {
  children: ReactNode;
  onClick?: () => void;
  tone?: ButtonTone;
  type?: "button" | "submit";
  disabled?: boolean;
  busy?: boolean;
  busyLabel?: string;
  ariaLabel?: string;
  ariaExpanded?: boolean;
}) {
  return (
    <button
      type={type}
      className="os-button"
      data-tone={tone}
      // A busy control is disabled for its duration. Writes in this shell are
      // non-idempotent from the person's side -- a second revoke is a second
      // audit row, a second mint is a second credential -- so a double click
      // must not become two calls.
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      aria-label={ariaLabel}
      aria-expanded={ariaExpanded}
      onClick={onClick}
    >
      {busy && busyLabel ? busyLabel : children}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

export function Input({
  value,
  onChange,
  id,
  label,
  placeholder,
  disabled = false,
  onEnter,
}: {
  value: string;
  onChange: (next: string) => void;
  id: string;
  /** Always required, visually hidden by default. A control with no name is
   *  unreachable by anyone not using their eyes. */
  label: string;
  placeholder?: string;
  disabled?: boolean;
  onEnter?: () => void;
}) {
  return (
    <>
      <label className="os-sr-only" htmlFor={id}>
        {label}
      </label>
      <input
        id={id}
        className="os-input"
        value={value}
        disabled={disabled}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={
          onEnter
            ? (e) => {
                if (e.key !== "Enter") return;
                e.preventDefault();
                onEnter();
              }
            : undefined
        }
      />
    </>
  );
}

export function Select({
  value,
  onChange,
  id,
  label,
  children,
}: {
  value: string;
  onChange: (next: string) => void;
  id: string;
  label: string;
  children: ReactNode;
}) {
  // The wrapper draws the chevron (DESIGN.md rule 5): `appearance: none`
  // drops the UA's own chrome so a select sits flush with the inputs beside
  // it, and a glyph in currentColor keeps it right in both modes and under
  // every theme pack -- a background-image chevron would be one fixed colour.
  return (
    <>
      <label className="os-sr-only" htmlFor={id}>
        {label}
      </label>
      <span className="os-select-wrap">
        <select
          id={id}
          className="os-select"
          value={value}
          onChange={(e) => onChange(e.target.value)}
        >
          {children}
        </select>
        <ChevronDown size={12} className="os-select-chevron" aria-hidden />
      </span>
    </>
  );
}

/**
 * Rule 3: sort is not a button. A quiet text control on the list's own scope
 * line -- click swaps the order, the accessible name says what it is and what
 * a click does. The default order stays an app-settings preference; this
 * steers the session.
 */
export function SortControl({
  ascending,
  onToggle,
  descLabel = "Newest first",
  ascLabel = "Oldest first",
}: {
  ascending: boolean;
  onToggle: () => void;
  descLabel?: string;
  ascLabel?: string;
}) {
  const current = ascending ? ascLabel : descLabel;
  const other = ascending ? descLabel : ascLabel;
  return (
    <button
      type="button"
      className="os-sort"
      aria-label={`Sorted ${current.toLowerCase()} -- switch to ${other.toLowerCase()}`}
      onClick={onToggle}
    >
      <ArrowUpDown size={11} aria-hidden />
      {current}
    </button>
  );
}

/** One active constraint shown while Refine is collapsed; removable in place. */
export interface RefineChip {
  id: string;
  label: string;
  onRemove: () => void;
}

/**
 * Rule 2: filters are questions, not furniture. The section's search and
 * facet controls live behind this one affordance on the Head line: collapsed,
 * it is a single quiet control (carrying the live search text once one is
 * typed); open, it is an anchored panel with the search first and the app's
 * facet controls after; active facets render as removable chips beside it
 * while collapsed, so the state of the question never hides.
 *
 * A disclosure, not a modal: Escape and clicking elsewhere collapse it, and
 * nothing underneath is blocked while it is open.
 */
export function Refine({
  search,
  onSearch,
  placeholder = "Search",
  chips = [],
  label,
  children,
}: {
  search: string;
  onSearch: (next: string) => void;
  placeholder?: string;
  chips?: readonly RefineChip[];
  /** Names the whole affordance for assistive tech ("Refine files"). */
  label: string;
  children?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      if (rootRef.current && event.target instanceof Node && !rootRef.current.contains(event.target)) {
        setOpen(false);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  // The search field takes focus when the panel opens -- the affordance is
  // search-shaped, so the first keystroke must land in the search.
  useEffect(() => {
    if (open) rootRef.current?.querySelector("input")?.focus();
  }, [open]);

  return (
    <div ref={rootRef} className="os-refine" role="group" aria-label={label}>
      {chips.map((chip) => (
        <span key={chip.id} className="os-chip os-chip-editable" data-tone="accent">
          {chip.label}
          <button
            type="button"
            className="os-chip-remove"
            aria-label={`Remove ${chip.label}`}
            onClick={chip.onRemove}
          >
            <X size={10} aria-hidden />
          </button>
        </span>
      ))}
      <button
        type="button"
        className="os-refine-open"
        aria-expanded={open}
        aria-label={label}
        data-active={search !== "" || undefined}
        onClick={() => setOpen((v) => !v)}
      >
        <Search size={13} aria-hidden />
        <span className="os-refine-open-text">{search !== "" ? search : placeholder}</span>
      </button>
      {open ? (
        <div className="os-refine-panel">
          <Input
            id={`refine-search-${label.toLowerCase().replace(/[^a-z0-9]+/g, "-")}`}
            label={placeholder}
            placeholder={placeholder}
            value={search}
            onChange={onSearch}
            onEnter={() => setOpen(false)}
          />
          {children}
        </div>
      ) : null}
    </div>
  );
}

export function Check({
  checked,
  onChange,
  children,
  disabled = false,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  children: ReactNode;
  disabled?: boolean;
}) {
  return (
    <label className="os-check">
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span>{children}</span>
    </label>
  );
}

export interface ChoiceOption {
  value: string;
  /** The value's own name. Rendered in the data voice -- these are enum
   *  members, and dressing them up as prose hides what to type elsewhere. */
  label: string;
  /** What it MEANS, in the reader's terms. "leastLoaded" does not say what it
   *  is least-loaded against. */
  description?: string;
}

/**
 * A vertical radio group where each option carries its own explanation.
 *
 * The horizontal `os-choice-row` (Settings' mode and theme pickers) is the
 * right control for options that need no explaining. This is its sibling for
 * options that do -- and it is a SIBLING rather than a native radio list on
 * purpose: the shell had exactly one selection language, the accent-bordered
 * pill, and a second surface reaching for the platform's own radio dot put
 * two appearances of one control in one window.
 */
export function ChoiceStack({
  name,
  value,
  onChange,
  options,
  label,
}: {
  name: string;
  value: string;
  onChange: (next: string) => void;
  options: readonly ChoiceOption[];
  label: string;
}) {
  return (
    <div className="os-choice-stack" role="radiogroup" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          role="radio"
          aria-checked={value === option.value}
          className="os-choice-card"
          onClick={() => onChange(option.value)}
        >
          <span className="os-choice-card-name os-mono">{option.label}</span>
          {option.description ? (
            <span className="os-choice-card-note">{option.description}</span>
          ) : null}
          {/* The name is enough for a screen reader; the group's own value is
              carried by aria-checked. */}
          <input type="radio" name={name} value={option.value} readOnly hidden />
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Containers and notices
// ---------------------------------------------------------------------------

export function Panel({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <section className="os-panel" aria-label={label}>
      {children}
    </section>
  );
}

export function Head({
  title,
  meta,
  children,
}: {
  title: string;
  /** A quiet fact beside the title -- a count, a scope note. Muted, tabular. */
  meta?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="os-head">
      <h3 className="os-settings-title">{title}</h3>
      {meta !== undefined && meta !== null ? <span className="os-head-meta">{meta}</span> : null}
      {children ? <div className="os-head-actions">{children}</div> : null}
    </div>
  );
}

export function Subhead({ children }: { children: ReactNode }) {
  return <h4 className="os-subhead">{children}</h4>;
}

export function FormRow({ children }: { children: ReactNode }) {
  return <div className="os-form-row">{children}</div>;
}

/**
 * A field with its name ABOVE it, rather than only inside it as a placeholder.
 *
 * The kit's `Input` carries a visually-hidden label, which is right for a
 * control whose purpose is obvious from what surrounds it -- a search box, a
 * rename field beside the thing being renamed. It is not enough for a FORM: a
 * column of boxes reading "shop" and "Storefront" in grey tells a sighted
 * person nothing about which is the name and which the label, and the moment
 * they type, the only explanation they had disappears.
 *
 * `aria-hidden` on the visible text, because `Input` has already given the
 * control its accessible name: without it a screen reader would announce the
 * same field twice.
 *
 * PROMOTED FROM apps/deployables (epic memql#4800), where it was local with a
 * note saying it earns a place here the day a second form needs one. The
 * Accounts app is three forms -- create, edit, first-run -- so this is that
 * day. `.os-deploy-field` survives as a CSS alias, because the shared
 * BEHAVIOUR is what had to move.
 */
export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="os-form-field">
      <span className="os-form-field-label" aria-hidden>
        {label}
      </span>
      <FormRow>{children}</FormRow>
    </div>
  );
}

export type NoticeTone = "info" | "warn" | "error";

/**
 * Something the surface needs to say, in place.
 *
 * ONE component with a tone, rather than the three near-identical boxes this
 * replaces. They differed only in colour and were reached for by feel, which
 * is how a warning and an error end up looking the same on one screen and
 * different on the next.
 *
 * NEVER A TOAST. A refusal here is usually the server's own sentence and
 * belongs beside the control that produced it, where the person can read it
 * while looking at what they were doing. A toast moves that sentence
 * somewhere else on a timer, and someone who looked away has lost the only
 * account of what happened.
 */
export function Notice({
  tone = "info",
  sentence,
  next,
  detail,
  children,
}: {
  tone?: NoticeTone;
  /** What happened, in the surface's own voice. */
  sentence?: ReactNode;
  /** What is true now, so the reader knows whether to retry or go and look. */
  next?: ReactNode;
  /** The server's reason, verbatim and in the data voice. A sentence we
   *  compose says what we THINK went wrong, which is the thing being checked. */
  detail?: string;
  children?: ReactNode;
}) {
  return (
    <div className="os-notice" data-tone={tone} role={tone === "error" ? "alert" : undefined}>
      {sentence ? <p className="os-notice-line">{sentence}</p> : null}
      {children}
      {next ? <p className="os-caption">{next}</p> : null}
      {detail ? <p className="os-notice-detail os-mono">{detail}</p> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/**
 * One line in a live list.
 *
 * An icon, a name, whatever the surface wants to say quietly in between, and a
 * state cluster on the trailing edge. Fleet wrote this as `.os-machine`; the
 * note in the stylesheet's app-local section says the day a second app wants
 * one is the day it moves up here rather than being copied sideways, and the
 * Users app is that second app.
 *
 * `current` is the row's own liveness -- online for a machine, active for a
 * person -- and it is what takes the row from muted to full ink, so a list
 * reads its own state before anybody has read a word of it. `dim` is the
 * other axis: still true, no longer live (revoked, deactivated). They are
 * independent, because a deactivated account can still have a live session.
 *
 * `onOpen` is what makes it a button. A row with no `onOpen` renders as a
 * plain line -- not a button with nothing behind it, which is a control that
 * announces itself to a screen reader and then does nothing.
 */
export function Row({
  icon,
  name,
  children,
  state,
  current = false,
  dim = false,
  onOpen,
  open,
}: {
  icon?: ReactNode;
  name: ReactNode;
  /** Quiet facts between the name and the state cluster. */
  children?: ReactNode;
  /** The trailing cluster: freshness, dots, chips, the arrival tick. */
  state?: ReactNode;
  current?: boolean;
  dim?: boolean;
  onOpen?: () => void;
  open?: boolean;
}) {
  const body = (
    <>
      {icon}
      <span className="os-row-name">{name}</span>
      {children}
      {state ? <span className="os-row-state">{state}</span> : null}
    </>
  );
  if (!onOpen) {
    return (
      <div className="os-row" data-current={current || undefined} data-dim={dim || undefined}>
        {body}
      </div>
    );
  }
  return (
    <button
      type="button"
      className="os-row"
      data-clickable
      data-current={current || undefined}
      data-dim={dim || undefined}
      aria-expanded={open}
      onClick={onOpen}
    >
      {body}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Data display
// ---------------------------------------------------------------------------

export function Facts({ children }: { children: ReactNode }) {
  return <dl className="os-facts">{children}</dl>;
}

/** One labelled value. An absent value is an em dash, never blank: a blank
 *  cell is indistinguishable from a cell that failed to render. */
export function Fact({
  label,
  value,
  mono = false,
  title,
}: {
  label: string;
  value: ReactNode;
  mono?: boolean;
  title?: string;
}) {
  const empty = value === "" || value === null || value === undefined;
  return (
    <>
      <dt>{label}</dt>
      <dd className={mono ? "os-mono" : undefined} title={title}>
        {empty ? "--" : value}
      </dd>
    </>
  );
}

export type ChipTone = "neutral" | "accent" | "muted";

export function Chip({
  children,
  tone = "neutral",
  title,
}: {
  children: ReactNode;
  tone?: ChipTone;
  title?: string;
}) {
  return (
    <span className="os-chip" data-tone={tone} title={title}>
      {children}
    </span>
  );
}

export function Chips({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <div className="os-chips" role="list" aria-label={label}>
      {children}
    </div>
  );
}
