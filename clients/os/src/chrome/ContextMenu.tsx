import { useEffect, useRef } from "react";

// One context menu for the whole shell: real menu semantics (role=menu,
// arrow keys, Esc, focus trap-in), positioned at the pointer, closed on
// any outside interaction. The desk and its items feed it their entries.

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
  x: number;
  y: number;
  label: string;
  entries: MenuEntry[];
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const el = ref.current;
    el?.querySelector<HTMLButtonElement>("button:not([disabled])")?.focus();
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

  return (
    <div
      ref={ref}
      role="menu"
      aria-label={label}
      className="os-menu os-context-menu"
      style={{ left: x, top: y }}
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
    </div>
  );
}
