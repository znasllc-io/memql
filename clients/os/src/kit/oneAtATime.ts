import { useEffect, useRef, useState } from "react";

// ONE PRESS AT A TIME -- the kit's guard against the same act started twice.
//
// On the Nov 19 call twenty quick clicks on "Save to my Library" made eighteen
// copies. José: even at a thousand clicks a second "it should only count as
// one click", and "that's definitely a bug that needs to be fixed, not just
// here". So the rule lives here, once, for every button the kit draws
// (`Button`, `IconButton` and everything built on them, `AddButton` and the
// `ActionBar`'s acts included), and a page opts in by doing nothing more than
// RETURNING THE PROMISE of the act it starts:
//
//   <IconButton label="Save to my Library" onClick={() => save(file)} />
//
// While that promise is pending, every further press of the same button is
// ignored -- taken by a ref, synchronously, so presses that arrive before React
// draws the busy state are ignored too. An act that returns nothing (opening a
// menu, a dialog, the desk) is not held: there is nothing to wait for.
//
// What the person sees (Jesús, Nov 20, draft B of item 7):
//   - a LABELLED button says its busy word ("Saving…") while the act runs,
//     then its done word with a tick ("Saved ✓") for two seconds, still taking
//     no press, then reads as it did;
//   - an ICON-ONLY button just dims while the act runs.
//
// THE HALF SECOND. Either way a press holds its button for at least
// MIN_HOLD_MS, however fast the act finishes. Holding only while the promise
// was pending let a burst through: a download or save on a local cluster
// finishes in a few hundredths of a second, so every spammed click found the
// button free again, the dim never showed, and Jesús's Downloads folder filled
// with copies (Nov 20 2026: "It should be half a second of delay"). A slower
// act simply holds for as long as it runs.
//
// A done word is a claim that the act happened, so it is shown only for a
// promise that resolved to something other than `false`. An act that reports
// its own failure in place (most of them do, beside the button) resolves
// `false`; one that rejects is treated the same way, and its rejection is
// passed on unchanged, exactly as it surfaced before the kit looked at it.
// Either way the button goes straight back to how it was.
//
// The engine holds the same line on its side (librarySaveSharedCopy refuses
// the same save while one runs), so an AI agent with no button is held to it
// too; this guard is what keeps the person's own extra clicks from ever being
// sent.

/** How long a done word stands, in milliseconds. */
export const DONE_MS = 2000;

/** The shortest a press holds its button, in milliseconds, however fast its act finishes. */
export const MIN_HOLD_MS = 500;

export type PressPhase = "idle" | "busy" | "done";

function isPromise(value: unknown): value is PromiseLike<unknown> {
  return typeof (value as { then?: unknown } | null)?.then === "function";
}

/**
 * The guard one button holds. `press(run, sayDone)` runs the act unless one is
 * already running (or its done word still stands); `phase` is what to draw.
 */
export function useOneAtATime(): { phase: PressPhase; press: (run: () => unknown, sayDone: boolean) => void } {
  const [phase, setPhase] = useState<PressPhase>("idle");
  const held = useRef(false);
  const live = useRef(true);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
      if (timer.current !== null) clearTimeout(timer.current);
    };
  }, []);

  const settle = () => {
    held.current = false;
    if (live.current) setPhase("idle");
  };

  const press = (run: () => unknown, sayDone: boolean) => {
    if (held.current) return;
    const result = run();
    if (!isPromise(result)) return;
    held.current = true;
    setPhase("busy");
    const pressedAt = Date.now();
    // Moves on only once the press has held for its half second.
    const afterHold = (next: () => void) => {
      const wait = pressedAt + MIN_HOLD_MS - Date.now();
      if (wait <= 0) next();
      else timer.current = setTimeout(next, wait);
    };
    result.then(
      (outcome) => {
        if (!live.current) return;
        if (!sayDone || outcome === false) return afterHold(settle);
        afterHold(() => {
          if (!live.current) return;
          setPhase("done");
          timer.current = setTimeout(settle, DONE_MS);
        });
      },
      (err: unknown) => {
        afterHold(settle);
        throw err;
      },
    );
  };

  return { phase, press };
}
