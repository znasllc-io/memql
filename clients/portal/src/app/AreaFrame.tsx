import type { ReactNode } from "react";

import { useAdminAccess } from "../admin/useAdminConsole";
import { PageHeader, Tabs } from "../ui";
import { destinationById, visibleTabs } from "./nav";

// The chrome an AREA's pages share: the page header, then the area's tab
// strip (decision D2 -- the rail lists areas, a page's sub-surfaces are its
// tabs, never both).
//
// ===========================================================================
// ONE FRAME, NOT FOUR
// ===========================================================================
// The epic asks for a LibraryFrame and a ClusterFrame beside the existing
// FleetFrame and AdminFrame. Written out, those four files would differ by a
// string and a label -- and the drift they would accumulate is precisely the
// drift memql#4502's Tabs consolidation removed one level down, where three
// hand-rolled secondary navs meant an operator re-learned "current section"
// per area.
//
// So there is one frame, parameterised by the destination it belongs to, and
// `destinationId` is a UNION rather than a string: a page naming an area that
// does not exist -- or one it is not part of -- is a type error rather than a
// tab strip that silently renders empty.
//
// ===========================================================================
// A ONE-TAB STRIP IS NOT RENDERED
// ===========================================================================
// Tab visibility is role-gated (nav.ts), so a reader on Cluster may be
// offered exactly one tab. A strip with a single item is not navigation; it
// is an underline under a word, and it invites the reader to look for the
// others. Below two, the frame is just a page header -- which is what the
// page would have been anyway.

// The areas that have tab strips. Console, Nexus and Views have no
// sub-surfaces at this altitude and use PageHeader directly.
export type AreaId = "concepts" | "fleet" | "library" | "cluster";

export function AreaFrame({
  area,
  pageId,
  title,
  subtitle,
  blurb,
  actions,
  meta,
  children,
}: {
  area: AreaId;
  // The guide registry key for THIS page -- a tab id, not the area's.
  pageId: string;
  title: ReactNode;
  subtitle?: ReactNode;
  blurb?: ReactNode;
  // Right-aligned, prominent. Absent when the caller can do nothing here.
  actions?: ReactNode;
  meta?: ReactNode;
  children: ReactNode;
}): ReactNode {
  const { role } = useAdminAccess();
  const destination = destinationById(area);
  const tabs = destination === undefined ? [] : visibleTabs(destination, role);

  return (
    <section className="flex min-h-full flex-col gap-6 pb-8">
      <PageHeader
        pageId={pageId}
        title={title}
        {...(subtitle === undefined ? {} : { subtitle })}
        {...(blurb === undefined ? {} : { blurb })}
        {...(actions === undefined ? {} : { actions })}
        {...(meta === undefined ? {} : { meta })}
      />

      {tabs.length > 1 && destination !== undefined ? (
        <div className="-mt-2">
          <Tabs
            label={destination.label}
            items={tabs.map((tab) => ({
              to: tab.to,
              label: tab.label,
              ...(tab.end === undefined ? {} : { end: tab.end }),
            }))}
          />
        </div>
      ) : null}

      {children}
    </section>
  );
}
