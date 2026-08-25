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
import { DATA_ORIGINS_ROOT } from "../dataorigins/urls";
import {
  Archive,
  Bot,
  Boxes,
  Building2,
  ChevronDown,
  ChevronRight,
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
  Store,
  Users,
  Wrench,
  Blocks,
} from "../ui/icons";
import { fleetPath } from "../fleet/urls";
import { VIEWS, type ViewGroup } from "../views/registry";
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

// Registry group -> nav rows. ONE derivation, used by both captions that
// carry registry views, so a new row's placement is decided in registry.ts and
// nowhere else.
function navItemsInGroup(group: ViewGroup): readonly NavItem[] {
  return VIEWS.filter((view) => view.group === group).map((view) => ({
    to: viewPath(view.id),
    label: view.label,
    icon: VIEW_ICONS[view.id] ?? Boxes,
  }));
}

const VIEW_ICONS: Record<string, NavItem["icon"]> = {
  users: Users,
  agents: Bot,
  accounts: Building2,
  deployments: Rocket,
  audit: ScrollText,
};

// ============================================================================
// THE RAIL (memql#4264, restructured in memql#4527)
// ============================================================================
//
// The captions are the decision:
//
//   NEXUS    ONE GOAL OF YOURS, seen whole -- the goal itself, and the agents
//            working on it.
//   VIEWS    the DATA in this cluster, as screens, in two sub-sections:
//            BUILT-IN (the ones that ship with the product) and CUSTOM (the
//            ones this operator composed, plus the door to composing another).
//   BUILD    the substrate those screens are made of -- the concept registry
//            and the modules that declare concepts.
//   FLEET    WHERE WORK RUNS -- this person's machines, and the cluster's own
//            sandboxed workbenches (epic memql#4349). See the FLEET constant
//            below for why it is neither a view nor cluster administration.
//   LIBRARY  the operator's own MATERIAL -- what they put in and what the
//            cluster made for them (memql#4343).
//   CLUSTER  the cluster ITSELF rather than the data in it.
//
// What memql#4264 replaced was Operate / Explore / Administer, and it was
// wrong in a specific way: "Administer" is where a person goes looking to ADD
// someone, so the user population appeared twice -- once as the population
// under Operate and again as the change surface under Administer -- with the
// "By role" band rendering in both, and a third time on the admin overview.
// Two doors to one thing is a question an operator has to answer before they
// can work.
//
// "Views" is also the composer's own word (it saves v1:portalviews:view rows
// and calls them saved views), so the rail, the composer and the concept agree
// on one noun.
//
// ----------------------------------------------------------------------------
// WHY CUSTOM IS A SUB-SECTION AND NOT A CAPTION (memql#4527)
// ----------------------------------------------------------------------------
// It used to be a top-level group, under a comment that conceded the point:
// the composer's output sat "under the same caption as the views that ship
// with the product -- because to the person reading the rail they are the same
// kind of thing", and then took a caption of its own anyway. Two captions for
// one kind of thing costs a top-level slot and teaches the reader a
// distinction that is not there. So there is ONE Views caption now, and the
// built-in / composed split is a sub-section inside it -- which is what the
// split actually is: provenance, not category.
//
// The sub-section is named BUILT-IN rather than:
//
//   Native      -- already a data-origin state in this product (Mirror /
//                  Origin / Native, epic memql#4378), rendered on this
//                  console's own Data origins page. One word carrying two
//                  meanings in one product is the drift this restructure
//                  exists to remove.
//   Predefined  -- code vocabulary. It is what registry.ts calls them to
//                  itself, and it is clunky as chrome.
//
// ----------------------------------------------------------------------------
// WHY AGENTS MOVED UNDER NEXUS (memql#4527)
// ----------------------------------------------------------------------------
// It is derived, not hard-coded: the row carries `group: "nexus"` in
// registry.ts and the Nexus caption filters on it exactly as Views filters on
// "operate", so adding a view stays one registry row plus a body module. Its
// ROUTE is untouched -- /views/agents, because rail placement is not URL shape
// (Library's entries live at /artifacts for the same reason).

// The built-in views: the ones that ship with the product, in registry order.
const BUILT_IN_VIEWS: readonly NavItem[] = navItemsInGroup("operate");

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
// Library is the person's OWN MATERIAL: the things they put into this cluster
// and the things it made for them. Artifacts is the Library index -- files,
// generated outputs, notes, to-dos, calendar events, memories -- and
// Deployables (memql#4346) is what they published out of it.
//
// It is a FOURTH group rather than a sub-heading under Cluster, and the split
// is the decision (design 3.7). Artifacts sat in Cluster on the reasoning
// that it was "a FIXED, cluster-native surface rather than a composed view" --
// true, and the wrong axis. What Cluster means is the machine, not the data in
// it; a person's uploaded files are as much theirs as a view they composed.
// The moment the page grew upload, export, search and archive, keeping it
// beside Signing keys stopped describing anything.
//
// DEPLOYABLES MOVED HERE OUT OF CLUSTER, and it is the same axis argument one
// surface over. As Sites it was cluster-owner-only, so it genuinely was a fact
// about the machine. v1:platform:site declares the composite tier now
// (memql#4344): an ordinary person owns deployables, and a bundle they deployed
// is an artifact they published rather than infrastructure. It is listed ONCE,
// here -- the duplicate-door failure memql#4264 exists to prevent.
const LIBRARY: readonly NavItem[] = [
  { to: "/artifacts", label: "Artifacts", icon: Archive },
  { to: "/deployables", label: "Deployables", icon: Globe },
];

// Cluster is the machine, not the operator's own data -- that split is what
// keeps this group distinct from Views (designed dashboards over whatever
// concept an operator points one at) and, since memql#4343, from Library.
//
// The admin surfaces are listed individually rather than behind an
// "Administration" entry with its own tab strip: one level of nesting for
// five destinations bought nothing except a landing page that duplicated the
// console.
const CLUSTER: readonly NavItem[] = [{ to: "/integrations", label: "Integrations", icon: Plug }];
// The user population is NOT here. It is one of the views, and the verbs an
// admin needs live on the row detail there (memql#4264) -- which is what
// removed the second door.
const CLUSTER_ADMIN: readonly NavItem[] = [
  // What this cluster owns, mirrors and pushes out (epic memql#4378). In the
  // ADMIN half of Cluster because every read and every action behind it is
  // clusterOwner-tier in the engine -- the rail should not advertise a door
  // that will not open, which is the same rule Modules follows.
  { to: DATA_ORIGINS_ROOT, label: "Data origins", icon: Plug },
  // The stores a Shopify connector mirrors (epic memql#4389): credentials,
  // scopes, webhook subscriptions, the cost bucket. Here rather than in
  // Library for the reason directly above -- v1:shopify:store is
  // clusterOwner-tier and the page refuses anyone else, so a rail entry in
  // the half everyone sees would be a door that will not open. It sits next
  // to Data origins because the two are halves of one job: that page runs
  // backfill, reconciliation and pause for a DOMAIN, this one holds the
  // credentials and subscriptions of the STORE those domains come from.
  { to: "/stores", label: "Stores", icon: Store },
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
// Goals, then whatever the registry files under "nexus" -- Agents today
// (memql#4527). Agents belongs here rather than beside Users and Accounts
// because an agent is not a population an operator administers: it is what
// works on a goal, which is the question this caption answers.
const NEXUS: readonly NavItem[] = [
  { to: "/nexus", label: "Goals", icon: Orbit },
  ...navItemsInGroup("nexus"),
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

// A sub-section's own key, beside the rail's (memql#4527). ONE KEY PER
// SECTION rather than one serialized set: a set has to be parsed, and a
// half-written blob would take every sub-section down at once instead of the
// one it belongs to. Same try/catch tolerance for the same reason -- a rail
// preference is not worth failing a render over.
//
// DEFAULT EXPANDED, and the default is expressed as "anything that is not the
// string 'collapsed'". A person who has never touched the control, and a
// browser that refuses storage, both get the whole rail.
function sectionKey(id: string): string {
  return `${RAIL_STORAGE_KEY}-section-${id}`;
}

function readStoredSection(id: string): boolean {
  try {
    return globalThis.localStorage?.getItem(sectionKey(id)) !== "collapsed";
  } catch {
    return true;
  }
}

function storeSection(id: string, expanded: boolean): void {
  try {
    globalThis.localStorage?.setItem(sectionKey(id), expanded ? "expanded" : "collapsed");
  } catch {
    // See storeRail.
  }
}

// The rows themselves, extracted so a captioned group and a sub-section render
// the IDENTICAL row -- components/navRow.ts stays the one look for a nav row,
// and a second copy of this list is how the two drift.
function NavRows({
  id,
  items,
  collapsed,
  end = false,
  hidden = false,
}: {
  id?: string;
  items: readonly NavItem[];
  collapsed: boolean;
  // NavLink end-matching, for the one item whose path prefixes every other.
  end?: boolean;
  hidden?: boolean;
}): ReactNode {
  return (
    <ul
      {...(id === undefined ? {} : { id })}
      // BOTH the attribute and the utility. `hidden` takes the rows out of the
      // accessibility tree (so a closed sub-section's links are not reachable
      // by a screen reader or by Tab); `hidden:` -> display:none is the
      // Tailwind utility that does not depend on a preflight rule surviving.
      hidden={hidden}
      className={"space-y-0.5" + (hidden ? " hidden" : "")}
    >
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
  );
}

function NavGroup({
  label,
  items,
  collapsed,
  end = false,
}: {
  // Omitted renders an uncaptioned group -- the Home entry above the
  // captioned ones.
  label?: string;
  items: readonly NavItem[];
  collapsed: boolean;
  end?: boolean;
}): ReactNode {
  return (
    <div>
      {collapsed || label === undefined ? null : <RailCaption>{label}</RailCaption>}
      <NavRows items={items} collapsed={collapsed} end={end} />
    </div>
  );
}

// The group caption. Its own component because a sub-caption has to be
// visibly LESS than it and consistently so -- two hand-tuned class strings
// would answer that question twice.
function RailCaption({ children }: { children: ReactNode }): ReactNode {
  return (
    <h2 className="px-3 pb-1 text-xs font-semibold tracking-wide text-subtle uppercase">
      {children}
    </h2>
  );
}

// A sub-section inside a captioned group: a disclosure row and the rows it
// governs (memql#4527).
//
// THE DISCLOSURE IS A <button>, which is the whole of the keyboard story: it
// is focusable in source order, Enter and Space activate it, and
// `aria-expanded` says which way it is. Writing that by hand on a <div> is
// how a rail ends up with a control a keyboard cannot reach.
//
// THE COLLAPSED ICON RAIL FLATTENS. No sub-caption, no chevron, no disclosure
// -- the icons simply render in order, which is the rule the group captions
// already follow ("an icon column has no room to caption"). It also means a
// sub-section closed in the wide rail still shows its icons here, and that is
// deliberate: in an icon column there is no caption to explain why four of a
// person's destinations vanished, so hiding them would read as a bug rather
// than as a fold.
function NavSubGroup({
  id,
  label,
  items,
  collapsed,
}: {
  id: string;
  label: string;
  items: readonly NavItem[];
  collapsed: boolean;
}): ReactNode {
  const [expanded, setExpanded] = useState(() => readStoredSection(id));

  if (collapsed) return <NavRows items={items} collapsed />;

  function toggle(): void {
    const next = !expanded;
    setExpanded(next);
    storeSection(id, next);
  }

  const listId = `rail-section-${id}`;
  const Chevron = expanded ? ChevronDown : ChevronRight;
  return (
    <div>
      <button
        type="button"
        onClick={toggle}
        aria-expanded={expanded}
        aria-controls={listId}
        className={
          "flex w-full items-center gap-1 rounded px-2 pb-1 text-[11px] font-medium " +
          "tracking-wide text-subtle uppercase hover:text-fg"
        }
      >
        <Chevron size={12} className="shrink-0" aria-hidden="true" />
        <span className="truncate">{label}</span>
      </button>
      <NavRows id={listId} items={items} collapsed={false} hidden={!expanded} />
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
  // The composer's door is the last Custom row, always, so an operator with no
  // saved views still has somewhere to start. It reads COMPOSE (memql#4527):
  // the composer's own noun, and the one the route has said since it was
  // built. "New view" named the OUTCOME and then handed the person a surface
  // that calls the act composing -- one door, two words for it.
  const custom: readonly NavItem[] = [
    ...saved.views.map((view) => ({
      to: composedViewPath(view.id),
      label: view.name,
      icon: LayoutGrid,
    })),
    { to: "/compose", label: "Compose", icon: Plus },
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
              the rail, and not a group -- the flat-nav ruling stands.

              The `border-b` is the counterpart to RailStatus's `border-t`
              (memql#4521). The footer has separated itself since memql#4316 --
              its own comment calls that border "the change that makes it a
              footer" -- while the header had none, so the scrolling middle slid
              nav rows straight under the profile block with no boundary. Same
              token, same 2-unit pad on the inner face; both borders span the
              nav's content width, so the rail reads header / scroll region /
              footer in the collapsed w-14 state and the expanded w-56 one
              alike. */}
          <div className="border-b border-line pb-2 pt-1">
            <RailProfileLink collapsed={collapsed} />
          </div>

          <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto">
            <NavGroup items={[CONSOLE_ITEM]} collapsed={collapsed} end />
            <NavGroup label="Nexus" items={NEXUS} collapsed={collapsed} />
            {/* ONE Views caption, two sub-sections. The built-in / composed
                split is PROVENANCE rather than category -- both are screens
                over this cluster's data -- so it reads as a fold inside one
                group instead of a second top-level caption. The header block
                above has the full argument. */}
            <div>
              {collapsed ? null : <RailCaption>Views</RailCaption>}
              <div className={collapsed ? "space-y-0.5" : "space-y-2"}>
                <NavSubGroup
                  id="built-in"
                  label="Built-in"
                  items={BUILT_IN_VIEWS}
                  collapsed={collapsed}
                />
                <NavSubGroup id="custom" label="Custom" items={custom} collapsed={collapsed} />
              </div>
            </div>
            <NavGroup label="Build" items={build} collapsed={collapsed} />
            <NavGroup label="Fleet" items={FLEET} collapsed={collapsed} />
            <NavGroup label="Library" items={LIBRARY} collapsed={collapsed} />
            <NavGroup label="Cluster" items={cluster} collapsed={collapsed} />
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
            scroll region and extends the DOCUMENT instead, which is the
            reported symptom exactly: a second bar at the right edge running up
            past the header.

            The element that did it is a `sr-only` label deep inside
            ui/LabelChips (Tailwind's sr-only is `position: absolute`), on
            /fleet/machines -- one of the two pages screenshotted. Every
            ancestor between it and here was static, so its containing block
            was the viewport and its static position was 1186px down inside
            main's scrolled content. Measured: document scrollHeight 1187
            against a 1020 viewport; with this one declaration, 1020.

            Fixing it HERE rather than on that label is deliberate. There is
            nothing special about LabelChips: any sr-only span, any absolutely
            positioned decoration a page adds later, has the same escape unless
            something on its chain is positioned. This makes the page's own
            scroll region the containing block for the page's own out-of-flow
            content, which is what it should always have been. */}
        <main className="relative min-w-0 flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
