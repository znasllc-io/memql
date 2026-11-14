import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";

import { AnchoredPicker } from "./AnchoredPicker";
import { listScrollTop } from "./controls";

// A FIELD THAT OPENS A LIST OF CHOICES JOINED TO IT, for choosing ONE thing out
// of many by its name: type to narrow, then choose. DESIGN.md has the popup
// joined to its field by `AnchoredPicker`, which escapes window scrolling and
// follows the field when its window moves. Promoted here on its second use --
// the apps to add to a group, a store's removal approver -- rather than spelled
// twice; the sharing editor's multi-select keeps its own, because it checks
// several and adds them together.
//
// Choosing is the act: a click, or Enter on the option the arrows reached. The
// list closes on a choice, on Escape, and when focus or a press goes elsewhere.
// The arrows' position is a wash on the option, the only state an option has:
// this picks one, it does not hold a selection.

export interface SearchChoice {
  id: string;
  /** What the choice is called, and the option's accessible name. */
  name: string;
  /** When short visible names repeat, include their distinguishing context. */
  ariaLabel?: string;
  /** One quieter line under the name: an address, a kind. */
  detail?: string;
}

export function SearchPicker({
  id,
  label,
  listLabel,
  choices,
  onChoose,
  emptyText,
  placeholder = "Search",
  autoFocus = false,
  onDismiss,
}: {
  id: string;
  /** The field's accessible name. */
  label: string;
  /** The list's accessible name. */
  listLabel: string;
  choices: readonly SearchChoice[];
  onChoose: (choice: SearchChoice) => void;
  /** What the list says when nothing matches what was typed. */
  emptyText: string;
  placeholder?: string;
  /** Open with focus in the field: a picker a press has just opened. */
  autoFocus?: boolean;
  /** The list closed with nothing chosen. */
  onDismiss?: () => void;
}) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const field = useRef<HTMLInputElement>(null);
  const owner = useRef<HTMLDivElement>(null);
  const popup = useRef<HTMLDivElement>(null);
  const listId = `${id}-choices`;

  const matches = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return choices;
    return choices.filter((c) => c.name.toLowerCase().includes(needle) || (c.detail ?? "").toLowerCase().includes(needle));
  }, [choices, query]);
  const activeIndex = active >= 0 && active < matches.length ? active : -1;

  useEffect(() => {
    if (autoFocus) field.current?.focus();
  }, [autoFocus]);

  // The option the arrows reached stays in view.
  useEffect(() => {
    const list = popup.current;
    const item = document.getElementById(`${listId}-${activeIndex}`);
    if (!open || !list || !item) return;
    list.scrollTop = listScrollTop({ scrollTop: list.scrollTop, viewHeight: list.clientHeight }, { top: item.offsetTop, height: item.offsetHeight });
  }, [open, activeIndex, listId]);

  function close(dismissed: boolean) {
    setOpen(false);
    setActive(-1);
    if (dismissed) onDismiss?.();
  }

  function choose(choice: SearchChoice) {
    setQuery("");
    close(false);
    onChoose(choice);
  }

  function onKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.nativeEvent.isComposing) return;
    const reached = open ? matches[activeIndex] : undefined;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      setOpen(true);
      if (matches.length > 0) setActive(event.key === "ArrowDown" ? Math.min(activeIndex + 1, matches.length - 1) : Math.max(activeIndex - 1, 0));
    } else if (event.key === "Enter" && reached !== undefined) {
      event.preventDefault();
      choose(reached);
    } else if (event.key === "Escape" && open) {
      event.preventDefault();
      event.stopPropagation();
      close(true);
    } else if (event.key === "Tab" && open) {
      close(true);
    }
  }

  return (
    <div ref={owner} className="os-search-picker">
      <label className="os-sr-only" htmlFor={id}>
        {label}
      </label>
      <input
        ref={field}
        id={id}
        className="os-input"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-activedescendant={open && activeIndex >= 0 ? `${listId}-${activeIndex}` : undefined}
        autoComplete="off"
        spellCheck={false}
        placeholder={placeholder}
        value={query}
        onChange={(event) => {
          setQuery(event.target.value);
          setOpen(true);
          setActive(0);
        }}
        onFocus={() => setOpen(true)}
        onClick={() => setOpen(true)}
        onKeyDown={onKeyDown}
      />
      {open ? (
        <AnchoredPicker anchor={field} owner={owner} popupRef={popup} onClose={() => close(true)}>
          <div id={listId} role="listbox" aria-label={listLabel}>
            {matches.map((choice, index) => (
              <div
                key={choice.id}
                id={`${listId}-${index}`}
                role="option"
                aria-selected={index === activeIndex}
                aria-label={choice.ariaLabel ?? choice.name}
                className="os-search-picker-option"
                data-active={index === activeIndex || undefined}
                onMouseDown={(event) => event.preventDefault()}
                onClick={() => choose(choice)}
              >
                <span className="os-search-picker-name">{choice.name}</span>
                {choice.detail ? <span className="os-search-picker-detail">{choice.detail}</span> : null}
              </div>
            ))}
          </div>
          {matches.length === 0 ? <p className="os-picker-empty">{emptyText}</p> : null}
        </AnchoredPicker>
      ) : null}
    </div>
  );
}
