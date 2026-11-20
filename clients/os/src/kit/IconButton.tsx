import { useState, type ButtonHTMLAttributes, type FocusEvent, type MouseEvent, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { osScale } from "../system/scale";
import { useOneAtATime } from "./oneAtATime";

/**
 * A compact reading/navigation action with the same visible and accessible name.
 *
 * An `onClick` that returns a promise holds the button until it settles
 * (kit/oneAtATime): further presses are ignored and the icon dims. An icon has
 * no words to change, so there is no done word (Jesús, Nov 20).
 */
export function IconButton({ label, children, className = "", tip = "native", onClick, ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "aria-label" | "title" | "onClick"> & {
  label: string;
  children: ReactNode;
  onClick?: (event: MouseEvent<HTMLButtonElement>) => unknown;
  /**
   * How the name shows on hover. "native" is the browser's own `title`
   * tooltip, which the browser holds back for a second or more and draws
   * under the pointer; a page cannot shorten that wait. "above" draws the
   * same name at once, above the button, where the pointer never covers it
   * (the Files Head's file icons: Jesús, Nov 20 2026, "it takes two seconds
   * for the little pop-up to appear"). Keyboard focus shows it too.
   */
  tip?: "native" | "above";
}) {
  const guard = useOneAtATime();
  const busy = guard.phase === "busy";
  const [at, setAt] = useState<{ x: number; y: number; host: Element } | null>(null);
  const above = tip === "above";
  const show = (button: HTMLElement) => {
    // Measured in screen pixels; the label is fixed in page pixels, which the
    // desktop's zoom multiplies (system/scale.ts), so divide once.
    const box = button.getBoundingClientRect();
    const scale = osScale();
    // Into the open dialog when there is one, as the kit's Select list goes:
    // the top layer would cover anything drawn on the body.
    setAt({ x: (box.left + box.width / 2) / scale, y: box.top / scale, host: button.closest("dialog") ?? document.body });
  };
  const hide = () => setAt(null);
  const keyboard = (event: FocusEvent<HTMLButtonElement>) => {
    try { return event.currentTarget.matches(":focus-visible"); } catch { return false; }
  };
  return <>
    <button type="button" {...props} className={`os-icon-button os-reading-action ${className}`} aria-label={label} title={above ? undefined : label}
      aria-busy={busy || props["aria-busy"] || undefined} data-busy={busy || undefined}
      onClick={onClick === undefined ? undefined : (event) => guard.press(() => onClick(event), false)}
      style={{ minWidth: "var(--os-icon-button-size)", minHeight: "var(--os-icon-button-size)", ...props.style }}
      {...(above ? {
        onPointerEnter: (event) => { show(event.currentTarget); props.onPointerEnter?.(event); },
        onPointerLeave: (event) => { hide(); props.onPointerLeave?.(event); },
        // A press hides it, as the browser's own tooltip does.
        onPointerDown: (event) => { hide(); props.onPointerDown?.(event); },
        onFocus: (event) => { if (keyboard(event)) show(event.currentTarget); props.onFocus?.(event); },
        onBlur: (event) => { hide(); props.onBlur?.(event); },
      } satisfies ButtonHTMLAttributes<HTMLButtonElement> : {})}>{children}</button>
    {at ? createPortal(<span className="os-tip" aria-hidden="true" style={{ left: at.x, top: at.y }}>{label}</span>, at.host) : null}
  </>;
}
