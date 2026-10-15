import type { AnchorHTMLAttributes, ButtonHTMLAttributes } from "react";
import { Plus } from "lucide-react";
import { IconButton } from "./IconButton";

/** One creation entry point, shared by app headers. Form submission stays explicit. */
export function AddButton({ label, className = "", ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children" | "aria-label" | "title"> & { label: string }) {
  return <IconButton label={label} {...props} className={`os-add-button ${className}`}><Plus size={16} aria-hidden /></IconButton>;
}

/** The same header affordance for creation that continues in another page. */
export function AddLink({ label, className = "", style, ...props }: Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "children" | "aria-label" | "title"> & { label: string }) {
  return <a {...props} className={`os-icon-button os-reading-action os-add-button ${className}`} aria-label={label} title={label} style={{ minWidth: 36, minHeight: 36, ...style }}><Plus size={16} aria-hidden /></a>;
}
