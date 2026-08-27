import type { ReactNode } from "react";

/** Quiet caption row used across the OS for state lines and hints. */
export function Caption({ children }: { children: ReactNode }) {
  return <p className="os-caption">{children}</p>;
}
