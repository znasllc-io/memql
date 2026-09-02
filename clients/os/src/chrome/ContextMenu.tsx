import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

// One context menu for the whole shell: real menu semantics (role=menu,
// arrow keys, Esc, focus trap-in), positioned at the pointer, closed on
// any outside interaction. The desk and its items feed it their entries.
//
// IT RENDERS THROUGH A PORTAL INTO document.body, AND THAT PORTAL IS THE
// POSITIONING FIX -- not a rendering convenience. `position: fixed` resolves
// against the viewport only while no ancestor establishes a containing block
// for it, and `transform`, `filter`, `backdrop-filter`, `perspective`,
// `contain` and `will-change` each establish one. The glass block gives
// `backdrop-filter: blur(...)` to `.os-window`, `.os-dock`, `.os-widget`,
// `.os-ask-sheet`, `.os-launcher-panel` and `.os-themes`, and the desk plates
// carry `transform: translateX(...)` for paging -- so nearly every surface
// that can raise this menu is also a containing block for it. A menu opened
// on a file row inside a window resolved against the WINDOW box, and one
// opened on a dock pin resolved against the DOCK strip.
//
// THE FAILURE LOOKS LIKE ARITHMETIC, WHICH IS WHY IT SPREAD. The menu lands a
// window's left edge and a title bar away from the cursor, so the obvious fix
// is to subtract a rect the caller happens to have -- and every caller then
// subtracts a DIFFERENT rect, none of which is the containing block. In Files
// the rail menus subtracted `.os-files` (wrong by the window chrome above it)
// while the file rows subtracted nothing at all (wrong by the whole window),
// and each read as reasonable at its own call site. Moving the node out of
// every one of those subtrees makes `position: fixed` mean what it says, so
// there is nothing left for a caller to compensate for. Callers pass
// `event.clientX` / `event.clientY` and stop there.
//
// The two ancestors the portal leaves it with were checked and carry none of
// the six properties: `html` and `body` set only height, margin, font,
// colour, background and `overflow: hidden`. (`#root` and `.os-root` are off
// the path entirely now; `.os-root` never trapped this anyway -- it is
// `position: fixed; inset: 0`, which makes it the containing block for
// `absolute` descendants but never for `fixed` ones, and is the viewport rect
// regardless.) Nothing else is lost by moving out: the theme tokens are
// stamped on `:root` (`app/theme.ts` writes `data-theme` on
// `documentElement`, packs compile to `:root[data-os-theme=...]`) and the
// only layout-scoped rule under `.os-root` is the phone Ask sheet, so a node
// under `document.body` renders identically. If `html` or `body` ever grows
// a transform, this menu moves with it and this paragraph is where to look.

/** How close to a viewport edge the menu is allowed to sit, in px. */
const EDGE_MARGIN = 8;

interface MenuBox {
  width: number;
  height: number;
  viewportWidth: number;
  viewportHeight: number;
}

/**
 * Where a menu of this size opened at this point should actually sit.
 *
 * FLIPPED, NOT SLID. A menu that slid left to fit would appear with the
 * pointer resting somewhere in its middle -- on an entry the person never
 * aimed at. Opening leftwards from the click keeps the click on a CORNER of
 * the menu, which is where the pointer sits for every menu that does fit, so
 * the two cases feel like one thing.
 */
export function placeContextMenu(at: { x: number; y: number } & MenuBox): {
  left: number;
  top: number;
} {
  const { x, y, width, height, viewportWidth, viewportHeight } = at;
  const left = x + width > viewportWidth - EDGE_MARGIN ? x - width : x;
  const top = y + height > viewportHeight - EDGE_MARGIN ? y - height : y;
  // A menu with no room on either side of the click has nowhere to flip to.
  // Pin it inside the margin rather than let it run off the near edge too.
  return { left: Math.max(EDGE_MARGIN, left), top: Math.max(EDGE_MARGIN, top) };
}

export interface MenuEntry {
  id: string;
  label: string;
  disabled?: boolean;
  onSelect: () => void;
}

export function ContextMenu({
  x,
  y,
  label,
  entries,
  onClose,
}: {
  /**
   * RAW VIEWPORT COORDINATES -- `event.clientX`, passed straight through.
   * Do not subtract a container's `getBoundingClientRect()`: the menu is
   * portalled out of every container (see the top of this file), so a
   * subtraction here is a second offset applied to a coordinate that was
   * already right.
   */
  x: number;
  /** Raw viewport coordinate -- `event.clientY`. Same rule as `x`. */
  y: number;
  label: string;
  entries: MenuEntry[];
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [box, setBox] = useState<MenuBox | null>(null);

  // Measured after every render and BEFORE the browser paints, so the menu is
  // never seen at the unclamped point first. No dependency list, because the
  // size changes with the entries; the updater returning the PREVIOUS object
  // when nothing moved is what stops that from looping.
  useLayoutEffect(() => {
    const el = ref.current;
    if (el === null) return;
    const rect = el.getBoundingClientRect();
    const next: MenuBox = {
      width: rect.width,
      height: rect.height,
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
    };
    setBox((prev) =>
      prev !== null &&
      prev.width === next.width &&
      prev.height === next.height &&
      prev.viewportWidth === next.viewportWidth &&
      prev.viewportHeight === next.viewportHeight
        ? prev
        : next,
    );
  });

  useEffect(() => {
    const el = ref.current;
    el?.querySelector<HTMLButtonElement>("button:not([disabled])")?.focus();
    // The node lives under `document.body` now, so a listener on the shell
    // root would never see a pointerdown inside the menu. This one is on
    // `window` in the capture phase and tests containment against the
    // PORTALLED node, which is the same node either way.
    const onPointer = (event: PointerEvent) => {
      if (!el?.contains(event.target as Node)) onClose();
    };
    window.addEventListener("pointerdown", onPointer, true);
    return () => window.removeEventListener("pointerdown", onPointer, true);
  }, [onClose]);

  function onKeyDown(event: React.KeyboardEvent) {
    const el = ref.current;
    if (!el) return;
    const buttons = Array.from(el.querySelectorAll<HTMLButtonElement>("button:not([disabled])"));
    const at = buttons.indexOf(document.activeElement as HTMLButtonElement);
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
    } else if (event.key === "ArrowDown") {
      event.preventDefault();
      buttons[(at + 1) % buttons.length]?.focus();
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      buttons[(at - 1 + buttons.length) % buttons.length]?.focus();
    }
  }

  // Unmeasured on the first pass, so it opens at the click and the layout
  // effect above corrects it before anything is painted.
  const placed = box === null ? { left: x, top: y } : placeContextMenu({ x, y, ...box });

  return createPortal(
    <div
      ref={ref}
      role="menu"
      aria-label={label}
      className="os-menu os-context-menu"
      style={{ left: placed.left, top: placed.top }}
      onKeyDown={onKeyDown}
      onContextMenu={(e) => e.preventDefault()}
    >
      {entries.map((entry) => (
        <button
          key={entry.id}
          type="button"
          role="menuitem"
          className="os-menu-item"
          disabled={entry.disabled}
          onClick={() => {
            onClose();
            entry.onSelect();
          }}
        >
          {entry.label}
        </button>
      ))}
    </div>,
    document.body,
  );
}
