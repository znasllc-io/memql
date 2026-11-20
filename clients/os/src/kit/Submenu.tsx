import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { ChevronRight } from "lucide-react";

/**
 * A menu row that opens a second menu beside it, like a desktop app's menu.
 *
 * In the kit because it is a shared interaction (repo rule): Files' Add menu
 * is the first user ("New file" -> the kinds of file, Jesús, Nov 20 2026),
 * and any `.os-menu` can nest one the same way.
 *
 * The row itself is not an action: clicking it does nothing, hovering opens
 * the side menu. Keyboard: Right arrow / Enter / Space opens it and focuses its
 * first entry; Left arrow / Escape closes it and returns focus to the row
 * (Escape stops here, so the outer menu stays open). It opens to the right and
 * flips to the left when the right side has no room.
 */
export function Submenu({ label, icon, children }: { label: string; icon?: ReactNode; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const [side, setSide] = useState<"right" | "left">("right");
  const row = useRef<HTMLButtonElement | null>(null);
  const menu = useRef<HTMLDivElement | null>(null);
  const focusFirst = useRef(false);
  const closing = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(closing.current), []);

  useLayoutEffect(() => {
    if (!open || !menu.current) return;
    // Room is measured against the nearest box that clips (a window's body),
    // else the viewport, so the side menu is never cut off.
    let limit = window.innerWidth;
    for (let node = menu.current.parentElement; node; node = node.parentElement) {
      if (/(auto|scroll|hidden|clip)/.test(getComputedStyle(node).overflowX)) {
        limit = Math.min(limit, node.getBoundingClientRect().right);
        break;
      }
    }
    const rowRect = row.current!.getBoundingClientRect();
    setSide(rowRect.right + menu.current.offsetWidth > limit ? "left" : "right");
    if (focusFirst.current) {
      focusFirst.current = false;
      menu.current.querySelector<HTMLElement>('[role="menuitem"]:not(:disabled)')?.focus();
    }
  }, [open]);

  const show = (withFocus: boolean) => {
    clearTimeout(closing.current);
    focusFirst.current = withFocus;
    if (open && withFocus) menu.current?.querySelector<HTMLElement>('[role="menuitem"]:not(:disabled)')?.focus();
    setOpen(true);
  };
  const back = () => {
    setOpen(false);
    row.current?.focus();
  };

  return (
    <div
      className="os-submenu"
      onMouseEnter={() => show(false)}
      // A short grace so a pointer cutting the corner to the side menu
      // does not shut it on the way.
      onMouseLeave={() => { closing.current = setTimeout(() => setOpen(false), 200); }}
    >
      <button
        ref={row}
        type="button"
        role="menuitem"
        aria-haspopup="menu"
        aria-expanded={open}
        className="os-menu-item os-submenu-row"
        onKeyDown={(event) => {
          if (event.key === "ArrowRight" || event.key === "Enter" || event.key === " ") {
            event.preventDefault();
            show(true);
          }
        }}
      >
        {icon}
        <span className="os-submenu-label">{label}</span>
        <ChevronRight size={13} aria-hidden />
      </button>
      {open ? (
        <div
          ref={menu}
          className="os-menu os-submenu-menu"
          data-side={side}
          role="menu"
          aria-label={label}
          onKeyDown={(event) => {
            if (event.key === "ArrowLeft" || event.key === "Escape") {
              event.stopPropagation();
              back();
            }
          }}
        >
          {children}
        </div>
      ) : null}
    </div>
  );
}
