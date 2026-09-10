import { createContext, useContext, type ReactNode } from "react";

import type { OsRuntimeConfig } from "../cluster/config";
import type { ProfileAccess } from "../modules/profile/access";
import type { Readiness } from "../live/readiness";

// Who is signed in, against which cluster. Read by chrome (avatar menu),
// Settings/About, and the items layer (the VS Code handoff needs the
// cluster domain). Kept as a context so apps receive OsAppProps only.

export interface SessionFacts {
  access: ProfileAccess | null;
  config: OsRuntimeConfig;
  /**
   * Whether the cluster's role ladder has landed (epic memql#4832, D1).
   *
   * It rides here so a gated surface can tell TWO STATES APART that both
   * hide an app: "you may not reach this" and "the shell does not know the
   * ordering yet". They must not read the same to the person in front of it
   * -- one is an answer and the other is a wait, and rendering the answer
   * during the wait is how a shell tells somebody they lack access they have.
   *
   * Optional so every existing harness that builds SessionFacts by hand keeps
   * compiling; absent reads as "not loaded", which is the fail-closed value.
   */
  ladderLoaded?: boolean;
  /**
   * Module readiness (design record 2026-09-06-configuration-readiness,
   * section 5.2): what the cluster has and has not been set up to do.
   *
   * Optional for the same reason ladderLoaded is -- every harness that builds
   * SessionFacts by hand keeps compiling -- and absent reads as "not loaded",
   * under which nothing is gated and nothing is drawn. That is the fail-open
   * value HERE, and deliberately the opposite of the ladder's: a shell that
   * does not know what is configured must show the app, not hide it behind a
   * setup screen the person cannot dismiss.
   */
  readiness?: Readiness;
}

const Ctx = createContext<SessionFacts | null>(null);

export function useSession(): SessionFacts {
  const value = useContext(Ctx);
  if (!value) throw new Error("useSession outside SessionProvider");
  return value;
}

/** Null outside SessionProvider -- for feeds that must not throw in harnesses. */
export function useSessionIfPresent(): SessionFacts | null {
  return useContext(Ctx);
}

export function SessionProvider({ value, children }: { value: SessionFacts; children: ReactNode }) {
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
