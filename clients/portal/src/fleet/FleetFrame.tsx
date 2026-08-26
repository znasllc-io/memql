import type { ReactNode } from "react";

import { AreaFrame } from "../app/AreaFrame";
import type { FleetSurface } from "./urls";

// The chrome the three Fleet screens share.
//
// It is now a thin binding of the shared AreaFrame (memql#4655) rather than
// its own header + tab strip: the tabs come from the nav definition, so the
// strip here and the rail row that opens it cannot disagree about what Fleet
// contains. The strip used to be built from FLEET_SURFACES directly, which
// was correct and which is exactly how Fleet came to be reachable from two
// rail captions -- two lists, one area.
//
// THE EYEBROW IS GONE. It carried the concept id, which was the single most
// useful fact for anybody about to go and query the rows and noise for
// everybody else. It lives in this page's guide now, under Technical details
// (decision D5), and the slot above the title carries a word instead.

export function FleetFrame({
  surface,
  actions,
  children,
}: {
  surface: FleetSurface;
  // Right-aligned, prominent. Absent when the caller can do nothing here.
  actions?: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <AreaFrame
      area="fleet"
      pageId={`fleet.${surface.id}`}
      subtitle="Fleet"
      title={surface.title}
      blurb={surface.blurb}
      {...(actions === undefined ? {} : { actions })}
    >
      {children}
    </AreaFrame>
  );
}

// LiveDegraded says the list has stopped being live, in the one place both
// screens need it.
//
// It is its own component rather than a line of JSX repeated twice because the
// SENTENCE matters: an operator reading a stale machine list has to know the
// staleness is the connection's and not the machine's, or they go and debug a
// computer that is working. useConceptRows keeps this state separate from the
// read error for the same reason.
export function LiveDegraded({ reason, noun }: { reason: string; noun: string }): ReactNode {
  if (reason === "") return null;
  return (
    <p className="rounded border border-warn bg-warn-subtle px-3 py-2 text-xs text-fg">
      Live updates are off for this page: {reason}. What is listed is what the last read
      returned -- a {noun} that changed since then will not show it here, and one that has gone
      quiet will still look reachable. Reload to read again.
    </p>
  );
}
