import type { AnchorHTMLAttributes, ButtonHTMLAttributes } from "react";
import { Plus } from "lucide-react";
import { IconButton } from "./IconButton";

/** One creation entry point, shared by app headers. Form submission stays explicit. */
export function AddButton({ label, ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children" | "aria-label" | "title"> & { label: string }) {
  return <IconButton label={label} {...props}><Plus size={16} aria-hidden /></IconButton>;
}

/** The same header affordance for creation that continues in another page. */
export function AddLink({ label, ...props }: Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "children" | "aria-label" | "title"> & { label: string }) {
  return <a {...props} className="os-icon-button os-reading-action" aria-label={label} title={label} style={{ minWidth: 36, minHeight: 36 }}><Plus size={16} aria-hidden /></a>;
}
