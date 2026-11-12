import type { ReactNode } from "react";

// A FACT THAT CAN BE EMPTY SAYS WHY. The kit's `Fact` draws a bare dash for an
// empty value; on a record of what the engine wrote, a bare dash reads as
// "this page lost it". So an empty value here is that same dash with its
// reason, one hover away and read out by a screen reader (DESIGN.md: a dash
// with its reason, never a blank).

/** The value, or the dash with its reason when the engine recorded none. */
export function orNotRecorded(value: string, shown: ReactNode = value): ReactNode {
  if (value.trim() !== "") return shown;
  return (
    <span className="os-figure-absent" title="Not recorded">
      <span aria-hidden>--</span>
      <span className="os-sr-only">Not recorded</span>
    </span>
  );
}
