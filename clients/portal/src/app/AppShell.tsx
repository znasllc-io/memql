import { useState, type ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { navClass } from "../components/navRow";
import { RailHandle } from "../components/RailHandle";
import { RailMark } from "../components/RailMark";
import { RailProfileLink } from "../components/RailProfileLink";
import { RailStatus } from "../components/RailStatus";
import { ThemeToggle } from "../components/ThemeToggle";
import { useAdminAccess } from "../admin/useAdminConsole";
import { CommandPalette } from "../palette/CommandPalette";
import { Outlet } from "react-router-dom";
import {
  DESTINATIONS,
  destinationIsActive,
  destinationPath,
  type NavDestination,
} from "./nav";

// The routed layout: a branded header, a nav rail, and an <Outlet>.
//
// THE BRAND LIVES IN THE HEADER (memql#4316). The mark doubles as the
// connection indicator (memql#4180 drives its animation states), and the
// wordmark beside it is the display face's one chrome appearance.
//
// THE RAIL reads top to bottom as person, places, machine: RailProfileLink
// (who you are, linking to /me), the seven destinations, then RailStatus
// behind a border-t (which node, which version). The collapse control is a
// tab on the rail's own edge (RailHandle).
//
// ===========================================================================
// SEVEN FLAT ROWS (memql#4655, decision D1)
// ===========================================================================
// This file used to carry the nav itself -- six captioned groups, two
// collapsible sub-sections with their own localStorage keys, a live
// saved-views read, and a by-id icon map with a fallback. All of it is gone.
// The nav is DATA now, in app/nav.ts, walked by this rail, by the command
// palette, by the frames' tab strips and by two repo-root gates. One
// definition, four readers -- because the failure the old shape produced was
// Fleet appearing under two captions and nobody noticing for a release.
//
// WHAT LEFT THE RAIL, AND WHERE IT WENT:
//
//   the saved views      -> the Views gallery at /views (and the palette,
//                           live, which is where a person with forty of them
//                           was always going to look)
//   Compose              -> the gallery's "New view" action
//   Modules              -> a tab on Concepts
//   the Fleet trio       -> tabs on Fleet, which they already were
//   Artifacts/Deployables-> tabs on Library
//   the six admin rows   -> tabs on Cluster
//   the group captions   -> nothing. Seven items need no grouping; that is
//                           the whole argument for seven.
//
// The rail is not shorter by hiding things. Everything above is one keystroke
// away in the palette (Cmd+K), which is what makes a flat rail safe rather
// than lossy.
//
// ===========================================================================
// ACTIVE STATE IS COMPUTED, NOT NavLink's
// ===========================================================================
// Library spans /artifacts and /deployables; Cluster spans /integrations,
// /data-origins, /stores and /admin/*. NavLink can only match one path, so a
// destination declares the prefixes it OWNS and the row asks nav.ts. That is
// also what let the restructure move zero routes: rail placement is not URL
// shape, so nothing needed a redirect.

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

function RailRow({
  destination,
  role,
  collapsed,
  pathname,
}: {
  destination: NavDestination;
  role: string;
  collapsed: boolean;
  pathname: string;
}): ReactNode {
  const Icon = destination.icon;
  const active = destinationIsActive(destination, pathname);
  return (
    <li>
      {/* A plain Link, because the ACTIVE decision is nav.ts's rather than the
          router's -- see the header. NavLink here would compute a second,
          disagreeing answer for Library and Cluster. */}
      <Link
        to={destinationPath(destination, role)}
        aria-current={active ? "page" : undefined}
        className={navClass(active, collapsed)}
        {...(collapsed ? { title: destination.label, "aria-label": destination.label } : {})}
      >
        <Icon size={16} className="shrink-0" aria-hidden="true" />
        {collapsed ? null : <span className="truncate">{destination.label}</span>}
      </Link>
    </li>
  );
}

export function AppShell(): ReactNode {
  const [rail, setRail] = useState<"expanded" | "collapsed">(() => readStoredRail());
  const collapsed = rail === "collapsed";
  const { role } = useAdminAccess();
  const { pathname } = useLocation();

  function toggleRail(): void {
    const next = collapsed ? "expanded" : "collapsed";
    setRail(next);
    storeRail(next);
  }

  return (
    <div className="flex h-full flex-col bg-bg text-fg">
      {/* Named, because a page renders its own <header> too and "the chrome
          header" should be addressable by what it IS rather than by which
          words happen to be unique in the document today. */}
      <header
        aria-label="Portal header"
        className="flex h-12 items-center gap-4 border-b border-line bg-surface px-4"
      >
        {/* The brand. The mark is the connection indicator (memql#4180 keys
            its animation off the stream state); the wordmark is Squada One's
            one appearance in the chrome. */}
        <div className="flex min-w-0 items-center gap-2.5">
          <RailMark size={24} />
          <span className="font-display text-lg leading-none tracking-wide">MemQL Portal</span>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <ThemeToggle />
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        {/* `relative` is what lets the handle straddle this element's right
            border. It is a child of the <nav> rather than a sibling because
            it is part of the rail -- a control parked outside it would be
            findable by neither the rail's landmark nor the main region's. */}
        <nav
          aria-label="Portal sections"
          className={
            "relative flex shrink-0 flex-col gap-4 border-r border-line bg-surface p-2 " +
            (collapsed ? "w-14" : "w-56")
          }
        >
          <RailHandle collapsed={collapsed} onToggle={toggleRail} />

          {/* Who you are, before the places you can go. Absolutely first in
              the rail, and not a group -- the flat-nav ruling stands.

              The `border-b` is the counterpart to RailStatus's `border-t`
              (memql#4521): the rail reads header / scroll region / footer in
              the collapsed w-14 state and the expanded w-56 one alike. */}
          <div className="border-b border-line pb-2 pt-1">
            <RailProfileLink collapsed={collapsed} />
          </div>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
            <ul className="space-y-0.5">
              {DESTINATIONS.map((destination) => (
                <RailRow
                  key={destination.id}
                  destination={destination}
                  role={role}
                  collapsed={collapsed}
                  pathname={pathname}
                />
              ))}
            </ul>
          </div>

          <RailStatus collapsed={collapsed} />
        </nav>

        {/* min-h-0 on both axes so a long row list scrolls inside the main
            pane instead of stretching the page and losing the header.

            `relative` IS THE SECOND SCROLLBAR FIX (memql#4505), and it is not
            cosmetic. An absolutely-positioned element is laid out against its
            nearest POSITIONED ancestor, and if it has none, against the
            viewport -- in which case `overflow: auto` here does not clip it,
            because main is not on its containing-block chain. It escapes the
            scroll region and extends the DOCUMENT instead: a second bar at the
            right edge running up past the header.

            The element that did it is a `sr-only` label deep inside
            ui/LabelChips (Tailwind's sr-only is `position: absolute`), on
            /fleet/machines. Every ancestor between it and here was static, so
            its containing block was the viewport and its static position was
            1186px down inside main's scrolled content. Measured: document
            scrollHeight 1187 against a 1020 viewport; with this one
            declaration, 1020.

            Fixing it HERE rather than on that label is deliberate. There is
            nothing special about LabelChips: any sr-only span, any absolutely
            positioned decoration a page adds later, has the same escape unless
            something on its chain is positioned. */}
        <main className="relative min-w-0 flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>

      {/* Cmd+K. Mounted at the shell so it is reachable from every routed
          page, and rendering nothing until it is opened. It is what makes the
          seven-item rail safe rather than lossy (memql#4656). */}
      <CommandPalette />
    </div>
  );
}
