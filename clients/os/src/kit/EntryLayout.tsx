import type { ReactNode } from "react";
import { ThemeSwitch } from "./ThemeSwitch";

/**
 * Shared frame for identity and cluster setup before the OS desktop opens.
 * `preferences={false}` leaves the theme switch out: the loading handoff
 * (EntryPending) shows for a moment on every reload, and a control nobody can
 * use in that moment only flashes in the corner (the owner, Nov 2026).
 */
export function EntryLayout({ children, preferences = true }: { children: ReactNode; preferences?: boolean }) {
  return <div className="os-entry-layout">
    {preferences ? <header className="os-entry-preferences"><ThemeSwitch /></header> : null}
    {children}
  </div>;
}
