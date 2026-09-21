// The connection seam, swapped for a fixture (browser QA harness only).
//
// IT RE-EXPORTS THE REAL MODULE'S WHOLE SURFACE. The swap replaces the module,
// so `Shell`'s `import { OsConnectionProvider, useOsConnection }` breaks on a
// shim carrying only the hook -- and it breaks in Vite's DEPENDENCY SCAN, as an
// esbuild error naming the import, which reads as a problem with the Shell
// rather than with the harness.
import type { ReactNode } from "react";

export * from "../src/live/connection";

let held: unknown = null;

/** The harness installs the fixture connection before it renders. */
export function installQaConnection(connection: unknown): void {
  held = connection;
}

export function useOsConnection(): unknown {
  return held;
}

/** The provider's only job is to dial, and the shim IS the dial. */
export function OsConnectionProvider({ children }: { children?: ReactNode }): ReactNode {
  return children;
}
