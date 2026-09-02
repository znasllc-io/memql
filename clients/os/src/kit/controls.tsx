import {
  Children,
  Fragment,
  isValidElement,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
// `Check` is aliased because this module already exports a control by that
// name -- the checkbox. One of the two has to say which it is, and the glyph
// is the one that is not part of the kit's vocabulary.
import { ArrowUpDown, Check as CheckGlyph, ChevronDown, Copy, Search, X } from "lucide-react";

// The OS's shared controls.
//
// ===========================================================================
// WHY THESE LIVE IN kit/ AND NOT IN THE APP THAT NEEDED THEM FIRST
// ===========================================================================
// Fleet was the first real app, and it arrived with fifty-five `os-fleet-*`
// classes -- among them a button, an input, a select, a panel, a chip, a
// checkbox and three different notice boxes. Every one of those is a control
// any app needs, so the next app would have written `os-users-button` beside
// them, and the shell would have drifted into one look per app while every
// individual app looked internally consistent. That is what "nobody went over
// the design" looks like from the inside: not a wrong colour, a second
// vocabulary.
//
// So the generic half is here, named for what it IS rather than where it was
// born, and the app keeps only what is genuinely about ITS subject -- a
// machine line, a routing record, a workspace card.
//
// The rule for adding to this file: a control earns a place here when a
// SECOND surface needs it. Promoting on the first use invents an abstraction
// from one example; waiting for the third means the second one has already
// forked.

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

export type ButtonTone = "quiet" | "primary" | "danger";

/**
 * One action.
 *
 * `tone` is about consequence, not emphasis: `primary` is the one thing this
 * surface is FOR, `danger` is a thing that cannot be undone, `quiet` is
 * everything else. A surface with two primaries has not decided what it is
 * for.
 */
export function Button({
  children,
  onClick,
  tone = "quiet",
  type = "button",
  disabled = false,
  busy = false,
  busyLabel,
  ariaLabel,
  ariaExpanded,
}: {
  children: ReactNode;
  onClick?: () => void;
  tone?: ButtonTone;
  type?: "button" | "submit";
  disabled?: boolean;
  busy?: boolean;
  busyLabel?: string;
  ariaLabel?: string;
  ariaExpanded?: boolean;
}) {
  return (
    <button
      type={type}
      className="os-button"
      data-tone={tone}
      // A busy control is disabled for its duration. Writes in this shell are
      // non-idempotent from the person's side -- a second revoke is a second
      // audit row, a second mint is a second credential -- so a double click
      // must not become two calls.
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      aria-label={ariaLabel}
      aria-expanded={ariaExpanded}
      onClick={onClick}
    >
      {busy && busyLabel ? busyLabel : children}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

export function Input({
  value,
  onChange,
  id,
  label,
  placeholder,
  disabled = false,
  onEnter,
}: {
  value: string;
  onChange: (next: string) => void;
  id: string;
  /** Always required, visually hidden by default. A control with no name is
   *  unreachable by anyone not using their eyes. */
  label: string;
  placeholder?: string;
  disabled?: boolean;
  onEnter?: () => void;
}) {
  return (
    <>
      <label className="os-sr-only" htmlFor={id}>
        {label}
      </label>
      <input
        id={id}
        className="os-input"
        value={value}
        disabled={disabled}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={
          onEnter
            ? (e) => {
                if (e.key !== "Enter") return;
                e.preventDefault();
                onEnter();
              }
            : undefined
        }
      />
    </>
  );
}

// ---------------------------------------------------------------------------
// The select
// ---------------------------------------------------------------------------
//
// THE OPEN LIST IS DRAWN BY THE SHELL, BECAUSE THE BROWSER'S CANNOT BE REACHED
// AT ALL.
//
// `Select` used to wrap a native `<select>`, and the CLOSED half of that was
// right: `appearance: none` drops the UA's chrome and a currentColor chevron
// keeps the control flush with the `Input` beside it in both modes and under
// every theme pack (DESIGN.md rule 5). The OPEN half was never ours. A native
// `<option>` list is painted by the browser and the platform, and NO rule in
// this project reaches it -- not its background, its radius, its font, its
// padding, its alignment, its hover colour. So the closed half spoke the
// shell's design language and the open half spoke Chrome's, which is exactly
// what a person saw the moment they clicked: a themed field opening a
// stranger's box, sitting off to one side of it.
//
// There is no styling fix for that, only not using it. The trigger is a
// button wearing the same `.os-select` field rule the select wore, and the
// list is a `role="listbox"` this file draws in the shell's own language.
//
// THE API IS THE CALL SITES' AND DID NOT MOVE. Twenty-two `<Select>`s across
// thirteen app files hand it `<option>` elements, so the options are read back
// OUT of `children` rather than taken as an `options={[...]}` prop. One edit
// improves every dropdown in the OS and not one app file changes, which is the
// whole reason a shared kit exists.
//
// THE LIST IS PORTALLED INTO document.body, AND THAT IS THE ALIGNMENT HALF
// RATHER THAN A RENDERING CONVENIENCE. `position: fixed` resolves against the
// viewport only while no ancestor establishes a containing block for it, and
// `backdrop-filter`, `transform`, `filter`, `perspective`, `contain` and
// `will-change` each establish one. `.os-window` carries `backdrop-filter` and
// the desk plates carry `transform` for paging, so a list left in the caller's
// subtree resolves against the WINDOW box and opens a title bar and a window
// edge away from the control that produced it -- which reads as arithmetic
// somebody got wrong, and is why every caller then invents a different rect to
// subtract. `chrome/ContextMenu.tsx` was rewritten for this exact trap and its
// file header is the long form of it. `test/kit/select.test.tsx` pins the
// structural half: the list must not be a descendant of the caller's subtree.
//
// The arithmetic is NOT `placeContextMenu`'s, and reusing it would have been
// wrong rather than tidy. That function places a box at a POINT and flips it
// through the click so the pointer keeps a corner of it; this one places a box
// against a BOX. `placeListbox` below is the anchored version, pure and tested
// on stubbed rects the same way.

/** One choice, read back out of the caller's `<option>` children. */
export interface SelectOption {
  value: string;
  label: string;
  disabled: boolean;
  /**
   * The `<optgroup label>` it sits under, or "" for none.
   *
   * GROUPS ARE READ AND NOT DRAWN. The walk keeps a grouped option, in order,
   * carrying its group name; no group HEADER is rendered. Nothing in this
   * shell uses `<optgroup>` today, and a heading style invented for zero call
   * sites is a second vocabulary nobody has looked at -- the field is here so
   * that the day a surface needs one, the walk is already right and only the
   * drawing is missing.
   */
  group: string;
}

/** Everything an `<option>` or `<optgroup>` element can hand the walk. */
interface OptionLikeProps {
  value?: string | number | readonly string[];
  disabled?: boolean;
  label?: string;
  children?: ReactNode;
}

/**
 * An element's text, the way HTML reads an option's label: every string and
 * number under it, concatenated with NOTHING between them.
 *
 * The separator is the part worth stating. Real call sites write
 * `{a.name || a.id}{a.status === "archived" ? " (archived)" : ""}` -- two
 * children whose join has to be "Acme (archived)", because the author already
 * wrote the space. Joining on " " would double it on every archived row.
 */
function textOf(node: ReactNode): string {
  if (typeof node === "string") return node;
  if (typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map((child) => textOf(child as ReactNode)).join("");
  if (isValidElement(node)) return textOf((node.props as OptionLikeProps).children);
  return "";
}

/**
 * The `<option>` children a caller passed, as data.
 *
 * `Children.toArray` does the flattening a real call site needs -- the array a
 * `.map()` returns, and the `null` a `{cond ? <option/> : null}` leaves behind
 * -- and fragments and `<optgroup>`s are walked here. Anything else among the
 * children (a stray string, an option somebody wrapped in a `<span>`) is
 * SKIPPED rather than guessed at: a select commits a value, and a thing that
 * is not an option has none.
 */
export function selectOptionsFrom(children: ReactNode): SelectOption[] {
  const options: SelectOption[] = [];
  walkOptions(children, "", options);
  return options;
}

function walkOptions(node: ReactNode, group: string, into: SelectOption[]): void {
  for (const child of Children.toArray(node)) {
    if (!isValidElement(child)) continue;
    const props = child.props as OptionLikeProps;
    if (child.type === Fragment) {
      walkOptions(props.children, group, into);
      continue;
    }
    if (child.type === "optgroup") {
      walkOptions(props.children, props.label ?? group, into);
      continue;
    }
    if (child.type !== "option") continue;
    const label = textOf(props.children).trim();
    // HTML's own rule: an option with no `value` is worth its own text.
    const value = props.value === undefined ? label : String(props.value);
    into.push({ value, label, disabled: props.disabled === true, group });
  }
}

/** How far the list sits off its trigger, and off a viewport edge, in px. */
const LIST_GAP = 4;
const LIST_EDGE_MARGIN = 8;

/** How long a type-ahead buffer stands before the next letter starts a new one. */
const TYPEAHEAD_RESET_MS = 1000;

export interface ListboxAnchor {
  anchorLeft: number;
  anchorTop: number;
  anchorBottom: number;
  anchorWidth: number;
  listWidth: number;
  listHeight: number;
  viewportWidth: number;
  viewportHeight: number;
}

export interface ListboxPlacement {
  left: number;
  top: number;
  minWidth: number;
  /** True when the list opened upwards. Carried so the CSS can say so. */
  above: boolean;
}

/**
 * Where a list of this size, opened from a trigger of this rect, should sit.
 *
 * ALIGNED TO A BOX, NOT FLIPPED THROUGH A POINT -- the whole difference from
 * `placeContextMenu`. A context menu opens at the cursor and has to keep the
 * cursor on a corner; this list belongs to a control already on screen, so its
 * left edge lines up with the control's and it is never narrower than it.
 * The two overflow cases then differ, and that is deliberate: running past the
 * RIGHT edge slides the list back inside (flipping would leave it hanging off
 * the side of the control it belongs to), while running past the BOTTOM flips
 * it above (there is nothing below the control to align to, and sliding up
 * would cover the control itself).
 */
export function placeListbox(at: ListboxAnchor): ListboxPlacement {
  const minWidth = Math.max(at.anchorWidth, at.listWidth);
  const rightmost = at.viewportWidth - LIST_EDGE_MARGIN - minWidth;
  const left = Math.max(LIST_EDGE_MARGIN, Math.min(at.anchorLeft, rightmost));

  const below = at.anchorBottom + LIST_GAP;
  const above = at.anchorTop - LIST_GAP - at.listHeight;
  const overflowsBelow = below + at.listHeight > at.viewportHeight - LIST_EDGE_MARGIN;
  // Flip only when there is genuinely room above. A list that fits on neither
  // side is clamped inside the margin instead -- the same last resort
  // `placeContextMenu` takes -- and its own `max-height` keeps that rare.
  const flip = overflowsBelow && above >= LIST_EDGE_MARGIN;
  const top = flip
    ? above
    : Math.max(
        LIST_EDGE_MARGIN,
        Math.min(below, at.viewportHeight - LIST_EDGE_MARGIN - at.listHeight),
      );
  return { left, top, minWidth, above: flip };
}

/**
 * The `scrollTop` that brings one item fully into view, and moves nothing else.
 *
 * `Element.scrollIntoView` is the obvious call and the wrong one: the list is
 * portalled to `document.body`, so asking the browser to scroll a node into
 * view can scroll the PAGE -- moving the surface underneath while somebody is
 * arrowing through a list. Writing `scrollTop` moves the list and only the
 * list. Pure, because jsdom lays nothing out and every rect this would be
 * measured from there is zero.
 */
export function listScrollTop(
  view: { scrollTop: number; viewHeight: number },
  item: { top: number; height: number },
): number {
  if (item.top < view.scrollTop) return item.top;
  const bottom = item.top + item.height;
  if (bottom > view.scrollTop + view.viewHeight) return bottom - view.viewHeight;
  return view.scrollTop;
}

export function Select({
  value,
  onChange,
  id,
  label,
  children,
}: {
  value: string;
  onChange: (next: string) => void;
  id: string;
  /** Always required, visually hidden. Names the control AND the open list. */
  label: string;
  /** `<option>` elements, exactly as a native select takes them. */
  children: ReactNode;
}) {
  const options = selectOptionsFrom(children);
  const [open, setOpen] = useState(false);
  const [anchor, setAnchor] = useState<{
    left: number;
    top: number;
    bottom: number;
    width: number;
  } | null>(null);
  const [placement, setPlacement] = useState<ListboxPlacement | null>(null);
  const [activeIndex, setActiveIndex] = useState(-1);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const typed = useRef({ buffer: "", at: 0 });

  const listId = `${id}-listbox`;
  const optionId = (index: number) => `${id}-option-${index}`;

  const selectedIndex = options.findIndex((option) => option.value === value);
  const selected = selectedIndex >= 0 ? options[selectedIndex] : undefined;
  // The options can shrink under a held index (a feed dropping a row while the
  // list is open), so what renders is always the clamped reading.
  const active = activeIndex >= 0 && activeIndex < options.length ? activeIndex : -1;

  /** The next selectable option in a direction, or `from` when there is none. */
  function stepIndex(from: number, direction: 1 | -1): number {
    for (let i = from + direction; i >= 0 && i < options.length; i += direction) {
      if (options[i]?.disabled !== true) return i;
    }
    return from;
  }

  function edgeIndex(direction: 1 | -1): number {
    const found = stepIndex(direction === 1 ? -1 : options.length, direction);
    return found >= 0 && found < options.length ? found : -1;
  }

  function openingIndex(): number {
    // A native select opens on the current value even when that option is
    // disabled -- it is still what the field holds, and arrowing off it finds
    // the next thing that can be chosen.
    return selectedIndex >= 0 ? selectedIndex : edgeIndex(1);
  }

  function openAt(index: number): void {
    const rect = triggerRef.current?.getBoundingClientRect();
    setAnchor(
      rect === undefined
        ? null
        : { left: rect.left, top: rect.top, bottom: rect.bottom, width: rect.width },
    );
    setActiveIndex(index);
    setOpen(true);
  }

  function close(): void {
    setOpen(false);
    setPlacement(null);
    setActiveIndex(-1);
    typed.current = { buffer: "", at: 0 };
  }

  /** Close and put focus back where it came from. Commits nothing. */
  function dismiss(): void {
    close();
    triggerRef.current?.focus();
  }

  function commit(index: number): void {
    const option = options[index];
    if (option === undefined || option.disabled) return;
    close();
    triggerRef.current?.focus();
    // NATIVE PARITY, AND IT EARNS ITS KEEP HERE. A `<select>` fires `change`
    // only when the value actually changes, and the handlers behind this
    // control are not all local state: one grants a cluster role, another
    // writes a recipient's subscription status and re-reads the roster.
    // Re-picking what is already chosen must not post either of those.
    if (option.value !== value) onChange(option.value);
  }

  /** Where a letter lands. Returns -1 when nothing starts with the buffer. */
  function typeAhead(char: string, from: number): number {
    const at = Date.now();
    const buffer = (at - typed.current.at > TYPEAHEAD_RESET_MS ? "" : typed.current.buffer) + char;
    typed.current = { buffer, at };
    // One letter pressed repeatedly CYCLES through the options starting with
    // it; a real word narrows in place from where the list already is. That is
    // what a select does, and the difference only shows on a list with several
    // "Se..." rows -- which is exactly the list somebody is type-ahead-ing.
    const repeat = [...buffer].every((c) => c.toLowerCase() === char.toLowerCase());
    const needle = (repeat ? char : buffer).toLowerCase();
    const start = repeat ? from + 1 : Math.max(from, 0);
    for (let step = 0; step < options.length; step += 1) {
      const i = (((start + step) % options.length) + options.length) % options.length;
      const option = options[i];
      if (option !== undefined && !option.disabled && option.label.toLowerCase().startsWith(needle)) {
        return i;
      }
    }
    return -1;
  }

  function onTriggerKeyDown(event: ReactKeyboardEvent<HTMLButtonElement>): void {
    const key = event.key;
    const typing = key.length === 1 && key !== " " && !event.ctrlKey && !event.metaKey && !event.altKey;

    if (!open) {
      if (key === "Enter" || key === " " || key === "ArrowDown" || key === "ArrowUp") {
        // Alt+ArrowDown lands here too, which is the platform's own spelling
        // of "open this" and costs nothing to honour.
        event.preventDefault();
        openAt(openingIndex());
      } else if (key === "Home" || key === "End") {
        event.preventDefault();
        commit(edgeIndex(key === "Home" ? 1 : -1));
      } else if (typing) {
        event.preventDefault();
        // A LETTER OPENS RATHER THAN COMMITTING, the one place this control
        // deliberately parts with the native one. A `<select>` writes the
        // value on the keystroke, and here that keystroke can be a role grant
        // or a subscription change with no list on screen to say what was
        // picked. Opening on the match shows the person what they are about to
        // choose and costs them one Enter.
        const found = typeAhead(key, openingIndex());
        openAt(found >= 0 ? found : openingIndex());
      }
      return;
    }

    if (key === "Escape") {
      event.preventDefault();
      // A portal bubbles through the REACT tree, so an Escape handled here
      // would otherwise also reach the window this select sits in and close it.
      event.stopPropagation();
      dismiss();
    } else if (key === "Enter" || key === " ") {
      event.preventDefault();
      commit(active);
    } else if (key === "ArrowDown") {
      event.preventDefault();
      setActiveIndex(stepIndex(active, 1));
    } else if (key === "ArrowUp") {
      event.preventDefault();
      setActiveIndex(stepIndex(active, -1));
    } else if (key === "Home") {
      event.preventDefault();
      setActiveIndex(edgeIndex(1));
    } else if (key === "End") {
      event.preventDefault();
      setActiveIndex(edgeIndex(-1));
    } else if (key === "Tab") {
      // NOT prevented. The list closes, nothing is committed, and focus moves
      // on to the next control exactly as it would from any other field.
      close();
    } else if (typing) {
      event.preventDefault();
      const found = typeAhead(key, active);
      if (found >= 0) setActiveIndex(found);
    }
  }

  // Placed after every render and BEFORE the browser paints, so the list is
  // never seen at the unclamped point first; the updater returning the
  // PREVIOUS object when nothing moved is what stops that looping. Scroll is
  // listened for in the CAPTURE phase because the surface that moves is
  // usually an inner one -- a window's own scrolling body -- and scroll does
  // not bubble.
  useLayoutEffect(() => {
    if (!open) return undefined;
    const measure = (): void => {
      const trigger = triggerRef.current;
      const list = listRef.current;
      if (trigger === null || list === null) return;
      const triggerRect = trigger.getBoundingClientRect();
      const listRect = list.getBoundingClientRect();
      const next = placeListbox({
        anchorLeft: triggerRect.left,
        anchorTop: triggerRect.top,
        anchorBottom: triggerRect.bottom,
        anchorWidth: triggerRect.width,
        listWidth: listRect.width,
        listHeight: listRect.height,
        viewportWidth: window.innerWidth,
        viewportHeight: window.innerHeight,
      });
      setPlacement((prev) =>
        prev !== null &&
        prev.left === next.left &&
        prev.top === next.top &&
        prev.minWidth === next.minWidth &&
        prev.above === next.above
          ? prev
          : next,
      );
    };
    measure();
    window.addEventListener("scroll", measure, true);
    window.addEventListener("resize", measure);
    return () => {
      window.removeEventListener("scroll", measure, true);
      window.removeEventListener("resize", measure);
    };
  });

  useEffect(() => {
    if (!open) return undefined;
    // The list lives under `document.body`, so a listener on the shell root
    // would never see a pointerdown inside it. Capture on `window`, testing
    // containment against the PORTALLED node -- the same node either way.
    const onPointerDown = (event: PointerEvent): void => {
      const target = event.target as Node | null;
      if (target === null) return;
      if (listRef.current?.contains(target) === true) return;
      if (triggerRef.current?.contains(target) === true) return;
      close();
    };
    window.addEventListener("pointerdown", onPointerDown, true);
    return () => window.removeEventListener("pointerdown", onPointerDown, true);
  }, [open]);

  // Opening lands on the current value, so a long list must arrive scrolled to
  // it rather than at the top with the answer somewhere below the fold.
  useEffect(() => {
    if (!open || active < 0) return;
    const list = listRef.current;
    const item = list?.children[active];
    if (list === null || !(item instanceof HTMLElement)) return;
    list.scrollTop = listScrollTop(
      { scrollTop: list.scrollTop, viewHeight: list.clientHeight },
      { top: item.offsetTop, height: item.offsetHeight },
    );
  }, [open, active]);

  // NOTHING RENDERS BLANK. A value matching no option is usually an id whose
  // row has not arrived yet, so the id itself goes on screen, muted -- it says
  // WHICH value is unresolved, where a blank box says only that something is
  // wrong. An empty value with no empty option to name it reads as the em dash
  // every absent value in this shell reads as.
  const display = selected !== undefined ? selected.label : value === "" ? "--" : value;

  const place =
    placement ??
    (anchor === null
      ? { left: 0, top: 0, minWidth: 0, above: false }
      : { left: anchor.left, top: anchor.bottom + LIST_GAP, minWidth: anchor.width, above: false });

  return (
    <>
      <label className="os-sr-only" htmlFor={id}>
        {label}
      </label>
      <span className="os-select-wrap" data-open={open || undefined}>
        <button
          ref={triggerRef}
          id={id}
          type="button"
          // `combobox` rather than the default `button` role, for one concrete
          // reason: focus STAYS on the trigger while the list is open -- so
          // Tab leaves the control natively and Escape has somewhere to return
          // to -- which means the active option can only be announced through
          // `aria-activedescendant`, an attribute defined on composite roles
          // and not on `button`. It is also what the control IS: the ARIA
          // select-only combobox, whose current value a screen reader reads
          // from the trigger's own contents while the visually-hidden
          // `<label for>` names the field.
          role="combobox"
          className="os-select"
          aria-haspopup="listbox"
          aria-expanded={open}
          // Pointed at the list only while there is a list: `aria-controls`
          // naming an id that is not in the document is a dangling reference.
          aria-controls={open ? listId : undefined}
          aria-activedescendant={open && active >= 0 ? optionId(active) : undefined}
          onClick={() => (open ? dismiss() : openAt(openingIndex()))}
          onKeyDown={onTriggerKeyDown}
        >
          <span className="os-select-value" data-placeholder={selected === undefined || undefined}>
            {display}
          </span>
        </button>
        <ChevronDown size={12} className="os-select-chevron" aria-hidden />
      </span>
      {open
        ? createPortal(
            <div
              ref={listRef}
              id={listId}
              role="listbox"
              aria-label={label}
              className="os-select-list"
              data-above={place.above || undefined}
              style={{ left: place.left, top: place.top, minWidth: place.minWidth }}
              onContextMenu={(event) => event.preventDefault()}
            >
              {options.length === 0 ? (
                // An empty list is still opened and still says so. A control
                // that does nothing when clicked reads as broken, and "there
                // is nothing here yet" is the account of itself that an absent
                // answer owes the person who asked for it.
                <p className="os-select-empty" role="presentation">
                  Nothing to choose here yet
                </p>
              ) : (
                options.map((option, index) => (
                  <div
                    key={`${index}:${option.value}`}
                    id={optionId(index)}
                    role="option"
                    aria-selected={option.value === value}
                    aria-disabled={option.disabled || undefined}
                    data-active={index === active || undefined}
                    className="os-select-option"
                    // Keeps focus on the trigger. A mousedown on a
                    // non-focusable node still blurs whatever held focus, and
                    // a list left open with focus on `body` answers no key.
                    onMouseDown={(event) => event.preventDefault()}
                    onClick={() => commit(index)}
                  >
                    <span className="os-select-check" aria-hidden>
                      {option.value === value ? <CheckGlyph size={12} /> : null}
                    </span>
                    <span className="os-select-option-label">{option.label}</span>
                  </div>
                ))
              )}
            </div>,
            document.body,
          )
        : null}
    </>
  );
}

/**
 * Rule 3: sort is not a button. A quiet text control on the list's own scope
 * line -- click swaps the order, the accessible name says what it is and what
 * a click does. The default order stays an app-settings preference; this
 * steers the session.
 */
export function SortControl({
  ascending,
  onToggle,
  descLabel = "Newest first",
  ascLabel = "Oldest first",
}: {
  ascending: boolean;
  onToggle: () => void;
  descLabel?: string;
  ascLabel?: string;
}) {
  const current = ascending ? ascLabel : descLabel;
  const other = ascending ? descLabel : ascLabel;
  return (
    <button
      type="button"
      className="os-sort"
      aria-label={`Sorted ${current.toLowerCase()} -- switch to ${other.toLowerCase()}`}
      onClick={onToggle}
    >
      <ArrowUpDown size={11} aria-hidden />
      {current}
    </button>
  );
}

/** One active constraint shown while Refine is collapsed; removable in place. */
export interface RefineChip {
  id: string;
  label: string;
  onRemove: () => void;
}

/**
 * Rule 2: filters are questions, not furniture. The section's search and
 * facet controls live behind this one affordance on the Head line: collapsed,
 * it is a single quiet control (carrying the live search text once one is
 * typed); open, it is an anchored panel with the search first and the app's
 * facet controls after; active facets render as removable chips beside it
 * while collapsed, so the state of the question never hides.
 *
 * A disclosure, not a modal: Escape and clicking elsewhere collapse it, and
 * nothing underneath is blocked while it is open.
 */
export function Refine({
  search,
  onSearch,
  placeholder = "Search",
  chips = [],
  label,
  children,
}: {
  search: string;
  onSearch: (next: string) => void;
  placeholder?: string;
  chips?: readonly RefineChip[];
  /** Names the whole affordance for assistive tech ("Refine files"). */
  label: string;
  children?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      if (rootRef.current && event.target instanceof Node && !rootRef.current.contains(event.target)) {
        setOpen(false);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  // The search field takes focus when the panel opens -- the affordance is
  // search-shaped, so the first keystroke must land in the search.
  useEffect(() => {
    if (open) rootRef.current?.querySelector("input")?.focus();
  }, [open]);

  return (
    <div ref={rootRef} className="os-refine" role="group" aria-label={label}>
      {chips.map((chip) => (
        <span key={chip.id} className="os-chip os-chip-editable" data-tone="accent">
          {chip.label}
          <button
            type="button"
            className="os-chip-remove"
            aria-label={`Remove ${chip.label}`}
            onClick={chip.onRemove}
          >
            <X size={10} aria-hidden />
          </button>
        </span>
      ))}
      <button
        type="button"
        className="os-refine-open"
        aria-expanded={open}
        aria-label={label}
        data-active={search !== "" || undefined}
        onClick={() => setOpen((v) => !v)}
      >
        <Search size={13} aria-hidden />
        <span className="os-refine-open-text">{search !== "" ? search : placeholder}</span>
      </button>
      {open ? (
        <div className="os-refine-panel">
          <Input
            id={`refine-search-${label.toLowerCase().replace(/[^a-z0-9]+/g, "-")}`}
            label={placeholder}
            placeholder={placeholder}
            value={search}
            onChange={onSearch}
            onEnter={() => setOpen(false)}
          />
          {children}
        </div>
      ) : null}
    </div>
  );
}

export function Check({
  checked,
  onChange,
  children,
  disabled = false,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  children: ReactNode;
  disabled?: boolean;
}) {
  return (
    <label className="os-check">
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span>{children}</span>
    </label>
  );
}

export interface ChoiceOption {
  value: string;
  /** The value's own name. Rendered in the data voice -- these are enum
   *  members, and dressing them up as prose hides what to type elsewhere. */
  label: string;
  /** What it MEANS, in the reader's terms. "leastLoaded" does not say what it
   *  is least-loaded against. */
  description?: string;
}

/**
 * A vertical radio group where each option carries its own explanation.
 *
 * The horizontal `os-choice-row` (Settings' mode and theme pickers) is the
 * right control for options that need no explaining. This is its sibling for
 * options that do -- and it is a SIBLING rather than a native radio list on
 * purpose: the shell had exactly one selection language, the accent-bordered
 * pill, and a second surface reaching for the platform's own radio dot put
 * two appearances of one control in one window.
 */
export function ChoiceStack({
  name,
  value,
  onChange,
  options,
  label,
}: {
  name: string;
  value: string;
  onChange: (next: string) => void;
  options: readonly ChoiceOption[];
  label: string;
}) {
  return (
    <div className="os-choice-stack" role="radiogroup" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          role="radio"
          aria-checked={value === option.value}
          className="os-choice-card"
          onClick={() => onChange(option.value)}
        >
          <span className="os-choice-card-name os-mono">{option.label}</span>
          {option.description ? (
            <span className="os-choice-card-note">{option.description}</span>
          ) : null}
          {/* The name is enough for a screen reader; the group's own value is
              carried by aria-checked. */}
          <input type="radio" name={name} value={option.value} readOnly hidden />
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Containers and notices
// ---------------------------------------------------------------------------

export function Panel({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <section className="os-panel" aria-label={label}>
      {children}
    </section>
  );
}

export function Head({
  title,
  meta,
  children,
}: {
  title: string;
  /** A quiet fact beside the title -- a count, a scope note. Muted, tabular. */
  meta?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="os-head">
      <h3 className="os-settings-title">{title}</h3>
      {meta !== undefined && meta !== null ? <span className="os-head-meta">{meta}</span> : null}
      {children ? <div className="os-head-actions">{children}</div> : null}
    </div>
  );
}

export function Subhead({ children }: { children: ReactNode }) {
  return <h4 className="os-subhead">{children}</h4>;
}

export function FormRow({ children }: { children: ReactNode }) {
  return <div className="os-form-row">{children}</div>;
}

/**
 * A field with its name ABOVE it, rather than only inside it as a placeholder.
 *
 * The kit's `Input` carries a visually-hidden label, which is right for a
 * control whose purpose is obvious from what surrounds it -- a search box, a
 * rename field beside the thing being renamed. It is not enough for a FORM: a
 * column of boxes reading "shop" and "Storefront" in grey tells a sighted
 * person nothing about which is the name and which the label, and the moment
 * they type, the only explanation they had disappears.
 *
 * `aria-hidden` on the visible text, because `Input` has already given the
 * control its accessible name: without it a screen reader would announce the
 * same field twice.
 *
 * PROMOTED FROM apps/deployables (epic memql#4800), where it was local with a
 * note saying it earns a place here the day a second form needs one. The
 * Accounts app is three forms -- create, edit, first-run -- so this is that
 * day. `.os-deploy-field` survives as a CSS alias, because the shared
 * BEHAVIOUR is what had to move.
 */
export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="os-form-field">
      <span className="os-form-field-label" aria-hidden>
        {label}
      </span>
      <FormRow>{children}</FormRow>
    </div>
  );
}

export type NoticeTone = "info" | "warn" | "error";

/**
 * Something the surface needs to say, in place.
 *
 * ONE component with a tone, rather than the three near-identical boxes this
 * replaces. They differed only in colour and were reached for by feel, which
 * is how a warning and an error end up looking the same on one screen and
 * different on the next.
 *
 * NEVER A TOAST. A refusal here is usually the server's own sentence and
 * belongs beside the control that produced it, where the person can read it
 * while looking at what they were doing. A toast moves that sentence
 * somewhere else on a timer, and someone who looked away has lost the only
 * account of what happened.
 */
export function Notice({
  tone = "info",
  sentence,
  next,
  detail,
  children,
}: {
  tone?: NoticeTone;
  /** What happened, in the surface's own voice. */
  sentence?: ReactNode;
  /** What is true now, so the reader knows whether to retry or go and look. */
  next?: ReactNode;
  /** The server's reason, verbatim and in the data voice. A sentence we
   *  compose says what we THINK went wrong, which is the thing being checked. */
  detail?: string;
  children?: ReactNode;
}) {
  return (
    <div className="os-notice" data-tone={tone} role={tone === "error" ? "alert" : undefined}>
      {sentence ? <p className="os-notice-line">{sentence}</p> : null}
      {children}
      {next ? <p className="os-caption">{next}</p> : null}
      {detail ? <p className="os-notice-detail os-mono">{detail}</p> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/**
 * One line in a live list.
 *
 * An icon, a name, whatever the surface wants to say quietly in between, and a
 * state cluster on the trailing edge. Fleet wrote this as `.os-machine`; the
 * note in the stylesheet's app-local section says the day a second app wants
 * one is the day it moves up here rather than being copied sideways, and the
 * Users app is that second app.
 *
 * `current` is the row's own liveness -- online for a machine, active for a
 * person -- and it is what takes the row from muted to full ink, so a list
 * reads its own state before anybody has read a word of it. `dim` is the
 * other axis: still true, no longer live (revoked, deactivated). They are
 * independent, because a deactivated account can still have a live session.
 *
 * `onOpen` is what makes it a button. A row with no `onOpen` renders as a
 * plain line -- not a button with nothing behind it, which is a control that
 * announces itself to a screen reader and then does nothing.
 */
export function Row({
  icon,
  name,
  children,
  state,
  current = false,
  dim = false,
  onOpen,
  open,
}: {
  icon?: ReactNode;
  name: ReactNode;
  /** Quiet facts between the name and the state cluster. */
  children?: ReactNode;
  /** The trailing cluster: freshness, dots, chips, the arrival tick. */
  state?: ReactNode;
  current?: boolean;
  dim?: boolean;
  onOpen?: () => void;
  open?: boolean;
}) {
  const body = (
    <>
      {icon}
      <span className="os-row-name">{name}</span>
      {children}
      {state ? <span className="os-row-state">{state}</span> : null}
    </>
  );
  if (!onOpen) {
    return (
      <div className="os-row" data-current={current || undefined} data-dim={dim || undefined}>
        {body}
      </div>
    );
  }
  return (
    <button
      type="button"
      className="os-row"
      data-clickable
      data-current={current || undefined}
      data-dim={dim || undefined}
      aria-expanded={open}
      onClick={onOpen}
    >
      {body}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Data display
// ---------------------------------------------------------------------------

export function Facts({ children }: { children: ReactNode }) {
  return <dl className="os-facts">{children}</dl>;
}

/** One labelled value. An absent value is an em dash, never blank: a blank
 *  cell is indistinguishable from a cell that failed to render. */
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

/**
 * A single-line value that can be copied whole.
 *
 * THE LINE IS TRUNCATED, WHICH IS WHAT MAKES THE BUTTON LOAD-BEARING. An
 * artifact id is one unbreakable word wider than any panel it lands in, so the
 * value ellipsizes and the whole of it lives on `title` and in the clipboard.
 * That is the difference from the Domains panel's record parts, where the value
 * is entirely on screen and a clipboard refusal costs nothing because selecting
 * it by hand is still available: here a refusal that said nothing would leave
 * somebody with no way at all to get the value out. So it is reported in place
 * -- a copy button that lies is worse than no copy button.
 *
 * The confirmation decays after a couple of seconds; the REFUSAL does not. A
 * refusal is a standing fact rather than a phase (the Ask voice rule), and it
 * stands until the next attempt says otherwise.
 *
 * The button holds its space at all times and only its INK arrives, on hover or
 * on focus. Adding it to the flow on hover would shift the line out from under
 * the pointer reaching for it. Where there is no hover to reveal it with
 * (`@media (hover: none)`) it simply stands, so a touch reader is never left
 * with a value they cannot take.
 */
export function CopyValue({
  value,
  /** Names what the button copies: "Id" gives "Copy Id". */
  label,
}: {
  value: string;
  label: string;
}) {
  const [state, setState] = useState<"idle" | "copied" | "refused" | "unavailable">("idle");
  const decay = useRef<number | null>(null);

  useEffect(
    () => () => {
      if (decay.current !== null) window.clearTimeout(decay.current);
    },
    [],
  );

  // A blank value has nothing to copy, so it reads as the em dash every absent
  // value in this shell reads as -- never a button that announces itself to a
  // screen reader and then does nothing (the `Row` rule, one control up).
  if (value.trim() === "") return <>--</>;

  async function copy(): Promise<void> {
    if (decay.current !== null) window.clearTimeout(decay.current);
    const clipboard = globalThis.navigator?.clipboard;
    if (!clipboard) {
      setState("unavailable");
      return;
    }
    try {
      await clipboard.writeText(value);
      setState("copied");
      decay.current = window.setTimeout(() => setState("idle"), 2000);
    } catch {
      setState("refused");
    }
  }

  const copied = state === "copied";
  return (
    <span className="os-copyvalue">
      <span className="os-copyvalue-line">
        <span className="os-copyvalue-text" title={value}>
          {value}
        </span>
        <button
          type="button"
          className="os-copyvalue-copy"
          // The confirmation is pinned visible for its whole life: it fires on
          // a click, and a pointer that has moved on by then would otherwise
          // fade the only answer the person gets.
          data-copied={copied || undefined}
          aria-label={copied ? "Copied" : `Copy ${label}`}
          onClick={() => void copy()}
        >
          {copied ? <CheckGlyph size={12} aria-hidden /> : <Copy size={12} aria-hidden />}
        </button>
      </span>
      {state === "refused" || state === "unavailable" ? (
        <span className="os-copyvalue-note">
          {state === "unavailable"
            ? "This browser offers no clipboard -- nothing was copied."
            : "The browser refused the copy -- nothing was copied."}
        </span>
      ) : null}
    </span>
  );
}

export type ChipTone = "neutral" | "accent" | "muted";

export function Chip({
  children,
  tone = "neutral",
  title,
}: {
  children: ReactNode;
  tone?: ChipTone;
  title?: string;
}) {
  return (
    <span className="os-chip" data-tone={tone} title={title}>
      {children}
    </span>
  );
}

export function Chips({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <div className="os-chips" role="list" aria-label={label}>
      {children}
    </div>
  );
}
