import { createContext, useContext, type ReactNode } from "react";

import { anonymousSource, type OsAuthSource } from "./source";

// The credential seam, reachable from inside an app.
//
// ===========================================================================
// WHY A CONTEXT, AND WHY IT HANDS OUT A CAPABILITY RATHER THAN A STRING
// ===========================================================================
// `OsAuthSource` already exists and the Shell already holds one; what it did
// not have was a way for an APP to reach it. The shell's own uploads provider
// is built at the top and handed down to the Desktop, which is right for the
// desk's file drops -- but an app that uploads somewhere ELSE (the Training
// app posts to the space attachment route, not to the Library) needs its own
// provider, and a provider needs a bearer.
//
// The alternatives were worse in the same direction. Putting the token on
// `SessionFacts` would place a credential beside `access` and `config` --
// values every surface reads and logs -- and passing the string through
// `OsAppProps` would put it in a prop table. This keeps the rule the source
// module states: components ask for CAPABILITY (`bearer()`), never for the
// string, so there stays exactly one place it can leak from.
//
// THE DEFAULT IS ANONYMOUS, not a throw. A test that renders an app without
// the provider, and a cluster with auth disabled, are both cases where
// "supply nothing, honestly" is the right answer -- and a throw would make
// every app harness carry a provider it has no opinion about.

const Ctx = createContext<OsAuthSource>(anonymousSource);

export function useAuthSource(): OsAuthSource {
  return useContext(Ctx);
}

export function AuthSourceProvider({
  source,
  children,
}: {
  source: OsAuthSource;
  children: ReactNode;
}) {
  return <Ctx.Provider value={source}>{children}</Ctx.Provider>;
}
