import { NavLink, Outlet } from "react-router-dom";
import type { ReactNode } from "react";

import { ClusterBadge } from "../components/ClusterBadge";
import { ConnectionIndicator } from "../components/ConnectionIndicator";
import { IdentityBadge } from "../components/IdentityBadge";
import { ThemeToggle } from "../components/ThemeToggle";

// The routed layout: a header carrying identity + connection state, a nav
// rail, and an <Outlet> the routes render into.
//
// The nav is a list rather than one hardcoded link because the epic adds to
// it (#3316 concept browsing, #3317's element library, #3319's predefined
// views). Adding a destination should be a line here and a <Route>, not a
// layout change.

interface NavItem {
  to: string;
  label: string;
}

const NAV: readonly NavItem[] = [{ to: "/concepts", label: "Concepts" }];

function navClass({ isActive }: { isActive: boolean }): string {
  return (
    "block rounded px-3 py-1.5 text-sm " +
    (isActive
      ? "bg-accent-subtle font-medium text-fg"
      : "text-muted hover:bg-raised hover:text-fg")
  );
}

export function AppShell(): ReactNode {
  return (
    <div className="flex h-full flex-col bg-bg text-fg">
      {/* The header answers the two questions an operations console must
          always answer without a click: WHICH cluster (ClusterBadge, plus the
          replica's node id on the indicator) and WHO am I acting as
          (IdentityBadge). */}
      <header className="flex items-center gap-4 border-b border-line bg-surface px-4 py-2">
        <ClusterBadge />
        <div className="ml-auto flex items-center gap-4">
          <ConnectionIndicator />
          <IdentityBadge />
          <ThemeToggle />
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        <nav
          aria-label="Portal sections"
          className="w-48 shrink-0 border-r border-line bg-surface p-2"
        >
          <ul className="space-y-0.5">
            {NAV.map((item) => (
              <li key={item.to}>
                <NavLink to={item.to} className={navClass}>
                  {item.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>

        {/* min-h-0 on both axes so a long row list scrolls inside the main
            pane instead of stretching the page and losing the header. */}
        <main className="min-w-0 flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
