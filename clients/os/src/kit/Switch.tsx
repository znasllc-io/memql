import type { ReactNode } from "react";

/** Binary preferences. Keep Check for selecting members of a set. */
export function Switch({ checked, onChange, children, disabled = false }: { checked: boolean; onChange: (next: boolean) => void; children: ReactNode; disabled?: boolean }) {
  return <label className="os-switch"><input type="checkbox" role="switch" checked={checked} disabled={disabled} onChange={event => onChange(event.target.checked)} /><span className="os-switch-track" aria-hidden /><span>{children}</span></label>;
}
