import type { ButtonHTMLAttributes, MouseEvent, ReactNode } from "react";
import { useOneAtATime } from "./oneAtATime";

/**
 * A compact reading/navigation action with the same visible and accessible name.
 *
 * An `onClick` that returns a promise holds the button until it settles
 * (kit/oneAtATime): further presses are ignored and the icon dims. An icon has
 * no words to change, so there is no done word (Jesús, Nov 20).
 */
export function IconButton({ label, children, className = "", onClick, ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "aria-label" | "title" | "onClick"> & {
  label: string;
  children: ReactNode;
  onClick?: (event: MouseEvent<HTMLButtonElement>) => unknown;
}) {
  const guard = useOneAtATime();
  const busy = guard.phase === "busy";
  return <button type="button" {...props} className={`os-icon-button os-reading-action ${className}`} aria-label={label} title={label}
    aria-busy={busy || props["aria-busy"] || undefined} data-busy={busy || undefined}
    onClick={onClick === undefined ? undefined : (event) => guard.press(() => onClick(event), false)}
    style={{ minWidth: "var(--os-icon-button-size)", minHeight: "var(--os-icon-button-size)", ...props.style }}>{children}</button>;
}
