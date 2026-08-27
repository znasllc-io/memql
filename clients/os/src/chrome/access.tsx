import { createContext, useContext, type ReactNode } from "react";

import type { OsRuntimeConfig } from "../cluster/config";
import type { ProfileAccess } from "../modules/profile/access";

// Who is signed in, against which cluster. Read by chrome (avatar menu),
// Settings/About, and the items layer (the VS Code handoff needs the
// cluster domain). Kept as a context so apps receive OsAppProps only.

export interface SessionFacts {
  access: ProfileAccess | null;
  config: OsRuntimeConfig;
}

const Ctx = createContext<SessionFacts | null>(null);

export function useSession(): SessionFacts {
  const value = useContext(Ctx);
  if (!value) throw new Error("useSession outside SessionProvider");
  return value;
}

export function SessionProvider({ value, children }: { value: SessionFacts; children: ReactNode }) {
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
