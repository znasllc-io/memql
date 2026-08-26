import { NavLink } from "react-router-dom";
import type { ReactNode } from "react";

// The one secondary-nav idiom: an underline strip with an accent active
// edge. Before this file the portal ran three -- an admin pill row, the
// concept browser's underline, and header-link buttons on integrations and
// sites -- and an operator moving between sections had to re-learn what
// "current section" looks like each time. The underline won because tabs are
// ROUTES here (a schema view is a link someone can send), and an underline
// on a hairline reads as "views of this page" where pills read as unrelated
// destinations.
//
// Active state comes from NavLink's own matching by default; a caller whose
// activeness is not expressible as a path match (the concept browser's
// rows-vs-schema split, where an open row still means "Rows") passes
// `active` explicitly and NavLink matching is ignored for that item.

export interface TabItem {
  to: string;
  label: string;
  // NavLink `end` semantics for the default matcher.
  end?: boolean;
  // Explicit override; when present it wins over path matching.
  active?: boolean;
}

function tabClass(active: boolean): string {
  return (
    // `motion-wash` eases the underline and the colour at the brand duration
    // (brand/theme.css). The underline SLIDING between positions is what
    // design E asks for; a border-colour transition is how a strip of
    // adjacent 2px borders reads as one edge moving.
    "motion-wash -mb-px border-b-2 px-3 py-1.5 text-sm " +
    (active
      ? "border-accent font-medium text-fg"
      : "border-transparent text-muted hover:text-fg")
  );
}

export function Tabs({
  label,
  items,
}: {
  // The aria-label naming what this strip switches between.
  label: string;
  items: readonly TabItem[];
}): ReactNode {
  return (
    <nav aria-label={label} className="flex flex-wrap gap-1 border-b border-line">
      {items.map((item) =>
        item.active === undefined ? (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end ?? false}
            className={({ isActive }) => tabClass(isActive)}
            aria-current={undefined}
          >
            {item.label}
          </NavLink>
        ) : (
          <NavLink
            key={item.to}
            to={item.to}
            aria-current={item.active ? "page" : undefined}
            className={tabClass(item.active)}
          >
            {item.label}
          </NavLink>
        ),
      )}
    </nav>
  );
}
