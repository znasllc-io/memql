import type { ReactNode } from "react";

import { PageHeader, Tabs } from "../ui";
import { fleetPath, FLEET_SURFACES, type FleetSurface } from "./urls";

// The chrome the two Fleet screens share.
//
// Same grammar as AdminFrame: the header is the shared PageHeader, the surface
// strip is the shared Tabs idiom, and what changes between screens is the
// eyebrow. It is NOT AdminFrame itself -- these are not owner-only surfaces,
// so the eyebrow cannot be a role floor. A person with no special role has
// machines of their own and workspaces of their own, and the whole point of
// this pair of screens is that they see them.
//
// THE EYEBROW IS THE CONCEPT ID, which is what a predefined view puts there:
// the address of the rows, and the most useful single fact about the page for
// anybody who is going to go and query them.

export function FleetFrame({
  surface,
  eyebrow,
  actions,
  children,
}: {
  surface: FleetSurface;
  eyebrow: ReactNode;
  // Right-aligned, prominent. Absent when the caller can do nothing here.
  actions?: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <section className="flex min-h-full flex-col gap-6 pb-8">
      <PageHeader
        eyebrow={eyebrow}
        title={surface.title}
        blurb={surface.blurb}
        {...(actions === undefined ? {} : { actions })}
      />

      <div className="-mt-2">
        <Tabs
          label="Fleet"
          items={FLEET_SURFACES.map((one) => ({
            to: fleetPath(one.id),
            label: one.label,
            end: true,
          }))}
        />
      </div>

      {children}
    </section>
  );
}

// FleetTabs is the surface strip on its own (epic memql#4661, task
// memql#4674).
//
// Split out of FleetFrame because the two Fleet screens are ARRANGEMENTS now:
// their header, their version strip and their regenerate control come from
// ArrangedPage, and what remains that is genuinely theirs is this -- which of
// the two surfaces you are on.
//
// It sits OUTSIDE the arrangement deliberately, in ArrangedPage's `nav` slot.
// A tab bar is route-level navigation rather than a reading of a population,
// and an arrangement that placed it would be an arrangement a regeneration
// could remove -- leaving a page you cannot navigate away from.
export function FleetTabs(): ReactNode {
  return (
    <div className="-mt-2">
      <Tabs
        label="Fleet"
        items={FLEET_SURFACES.map((one) => ({
          to: fleetPath(one.id),
          label: one.label,
          end: true,
        }))}
      />
    </div>
  );
}

// LiveDegraded says the list has stopped being live, in the one place both
// screens need it.
//
// It is its own component rather than a line of JSX repeated twice because the
// SENTENCE matters: an operator reading a stale machine list has to know the
// staleness is the connection's and not the machine's, or they go and debug a
// computer that is working. useConceptRows keeps this state separate from the
// read error for the same reason.
export function LiveDegraded({ reason, noun }: { reason: string; noun: string }): ReactNode {
  if (reason === "") return null;
  return (
    <p className="rounded border border-warn bg-warn-subtle px-3 py-2 text-xs text-fg">
      Live updates are off for this page: {reason}. What is listed below is what the last read
      returned -- a {noun} that changed since then will not show it here, and one that has gone
      quiet will still look reachable. Reload to read again.
    </p>
  );
}
