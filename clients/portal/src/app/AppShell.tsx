import { useState, type ComponentType, type ReactNode } from "react";
import { NavLink, Outlet } from "react-router-dom";

import { ClusterBadge } from "../components/ClusterBadge";
import { ConnectionIndicator } from "../components/ConnectionIndicator";
import { IdentityBadge } from "../components/IdentityBadge";
import { MemqlMark } from "../components/MemqlMark";
import { ThemeToggle } from "../components/ThemeToggle";
import { CONCEPTS_ROOT } from "../concepts/urls";
import {
  Bot,
  Boxes,
  Building2,
  ChevronsLeft,
  ChevronsRight,
  Globe,
  LayoutGrid,
  Plug,
  Rocket,
  ScrollText,
  Shield,
  Users,
} from "../ui/icons";
import { VIEWS } from "../views/registry";
import { viewPath } from "../views/urls";

// The routed layout: a branded nav rail, a quiet topbar, and an <Outlet>.
//
// THE BRAND LIVES IN THE RAIL. The mark at its head doubles as the
// connection indicator (memql#4180 drives its animation states), and the
// wordmark beside it is the display face's one chrome appearance. The topbar
// answers the operations console's two standing questions -- WHICH cluster
// (ClusterBadge: the origin host) and WHO am I acting as (IdentityBadge) --
// in one quiet control language on one row.
//
// THE NAV IS GROUPED, and the grouping is a real distinction rather than
// tidying. The predefined views are SURFACES an operator works in; the
// concept registry is the SUBSTRATE those surfaces are built on; Administer
// is the cluster ITSELF. Two short labelled groups stay legible where a flat
// list stops being scannable at about five.
//
// The view group is DERIVED from the registry, so adding a view is one row in
// src/views/registry.ts plus its body module -- never an edit here. Icons for
// derived items resolve from a by-id map with a safe default, for the same
// reason.
//
// THE RAIL COLLAPSES to an icon rail, and the preference persists beside the
// theme key. Collapsed items keep their accessible names (aria-label +
// title); the group captions hide because an icon column has no room to
// caption, and the icons are the captions' mnemonic.

interface NavItem {
  to: string;
  label: string;
  icon: ComponentType<{ size?: number | string; className?: string }>;
}

const VIEW_ICONS: Record<string, NavItem["icon"]> = {
  people: Users,
  agents: Bot,
  customers: Building2,
  deployments: Rocket,
  audit: ScrollText,
};

const OPERATE: readonly NavItem[] = VIEWS.filter((view) => view.group === "operate").map(
  (view) => ({
    to: viewPath(view.id),
    label: view.label,
    icon: VIEW_ICONS[view.id] ?? Boxes,
  }),
);

// Explore is the SUBSTRATE: the whole concept registry, plus the composer that
// turns any concept in it into a view without code.
const EXPLORE: readonly NavItem[] = [
  { to: CONCEPTS_ROOT, label: "Concepts", icon: Boxes },
  { to: "/compose", label: "Compose", icon: LayoutGrid },
];

// Administer is the cluster ITSELF rather than the data in it. Kept apart
// from Operate because an operator visits these occasionally and
// deliberately, where Operate is where they live.
const ADMINISTER: readonly NavItem[] = [
  { to: "/integrations", label: "Integrations", icon: Plug },
  { to: "/admin", label: "Administration", icon: Shield },
  { to: "/sites", label: "Sites", icon: Globe },
];

const RAIL_STORAGE_KEY = "memql-portal-rail";

function readStoredRail(): "expanded" | "collapsed" {
  try {
    return globalThis.localStorage?.getItem(RAIL_STORAGE_KEY) === "collapsed"
      ? "collapsed"
      : "expanded";
  } catch {
    return "expanded";
  }
}

function storeRail(state: "expanded" | "collapsed"): void {
  try {
    globalThis.localStorage?.setItem(RAIL_STORAGE_KEY, state);
  } catch {
    // A rail preference is not worth failing over -- same reasoning as the
    // theme store.
  }
}

function navClass(isActive: boolean, collapsed: boolean): string {
  return (
    "flex items-center gap-2.5 rounded py-1.5 text-sm " +
    (collapsed ? "justify-center px-0 " : "px-2.5 ") +
    // The active edge: a 2px accent bar on the left plus a soft fill. The
    // border is always present (transparent at rest) so activation never
    // shifts the text.
    "border-l-2 " +
    (isActive
      ? "border-accent bg-accent-subtle font-medium text-fg"
      : "border-transparent text-muted hover:bg-raised hover:text-fg")
  );
}

function NavGroup({
  label,
  items,
  collapsed,
}: {
  label: string;
  items: readonly NavItem[];
  collapsed: boolean;
}): ReactNode {
  return (
    <div>
      {collapsed ? null : (
        <h2 className="px-3 pb-1 text-xs font-semibold tracking-wide text-subtle uppercase">
          {label}
        </h2>
      )}
      <ul className="space-y-0.5">
        {items.map((item) => {
          const Icon = item.icon;
          return (
            <li key={item.to}>
              <NavLink
                to={item.to}
                className={({ isActive }) => navClass(isActive, collapsed)}
                {...(collapsed ? { title: item.label, "aria-label": item.label } : {})}
              >
                <Icon size={16} className="shrink-0" aria-hidden="true" />
                {collapsed ? null : <span className="truncate">{item.label}</span>}
              </NavLink>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

export function AppShell(): ReactNode {
  const [rail, setRail] = useState<"expanded" | "collapsed">(() => readStoredRail());
  const collapsed = rail === "collapsed";

  function toggleRail(): void {
    const next = collapsed ? "expanded" : "collapsed";
    setRail(next);
    storeRail(next);
  }

  return (
    <div className="flex h-full flex-col bg-bg text-fg">
      <header className="flex h-12 items-center gap-4 border-b border-line bg-surface px-4">
        <ClusterBadge />
        <div className="ml-auto flex items-center gap-2">
          <ConnectionIndicator />
          <IdentityBadge />
          <ThemeToggle />
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        <nav
          aria-label="Portal sections"
          className={
            "flex shrink-0 flex-col gap-4 border-r border-line bg-surface p-2 " +
            (collapsed ? "w-14" : "w-56")
          }
        >
          {/* The brand header. The mark is the connection indicator
              (memql#4180 keys its animation off the stream state); the
              wordmark is Squada One's one appearance in the chrome. */}
          <div
            className={
              "flex items-center gap-2.5 px-1.5 pt-1 " + (collapsed ? "justify-center" : "")
            }
          >
            <span className="text-accent">
              <MemqlMark size={24} />
            </span>
            {collapsed ? null : (
              <span className="font-display text-lg leading-none tracking-wide">
                MemQL Portal
              </span>
            )}
          </div>

          <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
            <NavGroup label="Operate" items={OPERATE} collapsed={collapsed} />
            <NavGroup label="Explore" items={EXPLORE} collapsed={collapsed} />
            <NavGroup label="Administer" items={ADMINISTER} collapsed={collapsed} />
          </div>

          <button
            type="button"
            onClick={toggleRail}
            aria-expanded={!collapsed}
            aria-label={collapsed ? "Expand the navigation rail" : "Collapse the navigation rail"}
            className="flex items-center justify-center rounded border border-line py-1 text-muted hover:bg-raised hover:text-fg"
          >
            {collapsed ? (
              <ChevronsRight size={14} aria-hidden="true" />
            ) : (
              <ChevronsLeft size={14} aria-hidden="true" />
            )}
          </button>
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
