import { useCallback, useEffect, useMemo, useState, type KeyboardEvent, type ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { Dialog, TextInput } from "../ui";
import { rank } from "./matcher";
import { usePaletteEntries, type PaletteEntry } from "./sources";

// The command palette (memql#4656). Cmd+K, or Ctrl+K.
//
// ===========================================================================
// THIS IS WHAT MAKES THE SEVEN-ITEM RAIL SAFE
// ===========================================================================
// Cutting seventeen rail rows to seven is only an improvement if nothing
// became unreachable. It did not: everything that left the rail is here, one
// keystroke away and reachable by typing three letters of its name -- which
// is faster than the rail row ever was, and does not cost a permanent slot.
//
// Without this, the restructure would be hiding things. With it, the rail is
// for the seven places you go daily and the palette is for the hundred you go
// occasionally, which is the split those two controls are actually good at.
//
// ===========================================================================
// KEYBOARD-FIRST, AND A REAL COMBOBOX
// ===========================================================================
// Type to filter, arrows to move, Enter to go, Escape to close. The input
// carries role=combobox with aria-activedescendant pointing at the highlighted
// option, because a bare input over a list of divs is a control a screen
// reader cannot follow -- and this control's entire audience is people using
// the keyboard.
//
// It renders in the existing Dialog vocabulary, which is where the focus trap,
// Escape and the inert-behind come from (native <dialog>.showModal).

const OPTION_PREFIX = "palette-option-";

// Enough to fill the list without turning a three-letter query into a scroll
// exercise. The cap is announced rather than silent -- see the footer.
const SHOWN = 40;

export function CommandPalette(): ReactNode {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [cursor, setCursor] = useState(0);
  const navigate = useNavigate();
  const location = useLocation();
  const entries = usePaletteEntries();

  // Cmd+K / Ctrl+K from anywhere, including inside a form field: it is a
  // modifier chord, so it cannot collide with typing.
  useEffect(() => {
    function onKey(event: globalThis.KeyboardEvent): void {
      if (event.key.toLowerCase() !== "k" || !(event.metaKey || event.ctrlKey)) return;
      event.preventDefault();
      setOpen((was) => !was);
    }
    globalThis.addEventListener?.("keydown", onKey);
    return () => globalThis.removeEventListener?.("keydown", onKey);
  }, []);

  // A navigation closes it. Not strictly reachable from the palette's own
  // Enter (which closes first), but a link inside the shell would otherwise
  // leave it open over a page the person just went to.
  useEffect(() => {
    setOpen(false);
  }, [location.pathname]);

  const matches = useMemo(() => rank(query, entries).slice(0, SHOWN), [query, entries]);
  const active = matches[Math.min(cursor, Math.max(0, matches.length - 1))];

  const close = useCallback(() => {
    setOpen(false);
    setQuery("");
    setCursor(0);
  }, []);

  const go = useCallback(
    (entry: PaletteEntry | undefined) => {
      if (entry === undefined) return;
      close();
      navigate(entry.to);
    },
    [close, navigate],
  );

  function onKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setCursor((c) => Math.min(c + 1, matches.length - 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setCursor((c) => Math.max(c - 1, 0));
    } else if (event.key === "Enter") {
      event.preventDefault();
      go(active);
    } else if (event.key === "Escape") {
      // Handled here as well as by the native <dialog>, because ui/Dialog
      // falls back to the `open` ATTRIBUTE where showModal is unavailable --
      // and in that mode Escape does nothing at all. A palette you cannot
      // dismiss from the keyboard is the wrong thing to ship to whichever
      // browser lands on the fallback.
      event.preventDefault();
      close();
    }
  }

  if (!open) return null;

  return (
    <Dialog open onClose={close} labelledBy="palette-label" size="xl">
      <div className="flex flex-col">
        <div className="border-b border-line p-3">
          <span id="palette-label" className="sr-only">
            Search everywhere you can go
          </span>
          <TextInput
            value={query}
            onChange={(next) => {
              setQuery(next);
              // Back to the top on every keystroke: the best match is first,
              // and a cursor left at row nine would commit whatever happened
              // to land there.
              setCursor(0);
            }}
            placeholder="Go to a page, a view, a concept…"
            ariaLabel="Search everywhere you can go"
            autoFocus
            onKeyDown={onKeyDown}
            combobox={{
              listId: "palette-list",
              ...(active === undefined ? {} : { activeId: OPTION_PREFIX + active.id }),
            }}
          />
        </div>

        <ul
          id="palette-list"
          role="listbox"
          aria-label="Results"
          className="max-h-96 overflow-y-auto p-1"
        >
          {matches.length === 0 ? (
            <li className="px-3 py-6 text-center text-sm text-subtle">
              Nothing matches “{query}”.
            </li>
          ) : (
            matches.map((entry, index) => (
              <li key={entry.id}>
                <button
                  type="button"
                  id={OPTION_PREFIX + entry.id}
                  role="option"
                  aria-selected={entry === active}
                  // The pointer moves the SELECTION rather than acting on its
                  // own, so the keyboard and the mouse never disagree about
                  // what Enter would do.
                  onMouseEnter={() => setCursor(index)}
                  onClick={() => go(entry)}
                  className={
                    "motion-wash flex w-full items-baseline gap-3 rounded px-3 py-1.5 text-left text-sm " +
                    (entry === active ? "bg-accent-subtle text-fg" : "text-muted hover:text-fg")
                  }
                >
                  <span className="min-w-0 truncate font-medium">{entry.label}</span>
                  {entry.hint === undefined ? null : (
                    <span className="min-w-0 truncate text-xs text-subtle">{entry.hint}</span>
                  )}
                  <span className="ml-auto shrink-0 text-xs text-subtle">{entry.group}</span>
                </button>
              </li>
            ))
          )}
        </ul>

        <p className="border-t border-line px-3 py-2 text-xs text-subtle">
          {/* The cap is stated rather than silent: a list that stopped at
              forty without saying so reads as "that is everything". */}
          {matches.length === SHOWN ? `First ${SHOWN} matches. ` : ""}
          Arrows to move, Enter to go, Escape to close.
        </p>
      </div>
    </Dialog>
  );
}
