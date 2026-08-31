import type { ReactNode } from "react";

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
  return (
    <>
      <label className="os-sr-only" htmlFor={id}>
        {label}
      </label>
      <select
        id={id}
        className="os-select"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        {children}
      </select>
    </>
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

export function Head({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="os-head">
      <h3 className="os-settings-title">{title}</h3>
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
