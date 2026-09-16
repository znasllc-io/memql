import type { ButtonHTMLAttributes } from "react";
import { Plus } from "lucide-react";
import { IconButton } from "./IconButton";

/** One creation entry point, shared by app headers. Form submission stays explicit. */
export function AddButton({ label, ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children" | "aria-label" | "title"> & { label: string }) {
  return <IconButton label={label} {...props}><Plus size={16} aria-hidden /></IconButton>;
}
