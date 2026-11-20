import { useState, type ButtonHTMLAttributes, type FocusEvent, type ReactNode } from "react";
import { createPortal } from "react-dom";

/** A compact reading/navigation action with the same visible and accessible name. */
export function IconButton({ label, children, className = "", tip = "native", ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "aria-label" | "title"> & {
  label: string;
  children: ReactNode;
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
  const [at, setAt] = useState<{ x: number; y: number; host: Element } | null>(null);
  const above = tip === "above";
  const show = (button: HTMLElement) => {
    const box = button.getBoundingClientRect();
    // Into the open dialog when there is one, as the kit's Select list goes:
    // the top layer would cover anything drawn on the body.
    setAt({ x: box.left + box.width / 2, y: box.top, host: button.closest("dialog") ?? document.body });
  };
  const hide = () => setAt(null);
  const keyboard = (event: FocusEvent<HTMLButtonElement>) => {
    try { return event.currentTarget.matches(":focus-visible"); } catch { return false; }
  };
  return <>
    <button type="button" {...props} className={`os-icon-button os-reading-action ${className}`} aria-label={label} title={above ? undefined : label}
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
