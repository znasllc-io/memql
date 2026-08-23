import type { ConceptLike } from "@znasllc-io/memql-view-kit";

import type { MeSession } from "./useMySessions";

// The row set the Sessions tab ADAPTS, and the descriptor the element library
// reads it through.
//
// Same arrangement src/admin/rows.ts and src/deploy/rows.ts use, and for the
// same reason: view-kit takes a ConceptLike (id, entity, displayCard) and
// nothing else, so an adapted set declares one here and the element library
// stays the only thing drawing rows.
//
// THE ID IS NOT A MEMQL ID. `me.session`, not `v1:identity:authSession`. A
// canonical id would claim there is a concept behind it an operator could open
// in the concept browser -- and these rows are not that. `authSessionSelf`
// projects them, `thisDevice` is not a field of the concept at all (it is a
// comparison against the caller's own session_id), and `device` is a shortened
// User-Agent rather than the stored one.

export const SESSION_CONCEPT: ConceptLike = {
  id: "me.session",
  entity: "session",
  displayCard: {
    primary: "device",
    secondary: "source",
    tertiary: "lastActive",
    status: "thisDevice",
  },
};

// SessionTableRow is MeSession with its two timestamps already rendered and
// "this device" turned into a word.
//
// The formatting happens HERE rather than in the element because view-kit
// draws scalars as it finds them: an ISO string would render as an ISO string,
// and a boolean would render as "true" in the column a person reads to find
// the machine in front of them.
export interface SessionTableRow extends Record<string, unknown> {
  id: string;
  device: string;
  source: string;
  signedIn: string;
  lastActive: string;
  // "This device" or "" -- the empty string rather than "Other", because a
  // column that labels every row is a column that distinguishes none of them.
  thisDevice: string;
}

export function sessionTableRows(
  sessions: readonly MeSession[],
  format: (value: string) => string,
): SessionTableRow[] {
  return sessions.map((session) => ({
    id: session.id,
    device: session.device,
    source: session.source,
    signedIn: format(session.signedIn),
    lastActive: format(session.lastActive),
    thisDevice: session.thisDevice ? "This device" : "",
  }));
}
