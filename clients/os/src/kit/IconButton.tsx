import type { ButtonHTMLAttributes, ReactNode } from "react";

/** A compact reading/navigation action with the same visible and accessible name. */
export function IconButton({ label, children, className = "", ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "aria-label" | "title"> & { label: string; children: ReactNode }) {
  return <button type="button" {...props} className={`os-icon-button os-reading-action ${className}`} aria-label={label} title={label} style={{ minWidth: 36, minHeight: 36, ...props.style }}>{children}</button>;
}
