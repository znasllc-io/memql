import type { ReactNode } from "react";

import { ChevronsLeft, ChevronsRight } from "../ui/icons";

// The rail's collapse control, as a tab on its own edge (memql#4316).
//
// # Why it moved off the brand row
//
// It used to live inside the brand row: `ml-auto` when expanded, centred
// under the mark when collapsed. That is a chevron floating in the rail with
// nothing to belong to, and it moved twice depending on state -- so the one
// control whose job is "this rail folds" was the hardest thing in the rail to
// find twice running.
//
// # Why the top corner, and why a circle
//
// An 18px circle centred ON the right border, at the top. Three things
// decided it:
//
//   - The BORDER is what the control acts on, so sitting astride it says what
//     will happen without a label. A grip pill at mid-height is quieter but
//     hides that the rail collapses at all.
//   - The TOP CORNER is the one place on that edge nothing will ever grow
//     into. A tab above the footer drifts as the nav gets longer; this one
//     cannot, because the header pins it.
//   - It is the SAME SPOT COLLAPSED. A control that relocates when you use it
//     costs a person the position they just learned.
//
// Borderless and absolutely positioned, so it reads as part of the edge
// rather than as a button parked near it -- and so it takes no row in the
// nav's flex column, which is what lets the profile row be the rail's first
// real content.
//
// The caller keeps the state and the storage (AppShell's `toggleRail` +
// `memql-portal-rail`); this component owns only how it looks and what it
// announces. `aria-expanded` describes the RAIL, and the label says the verb
// the press performs rather than the state it is in -- "Collapse the
// navigation rail" when expanded -- because a control is named for what it
// does.

export function RailHandle({
  collapsed,
  onToggle,
}: {
  collapsed: boolean;
  onToggle: () => void;
}): ReactNode {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-expanded={!collapsed}
      aria-label={collapsed ? "Expand the navigation rail" : "Collapse the navigation rail"}
      title={collapsed ? "Expand the navigation rail" : "Collapse the navigation rail"}
      // -right-[9px] is half of 18px: the circle straddles the 1px border
      // rather than sitting beside it. z-10 keeps it above the nav's own
      // scroll container.
      className={
        "absolute top-2 -right-[9px] z-10 flex h-[18px] w-[18px] items-center justify-center " +
        "rounded-full bg-surface text-muted hover:bg-raised hover:text-fg"
      }
    >
      {collapsed ? (
        <ChevronsRight size={12} aria-hidden="true" />
      ) : (
        <ChevronsLeft size={12} aria-hidden="true" />
      )}
    </button>
  );
}
