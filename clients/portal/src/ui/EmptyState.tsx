import type { ReactNode } from "react";

import { Constellation } from "./Constellation";

// An empty screen is an invitation to act, never a bare "No data". One plain
// statement of what is not here, and -- where an obvious next step exists --
// exactly one action with a verb for a label ("Create a site", "Invite
// someone"). No action slot filled means the emptiness is a fact the reader
// cannot change from here, and the statement alone is the honest render.
//
// FIRST RUN IS A DIFFERENT EMPTY (memql#4651), and that is the whole of what
// the flag decides. "You have never added a machine" is the product
// introducing itself and is worth a moment; "your filter matched nothing" is
// a dead end and giving it a signature mark would be celebrating a
// non-result. So the Constellation is opt-in, per call site, and every other
// EmptyState in the portal renders exactly as it did before.
//
// `icon` still wins when both are given -- a page that chose a specific glyph
// meant it, and stacking a mark above it would be two pictures for one
// sentence.

export function EmptyState({
  statement,
  action,
  icon,
  firstRun = false,
}: {
  statement: ReactNode;
  action?: ReactNode;
  icon?: ReactNode;
  // Opt in on a first-run empty only: nothing of this kind exists YET, as
  // opposed to nothing matching what was asked for.
  firstRun?: boolean;
}): ReactNode {
  return (
    <div className="flex flex-col items-center gap-3 rounded-lg border border-dashed border-line bg-surface px-6 py-10 text-center">
      {icon === undefined ? (
        firstRun ? (
          <span className="text-accent opacity-80">
            <Constellation size="sm" />
          </span>
        ) : null
      ) : (
        <div className="text-subtle">{icon}</div>
      )}
      <p className="max-w-md text-sm text-muted">{statement}</p>
      {action === undefined ? null : <div>{action}</div>}
    </div>
  );
}
