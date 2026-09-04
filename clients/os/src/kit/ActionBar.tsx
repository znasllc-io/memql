import type { ReactNode } from "react";

import { Button, type ButtonTone } from "./controls";

// ActionBar -- DESIGN.md rule 12, encoded (epic memql#4937).
//
// ===========================================================================
// ACTS FOLLOW THE STATE, IN ONE PLACE
// ===========================================================================
// A surface with a lifecycle carries ONE bar, pinned to the bottom edge of the
// window's content pane: the state in words on the left, the acts legal from
// that state on the right, at most three, primary last. Nothing that changes
// the thing's state lives anywhere else on the page.
//
// The rule exists because of what Deployables measured before it: on one
// deployable page, Pause sat at y=2412, Archive at y=2499, the Head's action
// at y=354, and "archive this source AND EVERY APP IT PRODUCED" -- the far
// more destructive one -- at y=885. Six controls read "Retry", carrying two
// different promises. Every one of those is a control somebody has to go
// looking for, in a place that says nothing about how dangerous it is.
//
// ===========================================================================
// AN ILLEGAL ACT IS ABSENT, NEVER DISABLED
// ===========================================================================
// This is the half that fixes a real bug rather than a layout. A draft
// deployable used to render an ENABLED "Archive this deployable" that the
// engine's status guard refuses -- archiving requires a prior status of
// `disabled`, and a draft had no control that could reach it. Six greyed-out
// buttons are six controls somebody has to read past to learn they are not for
// them; one enabled button the server refuses is worse, because they find out
// by being told no.
//
// So `acts` is computed from the state, and a state that does not offer an act
// does not render one. `kit`'s own contract is only that it draws what it is
// given -- the decision is the app's, and it is a PURE function there so it can
// be asserted without a DOM.
//
// ===========================================================================
// A GRID ROW, NOT position: fixed
// ===========================================================================
// The desk plate is CSS-transformed, which makes it the containing block for
// any fixed descendant -- so a `position: fixed` bar would anchor to the desk
// rather than to the window and travel with the wallpaper. The Logs app's jump
// pill records the same trap. The bar is the second row of the pane's grid;
// the content above it scrolls, and the bar does not.

/** One act the current state offers. */
export interface Act {
  /** The label, which is also the promise: an act keeps its name through the whole flow. */
  label: string;
  onAct: () => void;
  tone?: ButtonTone;
  /** Shown while this act is in flight. */
  busy?: boolean;
  /** An icon, drawn before the label. */
  icon?: ReactNode;
  /** Overrides the accessible name where the label alone is ambiguous out of context. */
  ariaLabel?: string;
}

/** The dot beside the state word, in the shell's own three-tone language. */
export type ActionBarTone = "live" | "paused" | "busy" | "none";

export interface ActionBarProps {
  /** The state, in the words a person uses -- "Published", never "live". */
  state: string;
  /**
   * What that state MEANS, in one clause. It is where the engine's own
   * distinctions are kept once the state word stops carrying them: an
   * unpublished deployable answers 503 rather than 404, and this is where that
   * is said.
   */
  detail?: string;
  tone?: ActionBarTone;
  /** At most three. Primary last, because that is where the eye lands. */
  acts?: readonly Act[];
  /** Rendered in place of the acts -- the typed confirmation a delete takes. */
  children?: ReactNode;
}

export function ActionBar({ state, detail, tone = "none", acts = [], children }: ActionBarProps) {
  // NOTHING HAPPENS WHERE NOTHING IS OFFERED. With no state to name and no act
  // to offer, the bar is absent rather than an empty band of chrome -- the
  // same rule the shell states about right-click.
  if (state === "" && acts.length === 0 && !children) return null;

  return (
    <div className="os-actbar" role="group" aria-label="What you can do with this">
      <div className="os-actbar-state">
        {tone === "none" ? null : <span className="os-actbar-dot" data-tone={tone} aria-hidden />}
        {state === "" ? null : <span className="os-actbar-word">{state}</span>}
        {detail ? <span className="os-actbar-detail">{detail}</span> : null}
      </div>
      <div className="os-actbar-acts">
        {children}
        {acts.map((act) => (
          <Button
            key={act.label}
            tone={act.tone}
            busy={act.busy}
            onClick={act.onAct}
            ariaLabel={act.ariaLabel}
          >
            {act.icon}
            {act.label}
          </Button>
        ))}
      </div>
    </div>
  );
}
