import type { ReactNode } from "react";
import { ThemeSwitch } from "./ThemeSwitch";

/** Shared frame for identity and cluster setup before the OS desktop opens. */
export function EntryLayout({ children }: { children: ReactNode }) {
  return <div className="os-entry-layout">
    <header className="os-entry-preferences"><ThemeSwitch /></header>
    {children}
  </div>;
}
