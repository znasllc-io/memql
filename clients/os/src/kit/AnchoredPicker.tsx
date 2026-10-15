import { useLayoutEffect, useRef, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";

/** A joined search popup that escapes window scrolling and transformed desks. */
export function AnchoredPicker({ anchor, owner, popupRef, onClose, children }: {
  anchor: RefObject<HTMLInputElement | null>;
  owner: RefObject<HTMLElement | null>;
  popupRef: RefObject<HTMLDivElement | null>;
  onClose: () => void;
  children: ReactNode;
}) {
  const close = useRef(onClose); close.current = onClose;
  useLayoutEffect(() => {
    const input = anchor.current, popup = popupRef.current;
    if (!input || !popup) return;
    const styles = getComputedStyle(input);
    // Preserve a caller's theme even when it is scoped to a window.
    for (const key of Array.from(styles)) if (key.startsWith("--os-")) popup.style.setProperty(key, styles.getPropertyValue(key));
    popup.style.fontFamily = styles.fontFamily;
    const clipping: HTMLElement[] = [];
    for (let node = input.parentElement; node; node = node.parentElement) {
      if (/(auto|scroll|hidden|clip)/.test(getComputedStyle(node).overflowY)) clipping.push(node);
    }
    let frame = 0, lastRect = "";
    const position = () => {
      if (input.closest('[hidden], [inert], [aria-hidden="true"]')) { close.current(); return; }
      const rect = input.getBoundingClientRect();
      const viewport = window.visualViewport;
      const leftEdge = viewport?.offsetLeft ?? 0, topEdge = viewport?.offsetTop ?? 0;
      const width = viewport?.width ?? window.innerWidth, height = viewport?.height ?? window.innerHeight;
      if (rect.width > 0 && (rect.bottom <= topEdge || rect.top >= topEdge + height || clipping.some(node => {
        const bounds = node.getBoundingClientRect();
        return bounds.height > 0 && (rect.bottom <= bounds.top || rect.top >= bounds.bottom);
      }))) { close.current(); return; }
      const signature = [rect.left, rect.top, rect.width, rect.bottom, leftEdge, topEdge, width, height].join(":");
      if (signature !== lastRect) {
        lastRect = signature;
        const below = Math.max(0, topEdge + height - rect.bottom - 8), above = Math.max(0, rect.top - topEdge - 8);
        const up = below < 280 && above > below;
        const popupWidth = Math.min(rect.width, Math.max(0, width - 16));
        popup.style.left = `${Math.max(leftEdge + 8, Math.min(rect.left, leftEdge + width - popupWidth - 8))}px`;
        popup.style.width = `${popupWidth}px`;
        popup.style.maxHeight = `${Math.min(320, up ? above : below)}px`;
        popup.style.top = `${up ? rect.top + 1 : rect.bottom - 1}px`;
        popup.style.transform = up ? "translateY(-100%)" : "none";
        popup.dataset.side = up ? "above" : "below";
        input.dataset.pickerSide = popup.dataset.side;
      }
      // Windows can move by transforms without a scroll/resize event.
      frame = requestAnimationFrame(position);
    };
    const outside = (event: Event) => {
      const target = event.target as Node | null;
      if (target && !owner.current?.contains(target) && !popup.contains(target)) close.current();
    };
    const visibility = () => { if (document.visibilityState === "hidden") close.current(); };
    position();
    window.addEventListener("pointerdown", outside, true);
    window.addEventListener("focusin", outside);
    document.addEventListener("visibilitychange", visibility);
    return () => {
      cancelAnimationFrame(frame);
      delete input.dataset.pickerSide;
      window.removeEventListener("pointerdown", outside, true);
      window.removeEventListener("focusin", outside);
      document.removeEventListener("visibilitychange", visibility);
    };
  }, [anchor, owner, popupRef]);
  return createPortal(<div ref={popupRef} className="os-anchored-picker">{children}</div>, anchor.current?.closest("dialog") ?? document.body);
}
