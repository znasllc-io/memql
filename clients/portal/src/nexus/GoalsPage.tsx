import { Link } from "react-router-dom";
import type { ReactNode } from "react";

import { Badge, Callout, Container, EmptyState, PageHeader, Skeleton } from "../ui";
import { NewGoalAction } from "./NewGoalDialog";
import { RunGoalButton } from "./RunGoal";
import { toneForStatus } from "./status";
import { nexusPath } from "./urls";
import { useGoals } from "./useGoals";

// /nexus -- your goals, as a page.
//
// ===========================================================================
// THIS REVERSES THE REDIRECT THE INDEX USED TO BE
// ===========================================================================
// The old index bounced straight to the most recent goal, arguing "the picker
// in the header is already the list and a second one would be a second door
// to the same place". That held while goals were few and mostly running. It
// stopped holding the day a population of goals carried TERMINAL statuses: a
// <select> renders no status, so three failed goals all read as still
// "planning" to the person who created them -- the operator was debugging an
// outage that the console knew, and would not say, had already been stamped
// failed. A list that shows each goal's status is not a second door to the
// picker's place; it answers a question the picker structurally cannot.
//
// The redirect is gone rather than kept beside the list: two doors to "where
// are my goals" with different behaviors is how a person learns to trust
// neither. Running goals are pinned to the top (useGoals' ordering), so the
// "jump back in" the redirect used to provide is the first row, one click.
//
// The empty-state copy is the invitation the old index carried (memql#4528);
// it moved here with the route.

export function GoalsPage(): ReactNode {
  const { goals, loading, error } = useGoals();

  // The empty state below is an invitation carrying its own New-goal button;
  // rendering the header's beside it is the same verb twice an inch apart,
  // and the empty state is the one doing the explaining.
  const inviting = !loading && error === "" && goals.length === 0;

  return (
    <Container>
      <section className="flex flex-col gap-6 pb-8">
        <PageHeader
          pageId="nexus"
          subtitle="Nexus"
          title="Goals"
          blurb="Everything you have asked for -- running first, then newest."
          {...(inviting ? {} : { actions: <NewGoalAction tone="primary" /> })}
        />
        {error !== "" ? (
          <Callout tone="danger" title="Your goals could not be read">
            {error}
          </Callout>
        ) : loading ? (
          <Skeleton variant="rows" rows={6} />
        ) : goals.length === 0 ? (
          <EmptyState
            statement={
              "You have no goals yet. Start one here, or ask an agent to do something " +
              "-- either way it lands on this map. The planner sizes a goal before it " +
              "spends anything, and asks you first if it is big."
            }
            action={<NewGoalAction tone="primary" />}
          />
        ) : (
          <ul
            aria-label="Your goals"
            className="flex flex-col divide-y divide-line overflow-hidden rounded border border-line bg-surface"
          >
            {goals.map((goal) => (
              <li key={goal.id} className="flex items-center gap-3 px-4 py-3">
                <div className="min-w-0 flex-1">
                  <Link
                    to={nexusPath(goal.id)}
                    className="block truncate text-sm font-medium hover:underline"
                  >
                    {goal.goal === "" ? goal.id : goal.goal}
                  </Link>
                  <div className="mt-0.5 text-xs text-muted">{whenCreated(goal.createdAt)}</div>
                </div>
                {/* The Run this row may need sits BESIDE the badge, not behind
                    the link: startPlan is a write, and a write reached through
                    what looks like navigation is a misclick away from spend. */}
                {goal.status === "queued" ? <RunGoalButton planId={goal.id} size="xs" /> : null}
                <Badge tone={toneForStatus(goal.status)}>{goal.status}</Badge>
              </li>
            ))}
          </ul>
        )}
      </section>
    </Container>
  );
}

// The list's "when" voice: an absolute moment an operator reads once,
// rendered the way the Fleet's formatMoment renders one. Local rather than
// imported from ../fleet/format -- that module is the Fleet's voice by its
// own header, and a cross-section import for one date format is a coupling
// neither section asked for.
function whenCreated(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "--";
  const parsed = new Date(trimmed);
  if (Number.isNaN(parsed.getTime())) return trimmed;
  return parsed.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
