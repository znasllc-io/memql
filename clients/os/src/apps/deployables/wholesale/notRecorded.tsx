import type { ReactNode } from "react";

import { Absent } from "../../../kit";

// A FACT THAT CAN BE EMPTY SAYS WHY. The kit's `Fact` draws a bare dash for an
// empty value; on a record of what the engine wrote, a bare dash reads as
// "this page lost it". So an empty value here is the kit's own dash with its
// reason (`Absent`, the one `Measure` draws), one hover away, and the same
// words for a screen reader.

/** The value, or the kit's dash with its reason when the engine recorded none. */
export function orNotRecorded(value: string, shown: ReactNode = value): ReactNode {
  if (value.trim() !== "") return shown;
  return <><Absent reason="Not recorded" /><span className="os-sr-only">Not recorded</span></>;
}
