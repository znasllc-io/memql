import type { ReactNode } from "react";
import { ThemeSwitch } from "../kit/ThemeSwitch";
import "./identity.css";

/** Persistent chrome for standalone identity pages, outside the OS desktop. */
export function IdentityLayout({ children }: { children: ReactNode }) {
  return <div className="os-identity-layout">
    <header className="os-identity-preferences"><ThemeSwitch /></header>
    {children}
  </div>;
}
