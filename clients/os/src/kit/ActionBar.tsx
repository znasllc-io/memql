import { useLayoutEffect, useRef, type ReactNode } from "react";

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
//
// ===========================================================================
// A CONFIRMATION IS NOT PRESSED BY THE CLICK THAT ASKED FOR IT
// ===========================================================================
// An act that asks first ("Send now", which names how many people it reaches)
// is answered by an act with THE SAME LABEL AT THE SAME PLACE: the primary
// stays on the right edge, so the confirmation stands exactly where the ask
// stood. A double-click is then two presses -- the first asks, the second
// lands on the confirmation and sends to everybody before the question was
// on screen long enough to be read. So an act that took the place of the one
// just pressed, under the same label, ignores presses for `CONFIRM_SETTLE_MS`
// after it appears. One guard, here, for every ask in the shell; the bar
// cannot tell an ask from any other act, and does not need to -- "the same
// promise, where the last press landed, in a bar that just changed" is the
// shape of every one of them. (It holds the way back too: a confirmation
// refused at once puts its ask back in that place, and the rest of the
// double-click does not open the question again.)
//
// What it leaves alone: an act whose bar did not change under it (a test sent
// twice, a save tried again after a refusal), a confirmation that stands
// somewhere else than its ask, which a double-click cannot reach, and a
// wizard's next step -- its `scope` (the open step) moved, so the Next that
// stands where the last Next stood is a new question.

/** How long an act that replaced the one just pressed, under its label and at its place, ignores presses. */
export const CONFIRM_SETTLE_MS = 400;

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
  /**
   * DRAWN AS TEXT, NOT AS A BUTTON. Two buttons side by side ask to be weighed
   * against each other, and on a wizard's floor they are not the same kind of
   * thing: one is what happens next, and the other is a way out of it. So the
   * bar carries ONE button -- the act -- and whatever stands beside it is a
   * clickable label: Cancel, Keep, Remove. Still a real `<button>`, with the
   * same focus ring and the same keyboard; only the dress is a label's.
   */
  text?: boolean;
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
  /**
   * A MEASURE OF A WAIT, and nothing else: "0:42", "checked 40s ago". A bar
   * that says "Waiting" with no figure beside it cannot answer the one thing
   * a person waiting wants to know -- whether this has been a moment or ten
   * minutes. Tabular, quiet, last in the state group. Drawn only when given.
   */
  meta?: string;
  /**
   * Announce the state when it changes. A wizard's bar is where "Waiting for
   * studio-mac-mini" becomes "Connected" with nobody touching anything, and a
   * change nobody caused is exactly what a polite live region is for. Off by
   * default: on a page whose state only moves when its reader acts, the act
   * already told them.
   */
  live?: boolean;
  /** At most three. Primary last, because that is where the eye lands. */
  acts?: readonly Act[];
  /** Rendered in place of the acts -- the typed confirmation a delete takes. */
  children?: ReactNode;
  /**
   * WHAT THE BAR IS ABOUT, when it moves through several things: a wizard's
   * open step. A confirmation is held only within one -- the next step's
   * forward act standing where the last one stood is a new question, not the
   * answer to the one just asked (`CONFIRM_SETTLE_MS`).
   */
  scope?: string;
}

/** The press the bar last acted on, and where it landed. */
interface Pressed {
  label: string;
  /** Counted from the right edge, where the primary is. */
  place: number;
  /** What the bar offered when it was pressed. */
  arrangement: string;
  scope: string;
}

export function ActionBar({ state, detail, tone = "none", meta, live = false, acts = [], children, scope = "" }: ActionBarProps) {
  // WHAT THE BAR OFFERS, and since when: the labels in order, and whether a
  // question stands beside them. A press that changes it (an ask) starts the
  // clock for whatever now stands where the press landed.
  const asking = children !== undefined && children !== null && children !== false;
  const arrangement = `${asking ? "?" : ""}${acts.map((act) => act.label).join("\u001f")}`;
  const shown = useRef({ arrangement, since: 0 });
  useLayoutEffect(() => {
    if (shown.current.arrangement !== arrangement) shown.current = { arrangement, since: Date.now() };
  }, [arrangement]);
  const pressed = useRef<Pressed | null>(null);

  // NOTHING HAPPENS WHERE NOTHING IS OFFERED. With no state to name and no act
  // to offer, the bar is absent rather than an empty band of chrome -- the
  // same rule the shell states about right-click.
  if (state === "" && acts.length === 0 && !children) return null;

  function press(act: Act, index: number) {
    const place = acts.length - 1 - index;
    const last = pressed.current;
    const settling =
      last !== null &&
      last.label === act.label &&
      last.place === place &&
      last.scope === scope &&
      last.arrangement !== shown.current.arrangement &&
      Date.now() - shown.current.since < CONFIRM_SETTLE_MS;
    // A press that lands too soon is the rest of the click that asked: it
    // neither confirms nor counts as a press of its own.
    if (settling) return;
    pressed.current = { label: act.label, place, arrangement: shown.current.arrangement, scope };
    act.onAct();
  }

  return (
    <div className="os-actbar" data-tone={tone} role="group" aria-label="What you can do with this">
      <div className="os-actbar-state" role={live ? "status" : undefined}>
        {tone === "none" ? null : <span className="os-actbar-dot" data-tone={tone} aria-hidden />}
        {state === "" ? null : <span className="os-actbar-word">{state}</span>}
        {detail ? <span className="os-actbar-detail">{detail}</span> : null}
        {meta ? <span className="os-actbar-meta">{meta}</span> : null}
      </div>
      <div className="os-actbar-acts">
        {children}
        {acts.map((act, index) =>
          act.text ? (
            <button
              key={act.label}
              type="button"
              className="os-actbar-text"
              data-tone={act.tone}
              disabled={act.busy}
              aria-busy={act.busy || undefined}
              aria-label={act.ariaLabel}
              onClick={() => press(act, index)}
            >
              {act.icon}
              {act.label}
            </button>
          ) : (
            <Button
              key={act.label}
              tone={act.tone}
              busy={act.busy}
              onClick={() => press(act, index)}
              ariaLabel={act.ariaLabel}
            >
              {act.icon}
              {act.label}
            </Button>
          ),
        )}
      </div>
    </div>
  );
}
