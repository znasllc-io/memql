import { useCallback, useRef, type ReactNode } from "react";

import { Button, type ButtonTone } from "./controls";
import { drawnBy, usePressHold } from "./pressHold";

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
// A BAR THAT HAS JUST CHANGED IS NOT PRESSED BY THE CLICK THAT CHANGED IT
// ===========================================================================
// A double-click is two presses, and the second lands on whatever now stands
// under the pointer. The acts are right-aligned, so WHICH act that is depends
// on widths, not on order: under another label, or at another place in the
// list (a place counts acts, not pixels), it can still be exactly where the
// first press landed. And a bar changes under a press in every flow the shell
// has: an ask turns it into a question ("Send now" answered by "Not yet" and
// "Send now", "Cancel" by "Keep" and "Cancel campaign"); the way back from one
// puts the old acts back, with "Discard changes" where "Keep" stood; Pause
// turns into Resume; a refusal into Try again; a wizard's Next opens a step
// whose Next stands where the last one did.
//
// So WHENEVER WHAT THE BAR OFFERS CHANGES -- its acts (their labels, order and
// dress), what they are about (`scope`, a wizard's open step), or what the bar
// draws beside them (its children: a question, a typed confirmation, a
// refusal, or controls of their own) -- EVERYTHING PRESSABLE IN THE BAR takes
// no press at all for `CONFIRM_SETTLE_MS`, wherever the press lands, by
// pointer or by keyboard, and whatever caused the change: a question is on
// screen long enough to be read before anything answers it, and nothing a
// person aimed at one bar is taken by another. One guard, in the kit, for every
// bar in the shell; nothing a page says turns it on or off. The cost is a
// deliberate press within that moment of a change, which needs a second press.
//
// The guard is the kit's press hold (kit/pressHold), the one implementation a
// bar shares with every question answered in place outside one (`InPlaceAsk`);
// how it works, and what it leaves alone, is told there. What the bar adds is
// WHAT IT OFFERS: the acts from the props, the children from the page. A bar's
// FIRST acts are not held -- a page whose record arrives after it opens
// offered nothing there for a press to have been aimed at.

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
   * WHERE TEXT THAT CHANGES ON ITS OWN GOES: the measure of a wait ("0:42",
   * "checked 40s ago"), or a live figure. A bar that says "Waiting" with no
   * figure beside it cannot answer the one thing a person waiting wants to
   * know -- whether this has been a moment or ten minutes. It stands in the
   * state group, outside the row the press hold watches, so a figure that ticks
   * never holds the bar's presses; the same text beside the acts (`children`)
   * would, on every tick. Tabular, quiet, last in the state group. Drawn only
   * when given.
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
  /**
   * Drawn beside the acts: the question an ask turns the bar into, or the typed
   * confirmation a delete takes. What it draws is part of what the bar offers,
   * so a change to it holds every press (`CONFIRM_SETTLE_MS`): text that
   * changes on its own -- a ticking wait, a live figure -- goes in `meta`, not
   * here, where every tick would hold the bar's presses again.
   */
  children?: ReactNode;
  /**
   * WHAT THE BAR IS ABOUT, when it moves through several things: a wizard's
   * open step. A move to another one is a change of the bar even where the
   * acts keep their labels -- the Next that stands where the last Next stood
   * asks about another step -- so it holds presses like any change
   * (`CONFIRM_SETTLE_MS`).
   */
  scope?: string;
}

export function ActionBar({ state, detail, tone = "none", meta, live = false, acts = [], children, scope = "" }: ActionBarProps) {
  // WHAT THE ACTS OFFER, from the props: what they are about, and each act's
  // label and dress, in order.
  const offered = acts.length === 0 ? "" : `${scope}\u001e${acts.map((act) => `${act.label}\u001d${act.tone ?? ""}\u001d${act.text ? "text" : "button"}`).join("\u001f")}`;
  const row = useRef<HTMLDivElement | null>(null);

  // What the bar offers NOW -- the acts from the props, the children from the
  // page. The acts are the row's last nodes, one element each, after the
  // children. A bar offering nothing has no arrangement to change.
  const hold = usePressHold(() => {
    const nodes = row.current === null ? [] : Array.from(row.current.childNodes);
    const drawn = drawnBy(nodes.slice(0, Math.max(0, nodes.length - acts.length)));
    return offered === "" && drawn === "" ? "" : `${offered}\u001e${drawn}`;
  });

  // A CHILD THAT CHANGES ON ITS OWN does not render the bar again (a
  // confirmation row that keeps its own state), so the row is watched too.
  const attach = useCallback(
    (element: HTMLDivElement | null) => {
      row.current = element;
      hold.watch(element);
    },
    [hold.watch],
  );

  // NOTHING HAPPENS WHERE NOTHING IS OFFERED. With no state to name and no act
  // to offer, the bar is absent rather than an empty band of chrome -- the
  // same rule the shell states about right-click.
  if (state === "" && acts.length === 0 && !children) return null;

  return (
    <div className="os-actbar" data-tone={tone} role="group" aria-label="What you can do with this" onClickCapture={hold.onClickCapture}>
      <div className="os-actbar-state" role={live ? "status" : undefined}>
        {tone === "none" ? null : <span className="os-actbar-dot" data-tone={tone} aria-hidden />}
        {state === "" ? null : <span className="os-actbar-word">{state}</span>}
        {detail ? <span className="os-actbar-detail">{detail}</span> : null}
        {meta ? <span className="os-actbar-meta">{meta}</span> : null}
      </div>
      <div className="os-actbar-acts" ref={attach}>
        {children}
        {acts.map((act) =>
          act.text ? (
            <button
              key={act.label}
              type="button"
              className="os-actbar-text"
              data-tone={act.tone}
              disabled={act.busy}
              aria-busy={act.busy || undefined}
              aria-label={act.ariaLabel}
              onClick={() => act.onAct()}
            >
              {act.icon}
              {act.label}
            </button>
          ) : (
            <Button
              key={act.label}
              tone={act.tone}
              busy={act.busy}
              onClick={() => act.onAct()}
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
