import { Outlet, useOutletContext, useParams } from "react-router-dom";
import type { ReactNode } from "react";

import { Badge, Callout, Container, ErrorNotice, PageHeader, Skeleton, Tabs } from "../ui";
import { useGoalWorld } from "./feed/useGoalWorld";
import { GoalPicker } from "./GoalPicker";
import { NewGoalAction } from "./NewGoalDialog";
import { RunGoalButton } from "./RunGoal";
import { toneForStatus } from "./status";
import { useGoals, type Goal } from "./useGoals";
import { NEXUS_SURFACES, surfacePath } from "./urls";
import type { GoalWorld } from "./scene/world";

// The chrome every Nexus page shares, and the one place a goal's world is
// read.
//
// ===========================================================================
// WHY THE WORLD IS READ HERE AND NOT PER PAGE
// ===========================================================================
// Map, Constructs and Replay are three questions about ONE goal, and the
// seed is seven reads plus seven subscriptions. Reading it per page would
// re-run all fourteen on every tab click and restart the map's arrival
// animations each time -- the same reason the concept browser nests its row
// route under the concept route rather than beside it (routes.tsx: "opening
// a row re-renders the pane without restarting paging").
//
// So this is a LAYOUT route. It holds the feed, renders the header and the
// tab strip, and hands the world down through the outlet context.
//
// ===========================================================================
// THE FOUR THINGS THAT CAN BE WRONG, AND WHY THEY ARE FOUR
// ===========================================================================
// loading / error / missing / refused are separate states because the
// remedies are different, and a page that collapses them tells a person to do
// the wrong thing:
//
//   loading   nothing to do but wait
//   error     the read failed -- retry is meaningful
//   missing   there is no such goal -- the link is stale or from another
//             cluster; retrying will not help
//   refused   the goal exists and is SOMEONE ELSE'S -- see useGoalWorld's
//             header on why that check is client-side today (memql#4366) and
//             why saying so plainly is better than a 404 that would imply the
//             goal does not exist

export interface GoalContext {
  world: GoalWorld;
  planId: string;
  liveDegraded: string;
}

export function useGoalContext(): GoalContext {
  return useOutletContext<GoalContext>();
}

export function GoalLayout(): ReactNode {
  const { planId = "" } = useParams();
  const { world, loading, error, refused, missing, liveDegraded } = useGoalWorld(planId);
  const { goals } = useGoals();

  const plan = world.plan;
  const status = plan?.status ?? "";

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          pageId="nexus"
          subtitle="Nexus"
          // The plan id stays in the MONO slot, which is the one case that
          // slot is for: this is a row's own address, and it is what an
          // operator pastes into a query or quotes in a thread.
          eyebrow={planId}
          title={plan === null ? (loading ? "Loading the goal" : "Goal") : plan.goal || planId}
          blurb="A goal's world, as the system works on it."
          // The picker is the LIST; this is the VERB. Both in the header
          // because that is where a person already goes to change which goal
          // they are looking at, and "start another" is the same gesture one
          // step further (memql#4528).
          actions={
            <>
              <GoalPicker goals={goals} currentId={planId} />
              {/* self-end so the button's bottom edge lines up with the
                  picker's SELECT rather than centring against the whole
                  field, whose label adds a line above it. */}
              <div className="self-end">
                <NewGoalAction />
              </div>
            </>
          }
          meta={
            status === "" ? undefined : (
              <span className="inline-flex items-center gap-2">
                <Badge tone={toneForStatus(status)}>{status}</Badge>
                {/* The Run a queued goal is, by definition, waiting for --
                    dsl/planner/concepts.memql: "planning complete, tasks
                    emitted, waiting for a human to click Run". RunGoal.tsx's
                    header says why it renders here and nowhere else. */}
                {status === "queued" ? <RunGoalButton planId={planId} /> : null}
              </span>
            )
          }
        />

        <div className="-mt-2">
          <Tabs
            label="Nexus"
            items={NEXUS_SURFACES.map((surface) => ({
              to: surfacePath(surface.id, planId),
              label: surface.label,
              // `end` on the Map only: its path prefixes the other two, so
              // without it the Map tab stays lit on Constructs and Replay.
              // The node route IS under the Map, and the Map tab should stay
              // lit there -- which is why this is `end` on the tab whose own
              // children belong to it rather than on all three.
              end: surface.id === "map",
            }))}
          />
        </div>

        {liveDegraded === "" ? null : (
          <Callout tone="warn" title="This map is not live">
            The cluster accepted the reads but refused the change feed, so the scene
            shows the goal as it was when this page loaded and will not move.{" "}
            {liveDegraded}
          </Callout>
        )}

        {refused !== "" ? (
          <Callout tone="warn" title="Not your goal">
            {refused}
          </Callout>
        ) : missing ? (
          <Callout tone="neutral" title="No such goal">
            This cluster has no goal with that id. It may have been deleted, or the link
            may name a goal from another cluster.
          </Callout>
        ) : error !== "" ? (
          <ErrorNotice sentence="Could not read this goal." detail={error} />
        ) : loading && plan === null ? (
          <Skeleton variant="rows" rows={6} />
        ) : (
          <Outlet context={{ world, planId, liveDegraded } satisfies GoalContext} />
        )}
      </section>
    </Container>
  );
}

// toneForStatus moved to ./status: the goals list and the recent-goals strip
// read it too, and both sit across the GoalLayout -> GoalPicker import edge.

// A goal the picker knows about but the feed has not read yet.
export type { Goal };
