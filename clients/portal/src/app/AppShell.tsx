import { useState, type ComponentType, type ReactNode } from "react";
import { NavLink, Outlet } from "react-router-dom";

import { navClass } from "../components/navRow";
import { RailHandle } from "../components/RailHandle";
import { RailMark } from "../components/RailMark";
import { RailProfileLink } from "../components/RailProfileLink";
import { RailStatus } from "../components/RailStatus";
import { ThemeToggle } from "../components/ThemeToggle";
import { composedViewPath } from "../compose/urls";
import { useSavedViews } from "../compose/useSavedViews";
import { CONCEPTS_ROOT } from "../concepts/urls";
import {
  Archive,
  Bot,
  Boxes,
  Building2,
  Globe,
  Inbox,
  Gauge,
  LayoutGrid,
  Monitor,
  Orbit,
  Plug,
  Plus,
  Rocket,
  ScrollText,
  Shield,
  Users,
  Wrench,
  Blocks,
} from "../ui/icons";
import { fleetPath } from "../fleet/urls";
import { VIEWS } from "../views/registry";
import { useAdminAccess } from "../admin/useAdminConsole";
import { viewPath } from "../views/urls";

// The routed layout: a branded header, a nav rail, and an <Outlet>.
//
// THE BRAND LIVES IN THE HEADER (memql#4316). The mark doubles as the
// connection indicator (memql#4180 drives its animation states), and the
// wordmark beside it is the display face's one chrome appearance. It moved
// out of the rail because the header had nothing to anchor it and the rail
// had the brand, the collapse chevron and no profile -- exactly backwards.
//
// WHAT THE HEADER NO LONGER SAYS is "Cluster: <hostname>". That value was
// window.location.host: the page's own origin, which is to say the address
// bar, retyped one line lower. It was also the only thing on the left of the
// header, so the chrome's most prominent slot carried its least informative
// fact. The individual replica serving this stream is a DIFFERENT fact and it
// still renders -- in the rail footer, where RailStatus labels the connection
// dot with it.
//
// THE RAIL now reads top to bottom as person, places, machine:
// RailProfileLink (who you are, linking to /me), the nav groups, then
// RailStatus behind a border-t (which node, which version). The collapse
// control is a tab on the rail's own edge (RailHandle) rather than a chevron
// floating in a brand row it did not belong to.
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

// ============================================================================
// THE RAIL (memql#4264)
// ============================================================================
//
// Three groups, and the names are the decision:
//
//   VIEWS    the DATA in this cluster, as screens. The five that ship with the
//            product, then the ones this operator composed.
//   BUILD    the substrate those screens are made of -- the concept registry
//            and the modules that declare concepts.
//   FLEET    WHERE WORK RUNS -- this person's machines, and the cluster's own
//            sandboxed workbenches (epic memql#4349). See the FLEET constant
//            below for why it is neither a view nor cluster administration.
//   CLUSTER  the cluster ITSELF rather than the data in it.
//
// What this replaced was Operate / Explore / Administer, and it was wrong in a
// specific way: "Administer" is where a person goes looking to ADD someone, so
// People appeared twice -- once as the population under Operate and again as
// the change surface under Administer -- with the "By role" band rendering in
// both, and a third time on the admin overview. Two doors to one thing is a
// question an operator has to answer before they can work.
//
// "Views" is also the composer's own word (it saves v1:portalviews:view rows
// and calls them saved views), so the rail, the composer and the concept now
// agree on one noun.

const PREDEFINED_VIEWS: readonly NavItem[] = VIEWS.filter(
  (view) => view.group === "operate",
).map((view) => ({
  to: viewPath(view.id),
  label: view.label,
  icon: VIEW_ICONS[view.id] ?? Boxes,
}));

// Build is the SUBSTRATE: the whole concept registry, plus the modules that
// declare what is in it. Modules is owner/admin territory (memql#4191) --
// below that access the item is HIDDEN rather than shown-and-refused, because
// the engine refuses the reads anyway and the rail should not advertise a door
// that will not open.
const BUILD: readonly NavItem[] = [{ to: CONCEPTS_ROOT, label: "Concepts", icon: Boxes }];
const MODULES_ITEM: NavItem = { to: "/modules", label: "Modules", icon: Blocks };

// Fleet is WHERE WORK RUNS (epic memql#4349), and it sits between Build and
// Cluster because it is neither. A machine is somebody's own computer, reached
// over a stream it opened and revocable by its owner -- so it is not cluster
// administration, and gating it behind the admin items would hide from a
// person the list of their own computers. A workbench is the cluster's own
// sandbox rather than anyone's data, so it is not a View either.
//
// Both entries are visible at every role, deliberately. The reads behind them
// are caller-scoped in the ENGINE (a person sees their own machines and
// workspaces; the all-cluster reads open with actor.isClusterOwner), so the
// rail is not advertising a door that will not open -- unlike Modules, which
// is hidden below owner/admin because there the whole surface is refused.
const FLEET: readonly NavItem[] = [
  { to: fleetPath("machines"), label: "Machines", icon: Monitor },
  // Local apps (memql#4363) sits beside Machines rather than under Cluster:
  // it is about work running on the person's OWN computer, which is the
  // question this group answers. Two doors to one thing is what the rail's
  // reshuffle removed, so the apps surface is a Fleet tab, not a second
  // Machines entry.
  { to: fleetPath("apps"), label: "Local apps", icon: Bot },
  { to: fleetPath("workbenches"), label: "Workbenches", icon: Wrench },
];

// Cluster is the machine, not the operator's OWN data -- that split is what
// keeps this group distinct from Views (designed dashboards over whatever
// concept an operator points one at). Artifacts sits here rather than there
// for the same reason Sites does: both are a FIXED, cluster-native surface
// (the Library that indexes what this cluster produces; the hosted web
// surfaces it serves) rather than a composed view over arbitrary rows.
//
// The admin surfaces are listed individually rather than behind an
// "Administration" entry with its own tab strip: one level of nesting for
// five destinations bought nothing except a landing page that duplicated the
// console.
const CLUSTER: readonly NavItem[] = [
  { to: "/integrations", label: "Integrations", icon: Plug },
  { to: "/sites", label: "Sites", icon: Globe },
  { to: "/artifacts", label: "Artifacts", icon: Archive },
];
// People is NOT here. It is one of the views, and the verbs an admin needs
// live on the row detail there (memql#4264) -- which is what removed the
// second door.
const CLUSTER_ADMIN: readonly NavItem[] = [
  { to: "/admin/tokens", label: "Sessions and tokens", icon: Inbox },
  { to: "/admin/keys", label: "Signing keys", icon: Shield },
  { to: "/admin/settings", label: "Settings", icon: Shield },
];

// The console is the rail's first destination: the landing surface every group
// hangs under (memql#4182). It is called Console because that is what the page
// it opens has always called itself -- the rail said "Home" and the heading
// said "Console", and a rail that disagrees with its own destination is the
// kind of small wrongness that makes a product feel unfinished (memql#4263).
// `end` matching keeps it inactive on deep routes.
const CONSOLE_ITEM: NavItem = { to: "/", label: "Console", icon: Gauge };

// NEXUS (memql#4369) is its own group rather than an entry under Views or
// Cluster, and the distinction it draws is real. A view is a designed screen
// over a POPULATION of rows; Cluster is the machine. Nexus is neither: it is
// ONE GOAL OF YOURS, seen whole -- what is working on it, what it built, and
// how it got here. It is the only surface in this console whose subject is a
// single piece of your own work rather than a table or a machine, and filing
// it under either of those would make it look like something it is not.
//
// One item today. The group exists because the section will grow into it
// (the cluster-wide view the design explicitly leaves out of scope is a
// second inhabitant), and because a rail caption is how a person learns a
// section's name.
const NEXUS: readonly NavItem[] = [{ to: "/nexus", label: "Goals", icon: Orbit }];

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

function NavGroup({
  label,
  items,
  collapsed,
  end = false,
}: {
  // Omitted renders an uncaptioned group -- the Home entry above the three
  // captioned ones.
  label?: string;
  items: readonly NavItem[];
  collapsed: boolean;
  // NavLink end-matching, for the one item whose path prefixes every other.
  end?: boolean;
}): ReactNode {
  return (
    <div>
      {collapsed || label === undefined ? null : (
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
                end={end}
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
  const { canAdminister } = useAdminAccess();
  const build = canAdminister ? [...BUILD, MODULES_ITEM] : BUILD;
  const cluster = canAdminister ? [...CLUSTER, ...CLUSTER_ADMIN] : CLUSTER;

  // The operator's own composed views, live: a view saved a moment ago is in
  // the rail without a reload (useSavedViews subscribes). They are listed by
  // NAME because that is what the person called them -- the composer stores a
  // free-text name and this is where it earns its keep.
  const saved = useSavedViews();
  const custom: readonly NavItem[] = [
    ...saved.views.map((view) => ({
      to: composedViewPath(view.id),
      label: view.name,
      icon: LayoutGrid,
    })),
    { to: "/compose", label: "New view", icon: Plus },
  ];

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
              the rail, and not a group -- the flat-nav ruling stands. */}
          <div className="pt-1">
            <RailProfileLink collapsed={collapsed} />
          </div>

          <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
            <NavGroup items={[CONSOLE_ITEM]} collapsed={collapsed} end />
            <NavGroup label="Nexus" items={NEXUS} collapsed={collapsed} />
            <NavGroup label="Views" items={PREDEFINED_VIEWS} collapsed={collapsed} />
            {/* The composer's own output, under the same caption as the views
                that ship with the product -- because to the person reading the
                rail they are the same kind of thing. "Custom" is the only word
                distinguishing them, and the last entry is always the door to
                making another one. */}
            <NavGroup label="Custom" items={custom} collapsed={collapsed} />
            <NavGroup label="Build" items={build} collapsed={collapsed} />
            <NavGroup label="Fleet" items={FLEET} collapsed={collapsed} />
            <NavGroup label="Cluster" items={cluster} collapsed={collapsed} />
          </div>

          <RailStatus collapsed={collapsed} />
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
