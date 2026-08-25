import type { FormEventHandler, ReactNode } from "react";

import { FIELD_LABEL_OFFSET } from "./Field";

// -----------------------------------------------------------------------------
// THE ROW, AND WHY IT ALIGNS AT THE TOP (memql#4504)
// -----------------------------------------------------------------------------
//
// Every inline form row in the portal was hand-rolled as
// `flex flex-wrap items-end gap-3`, twenty-one times across seventeen files,
// and `items-end` is what the operator's screenshot is a picture of.
//
// Bottom-alignment means the tallest child decides where every other child's
// BOTTOM edge lands. A Field is label + control + optional hint, so a field
// carrying a hint is taller than one without -- and under `items-end` the
// hint-less field slides DOWN by the height of a hint it does not have. Its
// control leaves the line. A bare submit button, shortest child of all, slides
// down furthest and parks on the hint's baseline.
//
// Nothing is misaligned in a way any single file could show you. The Vendor
// select is correct, the API-key input is correct, the button is correct; the
// row is wrong.
//
// Top-alignment inverts the dependency: every child starts at the same y, so
// what hangs BELOW a control -- a hint, an error, a second line of label -- is
// free to differ without moving anything else. That is the whole fix, and it
// is why `items-end` is banned in form rows and enforced by
// portal_control_vocabulary_test.go rather than left as advice.

/**
 * FormRow lays labelled controls out in a line that wraps.
 *
 * Pass `onSubmit` when the row IS the form -- a search box, a one-line
 * "add a thing" row -- and it renders a <form> instead of a <div>, so the
 * page does not have to hand-roll one around it (and Enter submits, which is
 * what a person expects from a single-line form).
 */
export function FormRow({
  onSubmit,
  children,
}: {
  onSubmit?: FormEventHandler<HTMLFormElement>;
  children: ReactNode;
}): ReactNode {
  const className = "flex flex-wrap items-start gap-3";
  if (onSubmit === undefined) {
    return <div className={className}>{children}</div>;
  }
  return (
    <form onSubmit={onSubmit} className={className}>
      {children}
    </form>
  );
}

/**
 * FormActions holds the buttons that end a FormRow.
 *
 * A Field puts its control under a label line, so a bare button in the same
 * row starts a label's height too high. This reproduces that offset with an
 * empty spacer of exactly the same box -- FIELD_LABEL_OFFSET is imported from
 * Field rather than restated, because the one thing this component must never
 * do is disagree with the thing it is compensating for.
 *
 * The spacer is aria-hidden and empty: it is a layout artifact, and a screen
 * reader announcing a blank line before a button would be worse than the
 * misalignment it fixes.
 *
 * Buttons inside are expected at size `sm` -- Button's own default -- because
 * `sm` IS the control line. An `xs` button here would be back to the 26px
 * button beside a 36px field that this row exists to prevent.
 */
export function FormActions({ children }: { children: ReactNode }): ReactNode {
  return (
    <div className="flex flex-col gap-1">
      <span aria-hidden="true" className={"block " + FIELD_LABEL_OFFSET} />
      <div className="flex flex-wrap items-center gap-2">{children}</div>
    </div>
  );
}
