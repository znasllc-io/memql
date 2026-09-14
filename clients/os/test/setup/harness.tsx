import type { ReactNode } from "react";

import { SetupFactsScope } from "../../src/apps/setup/SetupFactsScope";
import { OsProvider } from "../../src/chrome/state";
import { OS_REGISTRY } from "../../src/apps/registry";
import type { Readiness } from "../../src/live/readiness";
import type { Verdict } from "../../src/system/readinessFold";

// The wizard's harness. The connection is a module-level context read whose
// provider dials a WebSocket, so it is mocked at the module the way every
// other suite in this tree does; everything else is the REAL shell provider,
// the REAL registry and the REAL session, because what these cases assert is
// where an act lands and which role gets one.

export function verdict(module: string, state: Verdict["state"], core = true): Verdict {
  return { module, state, core, disagreement: [], nodes: [], lanes: [], unknown: [], stale: [], aside: [] };
}

export function readiness(loaded: boolean, verdicts: Verdict[]): Readiness {
  const by = new Map(verdicts.map((v) => [v.module, v]));
  return { loaded, state: "live", of: (id) => by.get(id) ?? null, reseed: () => {} };
}

/** A cluster whose three core modules report at the states given. */
export function coreAt(
  ai: Verdict["state"],
  storage: Verdict["state"],
  email: Verdict["state"],
): Readiness {
  return readiness(true, [
    verdict("ai", ai),
    verdict("storage", storage, true),
    verdict("email", email),
  ]);
}

/** The REAL shell provider, as every other suite mounts it. */
export function withOs(node: ReactNode, role: string) {
  return (
    <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 12, rows: 8 }} layout="desktop">
      {node}
    </OsProvider>
  );
}

/**
 * The setup-facts scope, which every setup surface now reads (epic
 * memql#5118).
 *
 * The wizard's gate, the core gate and the presence effect all take the stops
 * from one provider rather than each opening its own `passkeysForSelf` read,
 * so a test mounts what `Shell.tsx` mounts. It must sit INSIDE `withSession`
 * (it reads the feed) and it makes the connection mock load-bearing, which
 * every case in this tree already sets up.
 */
export function withSetupFacts(node: ReactNode) {
  return <SetupFactsScope>{node}</SetupFactsScope>;
}
