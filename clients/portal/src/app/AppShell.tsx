import { NavLink, Outlet } from "react-router-dom";
import type { ReactNode } from "react";

import { ClusterBadge } from "../components/ClusterBadge";
import { ConnectionIndicator } from "../components/ConnectionIndicator";
import { IdentityBadge } from "../components/IdentityBadge";
import { ThemeToggle } from "../components/ThemeToggle";
import { CONCEPTS_ROOT } from "../concepts/urls";
import { VIEWS } from "../views/registry";
import { viewPath } from "../views/urls";

// The routed layout: a header carrying identity + connection state, a nav
// rail, and an <Outlet> the routes render into.
//
// THE NAV IS GROUPED, and the grouping is a real distinction rather than
// tidying. The predefined views (memql#3319) are SURFACES an operator works
// in -- people, agents, customers, deployments, audit. The concept registry
// is the SUBSTRATE those surfaces are built on: every concept in the cluster,
// rendered generically. Flattening the two into one list of six would make
// "Concepts" look like a sixth destination of the same kind, and would keep
// looking worse as the epic adds more. Two short labelled groups stay legible
// where a flat list stops being scannable at about five.
//
// The view group is DERIVED from the registry, so adding a view is one row in
// src/views/registry.ts plus its body module -- never an edit here.

interface NavItem {
  to: string;
  label: string;
}

const OPERATE: readonly NavItem[] = VIEWS.filter((view) => view.group === "operate").map(
  (view) => ({ to: viewPath(view.id), label: view.label }),
);

// Explore is the SUBSTRATE: the whole concept registry, plus the composer that
// turns any concept in it into a view without code (memql#3320).
const EXPLORE: readonly NavItem[] = [
  { to: CONCEPTS_ROOT, label: "Concepts" },
  { to: "/compose", label: "Compose" },
];

// Administer is the cluster ITSELF rather than the data in it -- what is
// plugged in (memql#3323) and how the cluster is configured and keyed
// (memql#3324). Kept apart from Operate because an operator visits these
// occasionally and deliberately, where Operate is where they live.
const ADMINISTER: readonly NavItem[] = [
  { to: "/integrations", label: "Integrations" },
  { to: "/admin", label: "Administration" },
];

function navClass({ isActive }: { isActive: boolean }): string {
  return (
    "block rounded px-3 py-1.5 text-sm " +
    (isActive
      ? "bg-accent-subtle font-medium text-fg"
      : "text-muted hover:bg-raised hover:text-fg")
  );
}

function NavGroup({ label, items }: { label: string; items: readonly NavItem[] }): ReactNode {
  return (
    <div>
      <h2 className="px-3 pb-1 text-xs font-semibold tracking-wide text-subtle uppercase">
        {label}
      </h2>
      <ul className="space-y-0.5">
        {items.map((item) => (
          <li key={item.to}>
            <NavLink to={item.to} className={navClass}>
              {item.label}
            </NavLink>
          </li>
        ))}
      </ul>
    </div>
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
          className="flex w-48 shrink-0 flex-col gap-4 border-r border-line bg-surface p-2"
        >
          <NavGroup label="Operate" items={OPERATE} />
          <NavGroup label="Explore" items={EXPLORE} />
          <NavGroup label="Administer" items={ADMINISTER} />
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
