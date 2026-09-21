import type { ReactNode } from "react";
import { ChevronRight } from "lucide-react";

// THE RECORD LIST -- one way to draw a list of things that can be opened.
//
// PROMOTED FROM apps/deployables, where it was the Standalone list and lived
// in an app-local stylesheet scoped to `.deployable-overview` -- so nothing
// else COULD reuse it, which is most of why the shell grew thirty-five list,
// row and table class families while only three apps used `.os-row`. Fleet's
// machines are the second use, and the promotion rule is second use.
//
// WHAT IT IS, AGAINST `Row`. `Row` is a bordered pill: a border, a radius and
// a plate behind every line. This is FLAT -- no border but the hairline under
// it, no background until it is hovered -- with the name standing over a
// quieter second line. A long list of pills reads as a wall of boxes; a long
// list of these reads as a list. New lists are built with this one, and the
// apps still on `Row` move here as they are touched.
//
// ANATOMY, left to right, and what each part is FOR:
//
//   icon       what kind of thing it is, before a word is read
//   identity   the name, over ONE quieter line: where it is, or what it is
//   summary    0-3 quiet facts (`children`); the first thing to yield width
//   state      one word with a dot in the state's colour, never colour alone
//   chevron    it opens, and the whole row is the target
//
// A ROW NEVER HOLDS DETAIL -- it opens it. That is what keeps DESIGN.md rule 11
// true: a list and its detail never share a scroll column.

export type RecordTone = "accent" | "warn" | "muted";

/** The list's own box: it scopes the row styles and carries the density. */
export function RecordList({ children, label, density }: {
  children: ReactNode;
  /** Names the list to assistive tech when nothing visible already does. */
  label?: string;
  density?: "comfortable" | "compact";
}) {
  return <div className="os-record-list" aria-label={label} data-density={density}>{children}</div>;
}

export function RecordRow({ icon, name, secondary, children, state, tone = "muted", stateTitle, stateExtra, trailing, current = false, dim = false, open, onOpen, label }: {
  icon?: ReactNode;
  name: ReactNode;
  /** The one quieter line under the name: an address, a kind, a place. */
  secondary?: ReactNode;
  /** Quiet facts in the middle column. The first part of a row to give way. */
  children?: ReactNode;
  /** The state in ONE word. The dot beside it takes `tone`. */
  state?: ReactNode;
  tone?: RecordTone;
  /** A longer account of the state, for somebody who hovers the word. */
  stateTitle?: string;
  /** Marks that belong with the state: a "new" tick, a "Review needed". */
  stateExtra?: ReactNode;
  /** Sits between the state and the chevron: an attention marker. */
  trailing?: ReactNode;
  /** The row's own liveness -- live, online -- which takes it to full ink. */
  current?: boolean;
  /** Still true, no longer live: paused, archived, revoked. */
  dim?: boolean;
  open?: boolean;
  /** What makes it a button. Without it the row is a plain line. */
  onOpen?: () => void;
  /** The accessible name, when the visible name alone would not say enough. */
  label?: string;
}) {
  const body = <>
    <span className="os-record-icon">{icon}</span>
    <span className="os-record-identity">
      <span className="os-row-name">{name}</span>
      {secondary !== undefined && secondary !== null && secondary !== "" ? <span className="os-record-secondary">{secondary}</span> : null}
    </span>
    <span className="os-record-summary">{children}</span>
    <span className="os-record-state">
      {state !== undefined && state !== null && state !== "" ? <span className="os-record-status" data-tone={tone} title={stateTitle}>{state}</span> : null}
      {stateExtra}
    </span>
    {trailing}
    {onOpen ? <ChevronRight size={14} aria-hidden className="os-record-chevron" /> : <span className="os-record-chevron" aria-hidden />}
  </>;
  // `os-row` stays on the root: it is how a row is found, and it is what gives
  // `data-dim` its meaning. The pill it would otherwise draw is undone in the
  // stylesheet, exactly as the Deployables list always undid it.
  if (!onOpen) return <div className="os-row os-record-row" data-current={current || undefined} data-dim={dim || undefined}>{body}</div>;
  return (
    <button type="button" className="os-row os-record-row" aria-label={label} data-current={current || undefined} data-dim={dim || undefined} data-open={open || undefined} onClick={onOpen}>
      {body}
    </button>
  );
}
