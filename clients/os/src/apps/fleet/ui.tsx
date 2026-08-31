import type { ReactNode } from "react";

// The Fleet's shared atoms.
//
// ===========================================================================
// ERRORS RENDER IN SURFACE, NEVER AS TOASTS
// ===========================================================================
// Epic #4729's rule, and it is about custody rather than taste. A refusal
// here is nearly always the SERVER's sentence -- "the cluster refused the
// rename", a scope denial, a mint failure -- and it belongs beside the
// control that produced it, where the person can read it while looking at
// what they were doing. A toast takes that sentence somewhere else, on a
// timer, and a person who looked away has lost the only account of what
// happened. So every failure below is a node in the tree, dismissed by
// succeeding rather than by elapsing.
//
// `detail` is the server's own words and is rendered verbatim in the data
// voice. It is never substituted for: a sentence we compose says what we
// think went wrong, which is exactly the thing an operator is trying to
// check.

export function FleetError({
  sentence,
  detail,
  next,
}: {
  /** What did not happen, in the surface's own voice. */
  sentence: string;
  /** The server's reason, verbatim. Empty renders no line rather than an
   *  empty one -- "the cluster said: " with nothing after it reads as a
   *  truncation. */
  detail?: string;
  /** What is true now (typically "nothing was changed"), so a person knows
   *  whether to retry or to go and look. */
  next?: string;
}) {
  return (
    <div className="os-fleet-error" role="alert">
      <p className="os-fleet-error-line">{sentence}</p>
      {next ? <p className="os-caption">{next}</p> : null}
      {detail ? <p className="os-fleet-error-detail os-mono">{detail}</p> : null}
    </div>
  );
}

/** A label / value grid. `--` for an absent value is the one rendering an
 *  empty field gets: nothing is blank, because a blank cell is
 *  indistinguishable from a cell that failed to render. */
export function Facts({ children }: { children: ReactNode }) {
  return <dl className="os-facts">{children}</dl>;
}

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

/** One `key=value` label, marked with which side of the merge it came from. */
export function Chip({
  children,
  tone = "neutral",
  title,
}: {
  children: ReactNode;
  tone?: "neutral" | "operator" | "reported";
  title?: string;
}) {
  return (
    <span className="os-fleet-chip" data-tone={tone} title={title}>
      {children}
    </span>
  );
}

export function ChipRow({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <div className="os-fleet-chips" role="list" aria-label={label}>
      {children}
    </div>
  );
}

/** A section heading plus an optional action rail on the right. */
export function SectionHead({
  title,
  children,
}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="os-fleet-head">
      <h3 className="os-settings-title">{title}</h3>
      {children ? <div className="os-fleet-head-actions">{children}</div> : null}
    </div>
  );
}

/** A quiet in-surface panel: the add-machine flow, a detail body, an editor. */
export function Panel({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <section className="os-fleet-panel" aria-label={label}>
      {children}
    </section>
  );
}

export function Button({
  children,
  onClick,
  tone = "quiet",
  type = "button",
  disabled = false,
  busy = false,
  busyLabel,
  ariaLabel,
}: {
  children: ReactNode;
  onClick?: () => void;
  tone?: "quiet" | "primary" | "danger";
  type?: "button" | "submit";
  disabled?: boolean;
  busy?: boolean;
  busyLabel?: string;
  ariaLabel?: string;
}) {
  return (
    <button
      type={type}
      className="os-fleet-button"
      data-tone={tone}
      // A busy control is disabled for the duration: every write here is
      // non-idempotent from the operator's side (a second revoke is a second
      // audit row, a second mint is a second credential), so a double click
      // must not become two calls.
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      aria-label={ariaLabel}
      onClick={onClick}
    >
      {busy && busyLabel ? busyLabel : children}
    </button>
  );
}
