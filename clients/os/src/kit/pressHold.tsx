import { useCallback, useLayoutEffect, useMemo, useRef, type MouseEvent, type ReactNode } from "react";

// THE PRESS HOLD -- DESIGN.md rule 12, encoded once for the whole shell
// (memql#6033).
//
// A double-click is two presses, and the second lands on whatever now stands
// under the pointer. A question that takes the place of the act that asked it
// is exactly that: "Send now" answered by "Send now", "Revoke" answered by
// "Revoke it", an Archive that opens its own confirmation where it stood. Left
// alone, the second click answers a question nobody has read.
//
// So WHENEVER WHAT IS ON OFFER CHANGES, everything pressable in it takes no
// press at all for `CONFIRM_SETTLE_MS` -- wherever the press lands, by pointer
// or by keyboard, and whatever caused the change. A question is on screen long
// enough to be read before anything answers it, and nothing a person aimed at
// one thing is taken by another. The cost is a deliberate press within that
// moment of a change, which needs a second press.
//
// TWO PLACES USE IT, and they share this one implementation:
//
//   - `ActionBar` (kit/ActionBar): the bar's acts, and whatever it draws
//     beside them. Every bar, decided in the kit; nothing a page says turns it
//     on or off. A bar's FIRST acts are not held: a page whose record arrives
//     after it opens offered nothing there for a press to have been aimed at.
//   - `InPlaceAsk` (below): a question answered where the act stood, OUTSIDE
//     any bar -- a row's Revoke that turns into "Revoke it? / Keep it", a
//     panel's Disconnect that expands into Cancel and a confirmation. It is
//     held from the moment it appears, because the click that asked it was on
//     something else.
//
// HOW: what is on offer is read after every render and whenever the watched
// element changes on its own (a child that keeps its own state does not render
// its parent again); and every click inside the guarded element passes a
// capture-phase check first, which swallows it while the hold lasts -- the
// children's own buttons included, which never go through an act.
//
// What it leaves alone: something that did not change (a test sent twice, a
// save tried again); something that offered nothing for a moment (a write in
// flight takes its acts away) remembers what it last offered, so the same
// things back are not a change and other things are. Typing into a field it
// draws is not a change either; what the field asks is.

/** How long a bar, or a question answered in place, ignores presses after it changes. */
export const CONFIRM_SETTLE_MS = 400;

/** What counts as something to press. */
const PRESSABLE =
  "button, a[href], input, select, textarea, summary, [role='button'], [role='link'], [role='checkbox'], [role='switch'], [role='radio'], [role='menuitem'], [role='tab']";

/**
 * What nodes offer, read from the page: their words and each control in them,
 * by kind, dress and name. NOT a control's state -- whether it is busy or
 * enabled, or what has been typed into it -- which moves while the person is
 * acting on the very thing being offered.
 */
export function drawnBy(nodes: readonly Node[]): string {
  const parts: string[] = [];
  for (const node of nodes) {
    const words = (node.textContent ?? "").replace(/\s+/g, " ").trim();
    if (words !== "") parts.push(words);
    if (!(node instanceof Element)) continue;
    const controls = node.matches(PRESSABLE) ? [node, ...node.querySelectorAll(PRESSABLE)] : [...node.querySelectorAll(PRESSABLE)];
    for (const control of controls) {
      parts.push(
        ["tagName", "type", "role", "class", "data-tone", "aria-label", "href"]
          .map((name) => (name === "tagName" ? control.tagName : control.getAttribute(name) ?? ""))
          .join("\u001d"),
      );
    }
  }
  return parts.join("\u001f");
}

export interface PressHold {
  /**
   * A ref callback for the element to watch for changes it makes on its own:
   * a control that keeps its own state does not render the component that
   * owns the hold again.
   */
  watch: (element: HTMLElement | null) => void;
  /** The capture-phase guard: `onClickCapture` of the element that contains everything held. */
  onClickCapture: (event: MouseEvent<HTMLElement>) => void;
}

/**
 * The hold. `read` answers what is on offer NOW as a string (`""` for
 * nothing); the hold starts the moment that string changes, and ends
 * `CONFIRM_SETTLE_MS` later. It is called after every render, whenever the
 * watched element changes, and before every click is judged.
 *
 * `holdFirst`: whether the FIRST thing offered is held. A bar's first acts are
 * not (nothing stood there before them); a question that appeared where an act
 * stood is.
 */
export function usePressHold(read: () => string, { holdFirst = false }: { holdFirst?: boolean } = {}): PressHold {
  const reader = useRef(read);
  // What was last on offer, and since when. Offering nothing has no
  // arrangement to change, so it keeps the last one offered.
  const shown = useRef({ arrangement: "", since: 0 });

  const observe = useCallback(() => {
    const arrangement = reader.current();
    if (arrangement === "" || arrangement === shown.current.arrangement) return;
    shown.current = { arrangement, since: shown.current.arrangement === "" && !holdFirst ? 0 : Date.now() };
  }, [holdFirst]);

  useLayoutEffect(() => {
    reader.current = read;
    observe();
  });

  const watcher = useRef<MutationObserver | null>(null);
  const watch = useCallback(
    (element: HTMLElement | null) => {
      watcher.current?.disconnect();
      watcher.current = null;
      if (element === null || typeof MutationObserver === "undefined") return;
      watcher.current = new MutationObserver(observe);
      watcher.current.observe(element, {
        childList: true,
        subtree: true,
        characterData: true,
        attributes: true,
        attributeFilter: ["class", "data-tone", "aria-label", "type", "role", "href"],
      });
    },
    [observe],
  );

  /**
   * EVERY CLICK PASSES HERE FIRST, in the capture phase: an act's, a child's
   * own button, a link, and the click a browser makes of Enter or Space on any
   * of them. One that lands too soon after the change neither answers, nor
   * asks, nor counts as a press of its own.
   */
  const onClickCapture = useCallback(
    (event: MouseEvent<HTMLElement>) => {
      // A change made in this same moment is a change just now, whether or not
      // the watcher has been told yet.
      observe();
      const { since } = shown.current;
      // A clock set back is not a reason to hold until it catches up.
      const age = Date.now() - since;
      if (since === 0 || age < 0 || age >= CONFIRM_SETTLE_MS) return;
      event.preventDefault();
      event.stopPropagation();
    },
    [observe],
  );

  return useMemo(() => ({ watch, onClickCapture }), [watch, onClickCapture]);
}

/**
 * A QUESTION ANSWERED IN PLACE, outside any bar: the sentence and the buttons
 * that answer it, drawn where the act that asked it stood. A row's Revoke that
 * becomes "Revoke it / Keep it", a panel's Disconnect that expands into Cancel
 * and a confirmation, a dropped folder's "Archive / Cancel" (DESIGN.md rule 8).
 *
 * It holds presses on WHAT IT WRAPS: everything pressable inside takes no press
 * for `CONFIRM_SETTLE_MS` after it appears, and again after anything inside it
 * changes (a refusal drawn beside the buttons), so the second click of the
 * double-click that asked cannot answer it. What it does not wrap it does not
 * watch, so wrap what a press could be answering:
 *
 *   - THE BUTTONS, where the question cannot change while they stay. A row's
 *     Revoke, a panel's Disconnect: the ask is that row's or that panel's, it
 *     appears with its buttons and goes with them, and the sentence may stand
 *     beside the wrapper.
 *   - THE QUESTION TOO, where it CAN change while the same buttons stay. The
 *     dock's Bin, where a second file dropped on it changes the file under the
 *     same Archive; the Files notice, which another row or folder can take over.
 *     Wrap the whole Notice, so a new question holds its buttons again.
 *
 * Its element, class, role and label are the caller's: it adds the hold and
 * nothing else. An ask in a bar's own acts or children does not need it; the
 * bar holds those.
 */
export function InPlaceAsk({
  as: Tag = "div",
  className,
  role,
  "aria-label": ariaLabel,
  children,
}: {
  as?: "div" | "span";
  className?: string;
  role?: string;
  "aria-label"?: string;
  children: ReactNode;
}) {
  const root = useRef<HTMLElement | null>(null);
  const hold = usePressHold(() => (root.current === null ? "" : drawnBy([root.current])), { holdFirst: true });
  const attach = useCallback(
    (element: HTMLElement | null) => {
      root.current = element;
      hold.watch(element);
    },
    [hold.watch],
  );
  return (
    <Tag className={className} role={role} aria-label={ariaLabel} ref={attach} onClickCapture={hold.onClickCapture}>
      {children}
    </Tag>
  );
}
